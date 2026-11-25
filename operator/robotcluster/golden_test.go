package robotcluster_test

// A cluster on a dedicated server, shaped as production's, takes the same steps and Robot calls and reads the same
// from subscribe to unsubscribe, and a Cloud-only cluster whose operator names no gateway ports builds a gateway with
// the same firewall and list label.

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster/robotclustertest"
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

func assertDedicatedServerGolden(t *testing.T, opts ...robotclustertest.Option) {
	t.Helper()
	steps := robotclustertest.CaptureSteps(t)
	e := robotclustertest.New(t, productionSub, append([]robotclustertest.Option{robotclustertest.WithParams(map[string]string{
		robotcluster.ParamRobotServer: fmt.Sprint(productionServer), "serverType": "cx23,cpx22,cpx32",
	})}, opts...)...)
	e.Robot.AddServer(robottest.Server{Number: productionServer, IP: "203.0.113.50", DC: "FSN1-DC14"})

	obs, err := e.Settle()
	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	var golden strings.Builder
	section(&golden, "steps to present", steps.Steps())
	section(&golden, "created", createdLines(e.Created))
	section(&golden, "observed", []string{string(obs.State) + ": " + obs.StatusDetail})
	section(&golden, "robot requests", robotLines(e.Robot.Requests()))
	section(&golden, "progress", withoutSerials(e.EventsSoFar()))
	section(&golden, "worker installs", []string{fmt.Sprint(e.Installed())})

	requests, events := len(e.Robot.Requests()), len(e.EventsSoFar())
	require.NoError(t, e.Delete(), "the unsubscribe")
	gone := e.Observe()
	section(&golden, "robot requests to absent", robotLines(e.Robot.Requests()[requests:]))
	section(&golden, "progress to absent", e.EventsSoFar()[events:])
	section(&golden, "cloud resources after delete", []string{fmt.Sprint(e.Cloud.Count())})
	section(&golden, "observed after delete", []string{string(gone.State) + ": " + gone.StatusDetail})
	require.False(t, e.GitHub.Exists(e.Repo()), "the cluster repository after the unsubscribe")

	testutil.Golden(t, "testdata/dedicated-server-cluster.golden", []byte(golden.String()))
}

func TestDedicatedServerCluster_WithProductionsSetSizeCloudPoolKeepsItsMemberSteadyAsItsGoldenRecords(t *testing.T) {
	steps := robotclustertest.CaptureSteps(t)
	e := robotclustertest.New(t, productionSub, robotclustertest.WithParams(map[string]string{
		robotcluster.ParamRobotServer: fmt.Sprint(productionServer), "serverType": "cx23,cpx22,cpx32",
		"cloudPoolServerTypes": "cpx32", "cloudPoolLocations": "fsn1", "cloudPoolMin": "1", "cloudPoolMax": "1",
	}))
	e.Robot.AddServer(robottest.Server{Number: productionServer, IP: "203.0.113.50", DC: "FSN1-DC14"})

	obs, err := e.Settle()
	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	e.EnsureOrWait("the subscribe that carries the release")
	present := len(steps.Steps())
	var golden strings.Builder
	section(&golden, "steps to present", steps.Steps())
	var members []string
	for _, w := range e.ByRole(controlplane.RoleCloudWorker) {
		labels := slices.Sorted(maps.Keys(w.Labels))
		for i, k := range labels {
			labels[i] = k + "=" + w.Labels[k]
		}
		members = append(members, w.Name+": "+strings.Join(labels, ","))
	}
	section(&golden, "cloud pool members", members)
	section(&golden, "labeller burst nodes", []string{e.LabellerBurstNodes()})

	for range 3 {
		e.Progress()
		e.EnsureOrWait("a further subscribe")
	}
	section(&golden, "steps of further subscribes", steps.Steps()[present:])
	section(&golden, "observed", []string{string(e.Observe().State) + ": " + e.Observe().StatusDetail})

	testutil.Golden(t, "testdata/production-cloud-pool.golden", []byte(golden.String()))
}

func TestCloudOnlyCluster_WithNoGatewayEgressPortsBuildsTheGatewayFirewallAndListLabelItsGoldenRecords(t *testing.T) {
	e, gw := settledGateway(t)
	cloudConfig := robotclustertest.GatewayCloudConfig(t, e.Cloud.UserData[gw.ID])

	var golden strings.Builder
	section(&golden, "gateway list label", []string{gw.Labels[controlplane.KeyGatewayList]})
	section(&golden, "gateway /etc/nftables.conf", []string{writtenFile(t, cloudConfig, "/etc/nftables.conf")})

	testutil.Golden(t, "testdata/cloud-only-gateway.golden", []byte(golden.String()))
}

func settledGateway(t *testing.T, opts ...robotclustertest.Option) (*robotclustertest.Env, controlplane.Server) {
	t.Helper()
	e := robotclustertest.New(t, cloudOnlySub, append([]robotclustertest.Option{cloudOnly}, opts...)...)
	obs, err := e.Settle()
	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	gateways := e.ByRole(controlplane.RoleGateway)
	require.Len(t, gateways, 1, "the gateways of a cluster in one location")
	return e, gateways[0]
}

func writtenFile(t *testing.T, cloudConfig, path string) string {
	t.Helper()
	for _, written := range testutil.Field(t, testutil.Documents(t, []byte(cloudConfig))[0], "write_files").([]any) {
		if file, _ := written.(map[string]any); file["path"] == path {
			content, ok := file["content"].(string)
			require.True(t, ok, "the cloud-config writes %s as text", path)
			return strings.TrimRight(content, "\n")
		}
	}
	require.FailNow(t, "the cloud-config writes no "+path)
	return ""
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
