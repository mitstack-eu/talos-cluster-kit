package pools_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
)

type site struct {
	t        *testing.T
	cloud    *cloudtest.Fake
	plane    controlplane.Plane
	network  controlplane.Network
	firewall controlplane.Firewall
	image    controlplane.Snapshot
	drained  map[int64]bool
}

// newSite is a cluster with a network, a firewall, a worker image and one control-plane node, which no pool plan may touch.
func newSite(t *testing.T) *site {
	t.Helper()
	ctx := context.Background()
	clock := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	s := &site{t: t, drained: map[int64]bool{}}
	s.cloud = cloudtest.New(func() time.Time { clock = clock.Add(time.Minute); return clock })
	s.cloud.InstantBuild = true
	s.plane = controlplane.Plane{API: s.cloud, Block: "hcloud-hrobot-talos-cluster", Subscription: "sub_0123456789ab", AppName: "shop"}
	var err error
	s.network, err = s.cloud.CreateNetwork(ctx, controlplane.NetworkSpec{Name: "net", Labels: s.plane.Labels("network")})
	require.NoError(t, err)
	s.firewall, err = s.cloud.CreateFirewall(ctx, controlplane.FirewallSpec{Name: "fw", Labels: s.plane.Labels("firewall")})
	require.NoError(t, err)
	builders, err := s.cloud.CreateServers(ctx,
		controlplane.ServerSpec{Name: "builder", ServerType: "cx33", Location: "fsn1", Labels: s.plane.Labels(controlplane.RoleWorkerBuilder), Stopped: true},
		controlplane.ServerSpec{Name: "node", ServerType: "cx33", Location: "fsn1", Labels: s.plane.Labels(controlplane.RoleNode)})
	require.NoError(t, err)
	s.image, err = s.cloud.StartSnapshot(ctx, builders[0].ID, controlplane.SnapshotSpec{Labels: s.plane.Labels(controlplane.RoleWorkerImage)})
	require.NoError(t, err)
	return s
}

func cloudPool(size int, locations ...string) pools.CloudPool {
	return pools.CloudPool{ServerTypes: []string{"cpx32", "cx43"}, Locations: locations, Min: size, Max: size}
}

func (s *site) plan(pool pools.CloudPool) (pools.CloudPlan, error) {
	s.t.Helper()
	return pools.PlanCloud(context.Background(), s.plane, pool, func(srv controlplane.Server) bool { return s.drained[srv.ID] })
}

// reach plans pool and applies the plan until it plans nothing, as a block's upgrade does.
func (s *site) reach(pool pools.CloudPool) {
	s.t.Helper()
	for range 10 {
		plan, err := s.plan(pool)
		require.NoError(s.t, err)
		if plan.Empty() {
			return
		}
		for _, srv := range plan.Drain {
			s.drained[srv.ID] = true
		}
		require.NoError(s.t, plan.Apply(context.Background(), s.cloud))
	}
	require.Fail(s.t, "the plan never settled")
}

func (s *site) members() []controlplane.Server {
	s.t.Helper()
	servers, err := s.cloud.ListServers(context.Background(), s.plane.RoleSelector(pools.RoleCloudWorker))
	require.NoError(s.t, err)
	return servers
}

func locationsOf(servers []controlplane.Server) []string {
	var out []string
	for _, srv := range servers {
		out = append(out, srv.Location)
	}
	slices.Sort(out)
	return out
}

func refusalClass(t *testing.T, err error) string {
	t.Helper()
	var refusal *controlplane.Refusal
	require.ErrorAs(t, err, &refusal)
	return refusal.Class
}

func TestPlanCloud_APoolOfTwoOverTwoLocationsPlansOneServerInEachFromTheWorkerImage(t *testing.T) {
	s := newSite(t)

	plan, err := s.plan(cloudPool(2, "fsn1", "nbg1"))

	require.NoError(t, err)
	require.Len(t, plan.Create, 2)
	for i, location := range []string{"fsn1", "nbg1"} {
		spec := plan.Create[i]
		assert.Equal(t, location, spec.Location, "server %d's location", i)
		assert.Equal(t, "cpx32", spec.ServerType, "server %d's type", i)
		assert.Equal(t, s.image.ID, spec.ImageID, "server %d's image", i)
		assert.Empty(t, spec.UserData, "the image embeds the node's configuration")
		assert.Equal(t, []int64{s.network.ID}, spec.NetworkIDs, "server %d's network", i)
		assert.Equal(t, []int64{s.firewall.ID}, spec.FirewallIDs, "server %d's firewall", i)
		assert.Equal(t, map[string]string{
			controlplane.KeyBlock: s.plane.Block, controlplane.KeySubscription: s.plane.Subscription,
			controlplane.KeyRole: string(pools.RoleCloudWorker), pools.KeyMember: location + "-1",
		}, spec.Labels, "server %d's labels", i)
	}
	assert.NotEqual(t, plan.Create[0].Name, plan.Create[1].Name, "two servers named alike")
	assert.Empty(t, plan.Drain)
	assert.Empty(t, plan.Delete)
}

func TestPlanCloud_APoolRaisedFromOneToTwoPlansOneServerInTheLocationWithNone(t *testing.T) {
	s := newSite(t)
	s.reach(cloudPool(1, "fsn1", "nbg1"))
	require.Equal(t, []string{"fsn1"}, locationsOf(s.members()), "the pool of one")

	plan, err := s.plan(cloudPool(2, "fsn1", "nbg1"))

	require.NoError(t, err)
	require.Len(t, plan.Create, 1)
	assert.Equal(t, "nbg1", plan.Create[0].Location, "the new server's location")
	assert.Empty(t, plan.Drain)
	assert.Empty(t, plan.Delete)
}

func TestPlanCloud_APoolLoweredDrainsTheMemberFirstAndDeletesItOnlyOnceItsNodeIsGone(t *testing.T) {
	s := newSite(t)
	s.reach(cloudPool(2, "fsn1", "nbg1"))
	leaving := s.members()[slices.IndexFunc(s.members(), func(srv controlplane.Server) bool { return srv.Location == "nbg1" })]

	before, err := s.plan(cloudPool(1, "fsn1", "nbg1"))
	require.NoError(t, err)
	s.drained[leaving.ID] = true
	after, err := s.plan(cloudPool(1, "fsn1", "nbg1"))
	require.NoError(t, err)

	assert.Equal(t, []int64{leaving.ID}, ids(before.Drain), "the member drained while its node holds pods")
	assert.Empty(t, before.Delete, "a member deleted before its node drained")
	assert.Empty(t, after.Drain, "a member drained after its node is gone")
	assert.Equal(t, []int64{leaving.ID}, ids(after.Delete), "the member deleted once its node is gone")
}

func ids(servers []controlplane.Server) []int64 {
	var out []int64
	for _, srv := range servers {
		out = append(out, srv.ID)
	}
	return out
}

func TestPlanCloud_APoolLoweredRemovesTheNewestMemberOfTheLocationHoldingTheMost(t *testing.T) {
	s := newSite(t)
	s.reach(cloudPool(3, "fsn1", "nbg1"))
	byLocation := map[string][]controlplane.Server{}
	for _, srv := range s.members() {
		byLocation[srv.Location] = append(byLocation[srv.Location], srv)
	}
	require.Len(t, byLocation["fsn1"], 2, "the pool of three over two locations")
	newest := slices.MaxFunc(byLocation["fsn1"], func(a, b controlplane.Server) int { return a.Created.Compare(b.Created) })

	plan, err := s.plan(cloudPool(2, "fsn1", "nbg1"))

	require.NoError(t, err)
	assert.Equal(t, []int64{newest.ID}, ids(plan.Drain))
}

func TestPlanCloud_APoolAppliedLeavesTheControlPlaneAndEveryOtherServerAsItWas(t *testing.T) {
	s := newSite(t)
	nodes := func() []int64 {
		servers, err := s.cloud.ListServers(context.Background(), s.plane.RoleSelector(controlplane.RoleNode))
		require.NoError(t, err)
		return ids(servers)
	}
	before := nodes()

	s.reach(cloudPool(2, "fsn1", "nbg1"))
	s.reach(cloudPool(1, "fsn1", "nbg1"))
	s.reach(cloudPool(0, "fsn1", "nbg1"))

	assert.Equal(t, before, nodes(), "the control-plane nodes")
	assert.Empty(t, s.members(), "the pool of zero")
}

func TestPlanCloud_AMemberInALocationTheListDropsIsRemovedBeforeItsSuccessorIsCreatedSoThePoolNeverPassesItsMaximum(t *testing.T) {
	s := newSite(t)
	s.reach(cloudPool(1, "fsn1"))
	moved := cloudPool(1, "nbg1")

	first, err := s.plan(moved)
	require.NoError(t, err)
	s.reach(moved)

	assert.Empty(t, first.Create, "a server created while the old member still stands at the maximum")
	assert.Len(t, first.Drain, 1, "the old member drains first")
	assert.Equal(t, []string{"nbg1"}, locationsOf(s.members()), "the pool once it settled")
}

func TestPlanCloud_AMemberOfATypeTheListDropsIsReplacedByAServerOfAListedType(t *testing.T) {
	s := newSite(t)
	s.reach(cloudPool(1, "fsn1"))
	changed := pools.CloudPool{ServerTypes: []string{"cx43"}, Locations: []string{"fsn1"}, Min: 1, Max: 1}

	s.reach(changed)

	require.Len(t, s.members(), 1)
	assert.Equal(t, "cx43", s.members()[0].ServerType)
}

func TestPlanCloud_ATypeSoldOutFallsBackToTheNextTypeListed(t *testing.T) {
	s := newSite(t)
	s.cloud.SoldOut = []string{"cpx32"}

	plan, err := s.plan(cloudPool(1, "fsn1"))

	require.NoError(t, err)
	require.Len(t, plan.Create, 1)
	assert.Equal(t, "cx43", plan.Create[0].ServerType)
}

func TestPlanCloud_RefusesALocationWhereNoListedTypeIsAvailable(t *testing.T) {
	s := newSite(t)
	s.cloud.SoldOut = []string{"cpx32", "cx43"}

	_, err := s.plan(cloudPool(1, "fsn1"))

	assert.Equal(t, "no-server-type-available", refusalClass(t, err))
}

func TestPlanCloud_RefusesAPoolBeforeTheWorkerImageExists(t *testing.T) {
	s := newSite(t)
	require.NoError(t, s.cloud.DeleteSnapshot(context.Background(), s.image.ID))

	_, err := s.plan(cloudPool(1, "fsn1"))

	assert.Equal(t, "worker-image-missing", refusalClass(t, err))
}

func TestPlanCloud_TwoMembersOfOneLocationTakeNamesOfTheirOwn(t *testing.T) {
	s := newSite(t)

	s.reach(cloudPool(2, "fsn1"))

	names := map[string]bool{}
	for _, srv := range s.members() {
		names[srv.Name] = true
	}
	assert.Len(t, names, 2, "member names %v", names)
}

func TestPlanCloud_PassesOnAListFailureWithoutPlanningFromAPartialRead(t *testing.T) {
	s := newSite(t)
	s.cloud.FailNext = errors.New("hetzner is down")

	_, err := s.plan(cloudPool(1, "fsn1"))

	assert.ErrorContains(t, err, "hetzner is down")
}
