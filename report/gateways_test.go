package report_test

import (
	"encoding/json"
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
	for _, tc := range []struct {
		name, address, reason string
		named, passed         bool
	}{
		{name: "a reading the report made itself", address: "10.0.0.2", named: true, passed: true},
		{name: "a reading probed after since", address: "10.0.0.248", named: true, reason: "nothing took the connection to the mirror's port"},
		{name: "a reading probed before since", address: "10.0.0.249",
			reason: "the outside probe last read the gateway at 10.0.0.249 at 2026-11-08T09:59:10Z, not after 2026-11-08T10:00:00Z"},
		{name: "a reading whose time cannot be read", address: "10.0.0.250", reason: "its reading of the gateway at 10.0.0.250 names no time it was probed"},
		{name: "not named", address: "10.0.0.251"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			named, passed, reason := report.Gateway(body, netip.MustParseAddr(tc.address), since)
			assert.Equal(t, []any{tc.named, tc.passed}, []any{named, passed})
			if tc.reason == "" {
				assert.Empty(t, reason)
			} else {
				assert.Contains(t, reason, tc.reason)
			}
		})
	}
	named, _, reason := report.Gateway([]byte("not json"), netip.MustParseAddr("10.0.0.2"), since)
	assert.False(t, named, "a report that cannot be read")
	assert.Contains(t, reason, "the report cannot be read")
}

// mirror is a gateway's mirror that answers status on its API root, the path a node's pull starts from.
func mirror(t *testing.T, status int) string {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/" {
			t.Errorf("the probe asked the mirror %s %s", r.Method, r.URL.Path)
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

// tcpPeer takes each connection and hands it to answer, as a port that resets or closes connections does.
func tcpPeer(t *testing.T, answer func(*net.TCPConn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			answer(c.(*net.TCPConn))
		}
	}()
	return "https://" + l.Addr().String()
}

func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := l.Addr().String()
	require.NoError(t, l.Close())
	return "https://" + address
}

// probeGateways runs the outside probe with the real curl over gateways under the CA in caFile, and returns the
// ConfigMap it patched, as the report reads it.
func probeGateways(t *testing.T, gateways, caFile string) string {
	t.Helper()
	ca, err := os.ReadFile(caFile)
	require.NoError(t, err)
	patch := filepath.Join(t.TempDir(), "patch.json")
	tools := testutil.NewTools(t, map[string]string{"date": reportAt, "kubectl": `cp "${@: -1}" ` + patch,
		"openssl": `if [ "$1" = s_client ]; then cat testdata/mesh-probe-handshake.txt; exit 1; fi; exit 1`})
	tools.AddImageJq(t)
	script, env := renderedScript(t, reportURL, "mitstack-mesh-probe")

	out, err := testutil.RunBash(t, script, tools.Env("MESH_PROBE_HOST=127.0.0.1", "MESH_PROBE_PORT=1",
		"MESH_PROBE_CONFIGMAP="+env["MESH_PROBE_CONFIGMAP"], "POD_NAMESPACE=kube-system", "GATEWAYS="+gateways, "GATEWAY_CA="+string(ca)))
	require.NoError(t, err, out)
	var written map[string]any
	data, err := os.ReadFile(patch)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &written))
	written["apiVersion"], written["kind"] = "v1", "ConfigMap"
	written["metadata"] = map[string]any{"name": "mitstack-mesh-probe", "namespace": "kube-system"}
	configMap, err := json.Marshal(written)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "mesh-probe.json")
	require.NoError(t, os.WriteFile(path, configMap, 0o600))
	return path
}

// connections are the gateways' readings as the report carries them, each failed reason cut to the words before curl's.
func connections(t *testing.T, gateways any) map[string][]any {
	t.Helper()
	got := map[string][]any{}
	for _, item := range gateways.([]any) {
		reading := item.(map[string]any)
		reason, _, _ := strings.Cut(stringOr(reading["reason"]), ": ")
		got[reading["address"].(string)] = []any{reading["passed"], reading["connection"], reason, reading["probedAt"]}
	}
	return got
}

func stringOr(v any) string {
	s, _ := v.(string)
	return s
}

func TestOutsideProbe_ReadsAClosedPortAResetConnectionAFailedTLSHandshakeAndAServingMirrorAsSuchFromOutsideTheMesh(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(server.Close)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(plain.Close)
	gateways := strings.Join([]string{
		"10.0.0.2=" + closedPort(t),
		"10.0.0.3=" + tcpPeer(t, func(c *net.TCPConn) { _ = c.SetLinger(0); _ = c.Close() }),
		"10.0.0.4=" + tcpPeer(t, func(c *net.TCPConn) { _ = c.Close() }),
		"10.0.0.5=" + strings.Replace(plain.URL, "http://", "https://", 1),
		"10.0.0.6=" + mirror(t, http.StatusServiceUnavailable),
		"10.0.0.248=" + mirror(t, http.StatusOK),
	}, " ")
	probed := probeGateways(t, gateways, writeServerCA(t, server))
	other, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	require.NoError(t, err)
	otherCA := filepath.Join(t.TempDir(), "other-ca.pem")
	require.NoError(t, os.WriteFile(otherCA, other.Cert, 0o600))
	signedByAnother := probeGateways(t, "10.0.0.249="+mirror(t, http.StatusOK), otherCA)

	_, posted, out, err := runReport(t, configMap("cat "+probed), "GATEWAYS="+gateways)
	require.NoError(t, err, out)
	_, otherPosted, out, err := runReport(t, configMap("cat "+signedByAnother), "GATEWAYS=10.0.0.249=https://10.0.0.249")
	require.NoError(t, err, out)

	const at = "2026-10-23T15:45:00Z"
	assert.Equal(t, map[string][]any{
		"10.0.0.2":   {false, "closed", "nothing took the connection to the mirror's port", at},
		"10.0.0.3":   {false, "reset", "the connection was reset or closed before the mirror answered", at},
		"10.0.0.4":   {false, "reset", "the connection was reset or closed before the mirror answered", at},
		"10.0.0.5":   {false, "tls-failure", "the TLS handshake with the mirror failed", at},
		"10.0.0.6":   {false, "answered", "the mirror answered HTTP 503", at},
		"10.0.0.248": {true, "served", "", at},
	}, connections(t, posted.body["gateways"]))
	assert.Equal(t, map[string][]any{"10.0.0.249": {false, "tls-failure", "the TLS handshake with the mirror failed", at}},
		connections(t, otherPosted.body["gateways"]), "a mirror whose certificate another CA signed")
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
	assert.Equal(t, []any{map[string]any{"passed": false,
		"reason": "the TLS connection to https://127.0.0.1:1/directory ended with curl exit status 7"}}, readings([]any{posted.body["egress"]}))
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
	assert.Equal(t, "10.0.0.2=https://10.0.0.2 10.0.0.248=https://10.0.0.248", env["GATEWAYS"])
	assert.Equal(t, string(ca.Cert), env["GATEWAY_CA"], "the outside probe trusts the gateway CA alone for the mirrors")
	_, _, reportContainer := cronJob(t, rendered, "mitstack-node-report")
	assert.NotContains(t, testutil.ContainerEnv(t, reportContainer), "GATEWAY_CA", "the report no longer reads a mirror")
	policy := testutil.Kind(testutil.Documents(t, rendered), "CiliumClusterwideNetworkPolicy")[0]
	endpoints := map[string]testutil.CiliumEndpoint{
		"outside probe": testutil.Endpoint(namespace, nil, labels),
		"kube-dns":      testutil.Endpoint("kube-system", nil, map[string]string{"k8s-app": "kube-dns"}),
	}
	grants := testutil.Grants(t, policy, endpoints, "outside probe")
	assert.Subset(t, grants, []string{"egress to CIDR 10.0.0.2/32 on 443/TCP", "egress to CIDR 10.0.0.248/32 on 443/TCP"})
	assert.Equal(t, 2, len(slices.DeleteFunc(slices.Clone(grants), func(g string) bool { return !strings.Contains(g, "CIDR") })),
		"the outside probe reaches no address but the gateways': %v", grants)
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

// readings drops each failed reading's reason from its first ": ", where curl's own words begin.
func readings(member any) []any {
	list, _ := member.([]any)
	for _, item := range list {
		if reading, ok := item.(map[string]any); ok {
			if reason, ok := reading["reason"].(string); ok {
				reading["reason"], _, _ = strings.Cut(reason, ": ")
			}
		}
	}
	return list
}
