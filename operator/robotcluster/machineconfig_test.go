package robotcluster_test

// An Update gives every node the release's machine configuration through one rollout Job in the cluster repository,
// judged by a node report made since the cluster's newest server and naming every node the cluster runs.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/noderollout"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster/robotclustertest"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rolloutSub     = "sub_0e1f2a3b4c5d"
	earlierRelease = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// standingCluster is a dedicated-server cluster that reads present with main holding the release's component set.
func standingCluster(t *testing.T, opts ...robotclustertest.Option) *robotclustertest.Env {
	t.Helper()
	e := robotclustertest.New(t, rolloutSub, append([]robotclustertest.Option{robotclustertest.WithParams(map[string]string{
		robotcluster.ParamRobotServer: fmt.Sprint(productionServer), "serverType": "cx23,cpx22,cpx32",
	})}, opts...)...)
	e.Robot.AddServer(robottest.Server{Number: productionServer, IP: "203.0.113.50", DC: "FSN1-DC14"})
	obs, err := e.Settle()
	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	return e
}

// nodeNames is every node the cluster runs, the Robot worker included.
func nodeNames(e *robotclustertest.Env) []string {
	var names []string
	for _, s := range append(e.ByRole(controlplane.RoleNode), e.ByRole(controlplane.RoleCloudWorker)...) {
		names = append(names, s.Name)
	}
	return append(names, e.Plane().Name("worker"))
}

func holds(e *robotclustertest.Env, path string) bool {
	_, held := e.GitHub.RepoFiles(e.Repo())[path]
	return held
}

func plant(e *robotclustertest.Env, path string) {
	e.GitHub.Plant(e.Repo(), path, "kind: Job\n")
}

func TestUpdate_GivesEveryNodeOnAnEarlierMachineConfigurationTheReleasesThroughOneRolloutJobAndTakesItOutOnceEachRunsIt(t *testing.T) {
	e := standingCluster(t)
	release := e.Release()
	e.Earlier = map[string]string{}
	for _, name := range nodeNames(e) {
		e.Earlier[name] = earlierRelease
	}
	plant(e, noderollout.Path(earlierRelease))
	e.Now = e.Now.Add(time.Minute)
	e.ReportNodes()

	behind := e.Observe()
	require.Equal(t, contract.Unknown, behind.State, "the observe while every node runs an earlier configuration")
	for _, name := range nodeNames(e) {
		assert.Contains(t, behind.StatusDetail, name, "the observe names each node behind")
	}
	assert.True(t, behind.Redrive, "the observe asks for the subscribe that writes the rollout")

	e.EnsureOrWait("the subscribe that writes the rollout")
	job := e.GitHub.RepoFiles(e.Repo())[noderollout.Path(release)]
	assert.Contains(t, job, "name: "+noderollout.JobName(release), "main holds the release's rollout Job")
	assert.False(t, holds(e, noderollout.Path(earlierRelease)), "the earlier release's rollout is taken out")
	running := e.Observe()
	assert.Equal(t, contract.Unknown, running.State, "the observe while the rollout Job runs")
	assert.False(t, running.Redrive, "no subscribe is asked for while the Job runs")

	e.Now = e.Now.Add(time.Minute)
	e.Progress()
	ran := e.Observe()
	assert.Equal(t, contract.Unknown, ran.State, "the observe once every node runs the release's configuration and main still holds the rollout")
	assert.True(t, ran.Redrive, "the observe asks for the subscribe that takes the rollout out")
	e.EnsureOrWait("the subscribe that takes the rollout out")
	assert.False(t, holds(e, noderollout.Path(release)), "the rollout is taken out once every node runs the release's configuration")

	obs, err := e.Settle()
	require.NoError(t, err)
	assert.Contains(t, obs.StatusDetail, "every node runs the release's machine configuration")
}

func TestUpdate_RunsTheRolloutAgainForAPoolWorkerThatJoinsAfterItRanAndReadsPresentOnceTheWorkerRunsTheRelease(t *testing.T) {
	e := standingCluster(t, robotclustertest.WithParams(map[string]string{
		"cloudPoolServerTypes": "cpx32", "cloudPoolLocations": "fsn1", "cloudPoolMin": "1", "cloudPoolMax": "1",
	}))
	e.Earlier = map[string]string{}
	for _, name := range nodeNames(e) {
		e.Earlier[name] = earlierRelease
	}
	e.Now = e.Now.Add(time.Minute)
	e.ReportNodes()
	e.EnsureOrWait("the subscribe that writes the rollout")
	e.Now = e.Now.Add(time.Minute)
	e.Progress()
	require.True(t, holds(e, noderollout.Path(e.Release())), "main holds the rollout the Job ran from")
	gone := e.ByRole(controlplane.RoleCloudWorker)
	require.Len(t, gone, 1)

	e.Boots = earlierRelease
	require.NoError(t, e.Cloud.DeleteServer(context.Background(), gone[0].ID), "the delete outside mitstack")
	obs, err := e.Settle()

	require.NoError(t, err, "the subscribe after the delete: %s", obs.StatusDetail)
	back := e.ByRole(controlplane.RoleCloudWorker)
	require.Len(t, back, 1, "the pool's workers back at its minimum")
	assert.NotEqual(t, gone[0].ID, back[0].ID, "the worker is a new server")
	assert.Contains(t, obs.StatusDetail, "every node runs the release's machine configuration")
	assert.False(t, holds(e, noderollout.Path(e.Release())), "the rollout is taken out once the new worker runs the release's configuration")
}

// rolledOut is a standing cluster whose rollout Job ran, with missed, a control-plane node, read behind the release.
func rolledOut(t *testing.T) (e *robotclustertest.Env, missed string) {
	t.Helper()
	e = standingCluster(t)
	e.Earlier = map[string]string{}
	for _, name := range nodeNames(e) {
		e.Earlier[name] = earlierRelease
	}
	e.Now = e.Now.Add(time.Minute)
	e.ReportNodes()
	e.EnsureOrWait("the subscribe that writes the rollout")
	e.Now = e.Now.Add(time.Minute)
	e.Progress()
	require.True(t, holds(e, noderollout.Path(e.Release())), "main holds the rollout the Job ran from")
	missed = e.ByRole(controlplane.RoleNode)[0].Name
	e.Earlier = map[string]string{missed: earlierRelease}
	return e, missed
}

func TestUpdate_TakesOutARolloutWhoseJobCompletedWithANodeBehindAndCommitsItAgainOnlyOnceArgoCDPrunesTheJob(t *testing.T) {
	e, missed := rolledOut(t)
	path := noderollout.Path(e.Release())
	e.Now = e.Now.Add(time.Minute)
	e.ReportNodes()

	completed := e.Observe()
	assert.Contains(t, completed.StatusDetail, "reads "+missed+" on a machine configuration other than the release's", "the observe names the node behind")
	assert.Contains(t, completed.StatusDetail, "its Job completed without reaching them, so it is taken out to run again", "the observe says why the rollout is taken out")
	assert.True(t, completed.Redrive, "the observe asks for the subscribe that takes the rollout out")
	e.EnsureOrWait("the subscribe that takes the completed rollout out")
	require.False(t, holds(e, path), "the rollout whose Job completed with a node behind is taken out")

	pruning := e.Observe()
	assert.Contains(t, pruning.StatusDetail, "the rollout is committed again once Argo CD prunes the release's earlier Job", "the observe while Argo CD still holds the Job")
	assert.False(t, pruning.Redrive, "no subscribe is asked for while Argo CD still holds the Job")
	e.EnsureOrWait("the subscribe while Argo CD still holds the Job")
	assert.False(t, holds(e, path), "the rollout stays out while Argo CD still holds the Job")

	obs, err := e.Settle()
	require.NoError(t, err, "the subscribes once Argo CD prunes the Job: %s", obs.StatusDetail)
	assert.Contains(t, obs.StatusDetail, "every node runs the release's machine configuration")
	assert.NotContains(t, e.Earlier, missed, "a new Job gives the node behind the release's configuration")
}

func TestUpdate_KeepsTheRolloutAndNamesTheReportWhileTheNewestReportCarriesNoHealthOfWhatArgoCDApplied(t *testing.T) {
	e, missed := rolledOut(t)
	e.NoJobHealth = true
	e.Now = e.Now.Add(time.Minute)
	e.ReportNodes()

	obs := e.Observe()
	e.EnsureOrWait("the subscribe on a report that carries no Job health")

	assert.Contains(t, obs.StatusDetail, "reads "+missed+" on a machine configuration other than the release's", "the observe names the node behind")
	assert.Contains(t, obs.StatusDetail, "carries no health of what the Application "+controlplane.RemovalApplication+" applied", "the observe names the report that carries no Job health")
	assert.False(t, obs.Redrive, "no subscribe is asked for while the report carries no Job health")
	assert.True(t, holds(e, noderollout.Path(e.Release())), "the rollout stays while the report carries no Job health")
}

func TestPlane_ReadsANodeAReportNamesOnAnEarlierMachineConfigurationBehindTheReleaseSoAReplacementWaitsForIt(t *testing.T) {
	e := standingCluster(t)
	nodes := e.ByRole(controlplane.RoleNode)
	e.Earlier = map[string]string{nodes[0].Name: earlierRelease}
	body, err := json.Marshal(e.Report())
	require.NoError(t, err)

	behind, err := e.Plane().NodesBehindRelease(report.Read{Body: body, At: e.Now})

	require.NoError(t, err)
	assert.Equal(t, []string{nodes[0].Name}, behind, "the nodes a replacement of a control-plane node or Cloud worker waits on before it ends")
}

func TestUpdate_RefusesTheRolloutNamingTheEtcdMemberTheNewestReportReadsUnhealthyAndWritesNoJob(t *testing.T) {
	e := standingCluster(t)
	nodes := e.ByRole(controlplane.RoleNode)
	e.Earlier = map[string]string{nodes[0].Name: earlierRelease}
	e.EtcdDown = nodes[1].Name
	e.Now = e.Now.Add(time.Minute)
	e.ReportNodes()

	obs := e.Observe()
	err := e.Ensure()

	assert.Equal(t, contract.Unknown, obs.State, "the observe while an etcd member is unhealthy")
	assert.Contains(t, obs.StatusDetail, redundant.ClassBelowMinimum, "the observe names the refusal")
	assert.Contains(t, obs.StatusDetail, nodes[1].Name, "the observe names the unhealthy member")
	var refused *handler.Refused
	require.ErrorAs(t, err, &refused, "the subscribe while an etcd member is unhealthy")
	assert.Equal(t, redundant.ClassBelowMinimum, refused.Class)
	assert.Contains(t, refused.Detail, nodes[1].Name, "the refusal names the unhealthy member")
	assert.False(t, holds(e, noderollout.Path(e.Release())), "no rollout Job is written")
}

func TestUpdate_NamesTheSettingARolloutJobRefusesAsANodeTakesItOnlyAtRebootAndChangesNoNode(t *testing.T) {
	e := standingCluster(t)
	e.Earlier = map[string]string{}
	for _, name := range nodeNames(e) {
		e.Earlier[name] = earlierRelease
	}
	refusal := "apply-machine-config: " + nodeNames(e)[0] + " at 10.0.0.3: machine-config: the release changes " +
		".machine.kernel.modules: a node takes this setting only when it reboots, so no node changes"
	e.RolloutRefuses = refusal
	e.Now = e.Now.Add(time.Minute)
	e.ReportNodes()
	e.EnsureOrWait("the subscribe that writes the rollout")
	e.Now = e.Now.Add(time.Minute)
	e.Progress()

	obs := e.Observe()

	assert.Equal(t, contract.Unknown, obs.State, "the observe once the rollout Job refused the release")
	assert.Contains(t, obs.StatusDetail, refusal, "the observe names the setting, as the Job's failed pod left it")
	for _, name := range nodeNames(e) {
		assert.Equal(t, earlierRelease, e.Earlier[name], "%s keeps its configuration after the refusal", name)
	}
	assert.True(t, holds(e, noderollout.Path(e.Release())), "the refused rollout stays on main")
	assert.False(t, obs.Redrive, "no subscribe is asked for, since a retry renders the same refusal")
}

func TestUpdate_ReadsNoNodeOnTheReleaseFromAReportThatOmitsANodeTheClusterRunsAndKeepsTheRollout(t *testing.T) {
	e := standingCluster(t)
	plant(e, noderollout.Path(e.Release()))
	report := e.Report()
	nodes := report["nodes"].([]map[string]any)
	omitted := nodes[0]["name"].(string)
	nodes[0]["name"] = "a-node-the-cluster-does-not-run"
	e.Now = e.Now.Add(time.Minute)
	e.SendReport(report)

	obs := e.Observe()
	e.EnsureOrWait("the subscribe on a report that omits a node")

	assert.Equal(t, contract.Unknown, obs.State, "the observe on a report that omits a node")
	assert.Contains(t, obs.StatusDetail, omitted, "the observe names the node the report omits")
	assert.True(t, holds(e, noderollout.Path(e.Release())), "the rollout stays while the report omits a node")
}

func TestUpdate_ReadsNoNodeOnTheReleaseFromAReportMadeBeforeTheClustersNewestServerAndKeepsTheRollout(t *testing.T) {
	e := standingCluster(t)
	plant(e, noderollout.Path(e.Release()))
	var newest time.Time
	for _, r := range []controlplane.Role{controlplane.RoleGateway, controlplane.RoleNode, controlplane.RoleCloudWorker} {
		for _, s := range e.ByRole(r) {
			if s.Created.After(newest) {
				newest = s.Created
			}
		}
	}
	report := e.Report()
	madeAt := newest.Add(-time.Minute)
	report["madeAt"] = madeAt
	for _, gw := range report["gateways"].([]map[string]any) {
		gw["probedAt"] = e.Now
	}
	e.Now = e.Now.Add(time.Minute)
	e.SendReport(report)

	obs := e.Observe()
	e.EnsureOrWait("the subscribe on a report made before the newest server")

	assert.Equal(t, contract.Unknown, obs.State, "the observe on a report made before the newest server")
	assert.Contains(t, obs.StatusDetail, madeAt.UTC().Format(time.RFC3339), "the observe names when the report was made")
	assert.True(t, holds(e, noderollout.Path(e.Release())), "the rollout stays while the newest report predates the newest server")
	assert.NotContains(t, obs.StatusDetail, "every node runs", "no node is read on the release")
}
