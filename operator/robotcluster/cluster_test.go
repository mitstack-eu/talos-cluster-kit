package robotcluster_test

// A cluster needs its operator's id, boundary and worker install, names to replace only the nodes and Cloud workers a
// request names that the cluster runs, and refuses a control-plane size its block declares no servers for or the
// running cluster was not subscribed with.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
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

func sizeRequest(params map[string]string) contract.Desired {
	params["appName"] = "shop"
	return contract.Desired{
		Identity: contract.Identity{SubscriptionID: "sub_0123456789ab"},
		Params:   params,
		Secrets:  map[string]map[string]string{"hetzner": {"api_token": "test-token"}},
	}
}

func sizedCluster(declared int) *robotcluster.Cluster {
	cloud := cloudtest.New(time.Now)
	return robotcluster.New(robotcluster.Config{
		Block: testBlock, Boundary: "test-boundary", InstallWorker: installNothing, DeclaredSize: declared,
		CloudAPI: func(string) controlplane.API { return cloud },
	})
}

func TestPlanReplacements_RefusesAControlPlaneSizeTheBlockDeclaresNoServersForNamingTheSizeItKeeps(t *testing.T) {
	k := sizedCluster(3)
	for _, tc := range []struct {
		name, class, text string
		params            map[string]string
	}{
		{"one server", "control-plane-size-undeclared", "this block lists a control-plane server in each of 3 locations, and mitstack waits for each one",
			map[string]string{"controlPlaneSize": "1"}},
		{"two servers", "invalid-param", "a control plane runs 1 or 3 servers", map[string]string{"controlPlaneSize": "2"}},
		{"one server for a cluster that hosts the platform", "restore-drill-missing", "ADR 0125 restore drill",
			map[string]string{"controlPlaneSize": "1", "hostsPlatform": "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := k.PlanReplacements(context.Background(), sizeRequest(tc.params))

			var refused *handler.Refused
			require.ErrorAs(t, err, &refused, "an upgrade asking for %v", tc.params)
			assert.Equal(t, tc.class, refused.Class)
			assert.ErrorContains(t, err, tc.text)
		})
	}
	for _, size := range []string{"3", ""} {
		_, err := k.PlanReplacements(context.Background(), sizeRequest(map[string]string{"controlPlaneSize": size}))
		assert.NoError(t, err, "an upgrade asking for controlPlaneSize %q", size)
	}
}

func TestEnsure_RefusesAControlPlaneSizeTheBlockDeclaresNoServersForBeforeAnyChange(t *testing.T) {
	cloud := cloudtest.New(time.Now)
	k := robotcluster.New(robotcluster.Config{
		Block: testBlock, Boundary: "test-boundary", InstallWorker: installNothing, DeclaredSize: 3,
		CloudAPI: func(string) controlplane.API { return cloud },
	})

	err := k.Ensure(context.Background(), sizeRequest(map[string]string{"controlPlaneSize": "1", robotcluster.ParamRobotServer: "1234567"}))

	var refused *handler.Refused
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, "control-plane-size-undeclared", refused.Class, "the size is refused before the Robot login: %v", err)
	assert.Zero(t, cloud.Writes(), "a refused subscribe changes nothing in Hetzner Cloud")
}

func TestPlanReplacements_TakesOneOrThreeServersForABlockThatDeclaresNoSize(t *testing.T) {
	k := sizedCluster(0)
	for _, size := range []string{"1", "3"} {
		_, err := k.PlanReplacements(context.Background(), sizeRequest(map[string]string{"controlPlaneSize": size}))
		assert.NoError(t, err, "an upgrade asking for controlPlaneSize %q", size)
	}
}

func TestPlanReplacements_RefusesAControlPlaneSizeOtherThanTheOneTheRunningClusterWasSubscribedWith(t *testing.T) {
	ctx := context.Background()
	cloud := cloudtest.New(time.Now)
	k := robotcluster.New(robotcluster.Config{
		Block: testBlock, Boundary: "test-boundary", InstallWorker: installNothing,
		CloudAPI: func(string) controlplane.API { return cloud },
	})
	labels := k.PlaneFor(cloud, sizeRequest(map[string]string{})).Labels("network")
	labels[controlplane.KeySize] = "3"
	_, err := cloud.CreateNetwork(ctx, controlplane.NetworkSpec{Name: "subscribed", IPRange: controlplane.NetworkIPRange, Labels: labels})
	require.NoError(t, err)
	writes := cloud.Writes()

	_, err = k.PlanReplacements(ctx, sizeRequest(map[string]string{"controlPlaneSize": "1"}))

	var refused *handler.Refused
	require.ErrorAs(t, err, &refused, "an upgrade of a three-server cluster asking for one server")
	assert.Equal(t, "control-plane-size-fixed", refused.Class)
	assert.ErrorContains(t, err, "subscribed with controlPlaneSize 3, and a control plane keeps its size after subscribe")
	_, err = k.PlanReplacements(ctx, sizeRequest(map[string]string{"controlPlaneSize": "3"}))
	assert.NoError(t, err, "an upgrade asking for the size the cluster was subscribed with")
	assert.Equal(t, writes, cloud.Writes(), "Hetzner Cloud writes the dry runs made")
}

type networksFail struct {
	*cloudtest.Fake
	err error
}

func (n networksFail) ListNetworks(context.Context, string) ([]controlplane.Network, error) {
	return nil, n.err
}

func TestPlanReplacements_ReturnsAFailedNetworkReadRatherThanPassingTheSize(t *testing.T) {
	failed := errors.New("hetzner answered 503")
	k := robotcluster.New(robotcluster.Config{
		Block: testBlock, Boundary: "test-boundary", InstallWorker: installNothing,
		CloudAPI: func(string) controlplane.API { return networksFail{cloudtest.New(time.Now), failed} },
	})

	_, err := k.PlanReplacements(context.Background(), sizeRequest(map[string]string{"controlPlaneSize": "1"}))

	assert.ErrorIs(t, err, failed, "an upgrade whose network read fails")
}

func TestPlaneFor_RunsTheSizeTheRequestNamesAndThreeForARequestThatNamesNone(t *testing.T) {
	k := sizedCluster(0)

	one := k.PlaneFor(nil, sizeRequest(map[string]string{"controlPlaneSize": "1"}))
	unnamed := k.PlaneFor(nil, sizeRequest(map[string]string{}))

	assert.Equal(t, []string{"fsn1"}, one.NodeLocations(), "the locations of a cluster asked for one server")
	assert.Equal(t, controlplane.Locations, unnamed.NodeLocations(), "the locations of a cluster that names no size")
	assert.Equal(t, "shop", one.AppName)
}
