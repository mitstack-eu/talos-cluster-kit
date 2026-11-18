package controlplane_test

// A plane whose apps run on workers sends the ingress load balancer's traffic to its workers, never its control plane.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	workerAddress = "10.0.1.2"
	workerSubnet  = "10.0.1.0/24"
	vswitchID     = 7
)

// appsOnWorkers runs apps on a Robot worker whose vSwitch subnet the install couples to the network, as a block's does.
func appsOnWorkers(h *harness) {
	h.plane.AppsOnWorkers, h.plane.WorkerAddresses = true, []string{workerAddress}
	installed := h.plane.Installed
	h.plane.Installed = func(ctx context.Context, n controlplane.Network, c nodeconfig.Cluster, ca gateway.CA) error {
		if err := h.cloud.AddVSwitchSubnet(ctx, n.ID, vswitchID, workerSubnet); err != nil {
			return err
		}
		return installed(ctx, n, c, ca)
	}
}

func TestPlane_TheIngressTargetsEachWorkerAndNoControlPlaneNodeWhenAppsRunOnWorkers(t *testing.T) {
	for _, tc := range []struct {
		name          string
		opts          []func(*harness)
		nodes         bool
		wantAddresses []string
		wantDetail    string
	}{
		{name: "a plane that runs apps itself", nodes: true, wantDetail: "an API and an ingress load balancer in fsn1 reach them"},
		{name: "a plane whose apps run on workers", opts: []func(*harness){appsOnWorkers}, wantAddresses: []string{workerAddress},
			wantDetail: "an API load balancer in fsn1 reaches them, and its ingress load balancer the workers at 10.0.1.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)

			h.built()

			nodes := []string{h.plane.RoleSelector(controlplane.RoleNode)}
			ingress := h.loadBalancer(controlplane.RoleIngress)
			if tc.nodes {
				assert.Equal(t, nodes, ingress.TargetSelectors, "the ingress load balancer's label targets")
			} else {
				assert.Empty(t, ingress.TargetSelectors, "the ingress load balancer's label targets")
			}
			assert.Equal(t, tc.wantAddresses, ingress.TargetIPs, "the ingress load balancer's address targets")
			api := h.loadBalancer(controlplane.RoleAPI)
			assert.Equal(t, nodes, api.TargetSelectors, "the API load balancer's label targets")
			assert.Empty(t, api.TargetIPs, "the API load balancer's address targets")
			_, detail := h.plane.Outputs(h.read())
			assert.True(t, strings.HasSuffix(detail, tc.wantDetail), "the detail %q ends %q", detail, tc.wantDetail)
		})
	}
}

func TestPlane_AClusterBuiltBeforeAppsRanOnWorkersLosesItsControlPlaneIngressTarget(t *testing.T) {
	h := newHarness(t)
	h.built()
	ctx := context.Background()
	h.plane.AppsOnWorkers, h.plane.WorkerAddresses = true, []string{workerAddress}
	require.NoError(t, h.cloud.AddVSwitchSubnet(ctx, h.network().ID, vswitchID, workerSubnet))

	assert.Len(t, h.plane.Problems(ctx, h.read()), 2, "the problems of an ingress on the control plane and not the worker")
	require.NoError(t, h.converge())

	ingress := h.loadBalancer(controlplane.RoleIngress)
	assert.Empty(t, ingress.TargetSelectors, "the ingress load balancer's label targets")
	assert.Equal(t, []string{workerAddress}, ingress.TargetIPs, "the ingress load balancer's address targets")
	assert.Equal(t, []string{h.plane.RoleSelector(controlplane.RoleNode)}, h.loadBalancer(controlplane.RoleAPI).TargetSelectors,
		"the API load balancer's label targets")
	assert.Empty(t, h.plane.Problems(ctx, h.read()), "the plane's problems once the ingress targets the worker")
}

func TestPlane_AnIngressKeepsItsControlPlaneTargetUntilItCanTargetTheWorker(t *testing.T) {
	h := newHarness(t)
	h.built()
	ctx := context.Background()
	h.plane.AppsOnWorkers, h.plane.WorkerAddresses = true, []string{workerAddress}

	require.NoError(t, h.converge())

	ingress := h.loadBalancer(controlplane.RoleIngress)
	assert.Equal(t, []string{h.plane.RoleSelector(controlplane.RoleNode)}, ingress.TargetSelectors,
		"the ingress load balancer's label targets before the worker's subnet is coupled")
	assert.Empty(t, ingress.TargetIPs, "the ingress load balancer's address targets before the worker's subnet is coupled")
	assert.Contains(t, h.plane.Problems(ctx, h.read()), fmt.Sprintf("load balancer %d does not target the worker at %s", ingress.ID, workerAddress))
}

func TestPlane_AppsOnWorkersWithNoWorkerAddressIsAProblem(t *testing.T) {
	h := newHarness(t)
	h.built()

	h.plane.AppsOnWorkers = true

	assert.Contains(t, h.plane.Problems(context.Background(), h.read()),
		"apps run on workers, but the plane names no worker address for the ingress load balancer to target")
}

// appsOnCloudPool runs apps on the Cloud pool's workers alone, as a cluster with no Robot worker does.
func appsOnCloudPool(h *harness) {
	h.plane.AppsOnWorkers, h.plane.IngressTargetsCloudPool = true, true
}

// alsoOnCloudPool makes the ingress load balancer target the Cloud pool's workers beside any worker address.
func alsoOnCloudPool(h *harness) { h.plane.IngressTargetsCloudPool = true }

// builtOnCloudPool builds the plane and grows its Cloud pool to one worker once the worker image exists, as a block does.
func (h *harness) builtOnCloudPool() {
	h.t.Helper()
	imageReady := func() bool {
		images := h.workerImages()
		return len(images) == 1 && images[0].Status == controlplane.SnapshotAvailable
	}
	require.NoError(h.t, h.settle(imageReady))
	require.True(h.t, imageReady(), "the worker image never became available")
	h.poolWorkers(1, "fsn1")
	h.built()
}

const emptyPoolProblem = "the ingress load balancer targets the Cloud pool's workers, but the pool has no worker"

func TestPlane_TheIngressTargetsTheCloudPoolsWorkersByLabelWhenThePlaneSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name          string
		opts          []func(*harness)
		wantAddresses []string
		wantDetail    string
	}{
		{name: "a plane whose apps run on the Cloud pool's workers alone", opts: []func(*harness){appsOnCloudPool},
			wantDetail: "an API load balancer in fsn1 reaches them, and its ingress load balancer the Cloud pool's workers"},
		{name: "a plane whose apps run on a Robot worker and the Cloud pool's workers",
			opts: []func(*harness){appsOnWorkers, alsoOnCloudPool}, wantAddresses: []string{workerAddress},
			wantDetail: "an API load balancer in fsn1 reaches them, and its ingress load balancer the Cloud pool's workers and the workers at " + workerAddress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)

			h.builtOnCloudPool()

			ingress := h.loadBalancer(controlplane.RoleIngress)
			assert.Equal(t, []string{h.plane.RoleSelector(controlplane.RoleCloudWorker)}, ingress.TargetSelectors,
				"the ingress load balancer's label targets")
			assert.Equal(t, tc.wantAddresses, ingress.TargetIPs, "the ingress load balancer's address targets")
			api := h.loadBalancer(controlplane.RoleAPI)
			assert.Equal(t, []string{h.plane.RoleSelector(controlplane.RoleNode)}, api.TargetSelectors, "the API load balancer's label targets")
			assert.Empty(t, api.TargetIPs, "the API load balancer's address targets")
			_, detail := h.plane.Outputs(h.read())
			assert.True(t, strings.HasSuffix(detail, tc.wantDetail), "the detail %q ends %q", detail, tc.wantDetail)
		})
	}
}

func TestPlane_ALoadBalancerOfAPlaneWhoseIngressIsNotOnTheCloudPoolDropsAStaleLabelTargetWhileNoNodeStands(t *testing.T) {
	h := newHarness(t)
	h.built()
	ctx := context.Background()
	const stale = "mitstack.dev/role=retired"
	require.NoError(t, h.cloud.AddLoadBalancerTarget(ctx, h.loadBalancer(controlplane.RoleAPI).ID, stale))
	for _, node := range h.byRole(controlplane.RoleNode) {
		require.NoError(t, h.cloud.DeleteServer(ctx, node.ID))
	}
	h.steps = nil

	require.NoError(t, h.ensure())

	assert.Equal(t, []string{"stop the api load balancer in fsn1 targeting the servers " + stale + " selects"}, h.steps,
		"the step a plane takes while no node stands")
}

func TestPlane_AClusterBuiltWithItsIngressOnTheControlPlaneMovesItToTheCloudPoolsWorkers(t *testing.T) {
	h := newHarness(t)
	h.built()
	h.poolWorkers(1, "fsn1")
	ctx := context.Background()
	appsOnCloudPool(h)
	ingress := h.loadBalancer(controlplane.RoleIngress)

	before := h.plane.Problems(ctx, h.read())
	require.NoError(t, h.converge())

	assert.Contains(t, before, fmt.Sprintf("load balancer %d does not target the subscription's nodes by label", ingress.ID),
		"the problems of an ingress not yet on the Cloud pool's workers")
	assert.Contains(t, before, fmt.Sprintf("load balancer %d still targets the servers %s selects", ingress.ID,
		h.plane.RoleSelector(controlplane.RoleNode)), "the problems of an ingress still on the control plane")
	assert.Equal(t, []string{h.plane.RoleSelector(controlplane.RoleCloudWorker)}, h.loadBalancer(controlplane.RoleIngress).TargetSelectors,
		"the ingress load balancer's label targets once moved")
	assert.Empty(t, h.plane.Problems(ctx, h.read()), "the plane's problems once the ingress targets the Cloud pool's workers")
}

func TestPlane_AnIngressMovedToAnEmptyCloudPoolKeepsItsControlPlaneTargetAndReadsAProblemUntilAWorkerJoins(t *testing.T) {
	h := newHarness(t)
	h.built()
	ctx := context.Background()
	appsOnCloudPool(h)
	nodes, pool := h.plane.RoleSelector(controlplane.RoleNode), h.plane.RoleSelector(controlplane.RoleCloudWorker)

	require.NoError(t, h.converge())

	assert.ElementsMatch(t, []string{nodes, pool}, h.loadBalancer(controlplane.RoleIngress).TargetSelectors,
		"the ingress load balancer's label targets while the Cloud pool has no worker")
	assert.Contains(t, h.plane.Problems(ctx, h.read()), emptyPoolProblem, "the problems of an ingress on an empty Cloud pool")

	h.poolWorkers(1, "fsn1")
	require.NoError(t, h.converge())

	assert.Equal(t, []string{pool}, h.loadBalancer(controlplane.RoleIngress).TargetSelectors,
		"the ingress load balancer's label targets once a Cloud worker joined")
	assert.Empty(t, h.plane.Problems(ctx, h.read()), "the plane's problems once a Cloud worker joined")
}

func TestPlane_AnIngressOnTheCloudPoolOfAPlaneWhoseAppsAlsoRunOnTheControlPlaneIsAProblem(t *testing.T) {
	h := newHarness(t)
	h.built()
	h.poolWorkers(1, "fsn1")

	h.plane.IngressTargetsCloudPool = true

	assert.Contains(t, h.plane.Problems(context.Background(), h.read()),
		"the ingress load balancer targets the Cloud pool's workers, but apps also run on the control-plane nodes")
}

// robotWorkerRecord is the golden file's record of a plane whose apps run on a Robot worker.
type robotWorkerRecord struct {
	Steps    []string
	API      controlplane.LoadBalancer
	Ingress  controlplane.LoadBalancer
	Problems []string
	Detail   string
}

func TestPlane_APlaneWhoseAppsRunOnARobotWorkerBuildsAndReadsAsItsGoldenRecords(t *testing.T) {
	h := newHarness(t, appsOnWorkers)

	h.built()

	_, detail := h.plane.Outputs(h.read())
	record := robotWorkerRecord{
		Steps: h.steps, API: h.loadBalancer(controlplane.RoleAPI), Ingress: h.loadBalancer(controlplane.RoleIngress),
		Problems: h.plane.Problems(context.Background(), h.read()), Detail: detail,
	}
	got, err := json.MarshalIndent(record, "", "  ")
	require.NoError(t, err)
	testutil.Golden(t, "testdata/robot-worker-ingress.golden", append(got, '\n'))
}
