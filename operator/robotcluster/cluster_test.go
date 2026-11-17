package robotcluster_test

// A cluster needs its operator's id, boundary and worker install, and names to replace only the nodes and Cloud
// workers a request names that the cluster runs.

import (
	"context"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBlock = "test-robot-cluster"

func installNothing(context.Context, robotcluster.WorkerInstall) error { return nil }

func TestNew_PanicsOnAConfigWithoutItsBlockItsBoundaryOrItsWorkerInstall(t *testing.T) {
	full := robotcluster.Config{Block: testBlock, Boundary: "test-boundary", InstallWorker: installNothing}
	assert.NotPanics(t, func() { robotcluster.New(full) })

	noBlock, noBoundary, noInstall := full, full, full
	noBlock.Block, noBoundary.Boundary, noInstall.InstallWorker = "", "", nil
	for name, cfg := range map[string]robotcluster.Config{"block": noBlock, "boundary": noBoundary, "worker install": noInstall} {
		assert.Panics(t, func() { robotcluster.New(cfg) }, "a Config without its %s", name)
	}
}

func TestRobotName_KeepsTheWholeSubscriptionIdSoNoOtherSubscriptionsNameMatches(t *testing.T) {
	assert.Equal(t, "mitstack-sub-0123456789ab", robotcluster.RobotName("sub_0123456789ab"))
	assert.NotEqual(t, robotcluster.RobotName("sub_0123456789ab"), robotcluster.RobotName("sub_0123456789ac"))
}

func TestPlanReplacements_NamesOnlyTheNodesAndCloudWorkersTheRequestNamesThatTheClusterRuns(t *testing.T) {
	ctx := context.Background()
	cloud := cloudtest.New(time.Now)
	k := robotcluster.New(robotcluster.Config{
		Block: testBlock, Boundary: "test-boundary", InstallWorker: installNothing,
		CloudAPI: func(string) controlplane.API { return cloud },
	})
	plane := k.NewPlane(cloud, "sub_0123456789ab", "shop")
	worker := plane.Labels(pools.RoleCloudWorker)
	worker[pools.KeyMember] = "w1"
	_, err := cloud.CreateServers(ctx,
		controlplane.ServerSpec{Name: "node-fsn1", Location: "fsn1", Labels: plane.Labels(controlplane.RoleNode)},
		controlplane.ServerSpec{Name: "node-nbg1", Location: "nbg1", Labels: plane.Labels(controlplane.RoleNode)},
		controlplane.ServerSpec{Name: "worker-w1", Location: "fsn1", Labels: worker},
	)
	require.NoError(t, err)

	d := contract.Desired{
		Identity: contract.Identity{SubscriptionID: "sub_0123456789ab"},
		Params:   map[string]string{"appName": "shop"},
		Secrets:  map[string]map[string]string{"hetzner": {"api_token": "test-token"}},
		Replace: []string{
			robotcluster.Located(robotcluster.ResourceNode, "fsn1"), robotcluster.Located(robotcluster.ResourceNode, "hel1"),
			robotcluster.MemberName("w1"), robotcluster.MemberName("w2"), "gateway-fsn1",
		},
	}
	names, err := k.PlanReplacements(ctx, d)
	require.NoError(t, err)
	assert.Equal(t, []string{"node-fsn1", "cloud-worker/w1"}, names, "the node in hel1 and the worker w2 are not the cluster's, and a gateway is no node or Cloud worker")

	d.Secrets = nil
	names, err = k.PlanReplacements(ctx, d)
	require.NoError(t, err)
	assert.Empty(t, names, "a request without a Hetzner token names nothing, since the upgrade refuses it before it reads the cluster")
}
