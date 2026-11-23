package robotcluster_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
)

const runnersSubscription = "sub_runners0001"

var sweptAt = time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)

// sweepReport is a node report received at, carrying runners as its runners member, or none when runners is nil.
func sweepReport(t *testing.T, at time.Time, runners map[string]any) observe.NodeReport {
	t.Helper()
	body := map[string]any{"subscriptionId": runnersSubscription, "nodes": []any{}}
	if runners != nil {
		body["runners"] = runners
	}
	data, err := json.Marshal(body)
	require.NoError(t, err)
	return observe.NodeReport{ReceivedAt: at, Body: data}
}

// sweepingCluster is a cluster whose node reports are reports, or the read's err.
func sweepingCluster(reports []observe.NodeReport, err error) *robotcluster.Cluster {
	return robotcluster.New(robotcluster.Config{Block: testBlock, Boundary: "test-boundary", InstallWorker: installNothing,
		NodeReports: func(context.Context, contract.Desired, string) ([]observe.NodeReport, error) { return reports, err }})
}

// failedReport is the unsigned report the node report posts when one of its reads fails, which names the read and
// carries no runners member.
func failedReport(t *testing.T, at time.Time) observe.NodeReport {
	t.Helper()
	data, err := json.Marshal(map[string]any{"subscriptionId": runnersSubscription, "reportedAt": at.Format(time.RFC3339),
		"error": map[string]any{"read": "kubectl get nodes", "exitStatus": 3, "stderr": "error: the server is gone"}})
	require.NoError(t, err)
	return observe.NodeReport{ReceivedAt: at, Body: data}
}

func refused(name string, id, status int, message string) map[string]any {
	return map[string]any{"name": name, "id": id, "status": status, "message": message}
}

func TestCluster_RunnersProblemNamesEachRunnerTheNewestReportsSweepCouldNotRemoveOrWhyItReadNone(t *testing.T) {
	cleanSweep := map[string]any{"scaleSet": "mitstack-ci", "listed": 3, "removed": []any{"mitstack-ci-7xk2p-runner-q8w4z"}, "unremoved": []any{},
		"deferred": 0, "unlisted": 0}
	refusedSweep := map[string]any{"scaleSet": "mitstack-ci", "listed": 2, "removed": []any{}, "deferred": 0, "unlisted": 0, "unremoved": []any{
		refused("mitstack-ci-7xk2p-runner-q8w4z", 21, 403, "Resource not accessible by integration"),
		refused("mitstack-ci-7xk2p-runner-h5j6k", 22, 500, "Server Error")}}
	cases := map[string]struct {
		reports []observe.NodeReport
		err     error
		named   []string
	}{
		"a removal GitHub refuses reads unhealthy naming each runner with GitHub's status and message": {
			[]observe.NodeReport{sweepReport(t, sweptAt, refusedSweep)},
			nil, []string{"mitstack-ci-7xk2p-runner-q8w4z", "403", "Resource not accessible by integration", "mitstack-ci-7xk2p-runner-h5j6k", "500"}},
		"a sweep that removed every offline runner reads no problem": {
			[]observe.NodeReport{sweepReport(t, sweptAt, cleanSweep)}, nil, nil},
		"a later sweep that removed the runner GitHub refused before reads no problem": {
			[]observe.NodeReport{sweepReport(t, sweptAt, refusedSweep), sweepReport(t, sweptAt.Add(5*time.Minute), cleanSweep)}, nil, nil},
		"a later report whose other read failed carries no sweep, so the refusal before it still reads unhealthy": {
			[]observe.NodeReport{sweepReport(t, sweptAt, refusedSweep), failedReport(t, sweptAt.Add(5*time.Minute))},
			nil, []string{"mitstack-ci-7xk2p-runner-q8w4z", "403"}},
		"a cluster whose only reports name a failed read reads no problem, since no sweep has reported": {
			[]observe.NodeReport{failedReport(t, sweptAt)}, nil, nil},
		"a sweep that could not read GitHub's runners or the runner pods reads unhealthy naming why": {
			[]observe.NodeReport{sweepReport(t, sweptAt, map[string]any{"scaleSet": "mitstack-ci", "unread": "GitHub answered 401 to the runner list: Bad credentials"})},
			nil, []string{"GitHub answered 401 to the runner list: Bad credentials"}},
		"a sweep skipped before the first runner token is leased reads no problem": {
			[]observe.NodeReport{sweepReport(t, sweptAt, map[string]any{"scaleSet": "mitstack-ci", "skipped": "no runner token is leased yet"})}, nil, nil},
		"a report from a kit that sweeps nothing reads no problem": {
			[]observe.NodeReport{sweepReport(t, sweptAt, nil)}, nil, nil},
		"a cluster that has sent no report yet reads no problem": {nil, nil, nil},
		"node reports that cannot be read read unhealthy naming the read": {
			nil, errors.New("the platform answered 503 for the node reports"), []string{"the platform answered 503 for the node reports"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cluster := sweepingCluster(tc.reports, tc.err)

			problem := cluster.RunnersProblem(context.Background(), contract.Desired{Identity: contract.Identity{SubscriptionID: runnersSubscription}})

			if tc.named == nil {
				assert.Empty(t, problem)
				return
			}
			for _, named := range tc.named {
				assert.Contains(t, problem, named)
			}
		})
	}
}

func TestCluster_RunnersProblemNamesANewestReportThatIsNotTheNodeReportsJSON(t *testing.T) {
	cluster := sweepingCluster([]observe.NodeReport{{ReceivedAt: sweptAt, Body: json.RawMessage(`{"runners": "not an object"}`)}}, nil)

	problem := cluster.RunnersProblem(context.Background(), contract.Desired{Identity: contract.Identity{SubscriptionID: runnersSubscription}})

	assert.Contains(t, problem, "runners")
}
