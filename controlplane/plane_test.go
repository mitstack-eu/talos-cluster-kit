package controlplane_test

// A plane builds a three-node Talos cluster one change a call: the API and ingress load balancers in fsn1, gateway and
// builder, then the snapshot, then one node in each of fsn1, nbg1 and hel1 booted from it.

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
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
	// installed holds each cluster the Installed hook received, and installErr is what the hook answers.
	installed  []nodeconfig.Cluster
	installErr error
	// hostingKeys holds the keys each call to a hosting plane's DeployKeys hook received, and deployKeysErr is what it answers.
	hostingKeys   []controlplane.HostingKeys
	deployKeysErr error
	// waits holds how many Hetzner actions each call waited on.
	waits      []int
	sequential bool
}

func newHarness(t *testing.T, opts ...func(*harness)) *harness {
	t.Helper()
	h := &harness{t: t, now: testStart, buildTakes: 10 * time.Minute, imageTakes: 3 * time.Minute}
	h.cloud = cloudtest.New(func() time.Time { return h.now })
	h.plane = controlplane.Plane{
		API: h.cloud, Block: testBlock, Subscription: testSub, AppName: testApp, ServerType: controlplane.DefaultServerType,
		Boundary: "mitstack-test-boundary", PlatformURL: platformURL, RepoURL: testRepoURL,
		Now: func() time.Time { return h.now },
		Installed: func(_ context.Context, _ controlplane.Network, c nodeconfig.Cluster) error {
			h.installed = append(h.installed, c)
			return h.installErr
		},
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

// ensure takes the one change the plane lacks, as a block's Ensure does.
func (h *harness) ensure() error {
	ctx := context.Background()
	before := h.cloud.Waits()
	defer func() { h.waits = append(h.waits, h.cloud.Waits()-before) }()
	st, err := h.plane.Read(ctx)
	if err != nil {
		return err
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
	return step.Take(ctx, testBlock, testSub)
}

// converge calls ensure until one changes nothing, as the engine's re-drives would, and returns the first error.
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

// settle converges, then lets a minute pass for Hetzner and the builder, until the plane has no problem, stop reads
// true, a call fails, or the settle budget runs out.
func (h *harness) settle(stop func() bool) error {
	h.t.Helper()
	for began := h.now; h.now.Sub(began) < settleBudget; {
		if err := h.converge(); err != nil {
			return err
		}
		if stop != nil && stop() {
			return nil
		}
		if len(h.plane.Problems(context.Background(), h.read())) == 0 {
			return nil
		}
		h.now = h.now.Add(time.Minute)
		h.cloud.Advance(h.buildTakes, h.imageTakes)
	}
	return errors.New("the plane did not settle within the budget")
}

// built settles the plane and requires it to stand with no problem.
func (h *harness) built() {
	h.t.Helper()
	require.NoError(h.t, h.settle(nil))
	require.Empty(h.t, h.plane.Problems(context.Background(), h.read()), "the plane's problems once settled")
}

// building reports whether the builder runs its build.
func (h *harness) building() bool {
	builders := h.byRole(controlplane.RoleBuilder)
	return len(builders) == 1 && builders[0].Status == controlplane.ServerRunning
}

// buildingBuilder settles the plane until its builder runs a build that never ends.
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
	gateways := h.byRole(controlplane.RoleGateway)
	require.Len(t, gateways, 1, "gateways")
	assert.Equal(t, "fsn1", gateways[0].Location, "the gateway's location")
	assert.Equal(t, "fsn1", h.cloud.SpecFor(t, controlplane.RoleBuilder).Location, "the builder's location")
	assert.Empty(t, h.byRole(controlplane.RoleBuilder), "a builder outlived its snapshot")
}

func TestPlane_BuildsTheBuilderAndEveryNodeOnTheServerTypeItIsGiven(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.plane.ServerType = "cpx32" })

	h.built()

	typeByLocation := map[string]string{}
	for _, node := range h.byRole(controlplane.RoleNode) {
		typeByLocation[node.Location] = node.ServerType
	}
	assert.Equal(t, map[string]string{"fsn1": "cpx32", "nbg1": "cpx32", "hel1": "cpx32"}, typeByLocation, "each location's node server type")
	assert.Equal(t, "cpx32", h.cloud.SpecFor(t, controlplane.RoleBuilder).ServerType, "the builder's server type")
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
	for _, r := range []controlplane.Role{controlplane.RoleAPI, controlplane.RoleIngress} {
		lb := h.loadBalancer(r)
		assert.Equal(t, r == controlplane.RoleIngress, lb.PublicInterface, "load balancer %s's public interface", lb.Name)
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
			assert.Len(t, h.byRole(controlplane.RoleGateway), 1, "gateways after the retry")
			assert.Len(t, h.byRole(controlplane.RoleBuilder), 1, "builders after the retry")
		})
	}
}

func TestPlane_TheGatewayJoinsTheNetworkAtItsFixedAddressBeforeItBoots(t *testing.T) {
	h := newHarness(t)

	require.NoError(t, h.converge())

	gw := h.cloud.SpecFor(t, controlplane.RoleGateway)
	assert.True(t, gw.Stopped, "the gateway is created stopped")
	assert.Empty(t, gw.NetworkIDs, "the gateway is attached after creation, at its address")
	gateways := h.byRole(controlplane.RoleGateway)
	require.Len(t, gateways, 1)
	network := h.network()
	assert.Equal(t, controlplane.GatewayAddress, gateways[0].PrivateIPs[network.ID], "the gateway's private address")
	assert.Equal(t, controlplane.ServerRunning, gateways[0].Status, "the gateway's status")
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
	require.NoError(t, h.cloud.DeleteServer(context.Background(), h.byRole(controlplane.RoleGateway)[0].ID))

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

	assert.Len(t, h.byRole(controlplane.RoleGateway), 1, "gateways after the retry")
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
			require.Len(t, gateways, 1, "gateways")
			assert.Equal(t, tc.want, gateways[0].ServerType, "the gateway's server type")
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
