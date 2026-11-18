package robotcluster_test

// A request naming no dedicated server runs on Hetzner Cloud alone, with no Robot call, on an operator that makes the
// server optional, and is refused on one that requires it.

import (
	"context"
	"fmt"
	"testing"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cloudOnlySub = "sub_0c10d0a1b2c3"

func cloudOnly(e *env) {
	e.cfg.DedicatedServerOptional, e.cfg.DeclaredSize = true, 1
	e.params["controlPlaneSize"] = "1"
	e.params["cloudPoolServerTypes"], e.params["cloudPoolLocations"] = "cpx32", "fsn1"
	e.params["cloudPoolMin"], e.params["cloudPoolMax"], e.params["cloudPoolIdleMinutes"] = "1", "4", "15"
}

func noRobotLogin(e *env) { e.noRobotLogin = true }

func refusedAs(t *testing.T, err error, class string) *handler.Refused {
	t.Helper()
	var refused *handler.Refused
	require.ErrorAs(t, err, &refused, "the call's answer")
	assert.Equal(t, class, refused.Class, "the refusal's class: %s", refused.Detail)
	return refused
}

func TestCloudOnlyCluster_BuildsTheWorkerImageAndOnePoolWorkerTheIngressTargetsWithNoRobotCall(t *testing.T) {
	steps := captureSteps(t)
	e := newEnv(t, cloudOnlySub, cloudOnly)

	obs, err := e.settle()

	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	assert.Equal(t, contract.Present, obs.State, obs.StatusDetail)
	assert.Contains(t, obs.StatusDetail, "its ingress load balancer the Cloud pool's workers", "the ingress load balancer's targets")
	assert.NotContains(t, obs.StatusDetail, "10.0.1.2", "the ingress load balancer targets no Robot worker")
	assert.Len(t, e.byRole(controlplane.RoleNode), 1, "the control plane")
	assert.Len(t, e.byRole(controlplane.RoleCloudWorker), 1, "the pool's workers at its minimum")
	assert.Contains(t, steps.steps(), "power on the worker builder (took)", "the worker image is built at install")
	assert.Empty(t, e.robot.Requests(), "the Robot fake's calls")
	assert.Zero(t, e.installed(), "the dedicated server's installs")
	for _, c := range e.created {
		assert.NotContains(t, []string{robotcluster.ResourceDedicatedServer, robotcluster.ResourceVSwitch, robotcluster.ResourceRobotFirewall},
			c.Name, "a resource the cluster reports")
	}
}

func TestCloudOnlyCluster_ReadsItsRobotResourcesAbsentWithNoRobotCall(t *testing.T) {
	e := newEnv(t, cloudOnlySub, cloudOnly)
	_, err := e.settle()
	require.NoError(t, err)

	for _, name := range []string{robotcluster.ResourceDedicatedServer, robotcluster.ResourceVSwitch, robotcluster.ResourceRobotFirewall} {
		obs, err := e.k.Observers()[name].ObserveResource(context.Background(), contract.ResourceRef{Desired: e.desired()})
		require.NoError(t, err, name)
		assert.Equal(t, contract.Absent, obs.State, "%s: %s", name, obs.StatusDetail)
	}
	assert.Empty(t, e.robot.Requests(), "the Robot fake's calls")
}

func TestCloudOnlyCluster_BuildsAndIsRemovedOnAConnectionWithNoRobotLogin(t *testing.T) {
	e := newEnv(t, cloudOnlySub, cloudOnly, noRobotLogin)

	obs, err := e.settle()
	require.NoError(t, err, "the subscribe, which must not refuse missing-robot-access: %s", obs.StatusDetail)
	assert.Equal(t, contract.Present, obs.State, obs.StatusDetail)

	require.NoError(t, e.delete(), "the unsubscribe, which must not refuse robot-left-in-place")
	assert.Zero(t, e.cloud.Count(), "the Cloud resources left after the unsubscribe")
	assert.Empty(t, e.robot.Requests(), "the Robot fake's calls")
}

func TestCloudOnlyCluster_RemovalDeletesEveryCloudResourceAndTheRepositoryWithNoRobotCall(t *testing.T) {
	e := newEnv(t, cloudOnlySub, cloudOnly)
	_, err := e.settle()
	require.NoError(t, err)

	require.NoError(t, e.delete(), "the unsubscribe")

	assert.Zero(t, e.cloud.Count(), "the Cloud resources left after the unsubscribe")
	assert.False(t, e.github.exists(e.repo()), "the cluster repository after the unsubscribe")
	assert.Equal(t, contract.Absent, e.observe().State, "the cluster after the unsubscribe")
	assert.Empty(t, e.robot.Requests(), "the Robot fake's calls")
	assert.NotContains(t, e.eventsSoFar(), "releasing the dedicated server and cancelling its vSwitch", "the unsubscribe's progress")
}

func TestCloudOnlyCluster_CreatesAPoolWorkerBackUpToTheMinimumOnceOneIsDeletedOutsideMitstack(t *testing.T) {
	e := newEnv(t, cloudOnlySub, cloudOnly)
	_, err := e.settle()
	require.NoError(t, err)
	gone := e.byRole(controlplane.RoleCloudWorker)
	require.Len(t, gone, 1)

	require.NoError(t, e.cloud.DeleteServer(context.Background(), gone[0].ID), "the delete outside mitstack")
	obs, err := e.settle()

	require.NoError(t, err, "the subscribe after the delete: %s", obs.StatusDetail)
	back := e.byRole(controlplane.RoleCloudWorker)
	require.Len(t, back, 1, "the pool's workers back at its minimum")
	assert.NotEqual(t, gone[0].ID, back[0].ID, "the worker is a new server")
	assert.Empty(t, e.robot.Requests(), "the Robot fake's calls")
}

func TestCloudOnlyCluster_FailsNamingTheWorkerImageBuildAndIsRemovedWithEveryServerItMade(t *testing.T) {
	e := newEnv(t, cloudOnlySub, cloudOnly)
	e.workerBuildHangs = true

	_, err := e.settle()

	refused := refusedAs(t, err, "worker-image-build-failed")
	assert.Contains(t, refused.Detail, "worker", "the refusal names the failed step")
	require.NoError(t, e.delete(), "the unsubscribe")
	assert.Zero(t, e.cloud.Count(), "the Cloud resources left after the unsubscribe")
	assert.Empty(t, e.robot.Requests(), "the Robot fake's calls")
}

func TestCloudOnlyRequest_WithNoSteadyPoolWorkerIsRefusedNamingCloudPoolMinBeforeAnyChange(t *testing.T) {
	for name, params := range map[string]map[string]string{
		"a pool of 0 to 4": {"cloudPoolMin": "0"},
		"no pool":          {"cloudPoolServerTypes": "", "cloudPoolLocations": "", "cloudPoolMin": "", "cloudPoolMax": "", "cloudPoolIdleMinutes": ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, cloudOnlySub, cloudOnly, withParams(params))

			ensureErr := e.k.Ensure(context.Background(), e.desired())
			_, planErr := e.k.PlanReplacements(context.Background(), e.desired())

			for call, err := range map[string]error{"the subscribe": ensureErr, "the dry run": planErr} {
				refused := refusedAs(t, err, "invalid-param")
				assert.Contains(t, refused.Detail, `params["cloudPoolMin"]`, "%s names the input", call)
				assert.Contains(t, refused.Detail, "at least 1", "%s names the minimum", call)
			}
			assert.Zero(t, e.cloud.Writes(), "a refused subscribe changes nothing in Hetzner Cloud")
			assert.Empty(t, e.robot.Requests(), "the Robot fake's calls")
		})
	}
}

func TestDedicatedServerRequired_RefusesARequestNamingNoServerWithTodaysMessage(t *testing.T) {
	e := newEnv(t, cloudOnlySub, cloudOnly, func(e *env) { e.cfg.DedicatedServerOptional = false })

	err := e.k.Ensure(context.Background(), e.desired())

	refused := refusedAs(t, err, "missing-param")
	assert.Equal(t, `the dedicated server's number is required: params["robotServerNumber"] is empty, and the cluster's worker runs on that Robot server`,
		refused.Detail)
	assert.Zero(t, e.cloud.Writes(), "a refused subscribe changes nothing in Hetzner Cloud")
}

func TestDedicatedServerCluster_OnAnOperatorWithTheServerOptionalTakesTheSameGolden(t *testing.T) {
	assertDedicatedServerGolden(t, func(e *env) { e.cfg.DedicatedServerOptional = true })
}

func TestCloudOnlyRequest_OnAClusterBuiltOnADedicatedServerIsRefusedAndLeavesTheServerInPlace(t *testing.T) {
	e := newEnv(t, productionSub, withParams(map[string]string{
		robotcluster.ParamRobotServer: fmt.Sprint(productionServer), "serverType": "cx23,cpx22,cpx32",
		"cloudPoolServerTypes": "cpx32", "cloudPoolLocations": "fsn1", "cloudPoolMin": "1", "cloudPoolMax": "1",
	}), func(e *env) { e.cfg.DedicatedServerOptional = true })
	e.robot.AddServer(robottest.Server{Number: productionServer, IP: "203.0.113.50", DC: "FSN1-DC14"})
	_, err := e.settle()
	require.NoError(t, err)
	e.params[robotcluster.ParamRobotServer] = ""
	requests := len(e.robot.Requests())

	refused := refusedAs(t, e.k.Ensure(context.Background(), e.desired()), "dedicated-server-removed")
	assert.Contains(t, refused.Detail, `params["robotServerNumber"]`, "the refusal names the input")
	assert.Equal(t, contract.Unknown, e.observe().State, "the cluster")
	for _, name := range []string{robotcluster.ResourceDedicatedServer, robotcluster.ResourceVSwitch, robotcluster.ResourceRobotFirewall} {
		obs, err := e.k.Observers()[name].ObserveResource(context.Background(), contract.ResourceRef{Desired: e.desired()})
		require.NoError(t, err, name)
		assert.Equal(t, contract.Unknown, obs.State, "%s: %s", name, obs.StatusDetail)
	}
	refusedAs(t, e.delete(), "robot-left-in-place")
	assert.Zero(t, e.cloud.Count(), "the Cloud resources left after the unsubscribe")
	assert.Len(t, e.robot.Requests(), requests, "the Robot fake's calls after the number was dropped")
}
