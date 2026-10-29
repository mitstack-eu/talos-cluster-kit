package controlplane_test

// A plane whose apps run on workers sends the ingress load balancer's traffic to its workers, never its control plane.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
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
	h.plane.Installed = func(ctx context.Context, n controlplane.Network, c nodeconfig.Cluster) error {
		if err := h.cloud.AddVSwitchSubnet(ctx, n.ID, vswitchID, workerSubnet); err != nil {
			return err
		}
		return installed(ctx, n, c)
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
