package pools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/internal/redundanttest"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

type site struct {
	t        *testing.T
	cloud    *cloudtest.Fake
	plane    controlplane.Plane
	network  controlplane.Network
	firewall controlplane.Firewall
	image    controlplane.Snapshot
	drained  map[int64]bool
	clock    time.Time
	// notReady names the members the node report reads not Ready, openBao each OpenBao member's server, sealed the
	// members it reads sealed, and noReport whether no report has come since the removal began.
	notReady map[string]bool
	openBao  map[string]string
	sealed   map[string]bool
	noReport bool
}

func (s *site) tick() time.Time {
	s.clock = s.clock.Add(time.Minute)
	return s.clock
}

// newSite is a cluster with a network, a firewall, a worker image and one control-plane node, which no pool plan may
// touch, whose node report reads every member Ready when it is asked.
func newSite(t *testing.T) *site {
	t.Helper()
	ctx := context.Background()
	s := &site{t: t, drained: map[int64]bool{}, clock: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		notReady: map[string]bool{}, openBao: map[string]string{}, sealed: map[string]bool{}}
	s.cloud = cloudtest.New(s.tick)
	s.cloud.InstantBuild = true
	s.plane = controlplane.Plane{API: s.cloud, Block: "hcloud-hrobot-talos-cluster", Subscription: "sub_0123456789ab", AppName: "shop",
		Now: s.tick, ReadReport: s.readReport, Guard: redundanttest.Guard}
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

// readReport is the report a node would send now: each member's node, the pod and seal state of each OpenBao member.
func (s *site) readReport(context.Context, time.Time) (*report.Read, error) {
	if s.noReport {
		return nil, nil
	}
	type node struct {
		Name  string `json:"name"`
		Ready bool   `json:"ready"`
	}
	type pod struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Node      string `json:"node"`
	}
	type member struct {
		Name        string `json:"name"`
		Initialized bool   `json:"initialized"`
		Sealed      bool   `json:"sealed"`
	}
	var body struct {
		Nodes   []node `json:"nodes"`
		Pods    []pod  `json:"pods"`
		OpenBao struct {
			Members []member `json:"members"`
		} `json:"openbao"`
	}
	for _, srv := range s.members() {
		body.Nodes = append(body.Nodes, node{Name: srv.Name, Ready: !s.notReady[srv.Name]})
	}
	for _, name := range slices.Sorted(maps.Keys(s.openBao)) {
		body.Pods = append(body.Pods, pod{Namespace: "openbao", Name: name, Node: s.openBao[name]})
		body.OpenBao.Members = append(body.OpenBao.Members, member{Name: name, Initialized: true, Sealed: s.sealed[name]})
	}
	b, err := json.Marshal(body)
	require.NoError(s.t, err)
	return &report.Read{Body: b, At: s.tick()}, nil
}

// begin plans pool and applies the plan that begins a member's removal.
func (s *site) begin(pool pools.CloudPool) {
	s.t.Helper()
	plan, err := s.plan(pool)
	require.NoError(s.t, err)
	require.NotEmpty(s.t, plan.Mark, "a member whose removal begins")
	require.NoError(s.t, plan.Apply(context.Background(), s.cloud))
}

func cloudPool(size int, locations ...string) pools.CloudPool {
	return pools.CloudPool{ServerTypes: []string{"cx33", "cx43"}, Locations: locations, Min: size, Max: size}
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
		assert.Equal(t, "cx33", spec.ServerType, "server %d's type", i)
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
	s.begin(cloudPool(1, "fsn1", "nbg1"))

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
	assert.Equal(t, []int64{newest.ID}, ids(plan.Mark), "the member whose removal begins")
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
	assert.Len(t, first.Mark, 1, "the old member's removal begins first")
	assert.Equal(t, []string{"nbg1"}, locationsOf(s.members()), "the pool once it settled")
}

// lowered is a pool of three, one member in each location, lowered to two, which removes the member in hel1.
func lowered(t *testing.T, s *site) (pools.CloudPool, map[string]controlplane.Server) {
	t.Helper()
	s.reach(cloudPool(3, "fsn1", "nbg1", "hel1"))
	byLocation := map[string]controlplane.Server{}
	for _, srv := range s.members() {
		byLocation[srv.Location] = srv
	}
	require.Len(t, byLocation, 3, "the pool of three over three locations")
	return cloudPool(2, "fsn1", "nbg1", "hel1"), byLocation
}

func TestPlanCloud_ALoweredPoolDrainsNoWorkerWhileTheOpenBaoMemberOnAnotherWorkerReadsSealedNamingOpenBaosQuorum(t *testing.T) {
	s := newSite(t)
	pool, members := lowered(t, s)
	s.openBao = map[string]string{"openbao-0": members["hel1"].Name, "openbao-1": members["fsn1"].Name, "openbao-2": members["nbg1"].Name}
	s.sealed["openbao-1"] = true
	s.begin(pool)

	plan, err := s.plan(pool)

	require.NoError(t, err)
	assert.Empty(t, plan.Drain, "a member drained")
	assert.Empty(t, plan.Delete, "a member deleted")
	require.Len(t, plan.Held, 1, "the removals held")
	assert.Contains(t, plan.Held[0], "refused:redundancy-below-minimum")
	assert.Contains(t, plan.Held[0], `removing "openbao-0" from the OpenBao members`, "the hold names OpenBao's quorum")
	assert.Contains(t, plan.Held[0], `"openbao-1" read not serving at`, "the hold names the sealed member")
	assert.False(t, plan.Empty(), "a pool whose removal is held stands as wanted")
}

func TestPlanCloud_ALoweredPoolDrainsNoWorkerWhileAnotherWorkerReadsNotReady(t *testing.T) {
	s := newSite(t)
	pool, members := lowered(t, s)
	s.notReady[members["nbg1"].Name] = true
	s.begin(pool)

	plan, err := s.plan(pool)

	require.NoError(t, err)
	assert.Empty(t, plan.Drain, "a member drained")
	require.Len(t, plan.Held, 1, "the removals held")
	assert.Contains(t, plan.Held[0], fmt.Sprintf("removing %q from the Cloud pool's workers", members["hel1"].Name))
	assert.Contains(t, plan.Held[0], fmt.Sprintf("%q read not serving at", members["nbg1"].Name))
}

func TestPlanCloud_ALoweredPoolDrainsNoWorkerUntilANodeReportComesAfterItsRemovalBegan(t *testing.T) {
	s := newSite(t)
	pool, members := lowered(t, s)
	s.begin(pool)
	s.noReport = true

	held, err := s.plan(pool)
	require.NoError(t, err)
	s.noReport = false
	drained, err := s.plan(pool)
	require.NoError(t, err)

	assert.Empty(t, held.Drain, "a member drained with no report since its removal began")
	require.Len(t, held.Held, 1, "the removals held")
	assert.Contains(t, held.Held[0], "no node report")
	assert.Equal(t, []int64{members["hel1"].ID}, ids(drained.Drain), "the member drained once a report reads the rest serving")
}

func TestPlanCloud_ALoweredPoolDrainsTheWorkerWhoseOpenBaoMemberLeavesAMajorityUnsealed(t *testing.T) {
	s := newSite(t)
	pool, members := lowered(t, s)
	s.openBao = map[string]string{"openbao-0": members["hel1"].Name, "openbao-1": members["fsn1"].Name, "openbao-2": members["nbg1"].Name}
	s.begin(pool)

	plan, err := s.plan(pool)

	require.NoError(t, err)
	assert.Empty(t, plan.Held, "the removals held")
	assert.Equal(t, []int64{members["hel1"].ID}, ids(plan.Drain), "the member drained")
}

func TestPlanCloud_APoolRaisedAgainTakesTheRemovalLabelsOffTheMemberItKeeps(t *testing.T) {
	s := newSite(t)
	pool, members := lowered(t, s)
	s.begin(pool)

	s.reach(cloudPool(3, "fsn1", "nbg1", "hel1"))

	kept := map[int64]map[string]string{}
	for _, srv := range s.members() {
		kept[srv.ID] = srv.Labels
	}
	require.Contains(t, kept, members["hel1"].ID, "the member the raised pool keeps")
	assert.NotContains(t, kept[members["hel1"].ID], controlplane.KeyRemovalBegan, "a kept member's removal start")
	assert.NotContains(t, kept[members["hel1"].ID], controlplane.KeyRemovalFor, "a kept member's removal plan")
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
	s.cloud.SoldOut = []string{"cx33"}

	plan, err := s.plan(cloudPool(1, "fsn1"))

	require.NoError(t, err)
	require.Len(t, plan.Create, 1)
	assert.Equal(t, "cx43", plan.Create[0].ServerType)
}

func TestPlanCloud_RefusesALocationWhereNoListedTypeIsAvailable(t *testing.T) {
	s := newSite(t)
	s.cloud.SoldOut = []string{"cx33", "cx43"}

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
