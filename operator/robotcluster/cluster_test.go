package robotcluster_test

// A cluster needs its operator's id, boundary and worker install, names to replace only the nodes and Cloud workers a
// request names that the cluster runs, refuses a control-plane size its block declares no servers for or the running
// cluster was not subscribed with, and rolls out the runner services its operator's Config asks for.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster/robotclustertest"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
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
	plane, err := k.PlaneFor(cloud, sizeRequest(map[string]string{}))
	require.NoError(t, err)
	labels := plane.Labels("network")
	labels[controlplane.KeySize] = "3"
	_, err = cloud.CreateNetwork(ctx, controlplane.NetworkSpec{Name: "subscribed", IPRange: controlplane.NetworkIPRange, Labels: labels})
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

	one, oneErr := k.PlaneFor(nil, sizeRequest(map[string]string{"controlPlaneSize": "1"}))
	unnamed, unnamedErr := k.PlaneFor(nil, sizeRequest(map[string]string{}))

	require.NoError(t, oneErr)
	require.NoError(t, unnamedErr)
	assert.Equal(t, []string{"fsn1"}, one.NodeLocations(), "the locations of a cluster asked for one server")
	assert.Equal(t, controlplane.Locations, unnamed.NodeLocations(), "the locations of a cluster that names no size")
	assert.Equal(t, "shop", one.AppName)
}

const (
	runnerImage = "ghcr.io/actions/actions-runner:2.338.2@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300"
	dindImage   = "docker.io/library/docker:29.10.0-dind@sha256:4bbc7223f81db790097c6dea42fb93e05b8fead4f0431fbe6ccaba6e8473201c"
)

func sharedPool(contract.Desired) (*components.RunnerInputs, error) {
	return &components.RunnerInputs{Organization: robotclustertest.Owner, Group: "mitstack-shared", ScaleSet: "mitstack-ci",
		RunnerImage: runnerImage, DindImage: dindImage, MaxRunners: 4, LeaseResource: "github.runner-cluster"}, nil
}

func withRunners(runners func(contract.Desired) (*components.RunnerInputs, error)) robotclustertest.Option {
	return func(e *robotclustertest.Env) { e.Config.Runners = runners }
}

// rolledOutObjects is the kind and name of each object the cluster repository holds for Argo CD to apply.
func rolledOutObjects(t *testing.T, e *robotclustertest.Env) []string {
	t.Helper()
	var objects []string
	for file, content := range e.GitHub.RepoFiles(e.Repo()) {
		if !strings.HasSuffix(file, ".yaml") {
			continue
		}
		for _, doc := range testutil.Documents(t, []byte(content)) {
			objects = append(objects, fmt.Sprintf("%v %v", doc["kind"], testutil.Field(t, doc, "metadata", "name")))
		}
	}
	require.NotEmpty(t, objects, "the objects the cluster repository holds")
	return objects
}

func TestConfig_RunnersPutsTheRunnerServicesInTheReleaseTheClusterRollsOutAndAConfigWithoutRunnersPutsNone(t *testing.T) {
	runnerObjects := []string{"AutoscalingRunnerSet mitstack-ci", "CronJob mitstack-runner-lease", "Secret mitstack-runner-token"}
	for _, tc := range []struct {
		name    string
		runners func(contract.Desired) (*components.RunnerInputs, error)
		carried bool
	}{
		{"a Config with Runners rolls out the scale set and its lease job", sharedPool, true},
		{"a Config without Runners rolls out no runner object", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := robotclustertest.New(t, cloudOnlySub, cloudOnly, withRunners(tc.runners))

			obs, err := e.Settle()
			require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
			e.EnsureOrWait("the subscribe that carries the release")

			rolledOut := rolledOutObjects(t, e)
			for _, object := range runnerObjects {
				assert.Equal(t, tc.carried, slices.Contains(rolledOut, object), "whether the cluster repository holds %s", object)
			}
		})
	}
}

var errRunnersUnnamed = handler.Refuse("runner-organization-unset", "the request names no GitHub organisation for the runners to register in")

// settledThenRunnersUnnamed is a cluster settled with the shared pool whose Config's Runners then refuses every request.
func settledThenRunnersUnnamed(t *testing.T) *robotclustertest.Env {
	t.Helper()
	unnamed := false
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly, withRunners(func(d contract.Desired) (*components.RunnerInputs, error) {
		if unnamed {
			return nil, errRunnersUnnamed
		}
		return sharedPool(d)
	}))
	obs, err := e.Settle()
	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	e.EnsureOrWait("the subscribe that carries the release")
	unnamed = true
	return e
}

func TestConfig_RunnersThatFailsRefusesTheUpgradeBeforeAnyChangeAndTheObserveReadsUnknownNamingWhy(t *testing.T) {
	e := settledThenRunnersUnnamed(t)
	writes := e.Cloud.Writes()

	err := e.Ensure()
	obs := e.Observe()

	assert.ErrorIs(t, err, errRunnersUnnamed, "the upgrade answers the Config's refusal")
	assert.Equal(t, writes, e.Cloud.Writes(), "a refused upgrade changes nothing in Hetzner Cloud")
	assert.Equal(t, contract.Unknown, obs.State, "the observe of a cluster whose runners cannot be named: %s", obs.StatusDetail)
	assert.Contains(t, obs.StatusDetail, "names no GitHub organisation for the runners to register in")
}

func TestDelete_ACloudOnlyClusterWhoseRunnersCannotBeNamedIsDeletedAndReadsAbsent(t *testing.T) {
	e := settledThenRunnersUnnamed(t)

	err := e.Delete()

	require.NoError(t, err, "the unsubscribe, which needs no runner to delete the cluster")
	assert.Zero(t, e.Cloud.Count(), "the Cloud resources left after the unsubscribe")
	obs := e.Observe()
	assert.Equal(t, contract.Absent, obs.State, "the observe after the unsubscribe: %s", obs.StatusDetail)
}
