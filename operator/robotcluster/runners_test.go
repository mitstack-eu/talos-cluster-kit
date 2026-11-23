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

	"github.com/mitstack-eu/talos-cluster-kit/components"
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

// sweepingCluster is a cluster running the runner services runners names, or failing to read them with runnersErr,
// read ten minutes after sweptAt, whose node reports are reports, or the read's err.
func sweepingCluster(runners *components.RunnerInputs, runnersErr error, reports []observe.NodeReport, err error) *robotcluster.Cluster {
	return robotcluster.New(robotcluster.Config{Block: testBlock, Boundary: "test-boundary", InstallWorker: installNothing,
		Runners:     func(contract.Desired) (*components.RunnerInputs, error) { return runners, runnersErr },
		Now:         func() time.Time { return sweptAt.Add(10 * time.Minute) },
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
		"busy": []any{}, "deferred": 0, "unlisted": 0}
	refusedSweep := map[string]any{"scaleSet": "mitstack-ci", "listed": 2, "removed": []any{}, "busy": []any{}, "deferred": 0, "unlisted": 0, "unremoved": []any{
		refused("mitstack-ci-7xk2p-runner-q8w4z", 21, 403, "Resource not accessible by integration"),
		refused("mitstack-ci-7xk2p-runner-h5j6k", 22, 500, "Server Error")}}
	busySweep := map[string]any{"scaleSet": "mitstack-ci", "listed": 1, "removed": []any{}, "unremoved": []any{},
		"busy": []any{"mitstack-ci-7xk2p-runner-q8w4z"}, "deferred": 1, "unlisted": 0}
	otherSweep := map[string]any{"scaleSet": "mitstack-ci", "listed": 3, "removed": []any{"mitstack-ci-7xk2p-runner-h5j6k"}, "unremoved": []any{},
		"busy": []any{}, "deferred": 0, "unlisted": 0}
	emptySweep := map[string]any{"scaleSet": "mitstack-ci", "listed": 2, "removed": []any{}, "unremoved": []any{}, "busy": []any{}, "deferred": 0, "unlisted": 0}
	cases := map[string]struct {
		runsNone   bool
		runnersErr error
		reports    []observe.NodeReport
		err        error
		named      []string
	}{
		"a removal GitHub refuses reads unhealthy naming each runner with GitHub's status and message": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt, refusedSweep)},
			named:   []string{"mitstack-ci-7xk2p-runner-q8w4z", "403", "Resource not accessible by integration", "mitstack-ci-7xk2p-runner-h5j6k", "500"}},
		"a sweep that removed every offline runner reads no problem": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt, cleanSweep)}},
		"a runner the newest sweep and an older sweep both removed reads unhealthy naming it, since GitHub listed it again after answering its removal as done": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt.Add(-5*time.Minute), cleanSweep), sweepReport(t, sweptAt, cleanSweep)},
			named:   []string{"mitstack-ci-7xk2p-runner-q8w4z", "mitstack-ci", "removal"}},
		"a runner only the newest sweep removed reads no problem, though an older sweep removed another": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt.Add(-5*time.Minute), otherSweep), sweepReport(t, sweptAt, cleanSweep)}},
		"a runner only older sweeps removed reads no problem, since the newest sweep found none to remove": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt.Add(-10*time.Minute), cleanSweep), sweepReport(t, sweptAt.Add(-5*time.Minute), cleanSweep),
				sweepReport(t, sweptAt, emptySweep)}},
		"a later sweep that removed the runner GitHub refused before reads no problem": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt, refusedSweep), sweepReport(t, sweptAt.Add(5*time.Minute), cleanSweep)}},
		"a later report whose other read failed carries no sweep, so the refusal before it still reads unhealthy": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt, refusedSweep), failedReport(t, sweptAt.Add(5*time.Minute))},
			named:   []string{"mitstack-ci-7xk2p-runner-q8w4z", "403"}},
		"a cluster whose only reports name a failed read reads unhealthy, since no sweep has reported": {
			reports: []observe.NodeReport{failedReport(t, sweptAt)}, named: []string{"no node report", "runner sweep"}},
		"a sweep that could not read GitHub's runners or the runner pods reads unhealthy naming why": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt, map[string]any{"scaleSet": "mitstack-ci", "unread": "GitHub answered 401 to the runner list: Bad credentials"})},
			named:   []string{"GitHub answered 401 to the runner list: Bad credentials"}},
		"a sweep that read three pages while GitHub lists more runners reads unhealthy naming how many it left unread": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt, map[string]any{"scaleSet": "mitstack-ci", "listed": 0, "removed": []any{},
				"unremoved": []any{}, "busy": []any{}, "deferred": 0, "unlisted": 5})},
			named: []string{"5 more runners", "mitstack-ci"}},
		"a sweep with a refusal and runners past its pages names both": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt, map[string]any{"scaleSet": "mitstack-ci", "listed": 2, "removed": []any{}, "busy": []any{},
				"deferred": 0, "unlisted": 7, "unremoved": []any{refused("mitstack-ci-7xk2p-runner-q8w4z", 21, 403, "Resource not accessible by integration")}})},
			named: []string{"mitstack-ci-7xk2p-runner-q8w4z", "403", "7 more runners"}},
		"a sweep skipped before the first runner token is leased reads unhealthy naming why, since no runner registers before it either": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt, map[string]any{"scaleSet": "mitstack-ci", "skipped": "no runner token is leased yet"})},
			named:   []string{"no runner token is leased yet"}},
		"a sweep of another scale set than the one the cluster runs reads unhealthy naming both": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt, map[string]any{"scaleSet": "old-ci", "listed": 0, "removed": []any{}, "unremoved": []any{},
				"busy": []any{}, "deferred": 0, "unlisted": 0})},
			named: []string{`"old-ci"`, `"mitstack-ci"`}},
		"a sweep that names neither how many runners it listed nor why it listed none reads unhealthy": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt, map[string]any{"scaleSet": "mitstack-ci"})}, named: []string{"how many runners it listed"}},
		"a report from a kit that sweeps nothing reads unhealthy, since the cluster runs a scale set": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt, nil)}, named: []string{"carries no runner sweep"}},
		"a cluster that has sent no report yet reads unhealthy": {named: []string{"no node report"}},
		"a newest full report older than three report intervals reads unhealthy naming when it was received": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt.Add(-10*time.Minute), cleanSweep), failedReport(t, sweptAt.Add(5*time.Minute))},
			named:   []string{"2026-11-23T08:50:00Z"}},
		"a runner the sweeps half an hour apart both read busy with no pod reads unhealthy naming it": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt.Add(-30*time.Minute), busySweep), sweepReport(t, sweptAt, busySweep)},
			named:   []string{"mitstack-ci-7xk2p-runner-q8w4z", "busy", "30 minutes"}},
		"a runner read busy with no pod for less than half an hour reads no problem while GitHub may still end its job": {
			reports: []observe.NodeReport{sweepReport(t, sweptAt.Add(-25*time.Minute), busySweep), sweepReport(t, sweptAt, busySweep)}},
		"node reports that cannot be read read unhealthy naming the read": {
			err: errors.New("the platform answered 503 for the node reports"), named: []string{"the platform answered 503 for the node reports"}},
		"a cluster that runs no scale set reads no problem, having nothing to sweep": {runsNone: true},
		"runner services that cannot be read read unhealthy naming why": {
			runnersErr: errors.New("the request names no runner group"), named: []string{"the request names no runner group"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runners := &components.RunnerInputs{Organization: "acme-org", Group: "acme-ci-runners", ScaleSet: "mitstack-ci"}
			if tc.runsNone {
				runners = nil
			}
			cluster := sweepingCluster(runners, tc.runnersErr, tc.reports, tc.err)

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
	cluster := sweepingCluster(&components.RunnerInputs{ScaleSet: "mitstack-ci"}, nil,
		[]observe.NodeReport{{ReceivedAt: sweptAt, Body: json.RawMessage(`{"runners": "not an object"}`)}}, nil)

	problem := cluster.RunnersProblem(context.Background(), contract.Desired{Identity: contract.Identity{SubscriptionID: runnersSubscription}})

	assert.Contains(t, problem, "runners")
}
