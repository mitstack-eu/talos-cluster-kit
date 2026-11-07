package report_test

// securityscan_test.go pins the scan CronJob that runs kube-bench and kubescape and keeps their summaries for the report.

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const scanAt = "echo 2026-11-07T03:17:00Z"

// kubescapeWriting is a fake kubescape that writes testdata/scan/kubescape.json where its --output names.
const kubescapeWriting = `while [ $# -gt 0 ]; do if [ "$1" = --output ]; then cp testdata/scan/kubescape.json "$2"; fi; shift; done`

type scanRun struct {
	tools *testutil.Tools
	data  map[string]string
	out   string
	err   error
}

// summary is one summary the scan patched into its ConfigMap, decoded.
func (r scanRun) summary(t *testing.T, key string) map[string]any {
	t.Helper()
	var s map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.data[key]), &s), "%s: %q", key, r.data[key])
	return s
}

func runScan(t *testing.T, fakes map[string]string) scanRun {
	t.Helper()
	patch := filepath.Join(t.TempDir(), "patch.json")
	all := map[string]string{"date": scanAt, "kubectl": `cp "${@: -1}" ` + patch,
		"kube-bench": "cat testdata/scan/kube-bench.json", "kubescape": kubescapeWriting}
	maps.Copy(all, fakes)
	tools := testutil.NewTools(t, all)
	tools.AddImageJq(t)
	script, env := renderedScript(t, reportURL, "mitstack-security-scan")

	out, err := testutil.RunBash(t, script, tools.Env("SCAN_CONFIGMAP="+env["SCAN_CONFIGMAP"], "KUBE_BENCH_CONFIG=/kube-bench/cfg",
		"KUBE_BENCH_VERSION="+env["KUBE_BENCH_VERSION"], "KUBESCAPE_ARTIFACTS=testdata/scan",
		"KUBESCAPE_CONTROLS_VERSION="+env["KUBESCAPE_CONTROLS_VERSION"]))
	run := scanRun{tools: tools, out: out, err: err}
	var written struct{ Data map[string]string }
	data, readErr := os.ReadFile(patch)
	require.NoError(t, readErr, "the scan patched its ConfigMap: %s", out)
	require.NoError(t, json.Unmarshal(data, &written))
	run.data = written.Data
	return run
}

func TestSecurityScan_RunsKubeBenchOnTheTalosBenchmarkAndKubescapeOnNSAAndPatchesBothSummariesIntoItsConfigMap(t *testing.T) {
	run := runScan(t, nil)
	require.NoError(t, run.err, run.out)

	calls := run.tools.Calls(t)
	assert.Contains(t, calls, "kube-bench run --config-dir /kube-bench/cfg --benchmark cis-1.10 --targets master,etcd,controlplane,node,policies --json",
		"the benchmark Sidero documents for Talos, since kube-bench's version mapping picks none for Talos")
	assert.Contains(t, strings.Join(calls, "\n"), "kubescape scan framework nsa --use-from testdata/scan/nsa --controls-config ")
	for _, call := range calls {
		if strings.HasPrefix(call, "kubescape ") {
			assert.Regexp(t, `^kubescape scan framework nsa --use-from testdata/scan/nsa --controls-config \S+/inputs\.json `+
				`--exceptions \S+/exceptions\.json --host-scan=false --format json --output \S+/kubescape\.json --cache-dir \S+$`, call,
				"an empty exception list, since only the platform may exempt a finding, and no host sensor")
		}
	}
	assert.Regexp(t, `^kubectl patch configmap mitstack-security-scans -n kube-system --type merge --patch-file \S+$`, calls[len(calls)-1])

	assert.Equal(t, map[string]any{"tool": "kube-bench", "version": "v0.16.0", "startedAt": "2026-11-07T03:17:00Z",
		"finishedAt": "2026-11-07T03:17:00Z", "profile": "cis-1.10",
		"totals": map[string]any{"pass": float64(69), "fail": float64(3), "warn": float64(46), "info": float64(14)},
		"failed": []any{
			map[string]any{"id": "1.1.9", "scored": true,
				"text": "Ensure that the Container Network Interface file permissions are set to 600 or more restrictive (Automated)"},
			map[string]any{"id": "1.1.10", "scored": true, "text": "Ensure that the Container Network Interface file ownership is set to root:root (Automated)"},
			map[string]any{"id": "1.2.30", "scored": true,
				"text": "Ensure that the --service-account-extend-token-expiration parameter is set to false (Automated)"},
		}}, run.summary(t, "kube-bench"))

	scan := run.summary(t, "kubescape")
	failed := scan["failed"].([]any)
	delete(scan, "failed")
	assert.Equal(t, map[string]any{"tool": "kubescape", "version": "v4.0.15", "startedAt": "2026-11-07T03:17:00Z",
		"finishedAt": "2026-11-07T03:17:00Z", "framework": "NSA", "controlsVersion": "v2.0.38",
		"failedControlsBySeverity": map[string]any{"criticalSeverity": float64(0), "highSeverity": float64(2), "mediumSeverity": float64(8),
			"lowSeverity": float64(1)}}, scan)
	var order []string
	for _, c := range failed {
		order = append(order, testutil.Field(t, c, "id").(string))
	}
	assert.Equal(t, []string{"C-0270", "C-0271", "C-0002", "C-0013", "C-0016", "C-0030", "C-0034", "C-0035", "C-0054", "C-0055", "C-0017"}, order,
		"every failed control, the most severe first, then by id")
	workload := func(name string) any { return map[string]any{"namespace": "kube-system", "kind": "Deployment", "name": name} }
	assert.Equal(t, map[string]any{"id": "C-0270", "name": testutil.Field(t, failed[0], "name"), "severity": "High", "failedResources": float64(3),
		"resources": []any{workload("coredns"), workload("local-path-provisioner"), workload("metrics-server")}}, failed[0])
	assert.Equal(t, []any{workload("local-path-provisioner"), workload("metrics-server")}, testutil.Field(t, failed[1], "resources"))
	assert.Equal(t, []any{}, testutil.Field(t, failed[2], "resources"), "a Medium control lists no workload")

	assert.Equal(t, map[string]any{"finishedAt": "2026-11-07T03:17:00Z", "exitStatus": float64(0), "output": "kube-bench: ran; kubescape: ran"},
		run.summary(t, "last-run"))
}

func TestSecurityScan_KeepsAToolThatFailsAsAnErrorWithItsStderrRunsTheOtherAndExitsNonZero(t *testing.T) {
	run := runScan(t, map[string]string{"kube-bench": `echo 'unable to detect running programs for component "apiserver"' >&2; exit 1`})

	require.Error(t, run.err, "a failed tool fails the Job, so it runs again")
	assert.Equal(t, map[string]any{"tool": "kube-bench", "version": "v0.16.0", "startedAt": "2026-11-07T03:17:00Z",
		"finishedAt": "2026-11-07T03:17:00Z", "profile": "cis-1.10",
		"error": "kube-bench exited 1: unable to detect running programs for component \"apiserver\"\n"}, run.summary(t, "kube-bench"))
	assert.Equal(t, "kubescape", run.summary(t, "kubescape")["tool"])
	assert.NotContains(t, run.summary(t, "kubescape"), "error")
	assert.Equal(t, map[string]any{"finishedAt": "2026-11-07T03:17:00Z", "exitStatus": float64(1),
		"output": "kube-bench: failed; kubescape: ran"}, run.summary(t, "last-run"))
}

func TestSecurityScan_KeepsEachSummaryWithinTheReportsBoundAtAnySize(t *testing.T) {
	bench := `jq -n '{Totals: {total_pass: 0, total_fail: 2000, total_warn: 0, total_info: 0}, Controls: [{tests: [{results:
		[range(2000) | {test_number: "1.\(.)", test_desc: ("d" * 300), status: "FAIL", scored: true}]}]}]}'`
	scape := `while [ $# -gt 0 ]; do if [ "$1" = --output ]; then jq '.summaryDetails.controls |= with_entries(.value.severity = "High")
		| .results = [range(3000) as $i | {resourceID: "r\($i)", controls: [.summaryDetails.controls | keys[] | {controlID: ., status: {status: "failed"}}]}]
		| .resources = [range(3000) as $i | {resourceID: "r\($i)", object: {kind: "Deployment", metadata: {namespace: "app", name: ("w\($i)" * 10)}}}]' \
		testdata/scan/kubescape.json > "$2"; fi; shift; done`
	run := runScan(t, map[string]string{"kube-bench": bench, "kubescape": scape})
	require.NoError(t, run.err, run.out)

	for _, key := range []string{"kube-bench", "kubescape"} {
		assert.LessOrEqual(t, len(run.data[key]), 16384, "%s's summary fits what the report carries", key)
		assert.NotEmpty(t, run.summary(t, key)["failed"], "%s's summary keeps the failures that fit", key)
	}
	bench0 := testutil.Field(t, run.summary(t, "kube-bench"), "failed", "0", "text").(string)
	assert.Len(t, bench0, 160, "a check's text is cut at 160 characters")
	assert.Equal(t, float64(2000), testutil.Field(t, run.summary(t, "kube-bench"), "totals", "fail"),
		"the count stays whole, so the platform sees the list was cut")
}

func TestManifests_RunsTheScanDailyInKubeSystemOnAControlPlaneNodeWithItsHostsProcessesAndConfigReadOnly(t *testing.T) {
	rendered := manifests(t, reportURL)
	job, _, container := cronJob(t, rendered, "mitstack-security-scan")

	assert.Equal(t, "kube-system", testutil.Field(t, job, "metadata", "namespace"), "Talos and Kyverno leave kube-system out of pod security")
	assert.Regexp(t, regexp.MustCompile(`^\d+ \d+ \* \* \*$`), testutil.Field(t, job, "spec", "schedule"), "once a day")
	assert.Equal(t, "Forbid", testutil.Field(t, job, "spec", "concurrencyPolicy"))
	pod := testutil.Field(t, job, "spec", "jobTemplate", "spec", "template", "spec")
	assert.Equal(t, "mitstack-security-scan", testutil.Field(t, pod, "serviceAccountName"))
	assert.Equal(t, true, testutil.Field(t, pod, "hostPID"), "kube-bench reads each component's flags from its process")
	assert.Equal(t, map[string]any{"node-role.kubernetes.io/control-plane": ""}, testutil.Field(t, pod, "nodeSelector"))
	assert.Equal(t, testutil.ToolsImage, testutil.Field(t, container, "image"))
	assert.Equal(t, map[string]any{"runAsUser": 0, "runAsNonRoot": false, "allowPrivilegeEscalation": false,
		"readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []any{"ALL"}}}, testutil.Field(t, container, "securityContext"))
	assert.NotNil(t, testutil.Field(t, container, "resources", "limits", "memory"), "the scan sets its memory limit")
	env := testutil.ContainerEnv(t, container)
	assert.Equal(t, "kube-system/mitstack-security-scans", env["SCAN_CONFIGMAP"])
	assert.Equal(t, "v0.16.0", env["KUBE_BENCH_VERSION"])
	assert.Equal(t, "v2.0.38", env["KUBESCAPE_CONTROLS_VERSION"])
	var hostPaths []any
	for _, v := range testutil.Field(t, pod, "volumes").([]any) {
		if hostPath, ok := v.(map[string]any)["hostPath"]; ok {
			hostPaths = append(hostPaths, testutil.Field(t, hostPath, "path"))
		}
	}
	assert.ElementsMatch(t, []any{"/etc/kubernetes", "/var/lib/kubelet", "/var/lib/etcd"}, hostPaths,
		"the host's Kubernetes config, kubelet state and etcd data, and nothing else of the host")
	for _, m := range testutil.Field(t, container, "volumeMounts").([]any) {
		if testutil.Field(t, m, "mountPath") != "/tmp" {
			assert.Equal(t, true, testutil.Field(t, m, "readOnly"), "mount %v is read-only", m)
		}
	}
}

func TestManifests_LetsTheScanPatchOnlyItsConfigMapAndReadNoSecret(t *testing.T) {
	docs := testutil.Documents(t, manifests(t, reportURL))

	named(t, inNamespace(t, testutil.Kind(docs, "ConfigMap"), "kube-system"), "ConfigMap", "mitstack-security-scans")
	role := named(t, docs, "Role", "mitstack-security-scan")
	assert.Equal(t, []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"configmaps"}, "resourceNames": []any{"mitstack-security-scans"},
		"verbs": []any{"patch"}}}, testutil.Field(t, role, "rules"))
	cluster := named(t, docs, "ClusterRole", "mitstack-security-scan")
	var resources []any
	for _, rule := range testutil.Field(t, cluster, "rules").([]any) {
		assert.Equal(t, []any{"get", "list"}, testutil.Field(t, rule, "verbs"), "the scan only reads: %v", rule)
		resources = append(resources, testutil.Field(t, rule, "resources").([]any)...)
	}
	assert.NotContains(t, resources, "secrets")
	assert.NotContains(t, resources, "*")
	for _, needed := range []any{"pods", "configmaps", "deployments", "clusterrolebindings", "networkpolicies", "serviceaccounts"} {
		assert.Contains(t, resources, needed, "kubescape's NSA controls read %s", needed)
	}
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		binding := named(t, docs, kind, "mitstack-security-scan")
		assert.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "mitstack-security-scan", "namespace": "kube-system"}},
			testutil.Field(t, binding, "subjects"))
	}
}
