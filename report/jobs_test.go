package report_test

// A CronJob's newest finished run gives the message its failed pod left in the report, and none once it succeeded.

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/report"
)

// run is a pod of the labeller's run scheduled at minute, with the message its container left.
func run(minute, phase, message string) string {
	return `{"namespace": "mitstack-node", "name": "mitstack-node-topology-` + minute + `-x7k2q", "phase": "` + phase +
		`", "containers": [{"name": "label", "message": "` + message + `"}]}`
}

func TestRead_ScheduledFailureIsTheMessageOfTheCronJobsNewestFinishedRunWhenThatRunFailed(t *testing.T) {
	held := "label-nodes: pod argocd/argocd-server-0 stays on burst node fsn1-3 until a later run evicts it"
	cases := []struct {
		name string
		pods []string
		want string
	}{
		{"a failed run after a success", []string{run("29400000", "Succeeded", ""), run("29400005", "Failed", held)}, held},
		{"a success after a failed run", []string{run("29400005", "Failed", held), run("29400010", "Succeeded", "")}, ""},
		{"a failed run with a newer one still running", []string{run("29400005", "Failed", held), run("29400010", "Running", "")}, held},
		{"no finished run", []string{run("29400010", "Pending", "")}, ""},
		{"the run of a CronJob whose name the labeller's begins", []string{
			`{"namespace": "mitstack-node", "name": "mitstack-node-topology-x-29400005-x7k2q", "phase": "Failed", "containers": [{"message": "other"}]}`}, ""},
		{"a pod of that name in another namespace", []string{
			`{"namespace": "default", "name": "mitstack-node-topology-29400005-x7k2q", "phase": "Failed", "containers": [{"message": "other"}]}`}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"pods": [` + strings.Join(tc.pods, ", ") + `]}`
			read := report.Read{Body: []byte(body), At: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}

			failure, err := read.ScheduledFailure("mitstack-node", "mitstack-node-topology")

			require.NoError(t, err)
			assert.Equal(t, tc.want, failure)
		})
	}
}

func TestRead_ScheduledFailureFailsOnAReportItCannotRead(t *testing.T) {
	_, err := report.Read{Body: []byte(`{"pods": {}}`)}.ScheduledFailure("mitstack-node", "mitstack-node-topology")

	require.Error(t, err)
}
