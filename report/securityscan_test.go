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
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

const scanAt = "echo 2026-11-07T03:17:00Z"

// kubeBenchReading is a fake kube-bench that, as kube-bench does, takes KUBE_BENCH_VERSION for its --version flag and
// refuses it beside --benchmark.
const kubeBenchReading = `[ -z "${KUBE_BENCH_VERSION:-}" ] || { echo "It is an error to specify both --version and --benchmark flags" >&2; exit 1; }
cat testdata/scan/kube-bench.json`

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
		"kube-bench": kubeBenchReading, "kubescape": kubescapeWriting}
	maps.Copy(all, fakes)
	tools := testutil.NewTools(t, all)
	tools.AddImageJq(t)
	script, env := renderedScript(t, reportURL, "mitstack-security-scan")
	results := "SCAN_RESULTS=" + t.TempDir()
	var benchEnv []string
	for name, value := range testutil.ContainerEnv(t, testutil.Field(t, scanPod(t), "initContainers", "0")) {
		benchEnv = append(benchEnv, name+"="+value)
	}

	benchOut, benchErr := testutil.RunBash(t, script, tools.Env(append(benchEnv, results)...), "bench")
	require.NoError(t, benchErr, "kube-bench's step keeps a failure in its summary, so kubescape still runs: %s", benchOut)
	out, err := testutil.RunBash(t, script, tools.Env(results, "SCAN_CONFIGMAP="+env["SCAN_CONFIGMAP"], "KUBESCAPE_ARTIFACTS=testdata/scan",
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
	assert.Contains(t, calls, "kube-bench run --config-dir "+tools.ScriptDir+"/kube-bench/cfg --benchmark cis-1.10 --targets master,etcd,controlplane,node,policies --json",
		"the benchmark Sidero documents for Talos, since kube-bench's version mapping picks none for Talos")
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

func scanPod(t *testing.T) any {
	t.Helper()
	job, _, _ := cronJob(t, manifests(t, reportURL), "mitstack-security-scan")
	return testutil.Field(t, job, "spec", "jobTemplate", "spec", "template", "spec")
}

// mounts maps each of a container's mount paths to its readOnly, nil when writable.
func mounts(t *testing.T, container any) map[string]any {
	t.Helper()
	m := map[string]any{}
	for _, v := range testutil.Field(t, container, "volumeMounts").([]any) {
		m[testutil.Field(t, v, "mountPath").(string)] = v.(map[string]any)["readOnly"]
	}
	return m
}

func TestManifests_RunsKubeBenchAsRootOnTheHostsConfigWithNoTokenAndKubescapeAsNonRootWithNoHostPathDailyInKubeSystem(t *testing.T) {
	job, _, scan := cronJob(t, manifests(t, reportURL), "mitstack-security-scan")

	assert.Equal(t, "kube-system", testutil.Field(t, job, "metadata", "namespace"), "Talos and Kyverno leave kube-system out of pod security")
	assert.Regexp(t, regexp.MustCompile(`^\d+ \d+ \* \* \*$`), testutil.Field(t, job, "spec", "schedule"), "once a day")
	assert.Equal(t, "Forbid", testutil.Field(t, job, "spec", "concurrencyPolicy"))
	pod := scanPod(t)
	assert.Equal(t, "mitstack-security-scan", testutil.Field(t, pod, "serviceAccountName"))
	assert.Equal(t, true, testutil.Field(t, pod, "hostPID"), "kube-bench reads each component's flags from its process")
	assert.Equal(t, false, testutil.Field(t, pod, "automountServiceAccountToken"), "only the container that calls the API server holds a token")
	assert.Equal(t, map[string]any{"node-role.kubernetes.io/control-plane": ""}, testutil.Field(t, pod, "nodeSelector"))
	bench := testutil.Field(t, pod, "initContainers", "0")
	assert.Equal(t, []any{"bash", tools.ScriptDir + "/security-scan.sh", "bench"}, testutil.Field(t, bench, "command"))
	locked := func(user int, nonRoot bool) map[string]any {
		return map[string]any{"runAsUser": user, "runAsNonRoot": nonRoot, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
			"capabilities": map[string]any{"drop": []any{"ALL"}}}
	}
	assert.Equal(t, locked(0, false), testutil.Field(t, bench, "securityContext"), "kube-bench reads root-owned files on the host")
	assert.Equal(t, locked(65532, true), testutil.Field(t, scan, "securityContext"),
		"kubescape, which reads no host file, cannot reach a root process through the host's process table")
	assert.Equal(t, map[string]any{"/tmp": nil, "/results": nil, "/etc/kubernetes": true, "/var/lib/kubelet": true, "/var/lib/etcd": true},
		mounts(t, bench), "kube-bench alone mounts the host, read-only, and holds no token")
	assert.Equal(t, map[string]any{"/tmp": nil, "/results": nil, "/var/run/secrets/kubernetes.io/serviceaccount": true}, mounts(t, scan))
	for _, c := range []any{bench, scan} {
		assert.Equal(t, testutil.ToolsImage, testutil.Field(t, c, "image"))
		assert.NotNil(t, testutil.Field(t, c, "resources", "limits", "memory"), "each step sets its memory limit")
	}
}

func TestManifests_MountsThreeHostDirectoriesAndTypeChecksOnlyTheTwoTalossKubeletHolds(t *testing.T) {
	typeChecks := map[any]any{}
	for _, v := range testutil.Field(t, scanPod(t), "volumes").([]any) {
		if hostPath, ok := v.(map[string]any)["hostPath"].(map[string]any); ok {
			typeChecks[hostPath["path"]] = hostPath["type"]
		}
	}

	assert.Equal(t, map[any]any{"/etc/kubernetes": "Directory", "/var/lib/kubelet": "Directory", "/var/lib/etcd": nil}, typeChecks,
		"nothing else of the host, and Talos's kubelet checks a type inside its own container, which holds no /var/lib/etcd")
}

func TestManifests_LetsTheScanReadNoSecret(t *testing.T) {
	docs := testutil.Documents(t, manifests(t, reportURL))

	named(t, inNamespace(t, testutil.Kind(docs, "ConfigMap"), "kube-system"), "ConfigMap", "mitstack-security-scans")
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
	assert.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "mitstack-security-scan", "namespace": "kube-system"}},
		testutil.Field(t, named(t, docs, "ClusterRoleBinding", "mitstack-security-scan"), "subjects"))
}
