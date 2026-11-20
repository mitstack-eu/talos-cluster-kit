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

// newSite is a cluster with a network, a firewall, a worker image built on cx33 unless builder changes that spec, and
// one control-plane node no pool plan may touch, whose node report reads every member Ready when it is asked.
func newSite(t *testing.T, builder ...func(*controlplane.ServerSpec)) *site {
	t.Helper()
	ctx := context.Background()
	s := &site{t: t, drained: map[int64]bool{}, clock: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		notReady: map[string]bool{}, openBao: map[string]string{}, sealed: map[string]bool{}}
	s.cloud = cloudtest.New(s.tick)
	s.cloud.InstantBuild = true
	s.plane = s.newPlane()
	var err error
	s.network, err = s.cloud.CreateNetwork(ctx, controlplane.NetworkSpec{Name: "net", Labels: s.plane.Labels("network")})
	require.NoError(t, err)
	s.firewall, err = s.cloud.CreateFirewall(ctx, controlplane.FirewallSpec{Name: "fw", Labels: s.plane.Labels("firewall")})
	require.NoError(t, err)
	builderSpec := controlplane.ServerSpec{Name: "builder", ServerType: "cx33", Location: "fsn1", Labels: s.plane.Labels(controlplane.RoleWorkerBuilder), Stopped: true}
	for _, opt := range builder {
		opt(&builderSpec)
	}
	builders, err := s.cloud.CreateServers(ctx, builderSpec,
		controlplane.ServerSpec{Name: "node", ServerType: "cx33", Location: "fsn1", Labels: s.plane.Labels(controlplane.RoleNode)})
	require.NoError(t, err)
	s.image, err = s.cloud.StartSnapshot(ctx, builders[0].ID, controlplane.SnapshotSpec{Labels: s.plane.Labels(controlplane.RoleWorkerImage)})
	require.NoError(t, err)
	return s
}

// imagedOn runs the worker builder on serverType, so the worker image holds that type's disk.
func imagedOn(serverType string) func(*controlplane.ServerSpec) {
	return func(builder *controlplane.ServerSpec) { builder.ServerType = serverType }
}

// newPlane is the plane a block builds afresh on each call, so a test that replaces it stands in for a restart.
func (s *site) newPlane() controlplane.Plane {
	return controlplane.Plane{API: s.cloud, Block: "hcloud-hrobot-talos-cluster", Subscription: "sub_0123456789ab", AppName: "shop",
		Now: s.tick, ReadReport: s.readReport, Guard: redundanttest.Guard}
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

func TestPlanCloud_WithTheWorkerImageLargerThanThePoolsFirstTypeTheMemberTakesTheFirstTypeWhoseDiskHoldsIt(t *testing.T) {
	s := newSite(t, imagedOn("cx43"))

	plan, err := s.plan(cloudPool(1, "fsn1"))

	require.NoError(t, err)
	require.Len(t, plan.Create, 1, "the servers planned")
	assert.Equal(t, "cx43", plan.Create[0].ServerType, "the member's type for the worker image's %g GB", s.image.DiskGB)
}

func TestPlanCloud_APoolOverNbg1AndHel1GrowsItsBurstMemberInHel1WhenNbg1SellsNoTypeWhoseDiskHoldsTheWorkerImage(t *testing.T) {
	for _, tc := range []struct {
		name string
		nbg1 []string
	}{
		{"nbg1 sells none of the pool's types", []string{"cx33", "cx43"}},
		{"nbg1 sells only cx33, whose disk is smaller than the worker image", []string{"cx43"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSite(t, imagedOn("cx43"))
			s.cloud.SoldOutIn = map[string][]string{"nbg1": tc.nbg1}
			burst := pools.CloudPool{ServerTypes: []string{"cx33", "cx43"}, Locations: []string{"nbg1", "hel1"}, Min: 0, Max: 1, IdleWindow: 15 * time.Minute}

			plan, err := s.plan(burst)

			require.NoError(t, err)
			require.Len(t, plan.Create, 1, "the servers planned")
			assert.Equal(t, "hel1", plan.Create[0].Location, "the member's location")
			assert.Equal(t, "cx43", plan.Create[0].ServerType, "the member's type")
			assert.True(t, pools.IsBurst(controlplane.Server{Labels: plan.Create[0].Labels}), "the member above the pool's minimum is a burst member: %v", plan.Create[0].Labels)
		})
	}
}

func TestPlanCloud_RefusesAPoolNoListedLocationOfWhichSellsATypeWhoseDiskHoldsTheWorkerImageNamingEveryLocation(t *testing.T) {
	s := newSite(t, imagedOn("cx43"))
	s.cloud.SoldOutIn = map[string][]string{"nbg1": {"cx43"}, "hel1": {"cx33", "cx43"}}

	_, err := s.plan(cloudPool(1, "nbg1", "hel1"))

	assert.Equal(t, "no-server-type-available", refusalClass(t, err))
	for _, want := range []string{"nbg1", "hel1", "160 GB", "cx33 has 80 GB"} {
		assert.Contains(t, err.Error(), want, "the refusal names every location, the worker image's disk and each type's disk")
	}
}

func TestPlanCloud_APoolRefusalNamesItsLocationsInTheOrderThePoolListsThemWhicheverHoldsFewerMembers(t *testing.T) {
	s := newSite(t, imagedOn("cx43"))
	s.reach(cloudPool(1, "nbg1", "hel1"))
	require.Equal(t, "nbg1", s.members()[0].Location, "precondition: the member the pool already holds")
	s.cloud.SoldOutIn = map[string][]string{"nbg1": {"cx43"}, "hel1": {"cx43"}}

	_, err := s.plan(cloudPool(2, "nbg1", "hel1"))

	assert.Equal(t, "no-server-type-available", refusalClass(t, err))
	assert.Contains(t, err.Error(), "in nbg1 or hel1 now", "the locations the refusal names, in the pool's order")
	assert.Contains(t, err.Error(), "(nbg1: cx33 has 80 GB, cx43 is not on sale; hel1: ", "the details, in the same order")
}

func TestPlanCloud_AMemberCreateHetznerRefusesIsPlannedAgainFromTheTypesReadAgain(t *testing.T) {
	s := newSite(t, imagedOn("cx43"))
	pool := cloudPool(1, "nbg1", "hel1")
	hetzner := errors.New("creating server shop-nbg1-1: server type cx43 is unavailable (resource_unavailable)")
	s.cloud.FailCreate[controlplane.RoleCloudWorker] = hetzner
	refused, err := s.plan(pool)
	require.NoError(t, err)
	require.ErrorIs(t, refused.Apply(context.Background(), s.cloud), hetzner, "precondition: the create Hetzner refused")
	s.cloud.SoldOutIn = map[string][]string{"nbg1": {"cx43"}}

	s.reach(pool)

	require.Len(t, s.members(), 1, "the pool's members")
	assert.Equal(t, "hel1", s.members()[0].Location, "the member's location once nbg1 sells only cx33")
	assert.Equal(t, "cx43", s.members()[0].ServerType, "the member's type")
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

func TestPlanCloud_PlansNothingWhileTheNetworkRecordsAWorkerReplacement(t *testing.T) {
	s := newSite(t)
	s.reach(cloudPool(1, "fsn1"))
	labels := maps.Clone(s.network.Labels)
	labels[pools.KeyWorkerReplace+"fsn1-1"] = "7-1794605517"
	require.NoError(t, s.cloud.SetNetworkLabels(context.Background(), s.network.ID, labels))

	plan, err := s.plan(pools.CloudPool{ServerTypes: []string{"cx43"}, Locations: []string{"fsn1", "nbg1"}, Min: 2, Max: 2})

	require.NoError(t, err)
	assert.True(t, plan.Empty(), "the plan of a pool raised to two of another type while a replacement is recorded: %+v", plan)
}

func TestPlanCloud_PlansNothingWhileTheNetworkRecordsAWorkerRemoval(t *testing.T) {
	s := newSite(t)
	s.reach(cloudPool(1, "fsn1"))
	labels := maps.Clone(s.network.Labels)
	labels[pools.KeyWorkerRemove+"fsn1-1"] = "7-1794605517"
	require.NoError(t, s.cloud.SetNetworkLabels(context.Background(), s.network.ID, labels))

	plan, err := s.plan(cloudPool(3, "fsn1", "nbg1"))

	require.NoError(t, err)
	assert.True(t, plan.Empty(), "the plan of a pool raised to three while a removal is recorded: %+v", plan)
}

func TestPlanCloud_PassesOnAListFailureWithoutPlanningFromAPartialRead(t *testing.T) {
	s := newSite(t)
	s.cloud.FailNext = errors.New("hetzner is down")

	_, err := s.plan(cloudPool(1, "fsn1"))

	assert.ErrorContains(t, err, "hetzner is down")
}

// scaling is a pool of lowest to highest over fsn1 and nbg1, sized to want as the pool step sizes it from a decision.
func scaling(lowest, want int) pools.CloudPool {
	return pools.CloudPool{ServerTypes: []string{"cx33", "cx43"}, Locations: []string{"fsn1", "nbg1"}, Min: lowest, Max: want, IdleWindow: 15 * time.Minute}
}

func bursts(servers []controlplane.Server) []string {
	var out []string
	for _, srv := range servers {
		if pools.IsBurst(srv) {
			out = append(out, srv.Labels[pools.KeyMember])
		}
	}
	slices.Sort(out)
	return out
}

func burstSpecs(specs []controlplane.ServerSpec) []string {
	var out []string
	for _, spec := range specs {
		if spec.Labels[pools.KeyBurst] == "true" {
			out = append(out, spec.Labels[pools.KeyMember])
		}
	}
	return out
}

func TestPlanCloud_AJustCreatedMemberAboveThePoolsMinimumCarriesTheBurstLabelBeforeItReadsReady(t *testing.T) {
	s := newSite(t)

	plan, err := s.plan(scaling(2, 3))

	require.NoError(t, err)
	require.Len(t, plan.Create, 3)
	assert.Equal(t, []string{"fsn1-2"}, burstSpecs(plan.Create), "the member created past the minimum of 2")
	assert.Equal(t, "mitstack.dev/burst", pools.KeyBurst, "the label each burst member and its node carry")
}

func TestPlanCloud_APoolOfASetSizeCreatesNoBurstMember(t *testing.T) {
	s := newSite(t)

	plan, err := s.plan(cloudPool(3, "fsn1", "nbg1"))

	require.NoError(t, err)
	require.Len(t, plan.Create, 3)
	assert.Empty(t, burstSpecs(plan.Create), "the members of a pool of 3 to 3")
}

func TestPlanCloud_APoolThatLostASteadyMemberCreatesItsSuccessorSteadyAndTheBurstMemberKeepsItsLabel(t *testing.T) {
	s := newSite(t)
	s.reach(scaling(2, 3))
	require.Equal(t, []string{"fsn1-2"}, bursts(s.members()), "the pool of two steady members and one burst member")
	lost := slices.IndexFunc(s.members(), func(srv controlplane.Server) bool { return !pools.IsBurst(srv) })
	require.NoError(t, s.cloud.DeleteServer(context.Background(), s.members()[lost].ID))

	plan, err := s.plan(scaling(2, 3))

	require.NoError(t, err)
	require.Len(t, plan.Create, 1, "the lost member's successor")
	assert.Empty(t, burstSpecs(plan.Create), "the successor is steady")
	assert.Equal(t, []string{"fsn1-2"}, bursts(s.members()), "the burst member keeps its label")
}

func labelled(servers []controlplane.Server) map[string]bool {
	out := map[string]bool{}
	for _, srv := range servers {
		out[srv.Labels[pools.KeyMember]] = pools.IsBurst(srv)
	}
	return out
}

func TestReclassify_LabelsTheMembersPastTheMinimumBurstOrSteadyAsThePoolsBoundsChange(t *testing.T) {
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	member := func(key string, minutes int, burst bool) controlplane.Server {
		labels := map[string]string{pools.KeyMember: key}
		if burst {
			labels[pools.KeyBurst] = "true"
		}
		return controlplane.Server{ID: int64(minutes + 1), Name: "shop-cloud-worker-" + key, Location: key[:4], Created: at.Add(time.Duration(minutes) * time.Minute), Labels: labels}
	}
	setOfThree := []controlplane.Server{member("fsn1-1", 0, false), member("nbg1-1", 1, false), member("fsn1-2", 2, false)}
	cases := []struct {
		name    string
		pool    pools.CloudPool
		members []controlplane.Server
		want    map[string]bool
	}{
		{"a set-size pool of 3 upgraded to a minimum of 1 and a maximum of 3 labels the two newest of the fullest locations burst",
			scaling(1, 3), setOfThree, map[string]bool{"fsn1-2": true, "nbg1-1": true}},
		{"a pool that scales upgraded back to a set size of 3 takes the label off every burst member",
			cloudPool(3, "fsn1", "nbg1"), []controlplane.Server{member("fsn1-1", 0, false), member("nbg1-1", 1, true), member("fsn1-2", 2, true)},
			map[string]bool{"nbg1-1": false, "fsn1-2": false}},
		{"a pool that lost a steady member with room to create its successor relabels none",
			scaling(2, 3), []controlplane.Server{member("fsn1-1", 0, false), member("nbg1-1", 1, true)}, map[string]bool{}},
		{"a pool whose minimum is raised to its maximum's members with no room left takes the label off the oldest burst member",
			scaling(2, 3), []controlplane.Server{member("fsn1-1", 0, false), member("nbg1-1", 1, true), member("fsn1-2", 2, true)},
			map[string]bool{"nbg1-1": false}},
		{"a pool at its minimum of steady members relabels none",
			scaling(2, 5), []controlplane.Server{member("fsn1-1", 0, false), member("nbg1-1", 1, false), member("fsn1-2", 2, true)}, map[string]bool{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pools.Reclassify(tc.pool, tc.members)

			assert.Equal(t, tc.want, labelled(got), "each relabelled member, true when it becomes burst")
		})
	}
}

func TestReclassify_LeavesAMemberWhoseRemovalIsUnderWayAsItIs(t *testing.T) {
	leaving := controlplane.Server{ID: 3, Name: "shop-cloud-worker-fsn1-2", Location: "fsn1", Labels: map[string]string{pools.KeyMember: "fsn1-2"}}
	leaving.Labels = controlplane.BeginRemoval(leaving, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), "cloud-pool")
	members := []controlplane.Server{{ID: 1, Location: "fsn1", Labels: map[string]string{pools.KeyMember: "fsn1-1"}}, leaving}

	got := pools.Reclassify(scaling(1, 3), members)

	assert.Empty(t, got, "the steady member at the minimum and the member leaving")
}
