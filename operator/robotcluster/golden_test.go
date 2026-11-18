package robotcluster_test

// A cluster on a dedicated server, shaped as production's, takes the same steps and Robot calls and reads the same
// from subscribe to unsubscribe.

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/require"
)

const (
	productionSub    = "sub_0466391a571e"
	productionServer = 3041839
)

func TestDedicatedServerCluster_TakesTheStepsAndRobotCallsItsGoldenRecords(t *testing.T) {
	assertDedicatedServerGolden(t)
}

func assertDedicatedServerGolden(t *testing.T, opts ...envOption) {
	t.Helper()
	steps := captureSteps(t)
	e := newEnv(t, productionSub, append([]envOption{withParams(map[string]string{
		robotcluster.ParamRobotServer: fmt.Sprint(productionServer), "serverType": "cx23,cpx22,cpx32",
	})}, opts...)...)
	e.robot.AddServer(robottest.Server{Number: productionServer, IP: "203.0.113.50", DC: "FSN1-DC14"})

	obs, err := e.settle()
	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	var golden strings.Builder
	section(&golden, "steps to present", steps.steps())
	section(&golden, "created", createdLines(e.created))
	section(&golden, "observed", []string{string(obs.State) + ": " + obs.StatusDetail})
	section(&golden, "robot requests", robotLines(e.robot.Requests()))
	section(&golden, "progress", withoutSerials(e.eventsSoFar()))
	section(&golden, "worker installs", []string{fmt.Sprint(e.installed())})

	requests, events := len(e.robot.Requests()), len(e.eventsSoFar())
	require.NoError(t, e.delete(), "the unsubscribe")
	gone := e.observe()
	section(&golden, "robot requests to absent", robotLines(e.robot.Requests()[requests:]))
	section(&golden, "progress to absent", e.eventsSoFar()[events:])
	section(&golden, "cloud resources after delete", []string{fmt.Sprint(e.cloud.Count())})
	section(&golden, "observed after delete", []string{string(gone.State) + ": " + gone.StatusDetail})
	require.False(t, e.github.exists(e.repo()), "the cluster repository after the unsubscribe")

	testutil.Golden(t, "testdata/dedicated-server-cluster.golden", []byte(golden.String()))
}

func section(b *strings.Builder, title string, lines []string) {
	fmt.Fprintf(b, "## %s\n", title)
	for _, l := range lines {
		fmt.Fprintln(b, l)
	}
	b.WriteString("\n")
}

var serial = regexp.MustCompile(`serial [0-9a-f]+`)

// withoutSerials drops each certificate's serial, which is random.
func withoutSerials(events []string) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = serial.ReplaceAllString(ev, "serial <random>")
	}
	return out
}

func createdLines(created []contract.CreatedResource) []string {
	var out []string
	for _, c := range created {
		var identity []string
		for _, key := range slices.Sorted(maps.Keys(c.Identity)) {
			identity = append(identity, key+"="+c.Identity[key])
		}
		out = append(out, fmt.Sprintf("%s %s %s", c.Name, c.Type, strings.Join(identity, " ")))
	}
	slices.Sort(out)
	return out
}

func robotLines(requests []robottest.Request) []string {
	var out []string
	for _, r := range requests {
		out = append(out, strings.TrimSpace(r.Method+" "+r.Path+" "+r.Form.Encode()))
	}
	return out
}
