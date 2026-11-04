package report_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

func TestGateway_ReadsWhetherAReportNamesTheGatewayAndWhetherItsMirrorAnswered(t *testing.T) {
	body := []byte(`{"gateways":[{"address":"10.0.0.2","passed":true},{"address":"10.0.0.248","passed":false,"reason":"the mirror did not answer within 5s"}]}`)
	for _, tc := range []struct {
		name, address, reason string
		named, passed         bool
	}{
		{name: "answered", address: "10.0.0.2", named: true, passed: true},
		{name: "not answered", address: "10.0.0.248", named: true, reason: "the mirror did not answer within 5s"},
		{name: "not named", address: "10.0.0.249"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			named, passed, reason := report.Gateway(body, netip.MustParseAddr(tc.address))
			assert.Equal(t, []any{tc.named, tc.passed, tc.reason}, []any{named, passed, reason})
		})
	}
	named, _, reason := report.Gateway([]byte("not json"), netip.MustParseAddr("10.0.0.2"))
	assert.False(t, named, "a report that cannot be read")
	assert.Contains(t, reason, "the report cannot be read")
}

// mirror is a gateway's mirror that answers status on its API root, the path a node's pull starts from.
func mirror(t *testing.T, status int) string {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/" {
			t.Errorf("the report asked the mirror %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func servingGateways(t *testing.T) string {
	t.Helper()
	url := mirror(t, http.StatusOK)
	return "10.0.0.2=" + url + " 10.0.0.248=" + url + " 10.0.0.249=" + url
}

// egressTarget stands in for Let's Encrypt's ACME directory, under the CA every httptest server shares.
func egressTarget(t *testing.T) string {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(server.Close)
	return server.URL + "/directory"
}

func TestReport_ReadsEachGatewaysMirrorUnderTheGatewayCA(t *testing.T) {
	gateways := "10.0.0.2=" + mirror(t, http.StatusOK) + " 10.0.0.248=" + mirror(t, http.StatusServiceUnavailable) +
		" 10.0.0.249=https://127.0.0.1:1"

	_, posted, out, err := runReport(t, nil, "GATEWAYS="+gateways)

	require.NoError(t, err, out)
	got := readings(t, posted.body["gateways"])
	assert.Equal(t, []map[string]any{
		{"address": "10.0.0.2", "passed": true},
		{"address": "10.0.0.248", "passed": false, "reason": "the mirror answered HTTP 503"},
		{"address": "10.0.0.249", "passed": false, "reason": "curl ended with exit status 7"},
	}, got)
}

func TestReport_ReadsAMirrorWhoseCertificateAnotherCASignedAsFailed(t *testing.T) {
	other, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	require.NoError(t, err)

	_, posted, out, err := runReport(t, nil, "GATEWAYS=10.0.0.2="+mirror(t, http.StatusOK), "GATEWAY_CA="+string(other.Cert))

	require.NoError(t, err, out)
	assert.Equal(t, []map[string]any{{"address": "10.0.0.2", "passed": false, "reason": "curl ended with exit status 60"}},
		readings(t, posted.body["gateways"]))
}

func TestReport_ReadsATLSConnectionToTheEgressTargetAndWhyOneFailed(t *testing.T) {
	_, posted, out, err := runReport(t, nil)
	require.NoError(t, err, out)
	assert.Equal(t, map[string]any{"passed": true}, posted.body["egress"], "a TLS connection the target answered")

	_, posted, out, err = runReport(t, nil, "EGRESS_URL=https://127.0.0.1:1/directory")

	require.NoError(t, err, out)
	egress := readings(t, []any{posted.body["egress"]})[0]
	assert.Equal(t, map[string]any{"passed": false,
		"reason": "the TLS connection to https://127.0.0.1:1/directory ended with curl exit status 7"}, egress)
}

func TestReport_NamesNoGatewayWhenTheClusterHasNone(t *testing.T) {
	_, posted, out, err := runReport(t, nil, "GATEWAYS=")

	require.NoError(t, err, out)
	assert.Equal(t, []any{}, posted.body["gateways"])
}

// readings drops each failed reading's reason from its first ": ", where curl's own words begin.
func readings(t *testing.T, member any) []map[string]any {
	t.Helper()
	list, ok := member.([]any)
	require.True(t, ok, "the report carries no list of readings: %v", member)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		reading, ok := item.(map[string]any)
		require.True(t, ok, "a reading that is not an object: %v", item)
		if reason, ok := reading["reason"].(string); ok {
			reading["reason"], _, _ = strings.Cut(reason, ": ")
		}
		out = append(out, reading)
	}
	return out
}
