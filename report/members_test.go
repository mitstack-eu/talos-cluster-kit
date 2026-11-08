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
