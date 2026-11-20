package controlplane_test

// A plane builds a three-node Talos cluster one change a call: the API and ingress load balancers in fsn1, gateway and
// builder, then the snapshot, then one node in each of fsn1, nbg1 and hel1 booted from it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/internal/redundanttest"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBlock    = "a-block"
	testSub      = "sub_0123456789ab"
	testApp      = "shop"
	testRepoURL  = "https://github.com/acme/shop-cluster"
	platformURL  = "https://api.mitstack.dev"
	proofScript  = "echo report-proof\n"
	settleBudget = controlplane.BuildDeadline + 15*time.Minute
	mib          = 1 << 20
)

var testStart = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// harness is one plane under test against the Cloud fake, with the clock the builder's deadline is judged by.
type harness struct {
	t     *testing.T
	cloud *cloudtest.Fake
	plane controlplane.Plane
	now   time.Time
	// buildTakes and imageTakes are how long the builder and the snapshot run once started; a zero build never ends.
	buildTakes, imageTakes time.Duration
	// installed holds each cluster the Installed hook received, and installErr is what the hook answers. The hook keeps
	// the gateway CA as the plane's, as a block keeps it as its own secret.
	installed  []nodeconfig.Cluster
	installErr error
	ca         *gateway.CA
	lostCA     bool
	// events holds each detail the plane's Notify received, failing the gateway addresses a report reads failing, and
	// unread those a report names no gateway at.
	events  []string
	failing []string
	unread  []string
	// servesAfter is how long after its creation a gateway's mirror first answers a report.
	servesAfter time.Duration
	// images is the image list of each node report withReports sends, reportsStopAt when the cluster stops sending them,
	// and noImageList sends a report from before the list.
	images        []report.ImageRef
	reportsStopAt time.Time
	noImageList   bool
	// hostingKeys holds the keys each call to a hosting plane's DeployKeys hook received, and deployKeysErr is what it answers.
	hostingKeys   []controlplane.HostingKeys
	deployKeysErr error
	// waits holds how many Hetzner actions each call waited on, and steps what each step taken was for.
	waits      []int
	steps      []string
	sequential bool
}

func newHarness(t *testing.T, opts ...func(*harness)) *harness {
	t.Helper()
	h := &harness{t: t, now: testStart, buildTakes: 10 * time.Minute, imageTakes: 3 * time.Minute}
	h.cloud = cloudtest.New(func() time.Time { return h.now })
	h.plane = controlplane.Plane{
		API: h.cloud, Block: testBlock, Subscription: testSub, AppName: testApp, ServerTypes: []string{controlplane.DefaultServerType},
		Boundary: "mitstack-test-boundary", PlatformURL: platformURL, RepoURL: testRepoURL, ReportProofScript: proofScript,
		Now: func() time.Time { return h.now }, Guard: redundanttest.Guard,
		Installed: func(_ context.Context, _ controlplane.Network, c nodeconfig.Cluster, ca gateway.CA) error {
			h.installed = append(h.installed, c)
			if h.installErr == nil {
				h.ca = &ca
			}
			return h.installErr
		},
		Notify: func(_ context.Context, detail string) { h.events = append(h.events, detail) },
	}
	for _, opt := range opts {
		opt(h)
	}
	if !h.sequential {
		t.Parallel()
	}
	return h
}

// sequential keeps a test that swaps a package variable off the parallel run.
func sequential(h *harness) { h.sequential = true }

func (h *harness) read() controlplane.State {
	h.t.Helper()
	st, err := h.plane.Read(context.Background())
	require.NoError(h.t, err)
	return st
}

func (h *harness) ensure() error {
	ctx := context.Background()
	before := h.cloud.Waits()
	defer func() { h.waits = append(h.waits, h.cloud.Waits()-before) }()
	st, err := h.plane.Read(ctx)
	if err != nil {
		return err
	}
	if !h.lostCA {
		h.plane.GatewayCA = h.ca
	}
	if err := h.plane.Refusal(st); err != nil {
		return err
	}
	network, err := h.plane.EnsureNetwork(ctx, st)
	if err != nil {
		return err
	}
	step, ok, err := h.plane.NextStep(ctx, st, network)
	if err != nil || !ok {
		return err
	}
	h.steps = append(h.steps, step.What)
	return step.Take(ctx, testBlock, testSub)
}

func (h *harness) converge() error {
	h.t.Helper()
	for range 50 {
		before := h.cloud.Writes()
		if err := h.ensure(); err != nil || h.cloud.Writes() == before {
			return err
		}
	}
	h.t.Fatal("ensure still changed the cluster after 50 calls")
	return nil
}

func (h *harness) settle(stop func() bool) error {
	h.t.Helper()
	return h.advance(settleBudget, func() bool {
		return stop != nil && stop() || len(h.plane.Problems(context.Background(), h.read())) == 0
	})
}

// advance converges, then lets a minute pass for Hetzner and the builder, until done reads true after a call, a call
// fails, or the budget runs out.
func (h *harness) advance(budget time.Duration, done func() bool) error {
	h.t.Helper()
	for began := h.now; h.now.Sub(began) < budget; {
		if err := h.converge(); err != nil {
			return err
		}
		if done() {
			return nil
		}
		h.now = h.now.Add(time.Minute)
		h.cloud.Advance(h.buildTakes, h.imageTakes)
	}
	return errors.New("the plane did not settle within the budget")
}

func (h *harness) built() {
	h.t.Helper()
	require.NoError(h.t, h.settle(nil))
	require.Empty(h.t, h.plane.Problems(context.Background(), h.read()), "the plane's problems once settled")
}

func (h *harness) building() bool {
	builders := h.byRole(controlplane.RoleBuilder)
	return len(builders) == 1 && builders[0].Status == controlplane.ServerRunning
}

func (h *harness) buildingBuilder() {
	h.t.Helper()
	h.buildTakes = 0
	require.NoError(h.t, h.settle(h.building))
	require.True(h.t, h.building(), "the builder never started its build")
}

func (h *harness) byRole(r controlplane.Role) []controlplane.Server {
	return h.cloud.Selected(h.t, h.plane.RoleSelector(r))
}

func (h *harness) network() controlplane.Network {
	return h.cloud.OnlyNetwork(h.t, h.plane.Selector())
}

// held is the server of role r that Hetzner still runs an action on.
func (h *harness) held(r controlplane.Role) controlplane.Server {
	h.t.Helper()
	for _, s := range h.byRole(r) {
		if command, err := h.cloud.RunningAction(context.Background(), s.ID); err == nil && command != "" {
			return s
		}
	}
	h.t.Fatalf("no %s runs an action", r)
	return controlplane.Server{}
}

// sent counts the actions of command the server was sent.
func (h *harness) sent(id int64, command string) int {
	return len(slices.DeleteFunc(h.cloud.ActionsSent(id), func(c string) bool { return c != command }))
}

func (h *harness) loadBalancer(r controlplane.Role) controlplane.LoadBalancer {
	return h.cloud.OnlyLoadBalancer(h.t, h.plane.RoleSelector(r))
}

func refusalClass(t *testing.T, err error) string {
	t.Helper()
	var refusal *controlplane.Refusal
	require.ErrorAs(t, err, &refusal, "the error %v is no refusal", err)
	return refusal.Class
}

func TestPlane_BuildsANodeInEachOfFsn1Nbg1AndHel1OnOneNetwork(t *testing.T) {
	h := newHarness(t)

	h.built()

	network := h.network()
	require.Len(t, h.cloud.NetworkSpecs, 1, "network creates")
	assert.Equal(t, "eu-central", h.cloud.NetworkSpecs[0].Subnet.NetworkZone, "the network's zone")
	var locations []string
	for _, node := range h.byRole(controlplane.RoleNode) {
		locations = append(locations, node.Location)
		assert.NotEmpty(t, node.PrivateIPs[network.ID], "node %s is not on the cluster's network", node.Name)
		assert.True(t, strings.HasSuffix(node.Name, "-node-"+node.Location), "node %s in %s is not named after its location", node.Name, node.Location)
	}
	assert.ElementsMatch(t, controlplane.Locations, locations, "the nodes' locations")
	var gatewayLocations []string
	for _, gw := range h.byRole(controlplane.RoleGateway) {
		gatewayLocations = append(gatewayLocations, gw.Location)
	}
	assert.Equal(t, controlplane.Locations, gatewayLocations, "the gateways' locations")
	assert.Equal(t, "fsn1", h.cloud.SpecFor(t, controlplane.RoleBuilder).Location, "the builder's location")
	assert.Empty(t, h.byRole(controlplane.RoleBuilder), "a builder outlived its snapshot")
}

func TestPlane_ASizeOfOneBuildsOneNodeBesideOneGatewayInFsn1AndJudgesNoOtherLocation(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.plane.Size = 1 })

	h.built()

	assert.Equal(t, []string{"fsn1"}, h.plane.NodeLocations(), "the plane's node locations")
	assert.Equal(t, []string{"fsn1"}, locationsOf(h.byRole(controlplane.RoleNode)), "the nodes' locations, one etcd member each")
	assert.Equal(t, []string{"fsn1"}, locationsOf(h.byRole(controlplane.RoleGateway)), "the gateways' locations")
	require.Len(t, h.installed, 1, "installs")
	assert.Equal(t, []netip.Addr{netip.MustParseAddr(controlplane.GatewayAddress)}, h.installed[0].Gateways, "the gateways every node pulls through")
	assert.Equal(t, "1", h.network().Labels[controlplane.KeySize], "the size the network records")
}

func TestPlane_NodeLocationsGivesACopyAChangeToWhichReachesNeitherLocationsNorTheNextCall(t *testing.T) {
	for _, size := range []int{1, 3} {
		plane := controlplane.Plane{Size: size}
		given := plane.NodeLocations()

		given[0] = "changed"

		assert.Equal(t, []string{"fsn1", "nbg1", "hel1"}, controlplane.Locations, "Locations after a change to a size-%d plane's node locations", size)
		assert.Equal(t, "fsn1", plane.NodeLocations()[0], "a size-%d plane's next node locations", size)
	}
}

func TestPlane_ASizeOfThreeBuildsThePlaneAPlaneGivenNoSizeBuilds(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{name: "a plane given a size of three", size: 3},
		{name: "a plane given no size, as every cluster built before the size was an input", size: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) { h.plane.Size = tc.size })

			h.built()

			assert.Equal(t, controlplane.Locations, h.plane.NodeLocations(), "the plane's node locations")
			assert.ElementsMatch(t, controlplane.Locations, locationsOf(h.byRole(controlplane.RoleNode)), "the nodes' locations")
			assert.Equal(t, controlplane.Locations, locationsOf(h.byRole(controlplane.RoleGateway)), "the gateways' locations")
			assert.Equal(t, controlplane.GatewayAddrs(), h.installed[0].Gateways, "the gateways every node pulls through")
			assert.Equal(t, "3", h.network().Labels[controlplane.KeySize], "the size the network records")
		})
	}
}

func TestPlane_RefusesASizeOtherThanOneOrThreeBeforeAnyChange(t *testing.T) {
	for _, size := range []int{2, 4, 5, -1} {
		t.Run(fmt.Sprintf("a plane of %d servers", size), func(t *testing.T) {
			h := newHarness(t, func(h *harness) { h.plane.Size = size })

			err := h.ensure()

			assert.Equal(t, "invalid-param", refusalClass(t, err))
			assert.ErrorContains(t, err, fmt.Sprintf("1 or 3 servers, and %d were asked for", size))
			assert.Zero(t, h.cloud.Writes(), "changes after the refusal")
		})
	}
}

func TestPlane_RefusesASizeOtherThanTheOneTheClusterWasSubscribedWithNamingThatSize(t *testing.T) {
	for _, tc := range []struct {
		name             string
		built, asked     int
		dropRecordedSize bool
		wantSubscribed   string
	}{
		{name: "a one-server cluster asked for three", built: 1, asked: 3, wantSubscribed: "1"},
		{name: "a three-server cluster asked for one", built: 3, asked: 1, wantSubscribed: "3"},
		{name: "a cluster built before the size was an input, asked for one", built: 3, asked: 1, dropRecordedSize: true, wantSubscribed: "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) { h.plane.Size = tc.built })
			h.built()
			if tc.dropRecordedSize {
				network := h.network()
				labels := maps.Clone(network.Labels)
				delete(labels, controlplane.KeySize)
				require.NoError(t, h.cloud.SetNetworkLabels(context.Background(), network.ID, labels))
			}
			h.plane.Size = tc.asked
			before := h.cloud.Writes()

			err := h.ensure()

			assert.Equal(t, "control-plane-size-fixed", refusalClass(t, err))
			assert.ErrorContains(t, err, "subscribed with controlPlaneSize "+tc.wantSubscribed)
			assert.ErrorContains(t, err, "leave controlPlaneSize at "+tc.wantSubscribed)
			assert.Equal(t, before, h.cloud.Writes(), "changes after the refusal")
		})
	}
}

func TestPlane_RefusesOneServerForAClusterThatHostsThePlatformNamingTheRestoreDrillBeforeAnyChange(t *testing.T) {
	h := newHarness(t, hosting, func(h *harness) { h.plane.Size = 1 })

	err := h.ensure()

	assert.Equal(t, "restore-drill-missing", refusalClass(t, err))
	assert.ErrorContains(t, err, "OpenBao")
	assert.ErrorContains(t, err, "ADR 0125 restore drill")
	assert.Zero(t, h.cloud.Writes(), "changes after the refusal")
}

func TestPlane_SizeRefusalJudgesASizeFromAStateHoldingOnlyTheSubscriptionsNetworks(t *testing.T) {
	for _, tc := range []struct {
		name               string
		built, hosts       bool
		asked              int
		wantClass, wantMsg string
	}{
		{name: "a three-server cluster asked for one", built: true, asked: 1,
			wantClass: "control-plane-size-fixed", wantMsg: "subscribed with controlPlaneSize 3, and a control plane keeps its size after subscribe"},
		{name: "a three-server cluster asked for three", built: true, asked: 3},
		{name: "a cluster with no network yet asked for one", asked: 1},
		{name: "a hosting cluster with no network yet asked for one", hosts: true, asked: 1,
			wantClass: "restore-drill-missing", wantMsg: "ADR 0125 restore drill"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts []func(*harness)
			if tc.hosts {
				opts = append(opts, hosting)
			}
			h := newHarness(t, opts...)
			var st controlplane.State
			if tc.built {
				h.built()
				st.Networks = h.read().Networks
			}
			h.plane.Size = tc.asked

			err := h.plane.SizeRefusal(st)

			if tc.wantClass == "" {
				assert.NoError(t, err)
				return
			}
			assert.Equal(t, tc.wantClass, refusalClass(t, err))
			assert.ErrorContains(t, err, tc.wantMsg)
		})
	}
}

func TestPlane_SizeRefusalRefusesAStateHoldingTwoOfTheSubscriptionsNetworksNamingBoth(t *testing.T) {
	h := newHarness(t)
	h.built()
	first := h.network()
	second, err := h.cloud.CreateNetwork(context.Background(), controlplane.NetworkSpec{Name: first.Name, IPRange: first.IPRange, Labels: first.Labels})
	require.NoError(t, err)

	err = h.plane.SizeRefusal(controlplane.State{Networks: h.read().Networks})

	assert.Equal(t, "ambiguous-cluster", refusalClass(t, err))
	assert.ErrorContains(t, err, "2 networks carry this subscription's labels")
	assert.ErrorContains(t, err, fmt.Sprint(first.ID))
	assert.ErrorContains(t, err, fmt.Sprint(second.ID))
}

func TestPlane_AOneServerClusterWhoseNodeIsLostNamesTheLostNodeAndNoOtherLocation(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.plane.Size = 1 })
	h.built()
	lost, ok := h.read().NodeIn("fsn1")
	require.True(t, ok, "the cluster has no node in fsn1")
	require.NoError(t, h.cloud.DeleteServer(context.Background(), lost.ID))

	problems := h.plane.Problems(context.Background(), h.read())

	assert.Equal(t, []string{"no node in fsn1 carries this subscription's labels"}, problems, "the problems of a one-server cluster that lost its node")
}

func locationsOf(servers []controlplane.Server) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Location)
	}
	return out
}

func TestPlane_BuildsTheBuilderAndEveryNodeOnTheServerTypeItIsGiven(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.plane.ServerTypes = []string{"cpx32"} })

	h.built()

	typeByLocation := map[string]string{}
	for _, node := range h.byRole(controlplane.RoleNode) {
		typeByLocation[node.Location] = node.ServerType
	}
	assert.Equal(t, map[string]string{"fsn1": "cpx32", "nbg1": "cpx32", "hel1": "cpx32"}, typeByLocation, "each location's node server type")
	assert.Equal(t, "cpx32", h.cloud.SpecFor(t, controlplane.RoleBuilder).ServerType, "the builder's server type")
}

func TestPlane_EachNodeAndBuilderTakeTheFirstTypeOfTheListTheirLocationCanOrder(t *testing.T) {
	for _, tc := range []struct {
		name        string
		soldOut     []string
		soldOutIn   map[string][]string
		want        map[string]string
		wantBuilder string
	}{
		{
			name:      "a new cluster with cx23 sold out in nbg1 runs cpx22 there and cx23 in fsn1 and hel1",
			soldOutIn: map[string][]string{"nbg1": {"cx23"}}, want: map[string]string{"fsn1": "cx23", "nbg1": "cpx22", "hel1": "cx23"}, wantBuilder: "cx23",
		},
		{
			name:    "a cluster where Hetzner lists only the list's second type runs it everywhere",
			soldOut: []string{"cx23"}, want: map[string]string{"fsn1": "cpx22", "nbg1": "cpx22", "hel1": "cpx22"}, wantBuilder: "cpx22",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) { h.plane.ServerTypes = []string{"cx23", "cpx22"} })
			h.cloud.SoldOut, h.cloud.SoldOutIn = tc.soldOut, tc.soldOutIn

			h.built()

			typeByLocation := map[string]string{}
			for _, node := range h.byRole(controlplane.RoleNode) {
				typeByLocation[node.Location] = node.ServerType
			}
			assert.Equal(t, tc.want, typeByLocation, "each location's node server type")
			assert.Equal(t, tc.wantBuilder, h.cloud.SpecFor(t, controlplane.RoleBuilder).ServerType, "the builder's server type")
			assert.Equal(t, tc.wantBuilder, h.cloud.SpecFor(t, controlplane.RoleWorkerBuilder).ServerType, "the worker builder's server type")
		})
	}
}

func TestPlane_ALostNodeIsReplacedWithTheListsNextTypeWhenTheFirstIsSoldOutInItsLocation(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.plane.ServerTypes = []string{"cx23", "cpx22"} })
	h.built()
	lost, ok := h.read().NodeIn("hel1")
	require.True(t, ok, "the cluster has no node in hel1")
	require.NoError(t, h.cloud.DeleteServer(context.Background(), lost.ID))
	h.cloud.SoldOutIn = map[string][]string{"hel1": {"cx23"}}

	require.NoError(t, h.converge())

	successor, ok := h.read().NodeIn("hel1")
	require.True(t, ok, "no node replaced the lost one in hel1")
	assert.NotEqual(t, lost.ID, successor.ID, "the node in hel1")
	assert.Equal(t, "cpx22", successor.ServerType, "the successor's server type in hel1")
	for _, loc := range []string{"fsn1", "nbg1"} {
		node, _ := h.read().NodeIn(loc)
		assert.Equal(t, "cx23", node.ServerType, "the running node in %s", loc)
	}
}

func TestPlane_RefusesAServerNoTypeOfTheListCanBeOrderedForNamingTheListAndTheLocation(t *testing.T) {
	for _, tc := range []struct {
		name, loc string
		nodes     int
	}{
		{name: "in fsn1, where the builders run, the install creates no server", loc: "fsn1"},
		{name: "in hel1 the nodes of fsn1 and nbg1 stand and hel1 gets none", loc: "hel1", nodes: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) { h.plane.ServerTypes = []string{"cx33", "cx43"} })
			h.cloud.SoldOutIn = map[string][]string{tc.loc: {"cx33", "cx43"}}

			err := h.settle(nil)

			assert.Equal(t, "server-unavailable", refusalClass(t, err))
			for _, want := range []string{"cx33", "cx43", tc.loc} {
				assert.Contains(t, err.Error(), want, "the refusal names the list and the location")
			}
			assert.Len(t, h.byRole(controlplane.RoleNode), tc.nodes, "nodes after the refusal")
			if tc.nodes == 0 {
				assert.Empty(t, h.cloud.ServerSpecs, "servers an install created with no type to order in %s", tc.loc)
			}
		})
	}
}

func TestPlane_ANodeCreateHetznerRefusesIsRetriedWithTheTypesReadAgain(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.plane.ServerTypes = []string{"cx23", "cpx22"} })
	hetzner := &controlplane.Refusal{Class: "server-unavailable", Message: "Hetzner cannot create the server, of type cx23 in fsn1, now: resource_unavailable"}
	h.cloud.FailCreate[controlplane.RoleNode] = hetzner

	err := h.settle(nil)
	h.cloud.SoldOutIn = map[string][]string{"fsn1": {"cx23"}}
	h.built()

	require.ErrorIs(t, err, hetzner, "the failed create's error")
	assert.Contains(t, err.Error(), "resource_unavailable", "the failed create names Hetzner's error")
	node, ok := h.read().NodeIn("fsn1")
	require.True(t, ok, "no node in fsn1 after the retry")
	assert.Equal(t, "cpx22", node.ServerType, "the node the retry created once cx23 sold out in fsn1")
}

// builtOnCx43 lists cx33 then cx43 with cx33 sold out where the builders run, so the cluster image holds cx43's 160 GB.
func builtOnCx43(h *harness) {
	h.plane.ServerTypes = []string{"cx33", "cx43"}
	h.cloud.SoldOutIn = map[string][]string{"fsn1": {"cx33"}}
}

func TestPlane_WithTheBuilderOnALargerDiskThanTheListsFirstTypeEachNodeTakesTheFirstTypeWhoseDiskHoldsTheImage(t *testing.T) {
	h := newHarness(t, builtOnCx43)

	h.built()

	require.Equal(t, "cx43", h.cloud.SpecFor(t, controlplane.RoleBuilder).ServerType, "precondition: the builder's server type")
	typeByLocation := map[string]string{}
	for _, node := range h.byRole(controlplane.RoleNode) {
		typeByLocation[node.Location] = node.ServerType
	}
	assert.Equal(t, map[string]string{"fsn1": "cx43", "nbg1": "cx43", "hel1": "cx43"}, typeByLocation, "each location's node server type")
	named := slices.DeleteFunc(h.eventsNaming("nbg1"), func(event string) bool { return !strings.Contains(event, "cx43") })
	require.Len(t, named, 1, "the events naming the type nbg1's node took: %v", h.events)
	assert.Contains(t, named[0], "160 GB", "the event names the cluster image's disk")
}

func TestPlane_RefusesANodeWhoseLocationSellsNoListedTypeWhoseDiskHoldsTheImageBeforeAnyCreateNamingEachTypesDisk(t *testing.T) {
	h := newHarness(t, builtOnCx43)
	h.cloud.SoldOutIn["hel1"] = []string{"cx43"}

	err := h.settle(nil)

	assert.Equal(t, "server-unavailable", refusalClass(t, err))
	for _, want := range []string{"hel1", "160 GB", "cx33 has 80 GB", "cx43"} {
		assert.Contains(t, err.Error(), want, "the refusal names the location, the image's disk and each type's disk")
	}
	assert.Len(t, h.byRole(controlplane.RoleNode), 2, "nodes after the refusal")
	sent := 0
	for _, nodes := range h.cloud.NodesPerCreate {
		sent += nodes
	}
	assert.Equal(t, 2, sent, "the node creates sent to Hetzner, none of them for hel1")
}

func TestPlane_ANodeRefusedForNoTypeWhoseDiskHoldsTheImageIsCreatedOnceItsLocationSellsOneAgain(t *testing.T) {
	h := newHarness(t, builtOnCx43)
	h.cloud.SoldOutIn["hel1"] = []string{"cx43"}
	require.Equal(t, "server-unavailable", refusalClass(t, h.settle(nil)), "precondition: the refusal while hel1 sells only cx33")
	delete(h.cloud.SoldOutIn, "hel1")

	h.built()

	assert.Equal(t, "cx43", h.nodeIn("hel1").ServerType, "the node created in hel1 once it sells cx43 again")
}

func TestPlane_EveryNodeBootsFromTheClustersOwnSnapshot(t *testing.T) {
	h := newHarness(t)

	h.built()

	snaps, err := h.cloud.ListSnapshots(context.Background(), h.plane.RoleSelector(controlplane.RoleImage))
	require.NoError(t, err)
	require.Len(t, snaps, 1, "snapshots")
	nodes := h.byRole(controlplane.RoleNode)
	require.Len(t, nodes, 3, "nodes")
	for _, node := range nodes {
		assert.Equal(t, snaps[0].ID, node.ImageID, "node %d boots from image %d; want the snapshot %d", node.ID, node.ImageID, snaps[0].ID)
	}
	assert.Equal(t, string(controlplane.RoleImage), snaps[0].Labels[controlplane.KeyRole], "the snapshot's role label")
}

func TestPlane_NoNodeCarriesUserDataAndOnlyTheGatewayHoldsAPublicAddress(t *testing.T) {
	h := newHarness(t)

	h.built()

	nodes := 0
	for _, spec := range h.cloud.ServerSpecs {
		if spec.Labels[controlplane.KeyRole] == string(controlplane.RoleNode) {
			nodes++
			assert.Empty(t, spec.UserData, "node %s's user data", spec.Name)
		}
	}
	assert.Equal(t, 3, nodes, "nodes created")
	firewalls, err := h.cloud.ListFirewalls(context.Background(), h.plane.Selector())
	require.NoError(t, err)
	require.Len(t, firewalls, 1)
	assert.Zero(t, firewalls[0].RuleCount, "the firewall's rules")
	for _, spec := range h.cloud.ServerSpecs {
		wantPublic := spec.Labels[controlplane.KeyRole] == string(controlplane.RoleGateway)
		assert.Equal(t, wantPublic, spec.PublicIPv4, "server %s holds a public IPv4 = %v; want %v", spec.Name, spec.PublicIPv4, wantPublic)
		assert.Equal(t, []int64{firewalls[0].ID}, spec.FirewallIDs, "server %s's firewalls", spec.Name)
	}
	st := h.read()
	for _, r := range []controlplane.Role{controlplane.RoleAPI, controlplane.RoleIngress} {
		lb, ok := st.LoadBalancerIn(r, controlplane.GatewayLocation)
		require.True(t, ok, "the %s load balancer in the plane's state", r)
		assert.Equal(t, r == controlplane.RoleIngress, lb.PublicInterface, "load balancer %s's public interface", lb.Name)
		assert.NotEmpty(t, lb.AssignedPublicIPv4, "the address Hetzner assigned load balancer %s", lb.Name)
		wantPublic := ""
		if r == controlplane.RoleIngress {
			wantPublic = lb.AssignedPublicIPv4
		}
		assert.Equal(t, wantPublic, lb.PublicIPv4, "load balancer %s serves the public address %q; want %q", lb.Name, lb.PublicIPv4, wantPublic)
	}
}

func TestPlane_EveryResourceCarriesTheBlockItIsGivenAsItsLabel(t *testing.T) {
	h := newHarness(t)

	h.built()

	require.NotEmpty(t, h.cloud.ServerSpecs, "servers created")
	for _, spec := range h.cloud.ServerSpecs {
		assert.Equal(t, testBlock, spec.Labels[controlplane.KeyBlock], "server %s's block label", spec.Name)
	}
	for _, spec := range h.cloud.LBSpecs {
		assert.Equal(t, testBlock, spec.Labels[controlplane.KeyBlock], "load balancer %s's block label", spec.Name)
	}
	assert.Equal(t, testBlock, h.network().Labels[controlplane.KeyBlock], "the network's block label")
}

func TestPlane_EachCallWaitsOnOneHetznerActionAtMostAndCreatesOneNodeAtMost(t *testing.T) {
	h := newHarness(t)

	h.built()

	for i, waits := range h.waits {
		assert.LessOrEqual(t, waits, 1, "call %d waited on %d Hetzner actions", i, waits)
	}
	for i, nodes := range h.cloud.NodesPerCreate {
		assert.LessOrEqual(t, nodes, 1, "server create %d made %d nodes", i, nodes)
	}
}

func TestPlane_RefusesTheBuilderStillRunningPastItsDeadlineAsAFailedBuild(t *testing.T) {
	h := newHarness(t)
	h.buildingBuilder()
	created := h.byRole(controlplane.RoleBuilder)[0].Created

	err := h.settle(nil)

	assert.Equal(t, "image-build-failed", refusalClass(t, err))
	assert.Contains(t, err.Error(), "the cluster's image was not built or the gateway's mirror never completed")
	assert.Less(t, h.now.Sub(created), controlplane.BuildDeadline+5*time.Minute, "how long the plane waited on a failed build")
	assert.Empty(t, h.byRole(controlplane.RoleBuilder), "the builder past its deadline")
	assert.Empty(t, h.byRole(controlplane.RoleNode), "a node with no image")
}

// heldStart is a server of the build whose first start Hetzner runs on past the call that sent it, the builder whose
// deadline bounds the wait on it with the class of that refusal, and the problem that names it starting.
type heldStart struct {
	name    string
	role    controlplane.Role
	opts    []func(*harness)
	builder controlplane.Role
	failure string
	server  func(h *harness) controlplane.Server
	problem func(s controlplane.Server) string
}

// starting is the problem that names server s starting while Hetzner still runs its start_server action on it.
func starting(what string) func(s controlplane.Server) string {
	return func(s controlplane.Server) string {
		return fmt.Sprintf("%s %d, created at %s, is starting, since Hetzner still runs its start_server action on it",
			what, s.ID, s.Created.UTC().Format(time.RFC3339))
	}
}

// theOne is the one server of role r.
func theOne(r controlplane.Role) func(h *harness) controlplane.Server {
	return func(h *harness) controlplane.Server {
		h.t.Helper()
		servers := h.byRole(r)
		require.Len(h.t, servers, 1, "servers of role %s", r)
		return servers[0]
	}
}

var heldStarts = []heldStart{
	{
		name: "the builder", role: controlplane.RoleBuilder, builder: controlplane.RoleBuilder, failure: "image-build-failed",
		server: theOne(controlplane.RoleBuilder), problem: starting("builder"),
	},
	{
		name: "the worker builder", role: controlplane.RoleWorkerBuilder, builder: controlplane.RoleWorkerBuilder, failure: "worker-image-build-failed",
		server: theOne(controlplane.RoleWorkerBuilder), problem: starting("worker builder"),
	},
	{
		name: "the gateway", role: controlplane.RoleGateway, builder: controlplane.RoleBuilder, failure: "image-build-failed",
		server: func(h *harness) controlplane.Server { return h.gatewayIn(controlplane.GatewayLocation) },
		problem: func(s controlplane.Server) string {
			return fmt.Sprintf("gateway %d in %s is starting, since Hetzner still runs its start_server action on it", s.ID, s.Location)
		},
	},
	{
		name: "the serving server", role: controlplane.RoleServing, opts: []func(*harness){hosting}, builder: controlplane.RoleBuilder,
		failure: "image-build-failed", server: theOne(controlplane.RoleServing),
		problem: func(s controlplane.Server) string {
			return fmt.Sprintf("serving server %d is starting, since Hetzner still runs its start_server action on it", s.ID)
		},
	},
}

// holdStart builds until the first start of held's server outlives its call, and returns that server.
func (h *harness) holdStart(held heldStart) controlplane.Server {
	h.t.Helper()
	h.cloud.HoldNext[held.role] = cloudtest.StartServerCommand
	require.ErrorIs(h.t, h.converge(), context.DeadlineExceeded, "the call whose start of %s outlived it", held.name)
	return held.server(h)
}

func TestPlane_AServerOfTheBuildWhoseStartOutlivesItsCallIsReadStartingAndSentNoSecondStart(t *testing.T) {
	for _, held := range heldStarts {
		t.Run(held.name, func(t *testing.T) {
			h := newHarness(t, held.opts...)
			server := h.holdStart(held)

			err := h.converge()

			require.NoError(t, err, "a call while the start of %s runs", held.name)
			assert.Equal(t, 1, h.sent(server.ID, cloudtest.StartServerCommand), "the starts sent at %s: %v", held.name, h.cloud.ActionsSent(server.ID))
			assert.Contains(t, h.problems(), held.problem(server), "the problems while the start of %s runs", held.name)
		})
	}
}

func TestPlane_AServerOfTheBuildWhoseStartOutlivedItsCallBuildsOnOnceItFinishesAndIsStartedAgainOnlyOnceItFailed(t *testing.T) {
	for _, held := range heldStarts {
		for _, tc := range []struct {
			outcome string
			end     func(f *cloudtest.Fake, id int64)
			starts  int
		}{
			{outcome: "finished", end: (*cloudtest.Fake).FinishAction, starts: 1},
			{outcome: "failed", end: (*cloudtest.Fake).FailAction, starts: 2},
		} {
			t.Run(held.name+" whose start "+tc.outcome, func(t *testing.T) {
				h := newHarness(t, held.opts...)
				server := h.holdStart(held)
				require.NoError(t, h.converge(), "a call while the start of %s runs", held.name)
				tc.end(h.cloud, server.ID)

				h.built()

				assert.Equal(t, tc.starts, h.sent(server.ID, cloudtest.StartServerCommand), "the starts sent at %s once its start %s: %v",
					held.name, tc.outcome, h.cloud.ActionsSent(server.ID))
			})
		}
	}
}

func TestPlane_AServerOfTheBuildWhoseStartEndsBetweenTheListAndTheStepIsSentNoSecondStartAndBuildsOn(t *testing.T) {
	for _, held := range heldStarts {
		t.Run(held.name, func(t *testing.T) {
			h := newHarness(t, held.opts...)
			server := h.holdStart(held)
			require.NoError(t, h.converge(), "a call while the start of %s runs", held.name)
			h.cloud.EndAfterList[server.ID] = true

			err := h.ensure()

			require.NoError(t, err, "the call that listed %s off before its start ended", held.name)
			h.built()
			assert.Equal(t, 1, h.sent(server.ID, cloudtest.StartServerCommand), "the starts sent at %s: %v", held.name, h.cloud.ActionsSent(server.ID))
		})
	}
}

func TestPlane_RefusesABuildWhoseServerStillStartsAtTheBuildersDeadlineAsAFailedBuildWithNoSecondStart(t *testing.T) {
	for _, held := range heldStarts {
		t.Run(held.name, func(t *testing.T) {
			h := newHarness(t, held.opts...)
			server := h.holdStart(held)
			created := theOne(held.builder)(h).Created

			err := h.settle(nil)

			assert.Equal(t, held.failure, refusalClass(t, err))
			assert.ErrorContains(t, err, "Hetzner still runs its start_server action on it", "the refusal of a build whose start of %s never ended", held.name)
			assert.Less(t, h.now.Sub(created), controlplane.BuildDeadline+5*time.Minute, "how long the plane waited on the start of %s", held.name)
			assert.Empty(t, h.byRole(held.builder), "the %s past its deadline", held.builder)
			assert.Equal(t, 1, h.sent(server.ID, cloudtest.StartServerCommand), "the starts sent at %s: %v", held.name, h.cloud.ActionsSent(server.ID))
		})
	}
}

func TestPlane_AGatewayWhosePowerOnBesideTheSnapshotOutlivesItsCallIsReadStartingAndSentNoSecondStart(t *testing.T) {
	h := newHarness(t)
	imaging := func() bool {
		snap, ok := h.read().Snapshot()
		return ok && snap.Status != controlplane.SnapshotAvailable
	}
	require.NoError(t, h.settle(imaging))
	gw := h.gatewayIn(controlplane.GatewayLocation)
	starts := h.sent(gw.ID, cloudtest.StartServerCommand)
	h.cloud.StopServer(gw.ID)
	h.cloud.HoldNext[controlplane.RoleGateway] = cloudtest.StartServerCommand
	require.ErrorIs(t, h.converge(), context.DeadlineExceeded, "the call whose power-on of the gateway beside the snapshot outlived it")

	err := h.converge()

	require.NoError(t, err, "a call while the gateway's power-on beside the snapshot runs")
	assert.Equal(t, starts+1, h.sent(gw.ID, cloudtest.StartServerCommand), "the starts sent at the gateway: %v", h.cloud.ActionsSent(gw.ID))
	assert.Contains(t, h.problems(), fmt.Sprintf("gateway %d in %s is starting, since Hetzner still runs its start_server action on it", gw.ID, gw.Location))
	h.cloud.FinishAction(gw.ID)
	h.built()
	assert.Equal(t, starts+1, h.sent(gw.ID, cloudtest.StartServerCommand), "the starts sent at the gateway once its power-on finished: %v", h.cloud.ActionsSent(gw.ID))
}

func TestPlane_AGatewayInAnotherLocationWhoseFirstStartOutlivesItsCallIsReadStartingAndSentNoSecondStart(t *testing.T) {
	h := newHarness(t)
	for calls := 0; !slices.ContainsFunc(h.byRole(controlplane.RoleGateway), func(s controlplane.Server) bool { return s.Status == controlplane.ServerRunning }); calls++ {
		require.Less(t, calls, 50, "no gateway started within 50 calls")
		require.NoError(t, h.ensure())
	}
	h.cloud.HoldNext[controlplane.RoleGateway] = cloudtest.StartServerCommand
	require.ErrorIs(t, h.converge(), context.DeadlineExceeded, "the call whose first start of a second gateway outlived it")
	gw := h.held(controlplane.RoleGateway)
	require.NotEqual(t, controlplane.GatewayLocation, gw.Location, "the location of the gateway whose start outlived its call")

	err := h.converge()

	require.NoError(t, err, "a call while the first start of the gateway in %s runs", gw.Location)
	assert.Equal(t, 1, h.sent(gw.ID, cloudtest.StartServerCommand), "the starts sent at the gateway in %s: %v", gw.Location, h.cloud.ActionsSent(gw.ID))
	assert.Contains(t, h.problems(), fmt.Sprintf("gateway %d in %s is starting, since Hetzner still runs its start_server action on it", gw.ID, gw.Location))
	h.cloud.FinishAction(gw.ID)
	h.built()
	assert.Equal(t, 1, h.sent(gw.ID, cloudtest.StartServerCommand), "the starts sent at the gateway in %s once its start finished: %v", gw.Location, h.cloud.ActionsSent(gw.ID))
}

func TestPlane_AServerWhoseAttachOutlivesItsCallIsSentNoSecondAttach(t *testing.T) {
	for _, tc := range []struct {
		role controlplane.Role
		opts []func(*harness)
	}{
		{role: controlplane.RoleGateway},
		{role: controlplane.RoleBuilder},
		{role: controlplane.RoleWorkerBuilder},
		{role: controlplane.RoleServing, opts: []func(*harness){hosting}},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			h := newHarness(t, tc.opts...)
			h.cloud.HoldNext[tc.role] = cloudtest.AttachToNetworkCommand
			require.ErrorIs(t, h.converge(), context.DeadlineExceeded, "the call whose attach of the %s outlived it", tc.role)
			server := h.held(tc.role)

			err := h.converge()

			require.NoError(t, err, "a call while the attach of the %s runs", tc.role)
			assert.Equal(t, 1, h.sent(server.ID, cloudtest.AttachToNetworkCommand), "the attaches sent at the %s: %v", tc.role, h.cloud.ActionsSent(server.ID))
			assert.Empty(t, h.cloud.Servers[server.ID].PrivateIPs, "the %s's addresses while its attach runs", tc.role)
		})
	}
}

func TestPlane_AServerWhoseAttachEndsBetweenTheListAndTheStepIsSentNoSecondAttachAndBuildsOn(t *testing.T) {
	for _, tc := range []struct {
		role controlplane.Role
		opts []func(*harness)
	}{
		{role: controlplane.RoleGateway},
		{role: controlplane.RoleBuilder},
		{role: controlplane.RoleWorkerBuilder},
		{role: controlplane.RoleServing, opts: []func(*harness){hosting}},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			h := newHarness(t, tc.opts...)
			h.cloud.HoldNext[tc.role] = cloudtest.AttachToNetworkCommand
			require.ErrorIs(t, h.converge(), context.DeadlineExceeded, "the call whose attach of the %s outlived it", tc.role)
			server := h.held(tc.role)
			h.cloud.EndAfterList[server.ID] = true

			err := h.ensure()

			require.NoError(t, err, "the call that listed the %s off its network before its attach ended", tc.role)
			h.built()
			assert.Equal(t, 1, h.sent(server.ID, cloudtest.AttachToNetworkCommand), "the attaches sent at the %s: %v", tc.role, h.cloud.ActionsSent(server.ID))
		})
	}
}

func TestPlane_AGatewayIsPoweredOnOnlyOnceItIsOnTheNetworkWhileAnotherGatewaysAttachRuns(t *testing.T) {
	h := newHarness(t)
	attached := func() bool {
		gateways := controlplane.ServersIn(h.byRole(controlplane.RoleGateway), controlplane.GatewayLocation)
		return len(gateways) == 1 && len(h.cloud.ActionsSent(gateways[0].ID)) > 0
	}
	for calls := 0; !attached(); calls++ {
		require.Less(t, calls, 50, "the gateway in %s was not attached within 50 calls", controlplane.GatewayLocation)
		require.NoError(t, h.ensure())
	}
	h.cloud.HoldNext[controlplane.RoleGateway] = cloudtest.AttachToNetworkCommand
	require.ErrorIs(t, h.converge(), context.DeadlineExceeded, "the call whose attach of a second gateway outlived it")
	held := h.held(controlplane.RoleGateway)
	require.NotEqual(t, controlplane.GatewayLocation, held.Location, "the location of the gateway whose attach outlived its call")
	for range 5 {
		require.NoError(t, h.converge(), "a call while the attach of gateway %d runs", held.ID)
	}

	h.cloud.FinishAction(held.ID)
	h.built()

	require.Len(t, h.byRole(controlplane.RoleGateway), len(controlplane.Locations), "the gateways built")
	for _, gw := range h.byRole(controlplane.RoleGateway) {
		sent := h.cloud.ActionsSent(gw.ID)
		require.NotEmpty(t, sent, "the actions sent at the gateway in %s", gw.Location)
		assert.Equal(t, cloudtest.AttachToNetworkCommand, sent[0], "the first action sent at the gateway in %s: %v", gw.Location, sent)
	}
}

func TestPlane_AFinishedBuilderIsKeptWhileTheGatewaysPowerOnRunsPastTheBuildersDeadline(t *testing.T) {
	h := newHarness(t)
	h.buildingBuilder()
	builder, gw := h.byRole(controlplane.RoleBuilder)[0], h.gatewayIn(controlplane.GatewayLocation)
	for _, finished := range append(h.byRole(controlplane.RoleWorkerBuilder), builder) {
		h.cloud.PowerOff(finished.ID)
	}
	h.cloud.StopServer(gw.ID)
	h.now = builder.Created.Add(controlplane.BuildDeadline - time.Minute)
	h.cloud.HoldNext[controlplane.RoleGateway] = cloudtest.StartServerCommand
	require.ErrorIs(t, h.converge(), context.DeadlineExceeded, "the call whose power-on of the gateway beside the finished builder outlived it")
	starts := h.sent(gw.ID, cloudtest.StartServerCommand)
	h.now = builder.Created.Add(controlplane.BuildDeadline + 5*time.Minute)

	err := h.converge()

	require.NoError(t, err, "a call past the builder's deadline while the gateway's power-on runs")
	assert.Len(t, h.byRole(controlplane.RoleBuilder), 1, "the finished builder while the gateway's power-on runs")
	h.cloud.FinishAction(gw.ID)
	require.NoError(t, h.settle(func() bool { _, ok := h.read().Snapshot(); return ok }), "the calls once the gateway's power-on finished")
	assert.Equal(t, starts, h.sent(gw.ID, cloudtest.StartServerCommand), "the starts sent at the gateway: %v", h.cloud.ActionsSent(gw.ID))
}

func TestPlane_ACallThatCannotReadAServersActionOrTheServerReturnsThatFailure(t *testing.T) {
	for _, tc := range []struct {
		read string
		fail func(*cloudtest.Fake, error)
	}{
		{read: "RunningAction", fail: func(f *cloudtest.Fake, err error) { f.FailRunningAction = err }},
		{read: "GetServer", fail: func(f *cloudtest.Fake, err error) { f.FailGetServer = err }},
	} {
		t.Run(tc.read, func(t *testing.T) {
			h := newHarness(t)
			failure := errors.New("hcloud: the " + tc.read + " read failed")
			tc.fail(h.cloud, failure)

			var err error
			for calls := 0; err == nil; calls++ {
				require.Less(t, calls, 50, "no call read %s within 50 calls", tc.read)
				err = h.ensure()
			}

			assert.ErrorIs(t, err, failure, "the call whose %s read failed", tc.read)
		})
	}
}

func TestPlane_RefusesTheBuildNamingTheGatewaysImageMirrorOnceTheGatewayPowersItselfOff(t *testing.T) {
	h := newHarness(t)
	h.buildingBuilder()
	created := h.byRole(controlplane.RoleBuilder)[0].Created
	h.now = h.now.Add(5 * time.Minute)
	h.cloud.PowerOff(h.byRole(controlplane.RoleGateway)[0].ID)

	err := h.settle(nil)

	assert.Equal(t, "gateway-mirror-failed", refusalClass(t, err))
	assert.Contains(t, err.Error(), "the gateway's image mirror")
	assert.LessOrEqual(t, h.now.Sub(created), controlplane.BuildDeadline-30*time.Minute, "how long the plane waited on a gateway that powered itself off")
	assert.Empty(t, h.byRole(controlplane.RoleBuilder), "the builder, whose disk holds every key of the cluster")
	assert.Equal(t, controlplane.ServerOff, h.byRole(controlplane.RoleGateway)[0].Status, "the gateway was powered back on")
	assert.Equal(t, "gateway-mirror-failed", refusalClass(t, h.ensure()), "the next call's refusal")
}

func TestPlane_PowersOnAGatewayFoundOffOnceTheBuilderHasFinished(t *testing.T) {
	h := newHarness(t)
	h.buildingBuilder()
	h.cloud.PowerOff(h.byRole(controlplane.RoleBuilder)[0].ID)
	h.cloud.PowerOff(h.byRole(controlplane.RoleGateway)[0].ID)

	require.NoError(t, h.ensure())

	assert.Equal(t, controlplane.ServerRunning, h.byRole(controlplane.RoleGateway)[0].Status, "the gateway of a builder that finished")
	assert.Len(t, h.byRole(controlplane.RoleBuilder), 1, "builders")
}

func TestPlane_CreatesNoNodeBeforeTheBuilderReportsTheGatewaysMirrorComplete(t *testing.T) {
	h := newHarness(t)
	h.cloud.MirrorTakes = 40 * time.Minute

	h.built()

	complete, ok := h.cloud.MirrorCompleteAt()
	require.True(t, ok, "the gateway never ran")
	nodes := h.byRole(controlplane.RoleNode)
	require.Len(t, nodes, 3, "nodes")
	for _, node := range nodes {
		assert.False(t, node.Created.Before(complete), "node %s was created at %s, before the gateway's mirror completed at %s",
			node.Name, node.Created.Format(time.RFC3339), complete.Format(time.RFC3339))
	}
}

func TestPlane_AnswersProgressWhileTheBuilderWaitsForTheGatewaysMirror(t *testing.T) {
	h := newHarness(t)
	h.cloud.MirrorTakes = 40 * time.Minute
	waiting := testStart.Add(35 * time.Minute)

	err := h.settle(func() bool { return !h.now.Before(waiting) })

	require.NoError(t, err, "the plane gave up on a builder waiting for the gateway's mirror")
	require.True(t, h.building(), "the builder no longer runs while the mirror fills")
	assert.Empty(t, h.byRole(controlplane.RoleNode), "a node created before the gateway's mirror completed")
	assert.Contains(t, strings.Join(h.plane.Problems(context.Background(), h.read()), "; "),
		"is building the cluster's image or waiting for the gateway's mirror to complete")
}

func TestPlane_TheFailedBuildSaysWhereTheBuildStopped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		series map[string][]float64
		fail   error
		want   func(created time.Time) []string
	}{
		{
			name: "a build that wrote and went quiet",
			series: map[string][]float64{
				"cpu":                    {80, 95, 40, 2},
				"disk.0.bandwidth.read":  {0, 0, 0, 0},
				"disk.0.bandwidth.write": {2 * mib, 0, 0, 0},
			},
			want: func(created time.Time) []string {
				return []string{
					"CPU at 2.0% over the last minute",
					"0 B written to disk over the last minute",
					"120.0 MiB since its creation",
					"its disk last moved at " + created.Add(time.Minute).UTC().Format(time.RFC3339),
					"its CPU was last above 10% at " + created.Add(3*time.Minute).UTC().Format(time.RFC3339),
				}
			},
		},
		{
			name:   "a build that never got going",
			series: map[string][]float64{"cpu": {1, 2}, "disk.0.bandwidth.read": {0, 0}, "disk.0.bandwidth.write": {0, 0}},
			want: func(time.Time) []string {
				return []string{"0 B since its creation", "its disk never moved", "its CPU was never above 10%"}
			},
		},
		{
			name: "a build whose metrics cannot be read",
			fail: errors.New("hetzner answered 503"),
			want: func(time.Time) []string { return []string{"the builder's Hetzner metrics could not be read"} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.buildingBuilder()
			builder := h.byRole(controlplane.RoleBuilder)[0]
			h.cloud.GiveMetrics(builder, tc.series)
			h.cloud.FailMetrics = tc.fail

			err := h.settle(nil)

			require.Equal(t, "image-build-failed", refusalClass(t, err))
			for _, want := range tc.want(builder.Created) {
				assert.Contains(t, err.Error(), want)
			}
			if tc.fail == nil {
				reads := h.cloud.MetricsReads
				require.NotEmpty(t, reads, "the refusal never read the builder's metrics")
				assert.Equal(t, cloudtest.MetricsRead{ID: builder.ID, Start: builder.Created, End: h.now}, reads[len(reads)-1], "the refusal's metrics read")
			}
			assert.Empty(t, h.byRole(controlplane.RoleBuilder), "the builder past its deadline")
		})
	}
}

func TestPlane_TheProblemsOfARunningBuildCarryTheBuildersCPUAndDisk(t *testing.T) {
	for _, tc := range []struct {
		name   string
		series map[string][]float64
		fail   error
		want   []string
	}{
		{
			name: "metrics held",
			series: map[string][]float64{
				"cpu":                    {5, 120, 118.7},
				"disk.0.bandwidth.read":  {0, 0, 0},
				"disk.0.bandwidth.write": {0, 1 * mib, 2 * mib},
			},
			want: []string{"is building the cluster's image", "CPU at 118.7% over the last minute",
				"120.0 MiB written to disk over the last minute", "180.0 MiB since its creation"},
		},
		{name: "no metrics yet", want: []string{"Hetzner holds no metrics for the builder yet"}},
		{name: "a failed read", fail: errors.New("hetzner answered 503"),
			want: []string{"is building the cluster's image", "the builder's Hetzner metrics could not be read"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.buildingBuilder()
			builder := h.byRole(controlplane.RoleBuilder)[0]
			if tc.series != nil {
				h.cloud.GiveMetrics(builder, tc.series)
			}
			h.cloud.FailMetrics = tc.fail
			h.now = builder.Created.Add(4 * time.Minute)

			problems := strings.Join(h.plane.Problems(context.Background(), h.read()), "; ")

			for _, want := range tc.want {
				assert.Contains(t, problems, want)
			}
		})
	}
}

func TestPlane_ServesTheNodesThroughOneAPIAndOneIngressLoadBalancerInFsn1(t *testing.T) {
	h := newHarness(t)
	h.built()

	outputs, detail := h.plane.Outputs(h.read())

	network := h.network()
	want := map[controlplane.Role]struct {
		address  string
		services []controlplane.Service
	}{
		controlplane.RoleAPI: {address: "10.0.0.3", services: []controlplane.Service{{ListenPort: 6443, DestinationPort: 6443}}},
		controlplane.RoleIngress: {address: "10.0.0.4", services: []controlplane.Service{
			{ListenPort: 80, DestinationPort: 30080, ProxyProtocol: true},
			{ListenPort: 443, DestinationPort: 30081, ProxyProtocol: true},
		}},
	}
	assert.Len(t, h.cloud.LBSpecs, 2, "load balancer creates")
	for r, w := range want {
		lb := h.loadBalancer(r)
		assert.Equal(t, "fsn1", lb.Location, "the %s load balancer's location", r)
		assert.Equal(t, map[int64]string{network.ID: w.address}, lb.PrivateIPs, "the %s load balancer's address", r)
		assert.Equal(t, w.services, lb.Services, "the %s load balancer's services", r)
		assert.Equal(t, []string{h.plane.RoleSelector(controlplane.RoleNode)}, lb.TargetSelectors, "the %s load balancer's targets", r)
	}
	ingress := h.loadBalancer(controlplane.RoleIngress)
	require.NotEmpty(t, ingress.PublicIPv4, "the ingress load balancer's public address")
	var byLocation map[string]string
	require.NoError(t, json.Unmarshal([]byte(outputs["publicIpByLocation"]), &byLocation))
	assert.Equal(t, map[string]string{"fsn1": ingress.PublicIPv4}, byLocation, "publicIpByLocation")
	assert.Equal(t, "https://10.0.0.3:6443", outputs["privateEndpoint"], "the private endpoint is the API load balancer")
	assert.Equal(t, testSub, outputs["subscriptionId"])
	assert.Contains(t, detail, "an API and an ingress load balancer in fsn1 reach them")
}

func TestPlane_ALoadBalancerStepThatFailedIsTakenUpByTheNextCallWithoutADuplicate(t *testing.T) {
	for _, step := range []string{"attach", "disable-public", "add-service", "add-target"} {
		t.Run(step, func(t *testing.T) {
			h := newHarness(t)
			h.cloud.FailLBStep[step] = errors.New("hetzner answered 503 to the " + step)
			require.Error(t, h.converge(), "the call whose %s fails", step)
			assert.Empty(t, h.byRole(controlplane.RoleGateway), "a gateway built before the API load balancer was finished")

			require.NoError(t, h.converge())

			for _, r := range []controlplane.Role{controlplane.RoleAPI, controlplane.RoleIngress} {
				lb := h.loadBalancer(r)
				assert.Len(t, lb.PrivateIPs, 1, "the %s load balancer's networks", r)
				assert.Equal(t, r == controlplane.RoleIngress, lb.PublicInterface, "the %s load balancer's public interface", r)
				assert.Len(t, lb.Services, map[controlplane.Role]int{controlplane.RoleAPI: 1, controlplane.RoleIngress: 2}[r], "the %s load balancer's services", r)
				assert.Len(t, lb.TargetSelectors, 1, "the %s load balancer's targets", r)
			}
			assert.Len(t, h.byRole(controlplane.RoleGateway), len(controlplane.Locations), "gateways after the retry")
			assert.Len(t, h.byRole(controlplane.RoleBuilder), 1, "builders after the retry")
		})
	}
}

func TestPlane_TheGatewayJoinsTheNetworkAtItsFixedAddressBeforeItBoots(t *testing.T) {
	h := newHarness(t)

	require.NoError(t, h.converge())

	for _, spec := range h.cloud.ServerSpecs {
		if spec.Labels[controlplane.KeyRole] == string(controlplane.RoleGateway) {
			assert.True(t, spec.Stopped, "the gateway in %s is created stopped", spec.Location)
			assert.Empty(t, spec.NetworkIDs, "the gateway in %s is attached after creation, at its address", spec.Location)
		}
	}
	gateways := h.byRole(controlplane.RoleGateway)
	require.Len(t, gateways, len(controlplane.Locations))
	network := h.network()
	for _, gw := range gateways {
		assert.Equal(t, controlplane.GatewayAddresses[gw.Location], gw.PrivateIPs[network.ID], "the gateway in %s's private address", gw.Location)
		assert.Equal(t, controlplane.ServerRunning, gw.Status, "the gateway in %s's status", gw.Location)
	}
	assert.Equal(t, []controlplane.Route{{Destination: "0.0.0.0/0", Gateway: controlplane.GatewayAddress}}, network.Routes, "the network's routes")
}

func TestPlane_TheBuilderJoinsTheNetworkOnlyOnceTheGatewayHoldsItsAddress(t *testing.T) {
	h := newHarness(t)

	require.NoError(t, h.settle(h.building))

	network := h.network()
	assert.Equal(t, controlplane.GatewayAddress, h.byRole(controlplane.RoleGateway)[0].PrivateIPs[network.ID], "the gateway's address")
	builder := h.byRole(controlplane.RoleBuilder)[0].PrivateIPs[network.ID]
	assert.NotEmpty(t, builder, "the builder runs off the network, with no route to the gateway")
	assert.NotEqual(t, controlplane.GatewayAddress, builder, "the builder's address")
}

func TestPlane_AServerAttachThatLandedButFailedItsCallIsDoneOnTheNextCall(t *testing.T) {
	for _, r := range []controlplane.Role{controlplane.RoleGateway, controlplane.RoleBuilder} {
		t.Run(string(r), func(t *testing.T) {
			h := newHarness(t)
			h.cloud.FailAttach[r] = errors.New("context deadline exceeded while waiting for the attach action")
			require.Error(t, h.converge(), "the call whose %s attach fails after it landed", r)

			require.NoError(t, h.converge())

			assert.True(t, h.building(), "the builder runs its build")
			gw := h.byRole(controlplane.RoleGateway)[0]
			assert.Equal(t, []string{controlplane.GatewayAddress}, slices.Collect(maps.Values(gw.PrivateIPs)), "the gateway's addresses")
		})
	}
}

func TestPlane_CreatesNoGatewayWhileTheIngressLoadBalancerHasNoPublicAddress(t *testing.T) {
	h := newHarness(t)
	for range 20 {
		if len(h.cloud.LBSpecs) == 2 {
			break
		}
		require.NoError(t, h.ensure())
	}
	h.cloud.LoadBalancers[h.loadBalancer(controlplane.RoleIngress).ID].PublicIPv4 = ""

	err := h.converge()

	assert.ErrorContains(t, err, "the ingress load balancer has no public address")
	assert.Empty(t, h.byRole(controlplane.RoleGateway), "gateways created")
}

func TestPlane_RefusesAnAmbiguousClusterBeforeAnyChange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra func(h *harness)
		want  string
	}{
		{
			name: "two nodes in one location",
			extra: func(h *harness) {
				for _, node := range h.byRole(controlplane.RoleNode) {
					if node.Location == "nbg1" {
						node.ID = h.cloud.NextID()
						h.cloud.Servers[node.ID] = &node
					}
				}
			},
			want: "2 nbg1 node servers carry this subscription's labels",
		},
		{
			name: "a second API load balancer in another location",
			extra: func(h *harness) {
				lb := h.loadBalancer(controlplane.RoleAPI)
				lb.ID, lb.Location = h.cloud.NextID(), "nbg1"
				h.cloud.LoadBalancers[lb.ID] = &lb
			},
			want: "2 api load balancers carry this subscription's labels",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cloud.InstantBuild = true
			require.NoError(t, h.converge())
			tc.extra(h)
			before := h.cloud.Writes()

			err := h.ensure()

			assert.Equal(t, "ambiguous-cluster", refusalClass(t, err))
			assert.Contains(t, err.Error(), tc.want)
			assert.Equal(t, before, h.cloud.Writes(), "changes after the refusal")
		})
	}
}

func TestPlane_RefusesServersWhoseNetworkIsGone(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.converge())
	for id := range h.cloud.Networks {
		delete(h.cloud.Networks, id)
	}
	before := h.cloud.Writes()

	err := h.ensure()

	assert.Equal(t, "network-gone", refusalClass(t, err))
	assert.Equal(t, before, h.cloud.Writes(), "changes after the refusal")
}

func TestPlane_RefusesNodesWhoseGatewayIsGone(t *testing.T) {
	h := newHarness(t)
	h.built()
	for _, gw := range h.byRole(controlplane.RoleGateway) {
		require.NoError(t, h.cloud.DeleteServer(context.Background(), gw.ID))
	}

	err := h.ensure()

	assert.Equal(t, "gateway-lost", refusalClass(t, err))
}

func TestPlane_DeletesABuilderLeftBesideTheSnapshotWhoseDeleteFailed(t *testing.T) {
	h := newHarness(t)
	h.cloud.InstantBuild = true
	h.cloud.FailDelete = errors.New("hetzner answered 503 to the builder's delete")
	require.Error(t, h.converge(), "the call whose builder delete fails")

	require.NoError(t, h.converge())

	assert.Empty(t, h.byRole(controlplane.RoleBuilder), "a powered-off builder, whose disk holds every cluster key, outlived its snapshot")
	assert.Len(t, h.byRole(controlplane.RoleNode), 3, "nodes")
}

func TestPlane_AnInstallThatFailsAfterTheGatewayExistsLeavesNothingTheNextCallRefuses(t *testing.T) {
	h := newHarness(t)
	h.cloud.FailCreate[controlplane.RoleBuilder] = errors.New("hetzner answered 503 to the builder's create")
	require.Error(t, h.converge(), "the call whose builder create fails")
	assert.Empty(t, h.byRole(controlplane.RoleGateway), "the gateway of a failed install")
	assert.Empty(t, h.byRole(controlplane.RoleBuilder), "the builder of a failed install")

	require.NoError(t, h.converge())

	assert.Len(t, h.byRole(controlplane.RoleGateway), len(controlplane.Locations), "gateways after the retry")
	assert.Len(t, h.byRole(controlplane.RoleBuilder), 1, "builders after the retry")
}

func TestPlane_TheGatewayTakesTheFirstTypeTheLocationCanOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		soldOut []string
		want    string
	}{
		{name: "cx23 when the location can order it", want: "cx23"},
		{name: "cpx22 when cx23 is sold out", soldOut: []string{"cx23"}, want: "cpx22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cloud.SoldOut = tc.soldOut

			h.built()

			gateways := h.byRole(controlplane.RoleGateway)
			require.Len(t, gateways, len(controlplane.Locations), "gateways")
			for _, gw := range gateways {
				assert.Equal(t, tc.want, gw.ServerType, "the gateway in %s's server type", gw.Location)
			}
		})
	}
}

func TestPlane_RefusesBeforeAnyServerWhenNoGatewayTypeCanBeOrdered(t *testing.T) {
	h := newHarness(t)
	h.cloud.SoldOut = []string{"cx23", "cpx22"}

	err := h.converge()

	assert.Equal(t, "server-unavailable", refusalClass(t, err))
	for _, want := range []string{"cx23", "cpx22", "fsn1"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.Empty(t, h.cloud.ServerSpecs, "servers created with no gateway type to order")
}

func TestPlane_OutputsTheIssuerItsNodesSignTokensForInAnIssuerBucketOfItsOwn(t *testing.T) {
	h := newHarness(t)
	h.built()

	outputs, _ := h.plane.Outputs(h.read())

	bucket := outputs["issuerBucket"]
	assert.Regexp(t, `^mitstack-oidc-[a-z2-7]{26}$`, bucket, "issuerBucket")
	assert.Equal(t, "https://"+bucket+".s3.eu-central-1.amazonaws.com/"+testSub, outputs["issuerUrl"], "issuerUrl")
	accounts := testutil.Kind(builderConfiguration(t, h), "KubeServiceAccountConfig")
	require.Len(t, accounts, 1, "the builder's machine configuration")
	assert.Equal(t, outputs["issuerUrl"], testutil.Field(t, accounts[0], "issuer", "issuerURL"), "the issuer every node signs service-account tokens for")
}

func TestPlane_AGatewayWithoutItsLabelsIsAProblemAndCarriesNoEnrolmentDigest(t *testing.T) {
	h := newHarness(t)
	h.built()
	gw := h.byRole(controlplane.RoleGateway)[0]
	digest, gap := h.read().EnrolmentDigest()
	require.Empty(t, gap)
	assert.Equal(t, gw.Labels[controlplane.KeyEnrolmentSHA256], digest, "the enrolment digest")
	delete(h.cloud.Servers[gw.ID].Labels, controlplane.KeyIssuerBucket)
	delete(h.cloud.Servers[gw.ID].Labels, controlplane.KeyEnrolmentSHA256)

	problems := h.plane.Problems(context.Background(), h.read())
	digest, gap = h.read().EnrolmentDigest()

	assert.Contains(t, strings.Join(problems, "; "), "no "+controlplane.KeyIssuerBucket+" label")
	assert.Empty(t, digest)
	assert.Contains(t, gap, "no "+controlplane.KeyEnrolmentSHA256+" label")
}

func TestPlane_TheInstalledHookReceivesTheClusterWithItsKeysInTheInstallsCall(t *testing.T) {
	h := newHarness(t)

	require.NoError(t, h.converge())

	require.Len(t, h.installed, 1, "clusters the hook received")
	c := h.installed[0]
	require.NotNil(t, c.Secrets, "the cluster's keys")
	assert.Equal(t, testSub, c.SubscriptionID)
	assert.Equal(t, []string{"10.0.0.3"}, []string{c.APIAddresses[0].String()}, "the cluster's API address")
	assert.Equal(t, controlplane.GatewayAddress, c.Gateways[0].String(), "the cluster's gateway")
	worker, err := nodeconfig.RenderWorker(c, nodeconfig.Node{})
	require.NoError(t, err, "a worker renders from the cluster the hook received")
	builderMachine := builderConfiguration(t, h)[0]
	workerMachine := testutil.Documents(t, worker)[0]
	assert.Equal(t, testutil.Field(t, builderMachine, "machine", "ca", "crt"), testutil.Field(t, workerMachine, "machine", "ca", "crt"),
		"the worker trusts the control plane's machine CA")
	assert.Equal(t, testutil.Field(t, builderMachine, "cluster", "token"), testutil.Field(t, workerMachine, "cluster", "token"),
		"the worker joins with the control plane's cluster token")
}

func TestPlane_AnInstalledHookThatFailsRollsTheInstallBack(t *testing.T) {
	h := newHarness(t)
	h.installErr = errors.New("the worker's install could not start")

	err := h.converge()

	require.ErrorIs(t, err, h.installErr)
	assert.Empty(t, h.byRole(controlplane.RoleGateway), "the gateway of a rolled-back install")
	assert.Empty(t, h.byRole(controlplane.RoleBuilder), "the builder of a rolled-back install")
}

func TestPlane_AppsOnWorkersKeepsTheControlPlanesTaint(t *testing.T) {
	for _, tc := range []struct {
		name          string
		appsOnWorkers bool
		want          any
	}{
		{name: "a plane that runs apps itself", want: nil},
		{name: "a plane whose apps run on workers", appsOnWorkers: true,
			want: map[string]any{"node-role.kubernetes.io/control-plane": "NoSchedule"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) { h.plane.AppsOnWorkers = tc.appsOnWorkers })

			require.NoError(t, h.converge())

			nodeConfigs := testutil.Kind(builderConfiguration(t, h), "KubeNodeConfig")
			require.Len(t, nodeConfigs, 1, "KubeNodeConfig documents")
			assert.Equal(t, tc.want, nodeConfigs[0]["taints"], "the control-plane node's taints")
		})
	}
}

func TestPlane_DeleteRemovesEveryResourceItBuilt(t *testing.T) {
	h := newHarness(t)
	h.built()

	require.NoError(t, h.plane.Delete(context.Background()))

	assert.Zero(t, h.cloud.Count(), "resources after the delete")
	assert.True(t, h.read().Empty(), "the plane after the delete")
}

// A module that requires neither framework cannot import either, so a block on any framework release can use the plane.
func TestPlane_TheKitRequiresNeitherBlockKitNorOperatorKit(t *testing.T) {
	mod, err := os.ReadFile("../go.mod")
	require.NoError(t, err)

	assert.NotRegexp(t, `github\.com/mitstack-eu/(block-kit|operator-kit)\b`, string(mod), "the kit's go.mod")
}
