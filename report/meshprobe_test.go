package report_test

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const reportAt = "echo 2026-10-23T15:45:00Z"

func inNamespace(t *testing.T, docs []map[string]any, namespace string) []map[string]any {
	t.Helper()
	return slices.DeleteFunc(docs, func(d map[string]any) bool { return testutil.Field(t, d, "metadata", "namespace") != namespace })
}

// probeResult answers the outside probe's ConfigMap read with the fixture's result changed by the jq filter edit.
func probeResult(edit string) map[string]string {
	return map[string]string{"date": reportAt, "kubectl": kubectlAnswering(map[string]string{
		meshProbeRead: "jq '.data.result |= (fromjson | " + edit + " | tojson)' testdata/mesh-probe.json"})}
}

func TestReport_CarriesACallFromInsideTheMeshAnsweredAndAPlainCallFromOutsideRefusedWhileZtunnelPresentsTheTargetsCertificate(t *testing.T) {
	_, posted, out, err := runReport(t, map[string]string{"date": reportAt})

	require.NoError(t, err, out)
	assert.Equal(t, map[string]any{
		"callFromInside": map[string]any{"passed": true},
		"plainCallFromOutside": map[string]any{"passed": true, "probedAt": "2026-10-23T15:42:31Z", "curlExitStatus": float64(56),
			"stderr": "curl: (56) Recv failure: Connection reset by peer\n"},
		"workloadIdentity": map[string]any{"passed": true, "probedAt": "2026-10-23T15:42:31Z", "identity": components.MeshProbeIdentity,
			"serial": "5E1C0A9F3B2D47E8", "notBefore": "2026-10-23T09:12:00Z", "notAfter": "2026-10-24T09:12:00Z"},
	}, posted.body["mesh"])
}

func TestReport_NamesEachMeshResultItCannotPassAsFailedWithItsReason(t *testing.T) {
	configMap := func(answer string) map[string]string {
		return map[string]string{"date": reportAt, "kubectl": kubectlAnswering(map[string]string{meshProbeRead: answer})}
	}
	noConfigMap := configMap(`echo 'Error from server (NotFound): configmaps "mitstack-mesh-probe" not found' >&2; exit 1`)
	noResult := map[string]string{
		"plainCallFromOutside": "the ConfigMap kube-system/mitstack-mesh-probe holds no result the outside probe wrote",
		"workloadIdentity":     "the ConfigMap kube-system/mitstack-mesh-probe holds no result the outside probe wrote"}
	cases := map[string]struct {
		fakes  map[string]string
		env    []string
		failed map[string]string
	}{
		"the target is down": {map[string]string{"date": reportAt}, []string{"MESH_PROBE_URL=http://127.0.0.1:1/"}, map[string]string{
			"callFromInside":       "the call from inside the mesh ended with curl exit status 7: ",
			"plainCallFromOutside": "it did not answer the report either, so it may be down: the call from inside the mesh ended with curl exit status 7"}},
		"the outside probe names a time after the report": {probeResult(`.probedAt = "2026-10-23T16:00:00Z"`), nil, map[string]string{
			"plainCallFromOutside": "names 2026-10-23T16:00:00Z, after this report",
			"workloadIdentity":     "names 2026-10-23T16:00:00Z, after this report"}},
		"the certificate names no identity": {probeResult(`.handshake.certificate.identities = null`), nil, map[string]string{
			"workloadIdentity":     "the certificate on port 15008 names no identity, not " + components.MeshProbeIdentity,
			"plainCallFromOutside": "no workload certificate shows the mesh holds it: the certificate on port 15008 names no identity"}},
		"the result has a shape the report cannot judge": {probeResult(`.plainCall = "reset"`), nil, map[string]string{
			"plainCallFromOutside": "the outside probe's result in kube-system/mitstack-mesh-probe cannot be judged: "}},
		"the result is not JSON":            {configMap(`jq '.data.result = "oops"' testdata/mesh-probe.json`), nil, noResult},
		"the ConfigMap holds no data":       {configMap(`jq 'del(.data)' testdata/mesh-probe.json`), nil, noResult},
		"the ConfigMap read prints no JSON": {configMap(`echo 'not json'`), nil, noResult},
		"the ConfigMap is missing": {noConfigMap, nil, map[string]string{
			"plainCallFromOutside": "reading the ConfigMap kube-system/mitstack-mesh-probe ended with exit status 1: Error from server (NotFound)",
			"workloadIdentity":     "reading the ConfigMap kube-system/mitstack-mesh-probe ended with exit status 1: Error from server (NotFound)"}},
		"the outside probe stopped writing": {probeResult(`.probedAt = "2026-10-23T15:29:00Z"`), nil, map[string]string{
			"plainCallFromOutside": "the outside probe last wrote at 2026-10-23T15:29:00Z, over 15 minutes before this report",
			"workloadIdentity":     "the outside probe last wrote at 2026-10-23T15:29:00Z, over 15 minutes before this report"}},
		"the target answered a plain call": {probeResult(`.plainCall = {exitStatus: 0, answer: "mitstack-mesh-probe answered\n", stderr: ""}`), nil,
			map[string]string{"plainCallFromOutside": `the target answered a call that carried no workload identity: "mitstack-mesh-probe answered\n"`}},
		"the plain call reached nothing": {probeResult(`.plainCall = {exitStatus: 28, answer: "", stderr: "curl: (28) timed out\n"}`), nil,
			map[string]string{"plainCallFromOutside": "the plain call never reached the target: curl ended with exit status 28: curl: (28) timed out"}},
		"the handshake was refused": {probeResult(`.handshake = {exitStatus: 1, stderr: "connect: Connection refused\n", certificate: null}`), nil,
			map[string]string{
				"workloadIdentity":     "no workload certificate came back from port 15008: openssl s_client ended with exit status 1: connect: Connection refused",
				"plainCallFromOutside": "the target closed the plain call, and no workload certificate shows the mesh holds it: no workload certificate"}},
		"the certificate names another workload": {probeResult(`.handshake.certificate.identities = ["spiffe://cluster.local/ns/app-shop/sa/default"]`), nil,
			map[string]string{
				"workloadIdentity":     "the certificate on port 15008 names spiffe://cluster.local/ns/app-shop/sa/default, not " + components.MeshProbeIdentity,
				"plainCallFromOutside": "no workload certificate shows the mesh holds it: the certificate on port 15008 names"}},
		"the certificate had expired": {probeResult(`.handshake.certificate.notAfter = "2026-10-23T15:00:00Z"`), nil,
			map[string]string{
				"workloadIdentity":     "the certificate on port 15008 holds from 2026-10-23T09:12:00Z to 2026-10-23T15:00:00Z, which leaves out 2026-10-23T15:42:31Z",
				"plainCallFromOutside": "no workload certificate shows the mesh holds it: the certificate on port 15008 holds from"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, tc.fakes, tc.env...)

			require.NoError(t, err, out)
			assert.Len(t, posted.body["nodes"], 3, "a failed mesh result leaves a full report")
			for _, result := range []string{"callFromInside", "plainCallFromOutside", "workloadIdentity"} {
				got := testutil.Field(t, posted.body, "mesh", result).(map[string]any)
				want, failed := tc.failed[result]
				assert.Equal(t, !failed, got["passed"], "%s: %v", result, got)
				if failed {
					assert.Contains(t, got["reason"], want, result)
				}
			}
		})
	}
}

func runMeshProbe(t *testing.T, fakes map[string]string) (*testutil.Tools, map[string]any) {
	t.Helper()
	realOpenSSL, err := exec.LookPath("openssl")
	require.NoError(t, err)
	patch := filepath.Join(t.TempDir(), "patch.json")
	all := map[string]string{"date": reportAt, "kubectl": `cp "${@: -1}" ` + patch,
		"openssl": `if [ "$1" = s_client ]; then cat testdata/mesh-probe-handshake.txt; exit 1; fi; exec ` + realOpenSSL + ` "$@"`}
	maps.Copy(all, fakes)
	tools := testutil.NewTools(t, all)
	tools.AddImageJq(t)
	script, env := renderedScript(t, "https://api.mitstack.dev/v1/cluster-reports", "mitstack-mesh-probe")

	out, err := testutil.RunBash(t, script, tools.Env("MESH_PROBE_HOST="+env["MESH_PROBE_HOST"], "MESH_PROBE_PORT="+env["MESH_PROBE_PORT"],
		"MESH_PROBE_CONFIGMAP="+env["MESH_PROBE_CONFIGMAP"], "POD_NAMESPACE=kube-system"))
	require.NoError(t, err, out)
	var written struct{ Data struct{ Result string } }
	data, err := os.ReadFile(patch)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &written))
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(written.Data.Result), &result))
	return tools, result
}

func TestMeshProbe_WritesThePlainCallsOutcomeAndTheCertificateZtunnelPresentsIntoTheReportsConfigMap(t *testing.T) {
	tools, result := runMeshProbe(t, map[string]string{"curl": `echo 'curl: (56) Recv failure: Connection reset by peer' >&2; exit 56`})

	calls := tools.Calls(t)
	host := regexp.QuoteMeta(components.MeshProbeHost + ".")
	assert.Regexp(t, `^curl -sS --max-time 5 -o \S+ http://`+host+`:8080/$`, calls[0], "the plain call, by the absolute name its DNS rule admits")
	assert.Contains(t, calls, "openssl s_client -connect "+components.MeshProbeHost+".:15008 -alpn h2")
	assert.Regexp(t, `^kubectl patch configmap mitstack-mesh-probe -n kube-system --type merge --patch-file \S+$`, calls[len(calls)-1])
	assert.Equal(t, "2026-10-23T15:45:00Z", result["probedAt"])
	assert.Equal(t, map[string]any{"exitStatus": float64(56), "answer": "", "stderr": "curl: (56) Recv failure: Connection reset by peer\n"}, result["plainCall"])
	assert.Equal(t, map[string]any{"serial": "5E1C0A9F3B2D47E8", "notBefore": "2026-10-23T09:12:00Z", "notAfter": "2026-10-24T09:12:00Z",
		"identities": []any{components.MeshProbeIdentity}}, testutil.Field(t, result, "handshake", "certificate"))
	assert.Equal(t, float64(1), testutil.Field(t, result, "handshake", "exitStatus"), "s_client's status once the server refuses this side")
}

func TestMeshProbe_WritesAnAnsweredPlainCallAndAHandshakeThatGaveNoCertificateAsTheyCame(t *testing.T) {
	_, result := runMeshProbe(t, map[string]string{"curl": `printf 'mitstack-mesh-probe answered\n' > "$5"`,
		"openssl": `if [ "$1" = s_client ]; then echo 'connect:errno=111' >&2; exit 1; fi; exit 1`})

	assert.Equal(t, map[string]any{"exitStatus": float64(0), "answer": "mitstack-mesh-probe answered\n", "stderr": ""}, result["plainCall"])
	assert.Nil(t, testutil.Field(t, result, "handshake", "certificate"))
	assert.Equal(t, "connect:errno=111\n", testutil.Field(t, result, "handshake", "stderr"))
}

func TestManifests_LetsTheOutsideProbePatchAndTheReportReadOnlyTheProbesConfigMapByNameInKubeSystem(t *testing.T) {
	rendered, err := report.Manifests(inputs("https://api.mitstack.dev/v1/cluster-reports"))
	require.NoError(t, err)
	docs := testutil.Documents(t, rendered)

	granted := map[string]any{}
	for _, role := range inNamespace(t, testutil.Kind(docs, "Role"), "kube-system") {
		granted[testutil.Field(t, role, "metadata", "name").(string)] = role["rules"]
	}
	only := func(verb string) []any {
		return []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"configmaps"}, "resourceNames": []any{"mitstack-mesh-probe"}, "verbs": []any{verb}}}
	}
	assert.Equal(t, map[string]any{"mitstack-mesh-probe": only("patch"), "mitstack-node-report-mesh-probe": only("get")}, granted)
	subjects := map[string]any{}
	for _, binding := range inNamespace(t, testutil.Kind(docs, "RoleBinding"), "kube-system") {
		subjects[testutil.Field(t, binding, "roleRef", "name").(string)] = testutil.Field(t, binding, "subjects", "0")
	}
	assert.Equal(t, map[string]any{
		"mitstack-mesh-probe":             map[string]any{"kind": "ServiceAccount", "name": "mitstack-mesh-probe", "namespace": "kube-system"},
		"mitstack-node-report-mesh-probe": map[string]any{"kind": "ServiceAccount", "name": "mitstack-node-report", "namespace": "mitstack-node"},
	}, subjects)
	job, _, _ := cronJob(t, rendered, "mitstack-mesh-probe")
	assert.Equal(t, "kube-system", testutil.Field(t, job, "metadata", "namespace"), "the outside probe runs where the mesh leaves it out")
}

// The target in the component set takes calls from the report and the outside probe alone, as their pods are labelled.
func TestMeshProbe_TheTargetAdmitsOnlyTheReportAndTheOutsideProbeWhichLooksUpOnlyItsNameAndReachesOnlyItAndTheAPIServer(t *testing.T) {
	rendered, err := report.Manifests(inputs("https://api.mitstack.dev/v1/cluster-reports"))
	require.NoError(t, err)
	set := components.Default()
	target := testutil.Documents(t, testutil.Manifest(t, "mitstack-mesh-probe", set.Served).Bytes)
	_, reportLabels, _ := cronJob(t, rendered, "mitstack-node-report")
	_, probeLabels, _ := cronJob(t, rendered, "mitstack-mesh-probe")
	labels := func(m map[string]any) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[k] = v.(string)
		}
		return out
	}
	pod := testutil.Field(t, testutil.Kind(target, "Deployment")[0], "spec", "template")
	endpoints := map[string]testutil.CiliumEndpoint{
		"target":        testutil.Endpoint(components.MeshProbeNamespace, nil, labels(testutil.Field(t, pod, "metadata", "labels").(map[string]any))),
		"report":        testutil.Endpoint("mitstack-node", nil, labels(reportLabels)),
		"outside probe": testutil.Endpoint("kube-system", nil, labels(probeLabels)),
		"app pod":       testutil.Endpoint("app-shop", nil, map[string]string{"mitstack.dev/app": "shop"}),
		"cilium":        testutil.Endpoint("kube-system", nil, map[string]string{"k8s-app": "cilium"}),
		"kube-dns":      testutil.Endpoint("kube-system", nil, map[string]string{"k8s-app": "kube-dns"}),
	}

	policy := testutil.Kind(target, "CiliumClusterwideNetworkPolicy")[0]
	assert.Equal(t, []string{"ingress with outside probe on 8080/TCP 15008/TCP", "ingress with report on 8080/TCP 15008/TCP"},
		testutil.Grants(t, policy, endpoints, "target"))
	outside := testutil.Kind(testutil.Documents(t, rendered), "CiliumClusterwideNetworkPolicy")
	require.Len(t, outside, 1)
	assert.Equal(t, []string{
		"egress to entity kube-apiserver",
		"egress with kube-dns on 53/UDP 53/TCP through the DNS proxy",
		"egress with target on 8080/TCP 15008/TCP",
	}, testutil.Grants(t, outside[0], endpoints, "outside probe"))
	assert.Equal(t, []any{map[string]any{"matchName": components.MeshProbeHost}}, testutil.Field(t, outside[0], "spec", "egress", "0", "toPorts", "0", "rules", "dns"),
		"the DNS proxy answers the target's name alone, so no lookup leaves kube-system")
	assert.Empty(t, testutil.Grants(t, outside[0], endpoints, "cilium"), "the policy holds the outside probe's pod alone")
	assert.Equal(t, components.MeshProbeAnswer, testutil.Field(t, testutil.Kind(target, "ConfigMap")[0], "data", "index.html"))
	assert.Equal(t, components.MeshProbeHost, testutil.Field(t, testutil.Kind(target, "Service")[0], "metadata", "name").(string)+"."+components.MeshProbeNamespace+".svc.cluster.local")
	assert.Equal(t, components.MeshProbeIdentity, "spiffe://cluster.local/ns/"+components.MeshProbeNamespace+"/sa/"+testutil.Field(t, pod, "spec", "serviceAccountName").(string))
	assert.NotEmpty(t, testutil.Field(t, pod, "metadata", "labels", "mitstack.dev/app"), "the app label policy admits the target like an app's pod")
	without := set
	without.Served = slices.DeleteFunc(slices.Clone(set.Served), func(m components.Manifest) bool { return m.Name == "mitstack-mesh-probe" })
	images, err := set.Images()
	require.NoError(t, err)
	imagesWithout, err := without.Images()
	require.NoError(t, err)
	assert.Equal(t, imagesWithout, images, "the target runs an image the gateway already mirrors")
}
