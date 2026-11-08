package report_test

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func TestGateway_ReadsWhetherAReportNamesAReadingOfTheGatewayMadeAfterAGivenTimeAndWhetherItsMirrorServed(t *testing.T) {
	since := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"gateways":[
		{"address":"10.0.0.2","passed":true},
		{"address":"10.0.0.248","passed":false,"connection":"closed","probedAt":"2026-11-08T10:04:10Z","reason":"nothing took the connection to the mirror's port"},
		{"address":"10.0.0.249","passed":true,"connection":"served","probedAt":"2026-11-08T09:59:10Z"},
		{"address":"10.0.0.250","passed":true,"probedAt":"yesterday"}]}`)
	for address, want := range map[string][]any{
		"10.0.0.2":   {true, true, ""},                                                  // a reading the report made itself
		"10.0.0.248": {true, false, "nothing took the connection to the mirror's port"}, // probed after since
		"10.0.0.249": {false, false, "the outside probe last read the gateway at 10.0.0.249 at 2026-11-08T09:59:10Z, not after 2026-11-08T10:00:00Z"},
		"10.0.0.250": {false, false, "its reading of the gateway at 10.0.0.250 names no time it was probed"},
		"10.0.0.251": {false, false, ""}, // not named
	} {
		named, passed, reason := report.Gateway(body, netip.MustParseAddr(address), since)
		reason, _, _ = strings.Cut(reason, ": ")
		assert.Equal(t, want, []any{named, passed, reason}, "the reason up to its parse error, for %s", address)
	}
	named, _, reason := report.Gateway([]byte("not json"), netip.MustParseAddr("10.0.0.2"), since)
	assert.False(t, named, "a report that cannot be read")
	assert.Contains(t, reason, "the report cannot be read")
}

// mirror is a gateway's mirror that answers status on its API root, the path a node's pull starts from.
func mirror(t *testing.T, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/" {
			t.Errorf("the probe asked the mirror %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server
}

// egressTarget stands in for Let's Encrypt's ACME directory, under the CA every httptest server shares.
func egressTarget(t *testing.T) string {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(server.Close)
	return server.URL + "/directory"
}

// tcpPeer takes the probe's one connection, reads its TLS hello and hands it to answer, as a port that resets, closes
// or speaks no TLS once asked does.
func tcpPeer(t *testing.T, answer func(*net.TCPConn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		if c, err := l.Accept(); err == nil {
			readHello(c)
			answer(c.(*net.TCPConn))
		}
	}()
	return "https://" + l.Addr().String()
}

// readHello reads the probe's first TLS record whole, since a peer that answers before it races curl's connect, which
// curl then reads as a closed port, and one that closes with it unread sends a reset.
func readHello(c net.Conn) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(c, header); err == nil {
		_, _ = io.ReadFull(c, make([]byte, int(header[3])<<8|int(header[4])))
	}
}

// probeGateways runs the outside probe with the real curl over gateways under ca, and returns a file holding the
// ConfigMap data it patched, as the report reads it.
func probeGateways(t *testing.T, gateways string, ca []byte) string {
	t.Helper()
	_, result := runMeshProbe(t, nil, "MESH_PROBE_HOST=127.0.0.1", "MESH_PROBE_PORT=1", "GATEWAYS="+gateways, "GATEWAY_CA="+string(ca))
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	data, err := json.Marshal(map[string]any{"data": map[string]string{"result": string(encoded)}})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "mesh-probe.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// connections are the gateways' readings as the report carries them, each failed reason cut to the words before curl's.
func connections(gateways any) map[string][]any {
	got := map[string][]any{}
	for _, item := range gateways.([]any) {
		reading := item.(map[string]any)
		reason, _ := reading["reason"].(string)
		reason, _, _ = strings.Cut(reason, ": ")
		got[reading["address"].(string)] = []any{reading["passed"], reading["connection"], reason, reading["probedAt"]}
	}
	return got
}

func TestOutsideProbe_ReadsAClosedPortAResetConnectionAFailedTLSHandshakeAndAServingMirrorAsSuchFromOutsideTheMesh(t *testing.T) {
	serving := mirror(t, http.StatusOK)
	gateways := strings.Join([]string{
		"10.0.0.2=https://127.0.0.1:1",
		"10.0.0.3=" + tcpPeer(t, func(c *net.TCPConn) { _ = c.SetLinger(0); _ = c.Close() }),
		"10.0.0.4=" + tcpPeer(t, func(c *net.TCPConn) { _ = c.Close() }),
		"10.0.0.5=" + tcpPeer(t, func(c *net.TCPConn) { _, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n")); _ = c.Close() }),
		"10.0.0.6=" + mirror(t, http.StatusServiceUnavailable).URL,
		"10.0.0.248=" + serving.URL,
	}, " ")
	probed := probeGateways(t, gateways, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serving.Certificate().Raw}))
	other, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	require.NoError(t, err)
	signedByAnother := probeGateways(t, "10.0.0.249="+serving.URL, other.Cert)

	_, posted, out, err := runReport(t, configMap("cat "+probed), "GATEWAYS=10.0.0.2 10.0.0.3 10.0.0.4 10.0.0.5 10.0.0.6 10.0.0.248")
	require.NoError(t, err, out)
	_, otherPosted, out, err := runReport(t, configMap("cat "+signedByAnother), "GATEWAYS=10.0.0.249")
	require.NoError(t, err, out)

	const at = "2026-10-23T15:45:00Z"
	assert.Equal(t, map[string][]any{
		"10.0.0.2":   {false, "closed", "nothing took the connection to the mirror's port", at},
		"10.0.0.3":   {false, "reset", "the connection was reset or closed before the mirror answered", at},
		"10.0.0.4":   {false, "reset", "the connection was reset or closed before the mirror answered", at},
		"10.0.0.5":   {false, "tls-failure", "the TLS handshake with the mirror failed", at},
		"10.0.0.6":   {false, "answered", "the mirror answered HTTP 503", at},
		"10.0.0.248": {true, "served", "", at},
	}, connections(posted.body["gateways"]))
	assert.Equal(t, map[string][]any{"10.0.0.249": {false, "tls-failure", "the TLS handshake with the mirror failed", at}},
		connections(otherPosted.body["gateways"]), "a mirror whose certificate another CA signed")
}

func TestReport_ReadsEveryGatewayFailedWithTheReasonWhenTheOutsideProbesResultCannotBeTrusted(t *testing.T) {
	for name, tc := range map[string]struct {
		fakes  map[string]string
		reason string
	}{
		"the outside probe stopped writing": {probeResult(`.probedAt = "2026-10-23T15:29:00Z"`),
			"the outside probe last wrote at 2026-10-23T15:29:00Z, over 15 minutes before this report"},
		"the outside probe read no gateway": {probeResult(`del(.gateways)`),
			"the outside probe's result in kube-system/mitstack-mesh-probe holds no reading of the gateway at 10.0.0.2"},
		"the ConfigMap is missing": {configMap(`echo 'Error from server (NotFound)' >&2; exit 1`),
			"reading the ConfigMap kube-system/mitstack-mesh-probe ended with exit status 1"},
		"a reading has a shape the report cannot judge": {probeResult(`.gateways = [1]`),
			"the outside probe's result in kube-system/mitstack-mesh-probe cannot be judged"},
	} {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, tc.fakes)

			require.NoError(t, err, out)
			readings := posted.body["gateways"].([]any)
			require.Len(t, readings, 3, "a reading for each gateway the report names")
			for _, r := range readings {
				reading := r.(map[string]any)
				assert.Equal(t, false, reading["passed"], reading)
				assert.Contains(t, reading["reason"], tc.reason)
				assert.Nil(t, reading["probedAt"], "a reading the probe did not make names no time it made it")
			}
		})
	}
}

func TestReport_NamesNoGatewayWhenTheClusterHasNone(t *testing.T) {
	_, posted, out, err := runReport(t, nil, "GATEWAYS=")
	require.NoError(t, err, out)
	assert.Equal(t, []any{}, posted.body["gateways"])
}

func TestReport_ReadsATLSConnectionToTheEgressTargetAndWhyOneFailed(t *testing.T) {
	_, posted, out, err := runReport(t, nil)
	require.NoError(t, err, out)
	assert.Equal(t, map[string]any{"passed": true}, posted.body["egress"], "a TLS connection the target answered")

	_, posted, out, err = runReport(t, nil, "EGRESS_URL=https://127.0.0.1:1/directory")
	require.NoError(t, err, out)
	assert.Equal(t, false, testutil.Field(t, posted.body, "egress", "passed"))
	assert.Contains(t, testutil.Field(t, posted.body, "egress", "reason"), "the TLS connection to https://127.0.0.1:1/directory ended with curl exit status 7: ")
}

func TestManifests_RunsTheOutsideProbeWhereTheMeshLeavesItOutAndLetsItReachEachGatewaysMirrorOn443Alone(t *testing.T) {
	ca, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{netip.MustParseAddr("10.0.0.2")})
	require.NoError(t, err)
	in := inputs(reportURL)
	in.Gateways = []netip.Addr{netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.248")}
	in.GatewayCA = ca.Cert
	rendered, err := report.Manifests(in)
	require.NoError(t, err)
	cni := testutil.Kind(testutil.Documents(t, testutil.Manifest(t, "istio-cni", components.Default().Served).Bytes), "ConfigMap")[0]["data"].(map[string]any)

	job, labels, container := cronJob(t, rendered, "mitstack-mesh-probe")
	namespace := testutil.Field(t, job, "metadata", "namespace").(string)
	assert.Contains(t, strings.Split(cni["EXCLUDE_NAMESPACES"].(string), ","), namespace, "istio-cni captures the outside probe's pod")
	env := testutil.ContainerEnv(t, container)
	assert.Equal(t, "10.0.0.2 10.0.0.248", env["GATEWAYS"], "each gateway's address, where its mirror answers on https")
	assert.Equal(t, string(ca.Cert), env["GATEWAY_CA"], "the outside probe trusts the gateway CA alone for the mirrors")
	_, _, reportContainer := cronJob(t, rendered, "mitstack-node-report")
	assert.NotContains(t, testutil.ContainerEnv(t, reportContainer), "GATEWAY_CA", "the report reads no mirror itself")
	policy := testutil.Kind(testutil.Documents(t, rendered), "CiliumClusterwideNetworkPolicy")[0]
	grants := testutil.Grants(t, policy, map[string]testutil.CiliumEndpoint{"probe": testutil.Endpoint(namespace, nil, labels)}, "probe")
	cidrs := slices.DeleteFunc(grants, func(g string) bool { return !strings.Contains(g, "CIDR") })
	assert.Equal(t, []string{"egress to CIDR 10.0.0.2/32 on 443/TCP", "egress to CIDR 10.0.0.248/32 on 443/TCP"}, cidrs,
		"the outside probe reaches no address but the gateways'")
}

func TestManifests_ShipNoCommentLine(t *testing.T) {
	assert.NotRegexp(t, `(?m)^#`, string(manifests(t, reportURL)), "a comment line costs a builder's user data and nothing reads it")
}

func TestManifests_RefusesGatewaysWithoutTheirCAsCertificate(t *testing.T) {
	ca, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{netip.MustParseAddr("10.0.0.2")})
	require.NoError(t, err)
	in := inputs(reportURL)
	in.Gateways = []netip.Addr{netip.MustParseAddr("10.0.0.2")}
	for name, pem := range map[string][]byte{
		"no certificate":           nil,
		"a key":                    ca.Key,
		"a certificate and a key":  append(append([]byte{}, ca.Cert...), ca.Key...),
		"two certificates":         append(append([]byte{}, ca.Cert...), ca.Cert...),
		"text that is no PEM file": []byte("mitstack gateway CA"),
	} {
		t.Run(name, func(t *testing.T) {
			in.GatewayCA = pem
			_, err := report.Manifests(in)
			assert.ErrorContains(t, err, "the gateway CA is not one PEM certificate")
		})
	}
	in.GatewayCA = ca.Cert
	_, err = report.Manifests(in)
	assert.NoError(t, err, "the CA's certificate alone")
}
