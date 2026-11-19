package report_test

// A step that removes a member judges the rest by what a node report read of them.

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/report"
)

func healthyRead(t *testing.T) report.Read {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "golden", "healthy-body.json"))
	require.NoError(t, err)
	return report.Read{Body: body, At: time.Date(2026, 10, 23, 15, 45, 0, 0, time.UTC)}
}

// serving is each member's name with whether its reading serves, and whether it has one at all.
func serving(t *testing.T, read report.Read, names []string, members []report.Member) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range members {
		switch {
		case m.Reading == nil:
			out[m.Name] = "unread"
		case !m.Reading.At.Equal(read.At):
			out[m.Name] = "read at another time"
		case m.Reading.Serving:
			out[m.Name] = "serving"
		default:
			out[m.Name] = "not serving"
		}
	}
	require.Len(t, out, len(names), "the members read: %v", out)
	return out
}

func TestRead_EtcdMembersServeWhenTheirNodesAnswerWithNoError(t *testing.T) {
	read := healthyRead(t)

	members, err := read.EtcdMembers()

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"poc-talos4-cp1": "serving", "poc-talos4-cp2": "serving", "poc-talos4-cp4": "serving"},
		serving(t, read, []string{"poc-talos4-cp1", "poc-talos4-cp2", "poc-talos4-cp4"}, members))
}

func TestRead_NodesServeWhenReadyAndAreUnreadWhenTheReportNamesNone(t *testing.T) {
	read := healthyRead(t)
	names := []string{"poc-talos4-cp1", "poc-talos4-cp3", "shop-cloud-worker-fsn1-1"}

	members, err := read.Nodes(names)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"poc-talos4-cp1": "serving", "poc-talos4-cp3": "not serving", "shop-cloud-worker-fsn1-1": "unread"},
		serving(t, read, names, members))
}

func TestRead_NodesBehindNamesEachNodeWhoseMachineConfigurationIsNotTheRelease(t *testing.T) {
	behind, err := healthyRead(t).NodesBehind("sha256:5f0c5f0c5f0c5f0c5f0c5f0c5f0c5f0c5f0c5f0c5f0c5f0c5f0c5f0c5f0c5f0c")

	require.NoError(t, err)
	assert.Equal(t, []string{"poc-talos4-cp2", "poc-talos4-cp3"}, behind,
		"a node at another release is behind, and so is one whose release the report does not name")
}

func TestRead_NoNodeIsBehindWhenEveryNodeRunsTheRelease(t *testing.T) {
	read := report.Read{Body: []byte(`{"nodes":[{"name":"poc-cp1","machineConfig":"sha256:5f0c"},{"name":"poc-worker1","machineConfig":"sha256:5f0c"}]}`)}

	behind, err := read.NodesBehind("sha256:5f0c")

	require.NoError(t, err)
	assert.Empty(t, behind)
}

func TestRead_NodesBehindRefusesNoReleaseOrAReportItCannotReadOrThatListsNoNode(t *testing.T) {
	_, err := report.Read{Body: []byte(`{"nodes":[{"name":"poc-cp1"}]}`)}.NodesBehind("")
	require.ErrorContains(t, err, "names no release", "a node whose annotation names none would read as at it")

	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	_, err = report.Read{Body: []byte("not json"), At: at}.NodesBehind("sha256:5f0c")
	require.ErrorContains(t, err, "the node report read at 2026-11-19T09:00:00Z cannot be read")

	_, err = report.Read{Body: []byte(`{"nodes":[]}`), At: at}.NodesBehind("sha256:5f0c")
	require.ErrorContains(t, err, "the node report read at 2026-11-19T09:00:00Z lists no node, so which nodes are behind is unknown")
}

func TestGatewayReadAt_IsTheProbesTimeElseTheReportsMadeAtElseWhenThePlatformReceivedIt(t *testing.T) {
	received := time.Date(2026, 11, 8, 9, 5, 0, 0, time.UTC)
	gateway := netip.MustParseAddr("10.0.0.248")
	for _, tc := range []struct {
		name, body string
		want       time.Time
	}{
		{"probed", `{"madeAt":"2026-11-08T09:04:30Z","gateways":[{"address":"10.0.0.248","passed":true,"probedAt":"2026-11-08T09:03:10Z"}]}`, time.Date(2026, 11, 8, 9, 3, 10, 0, time.UTC)},
		{"read by the report itself", `{"madeAt":"2026-11-08T09:04:30Z","gateways":[{"address":"10.0.0.248","passed":true}]}`, time.Date(2026, 11, 8, 9, 4, 30, 0, time.UTC)},
		{"from a report with no madeAt", `{"gateways":[{"address":"10.0.0.248","passed":true}]}`, received},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, report.GatewayReadAt([]byte(tc.body), gateway, received))
		})
	}
}

func TestRead_OpenBaoMembersServeWhenInitializedAndUnsealedAndNameTheirNodes(t *testing.T) {
	read := healthyRead(t)

	members, nodes, err := read.OpenBaoMembers()

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"openbao-0": "serving", "openbao-1": "serving", "openbao-2": "serving"},
		serving(t, read, []string{"openbao-0", "openbao-1", "openbao-2"}, members))
	assert.Equal(t, map[string]string{"openbao-0": "poc-talos4-cp1"}, nodes, "the node each member's pod runs on")
}

// waitingRead is a read whose one pod waits with no node to take it, placed as placement holds.
func waitingRead(t *testing.T, placement string) report.Read {
	t.Helper()
	body := `{"pods": [{"namespace": "ci", "name": "build", "node": null, "phase": "Pending", "ownerKind": "Job", "unschedulable": true,
		"containers": [], "placement": ` + placement + `}]}`
	return report.Read{Body: []byte(body), At: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
}

func TestRead_AWaitingPodAdmitsANodeWhoseLabelsItsPlacementAccepts(t *testing.T) {
	burst := map[string]string{"mitstack.dev/burst": "true", "kubernetes.io/os": "linux", "topology.kubernetes.io/zone": "fsn1"}
	cases := []struct {
		name      string
		placement string
		admits    bool
	}{
		{"a pod with no placement", `null`, true},
		{"a pod with an empty placement", `{"nodeSelector": null, "nodeSelectorTerms": null}`, true},
		{"a node selector every label of which the node carries", `{"nodeSelector": {"kubernetes.io/os": "linux"}}`, true},
		{"a node selector naming a label the node lacks", `{"nodeSelector": {"node-role.kubernetes.io/control-plane": ""}}`, false},
		{"a node selector naming another value", `{"nodeSelector": {"topology.kubernetes.io/zone": "nbg1"}}`, false},
		{"a term keeping off every burst node", `{"nodeSelectorTerms": [{"matchExpressions": [{"key": "mitstack.dev/burst", "operator": "DoesNotExist"}]}]}`, false},
		{"a term requiring a label the node carries", `{"nodeSelectorTerms": [{"matchExpressions": [{"key": "mitstack.dev/burst", "operator": "Exists"}]}]}`, true},
		{"a term whose values hold the node's zone", `{"nodeSelectorTerms": [{"matchExpressions": [{"key": "topology.kubernetes.io/zone", "operator": "In", "values": ["nbg1", "fsn1"]}]}]}`, true},
		{"a term whose values leave out the node's zone", `{"nodeSelectorTerms": [{"matchExpressions": [{"key": "topology.kubernetes.io/zone", "operator": "In", "values": ["nbg1"]}]}]}`, false},
		{"a term refusing the node's zone", `{"nodeSelectorTerms": [{"matchExpressions": [{"key": "topology.kubernetes.io/zone", "operator": "NotIn", "values": ["fsn1"]}]}]}`, false},
		{"a term refusing a label the node lacks", `{"nodeSelectorTerms": [{"matchExpressions": [{"key": "dedicated", "operator": "NotIn", "values": ["db"]}]}]}`, true},
		{"a term requiring a numeric label the node lacks", `{"nodeSelectorTerms": [{"matchExpressions": [{"key": "cpus", "operator": "Gt", "values": ["4"]}]}]}`, false},
		{"a term naming a node by its name", `{"nodeSelectorTerms": [{"matchFields": [{"key": "metadata.name", "operator": "In", "values": ["poc-talos4-cp1"]}]}]}`, false},
		{"an empty term", `{"nodeSelectorTerms": [{}]}`, false},
		{"one term refusing the node and one admitting it", `{"nodeSelectorTerms": [{"matchExpressions": [{"key": "mitstack.dev/burst", "operator": "DoesNotExist"}]},
			{"matchExpressions": [{"key": "kubernetes.io/os", "operator": "In", "values": ["linux"]}]}]}`, true},
		{"one term whose expressions admit the node and refuse it", `{"nodeSelectorTerms": [{"matchExpressions": [{"key": "kubernetes.io/os", "operator": "In", "values": ["linux"]},
			{"key": "mitstack.dev/burst", "operator": "DoesNotExist"}]}]}`, false},
		{"a node selector the node carries and a term refusing it", `{"nodeSelector": {"kubernetes.io/os": "linux"},
			"nodeSelectorTerms": [{"matchExpressions": [{"key": "mitstack.dev/burst", "operator": "DoesNotExist"}]}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			waiting, err := waitingRead(t, tc.placement).Unschedulable()

			require.NoError(t, err)
			require.Len(t, waiting, 1, "the pod that waits")
			assert.Equal(t, "ci/build", waiting[0].Name)
			assert.Equal(t, tc.admits, waiting[0].Admits(burst), "whether %s admits the burst node %v", tc.placement, burst)
		})
	}
}
