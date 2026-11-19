package robotcluster_test

// An Update gives every node the release's machine configuration through one rollout Job in the cluster repository,
// judged by a node report made since the cluster's newest server and naming every node the cluster runs.

import (
	"context"
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
