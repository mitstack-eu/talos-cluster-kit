package report_test

// sources_test.go pins the credentials, admission and scan members the report sends, and what lets it read each.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	probeNamespace = "mitstack-admission-probe"
	sourceFailed   = "echo 'error: the server is gone' >&2; exit 3"
)

var fixtureRules = []string{"mitstack-disallow-host-path", "mitstack-require-app-label", "mitstack-require-image-digest",
	"mitstack-require-resource-limits", "mitstack-restrict-image-registries", "pod-security-restricted"}

func reportSubject(t *testing.T, binding map[string]any) bool {
	t.Helper()
	subjects, _ := testutil.Field(t, binding, "subjects").([]any)
	return slices.ContainsFunc(subjects, func(s any) bool {
		return testutil.Field(t, s, "kind") == "ServiceAccount" && testutil.Field(t, s, "name") == "mitstack-node-report"
	})
}

// reportRules are the rules of every Role and ClusterRole a binding gives the report's service account, by namespace.
func reportRules(t *testing.T) map[string][]any {
	t.Helper()
	docs := testutil.Documents(t, manifests(t, reportURL))
	rules := map[string][]any{}
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		for _, binding := range testutil.Kind(docs, kind) {
			if !reportSubject(t, binding) {
				continue
			}
			roleKind, roleName := testutil.Field(t, binding, "roleRef", "kind").(string), testutil.Field(t, binding, "roleRef", "name").(string)
			namespace, _ := testutil.Field(t, binding, "metadata", "namespace").(string)
			role := named(t, docs, roleKind, roleName)
			if roleKind == "Role" {
				require.Equal(t, namespace, testutil.Field(t, role, "metadata", "namespace"), "binding %s names a Role of its own namespace", roleName)
			}
			rules[namespace] = append(rules[namespace], testutil.Field(t, role, "rules").([]any)...)
		}
	}
	return rules
}

func TestManifests_NoRoleBoundToTheReportGrantsAVerbOnSecretsOrOnEveryResource(t *testing.T) {
	for namespace, rules := range reportRules(t) {
		for _, rule := range rules {
			resources := testutil.Field(t, rule, "resources").([]any)
			assert.NotContains(t, resources, "secrets", "a rule in %q grants a verb on Secrets: %v", namespace, rule)
			assert.NotContains(t, resources, "*", "a rule in %q grants a verb on every resource: %v", namespace, rule)
		}
	}
}

func TestManifests_LetsTheReportListSealedSecretsInArgoCDReadTheScansByNameAndDryRunFixturesInTheProbeNamespaceAlone(t *testing.T) {
	rules := reportRules(t)

	assert.Equal(t, []any{map[string]any{"apiGroups": []any{"bitnami.com"}, "resources": []any{"sealedsecrets"}, "verbs": []any{"list"}}},
		rules["argocd"], "the sealed credentials carry ciphertext only, so listing them reads no credential")
	assert.ElementsMatch(t, []any{
		map[string]any{"apiGroups": []any{""}, "resources": []any{"configmaps"}, "resourceNames": []any{"mitstack-mesh-probe"}, "verbs": []any{"get"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"configmaps"}, "resourceNames": []any{"mitstack-security-scans"}, "verbs": []any{"get"}},
		map[string]any{"apiGroups": []any{"batch"}, "resources": []any{"cronjobs"}, "resourceNames": []any{"mitstack-security-scan"}, "verbs": []any{"get"}},
	}, rules["kube-system"])
	assert.ElementsMatch(t, []any{
		map[string]any{"apiGroups": []any{""}, "resources": []any{"configmaps"}, "resourceNames": []any{"admission-fixtures"}, "verbs": []any{"get"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"create"}},
		map[string]any{"apiGroups": []any{"apps"}, "resources": []any{"deployments"}, "verbs": []any{"create"}},
	}, rules[probeNamespace])
}

func TestManifests_TheProbeNamespaceEnforcesPodSecurityRestrictedAndHoldsEachRulesFixtureUnderTheRulesName(t *testing.T) {
	docs := testutil.Documents(t, manifests(t, reportURL))

	namespace := named(t, docs, "Namespace", probeNamespace)
	assert.Equal(t, map[string]string{"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "latest"},
		stringMap(testutil.Field(t, namespace, "metadata", "labels")))
	fixtures := inNamespace(t, testutil.Kind(docs, "ConfigMap"), probeNamespace)
	require.Len(t, fixtures, 1)
	assert.Equal(t, "admission-fixtures", testutil.Field(t, fixtures[0], "metadata", "name"))
	data := stringMap(testutil.Field(t, fixtures[0], "data"))
	assert.ElementsMatch(t, fixtureRules, slices.Collect(func(yield func(string) bool) {
		for key := range data {
			if !yield(key) {
				return
			}
		}
	}))
	for _, rule := range fixtureRules {
		raw, err := os.ReadFile(filepath.Join("admission", rule+".yaml"))
		require.NoError(t, err)
		want, err := json.Marshal(testutil.Documents(t, raw)[0])
		require.NoError(t, err)
		assert.JSONEq(t, string(want), data[rule], "the ConfigMap holds %s's fixture as JSON", rule)
		assert.NotContains(t, data[rule], "\n", "one line of JSON holds no image line the gateway would mirror")
	}
}

// fixturePod is the pod a fixture's admission is judged on: the Pod itself, or a Deployment's pod template.
func fixturePod(t *testing.T, rule string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("admission", rule+".yaml"))
	require.NoError(t, err)
	fixture := testutil.Documents(t, raw)[0]
	assert.Equal(t, probeNamespace, testutil.Field(t, fixture, "metadata", "namespace"), "%s's fixture names the probe namespace", rule)
	if fixture["kind"] == "Pod" {
		return fixture
	}
	return map[string]any{"metadata": testutil.Field(t, fixture, "spec", "template", "metadata"),
		"spec": testutil.Field(t, fixture, "spec", "template", "spec")}
}

func TestAdmissionFixtures_EachBreaksNoTalosPolicyButItsOwnRule(t *testing.T) {
	namespace := map[string]any{"metadata": map[string]any{"name": probeNamespace}}
	for _, path := range []string{"mitstack-require-app-label", "mitstack-require-mesh"} {
		raw, err := os.ReadFile(filepath.Join("..", "components", "admission", "policies", path+".yaml"))
		require.NoError(t, err)
		policy := testutil.Documents(t, raw)[0]
		for _, rule := range fixtureRules {
			assert.Equal(t, rule != path, testutil.Admits(t, policy, fixturePod(t, rule), namespace),
				"policy %s on the fixture for %s", path, rule)
		}
	}
}

func TestManifests_RefusesEveryRequestInTheProbeNamespaceThatIsNotADryRun(t *testing.T) {
	docs := testutil.Documents(t, manifests(t, reportURL))

	policy := named(t, docs, "ValidatingAdmissionPolicy", "mitstack-admission-probe-dry-run-only")
	binding := named(t, docs, "ValidatingAdmissionPolicyBinding", "mitstack-admission-probe-dry-run-only")
	assert.Equal(t, "mitstack-admission-probe-dry-run-only", testutil.Field(t, binding, "spec", "policyName"))
	assert.Equal(t, []any{"Deny"}, testutil.Field(t, binding, "spec", "validationActions"))
	assert.Equal(t, map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": probeNamespace}},
		testutil.Field(t, binding, "spec", "matchResources", "namespaceSelector"))
	assert.ElementsMatch(t, []any{
		map[string]any{"apiGroups": []any{""}, "apiVersions": []any{"*"}, "operations": []any{"*"}, "resources": []any{"pods"}},
		map[string]any{"apiGroups": []any{"apps"}, "apiVersions": []any{"*"}, "operations": []any{"*"}, "resources": []any{"deployments"}},
	}, testutil.Field(t, policy, "spec", "matchConstraints", "resourceRules"))
	validations := testutil.Field(t, policy, "spec", "validations").([]any)
	require.Len(t, validations, 1)
	env, err := cel.NewEnv(cel.Variable("request", cel.DynType))
	require.NoError(t, err)
	ast, issues := env.Compile(testutil.Field(t, validations[0], "expression").(string))
	require.NoError(t, issues.Err())
	program, err := env.Program(ast)
	require.NoError(t, err)
	for dryRun, admitted := range map[bool]bool{true: true, false: false} {
		out, _, err := program.Eval(map[string]any{"request": map[string]any{"dryRun": dryRun}})
		require.NoError(t, err)
		assert.Equal(t, admitted, out.Value(), "a request with dryRun %v", dryRun)
	}
}

func TestReport_SendsNoSealedCiphertextAndDryRunsEachFixtureWithTheServerInTheProbeNamespace(t *testing.T) {
	tools, posted, out, err := runReport(t, map[string]string{"date": reportAt})
	require.NoError(t, err, out)

	assert.NotContains(t, string(posted.raw), "CIPHERTEXT", "no encrypted credential leaves the cluster")
	var dryRuns []string
	for _, call := range tools.Calls(t) {
		if rest, ok := strings.CutPrefix(call, "kubectl "+dryRun); ok {
			dryRuns = append(dryRuns, rest)
		}
	}
	assert.Len(t, dryRuns, 3, "one server dry run for each fixture named for a rule, and none for Not_A_Rule")
	for _, path := range dryRuns {
		assert.NotContains(t, path, " ", "each dry run reads one fixture file")
	}
}

func TestReport_ClipsEachFixtureTo4KiBAndEachAnswerTo2KiB(t *testing.T) {
	fakes := map[string]string{"kubectl": kubectlAnswering(map[string]string{
		fixturesRead: `jq -n '{data: {"mitstack-require-app-label": ("refused-require-app-label " * 400)}}'`,
		dryRun:       `head -c 5000 /dev/zero | tr '\0' m >&2; exit 1`})}
	_, posted, out, err := runReport(t, fakes)
	require.NoError(t, err, out)

	admission, ok := testutil.Field(t, posted.body, "admission").([]any)
	require.True(t, ok, "admission is a list of tried fixtures: %v", testutil.Field(t, posted.body, "admission"))
	require.Len(t, admission, 1)
	assert.Len(t, testutil.Field(t, admission[0], "fixture"), 4096)
	assert.Len(t, testutil.Field(t, admission[0], "message"), 2048)
	assert.Equal(t, float64(1), testutil.Field(t, admission[0], "exitStatus"))
}

func TestReport_NamesASourceReadThatFailsInsideItsMemberInAFullReport(t *testing.T) {
	failed := map[string]any{"exitStatus": float64(3)}
	cases := map[string]struct {
		read, member string
		want         map[string]any
	}{
		"the sealed credentials read fails": {sealedSecretsRead, "repositorySecrets", failed},
		"the AppProjects read fails":        {appProjectsRead, "appProjects", failed},
		"the fixtures read fails":           {fixturesRead, "admission", failed},
		"the scan CronJob read fails":       {scanCronJobRead, "securityScans.runner", failed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, kubectlWith(tc.read, sourceFailed))
			require.NoError(t, err, out)

			got, ok := testutil.Field(t, posted.body, strings.Split(tc.member, ".")...).(map[string]any)
			require.True(t, ok, "the report carries %s as an object", tc.member)
			stderr, _ := got["stderr"].(string)
			if stderr == "" {
				stderr, _ = got["output"].(string)
			}
			assert.True(t, strings.HasSuffix(stderr, "error: the server is gone\n"), "the member carries the read's stderr: %q", stderr)
			assert.Equal(t, tc.want["exitStatus"], got["exitStatus"])
			assert.NotRegexp(t, reportToken+`|Bearer|eyJ`, string(posted.raw))
		})
	}
}

func TestReport_SendsTheScansAsUnreadableWhenTheirConfigMapCannotBeRead(t *testing.T) {
	_, posted, out, err := runReport(t, kubectlWith(scansRead, sourceFailed))
	require.NoError(t, err, out)

	reason, ok := testutil.Field(t, posted.body, "securityScans", "unreadable").(string)
	require.True(t, ok, "the report carries securityScans as unreadable: %v", testutil.Field(t, posted.body, "securityScans"))
	assert.True(t, strings.HasPrefix(reason, "reading the ConfigMap kube-system/mitstack-security-scans ended with exit status 3: "), reason)
	assert.True(t, strings.HasSuffix(reason, "error: the server is gone\n"), reason)
}

func TestReport_KeepsAScanSummaryOnlyAsOneJSONObjectWithin16KiB(t *testing.T) {
	scans := func(edit string) map[string]string {
		return kubectlWith(scansRead, "jq '"+edit+"' testdata/security-scans.json")
	}
	cases := map[string]struct {
		fakes map[string]string
		want  any
	}{
		"no kube-bench run has finished": {scans(`del(.data["kube-bench"])`), nil},
		"the summary is no object":       {scans(`.data["kube-bench"] = "[1]"`), map[string]any{"unreadable": "the kept kube-bench summary is not one JSON object"}},
		"the summary is no JSON":         {scans(`.data["kube-bench"] = "{"`), map[string]any{"unreadable": "the kept kube-bench summary is not one JSON object"}},
		"the summary is over its bound": {scans(`.data["kube-bench"] = ({failed: ("x" * 16400)} | tojson)`),
			map[string]any{"unreadable": "the kept kube-bench summary is 16413 bytes, over the 16384 the report carries"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, tc.fakes)
			require.NoError(t, err, out)

			assert.Equal(t, tc.want, testutil.Field(t, posted.body, "securityScans", "kubeBench"))
			assert.NotNil(t, testutil.Field(t, posted.body, "securityScans", "kubescape"), "the other summary is kept")
		})
	}
}
