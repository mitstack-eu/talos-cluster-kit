package report_test

import (
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	reportToken       = "report-token-fixture"
	nodesRead         = "get nodes -o json"
	applicationsRead  = "get applications.argoproj.io -n argocd -o json"
	keySetRead        = "get --raw /openid/v1/jwks"
	cloudNativePGRead = "get clusters.postgresql.cnpg.io,databases.postgresql.cnpg.io,databaseroles.postgresql.cnpg.io -A -o json"
	membersRead       = "-n 10.0.0.6 etcd members"
	volumesRead       = "-n 10.0.0.5,10.0.0.6,10.0.0.7 get volumestatus -o json"
	tokenRequest      = "create token mitstack-node-report -n mitstack-node "
	reportProof       = "report-proof-fixture"
	// failedRead writes 2000 bytes of noise, its message, the report token, a bearer header, a JWT and a NUL to stderr.
	failedRead = `head -c 2000 /dev/zero | tr '\0' x >&2; printf '\nerror: the server is gone\n' >&2
printf 'token ` + reportToken + `\nAuthorization: Bearer abc.def\nsigned eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln\n\0' >&2
exit 3`
)

var subscriptionKinds = []string{"namespace", "serviceaccount", "service", "resourcequota", "limitrange", "deployment.apps",
	"ingress.networking.k8s.io", "networkpolicy.networking.k8s.io", "role.rbac.authorization.k8s.io",
	"rolebinding.rbac.authorization.k8s.io", "application.argoproj.io", "appproject.argoproj.io",
	"externalsecret.external-secrets.io"}

func subscriptionRead(kind string) string {
	return "get " + kind + " -A -l mitstack.dev/subscription -o json"
}

var fakeKubectl, failingCloudNativePGKubectl = kubectlAnswering(nil), kubectlAnswering(map[string]string{
	cloudNativePGRead: `echo 'error: the server does not have a resource type "clusters"' >&2; exit 1`})

var fakeTalosctl = talosctlAnswering("cat testdata/volumes.json")

func talosctlAnswering(volumes string) string {
	return "case \"$*\" in\n\"" + membersRead + "\")\ncat testdata/members.txt\n;;\n\"" + volumesRead + "\")\n" + volumes + "\n;;\n*) exit 1 ;;\nesac\n"
}

func kubectlAnswering(overrides map[string]string) string {
	answers := map[string]string{nodesRead: "cat testdata/nodes.json", applicationsRead: "cat testdata/applications.json",
		keySetRead: "cat testdata/jwks.json", cloudNativePGRead: "cat testdata/cloudnative-pg.json", tokenRequest: "echo " + reportProof}
	for _, kind := range subscriptionKinds {
		answers[subscriptionRead(kind)] = "cat testdata/subscription-resources/" + kind + ".json"
	}
	maps.Copy(answers, overrides)
	script := "case \"$*\" in\n"
	for read, answer := range answers {
		script += "\"" + read + "\"*)\n" + answer + "\n;;\n"
	}
	return script + "*) exit 1 ;;\nesac\n"
}

type postedReport struct {
	mu            sync.Mutex
	posts         int
	authorization string
	proof         string
	origin        string
	raw           []byte
	body          map[string]any
}

func fakePlatform(t *testing.T) (*httptest.Server, *postedReport) {
	t.Helper()
	posted := &postedReport{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted.mu.Lock()
		defer posted.mu.Unlock()
		posted.posts++
		posted.authorization = r.Header.Get("Authorization")
		posted.proof = r.Header.Get("Mitstack-Report-Proof")
		posted.origin = "https://" + r.Host
		data, err := io.ReadAll(r.Body)
		posted.raw = data
		if err == nil {
			err = json.Unmarshal(data, &posted.body)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	return server, posted
}

func writeServerCA(t *testing.T, server *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func renderedScript(t *testing.T, url string) (script, proofScript string) {
	t.Helper()
	rendered, err := report.Manifests(report.Inputs{URL: url, SubscriptionID: "sub_0123456789ab", ToolsImage: testutil.ToolsImage})
	require.NoError(t, err)
	cronJobs := testutil.Kind(testutil.Documents(t, rendered), "CronJob")
	require.Len(t, cronJobs, 1)
	container := testutil.Field(t, cronJobs[0], "spec", "jobTemplate", "spec", "template", "spec", "containers", "0")
	return testutil.Field(t, container, "command", "2").(string), testutil.ContainerEnv(t, container)["REPORT_PROOF_SCRIPT"]
}

func runReport(t *testing.T, fakes map[string]string, env ...string) (*testutil.Tools, *postedReport, string, error) {
	t.Helper()
	server, posted := fakePlatform(t)
	all := map[string]string{"talosctl": fakeTalosctl, "kubectl": fakeKubectl}
	maps.Copy(all, fakes)
	tools := testutil.NewTools(t, all)
	tools.AddImageJq(t)
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte(reportToken), 0o600))

	script, proofScript := renderedScript(t, server.URL)

	out, err := testutil.RunBash(t, script, tools.Env(append([]string{
		"REPORT_URL=" + server.URL, "SUBSCRIPTION_ID=sub_0123456789ab", "HOST_IP=10.0.0.6", "REPORT_PROOF_SCRIPT=" + proofScript,
		"REPORT_TOKEN_FILE=" + token, "CURL_CA_BUNDLE=" + writeServerCA(t, server)}, env...)...))
	posted.mu.Lock()
	defer posted.mu.Unlock()
	return tools, posted, out, err
}

func TestReport_PostsEachNodeWithItsTalosVersionAndAllocatableTheEtcdMembershipAndEachApplicationsHealthAndAppliedObjectsSignedForItsExactBody(t *testing.T) {
	tools, posted, out, err := runReport(t, nil)

	require.NoError(t, err, out)
	assert.Equal(t, "Bearer report-token-fixture", posted.authorization)
	assert.Equal(t, "sub_0123456789ab", posted.body["subscriptionId"])
	assert.Equal(t, []any{
		map[string]any{"name": "poc-talos4-cp1", "ready": true, "talosVersion": "v1.14.4", "kubeletVersion": "v1.37.1", "internalIP": "10.0.0.5",
			"allocatable": map[string]any{"cpu": "3950m", "memory": "7712Mi", "pods": "110"}},
		map[string]any{"name": "poc-talos4-cp2", "ready": true, "talosVersion": "v1.14.4", "kubeletVersion": "v1.37.1", "internalIP": "10.0.0.6",
			"allocatable": map[string]any{"cpu": "4", "memory": "15622080Ki", "pods": "110"}},
		map[string]any{"name": "poc-talos4-cp3", "ready": false, "talosVersion": "v1.14.3", "kubeletVersion": "v1.37.1", "internalIP": "10.0.0.7",
			"allocatable": map[string]any{"cpu": "1950m", "memory": "3615Mi", "pods": "110"}},
	}, posted.body["nodes"])
	assert.Equal(t, map[string]any{"members": []any{
		map[string]any{"id": "25afe91c7e8956f1", "hostname": "poc-talos4-cp2", "peerURLs": []any{"https://10.0.0.6:2380"}, "learner": false},
		map[string]any{"id": "a48f107742a8605c", "hostname": "poc-talos4-cp1", "peerURLs": []any{"https://10.0.0.5:2380"}, "learner": false},
		map[string]any{"id": "1948ccd42e0e52cd", "hostname": "poc-talos4-cp4", "peerURLs": []any{"https://10.0.0.7:2380"}, "learner": true},
	}}, posted.body["etcd"])
	assert.Equal(t, readJSON(t, "testdata", "golden", "argocd.json"), posted.body["argocd"], "a Secret is among the applied objects, each with five fields")
	wantCalls := []string{
		"kubectl get nodes -o json",
		"kubectl get applications.argoproj.io -n argocd -o json",
		"kubectl " + keySetRead,
		"talosctl " + membersRead,
		"talosctl " + volumesRead,
		"kubectl " + cloudNativePGRead,
	}
	for _, kind := range subscriptionKinds {
		wantCalls = append(wantCalls, "kubectl "+subscriptionRead(kind))
	}
	assert.Equal(t, append(wantCalls, "kubectl "+proofRequest(posted)), tools.Calls(t), "the proof is minted once the body is final")
	assert.Equal(t, reportProof, posted.proof, "the post carries the token the API server minted for its exact body")
}

func proofRequest(posted *postedReport) string {
	return fmt.Sprintf("%s--audience %s --audience report-sha256:%x --duration 10m", tokenRequest, posted.origin, sha256.Sum256(posted.raw))
}

func TestReport_LeavesOutEveryApplicationsAppliedObjectsWhenCarryingThemWouldPassThePlatformsLimit(t *testing.T) {
	crowded := `jq '.items[0].status.resources = [range(4000) | {version: "v1", kind: "ConfigMap", namespace: "app-shop", name: "settings-\(.)", status: "Synced"}]' testdata/applications.json`

	_, posted, out, err := runReport(t, map[string]string{"kubectl": kubectlAnswering(map[string]string{applicationsRead: crowded})})

	require.NoError(t, err, out)
	assert.LessOrEqual(t, len(posted.raw), 256<<10, "the body fits the platform's limit")
	assert.Equal(t, readJSON(t, "testdata", "golden", "argocd-without-applied.json"), posted.body["argocd"],
		"no Application carries a list, so no observer reads a left-out object absent")
	assert.Len(t, posted.body["nodes"], 3, "the rest of the report stays whole")
}

func readJSON(t *testing.T, path ...string) any {
	t.Helper()
	var v any
	data, err := os.ReadFile(filepath.Join(path...))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &v))
	return v
}

func TestReport_NamesEachNodesStateAndEphemeralVolumeWithItsEncryption(t *testing.T) {
	_, posted, out, err := runReport(t, nil)

	require.NoError(t, err, out)
	assert.Equal(t, readJSON(t, "testdata", "golden", "system-volumes.json"), posted.body["systemVolumes"])
}

func TestReport_ListsTheObjectsOfEachKindItReadWholeAndLeavesOutAKindWhoseReadFailsOrIsCutShort(t *testing.T) {
	without := func(left string) []string {
		return slices.DeleteFunc(slices.Clone(subscriptionKinds), func(k string) bool { return k == left })
	}
	cases := map[string]struct {
		read, answer string
		wantKinds    []string
	}{
		"every read answers":                 {nodesRead, "cat testdata/nodes.json", subscriptionKinds},
		"the deployment read fails":          {subscriptionRead("deployment.apps"), `echo 'error: forbidden' >&2; exit 1`, without("deployment.apps")},
		"the deployment read prints no list": {subscriptionRead("deployment.apps"), `echo 'not json'`, without("deployment.apps")},
		"the deployment read is cut short by the time limit": {subscriptionRead("deployment.apps"), "sleep 10",
			subscriptionKinds[:slices.Index(subscriptionKinds, "deployment.apps")]},
		"the namespace read holds more objects than one report carries": {subscriptionRead("namespace"),
			`jq -n '{apiVersion: "v1", kind: "List", items: [range(1001) | {metadata: {name: "app-\(.)", labels: {"mitstack.dev/subscription": "sub_slice0123456"}}}]}'`,
			without("namespace")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			kubectl := kubectlAnswering(map[string]string{tc.read: tc.answer})

			_, posted, out, err := runReport(t, map[string]string{"kubectl": kubectl}, "REPORT_READ_SECONDS=1")

			require.NoError(t, err, out)
			assert.Len(t, posted.body["nodes"], 3, "a kind left out still leaves a full report")
			kinds := []any{}
			for _, kind := range tc.wantKinds {
				kinds = append(kinds, kind)
			}
			assert.Equal(t, kinds, posted.body["subscriptionResourceKinds"])
			listed := slices.DeleteFunc(readJSON(t, "testdata", "golden", "subscription-resources.json").([]any),
				func(o any) bool { return !slices.Contains(kinds, o.(map[string]any)["kind"]) })
			assert.Equal(t, listed, posted.body["subscriptionResources"], "the list holds exactly the objects of the kinds it names")
			assert.NotContains(t, posted.body, "subscriptionResourcesComplete", "no whole-list flag lets an observer read an unread kind absent")
			madeAt := fmt.Sprint(posted.body["madeAt"])
			_, err = time.Parse("2006-01-02T15:04:05Z", madeAt)
			require.NoError(t, err, "madeAt, when the reads began, in reportedAt's UTC format")
			assert.LessOrEqual(t, madeAt, posted.body["reportedAt"], "the reads begin no later than the report is made")
		})
	}
}

func TestReport_CarriesTheKeySetTheAPIServerSignsServiceAccountTokensWithUnderJwks(t *testing.T) {
	_, posted, out, err := runReport(t, nil)

	require.NoError(t, err, out)
	assert.Equal(t, readJSON(t, "testdata", "jwks.json"), posted.body["jwks"], "the report's jwks member is the API server's key set as read")
}

func TestReport_ListsCloudNativePGsClustersDatabasesAndRoles(t *testing.T) {
	_, posted, out, err := runReport(t, nil)

	require.NoError(t, err, out)
	assert.Equal(t, []any{
		map[string]any{"namespace": "app-pg", "kind": "Cluster", "name": "shared", "subscription": "sub_pg0123456789",
			"phase": "Cluster in healthy state", "instances": float64(2), "readyInstances": float64(2)},
		map[string]any{"namespace": "app-pg", "kind": "Database", "name": "shop", "subscription": "sub_shop01234567",
			"applied": true, "message": nil},
		map[string]any{"namespace": "app-pg", "kind": "DatabaseRole", "name": "shop", "subscription": nil,
			"applied": false, "message": `role "shop" cannot be created: the cluster is not ready`},
	}, posted.body["cloudNativePG"])
}

func TestReport_DeclaresItsNodeReportContractAndCarriesNoneOfTheVerdictsChecks(t *testing.T) {
	_, posted, out, err := runReport(t, nil)

	require.NoError(t, err, out)
	assert.Equal(t, map[string]any{"contract": "node-report/v1", "names": []any{}}, posted.body["checks"])
}

func TestReport_NamesAFailedVolumeOrCloudNativePGReadInlineWithItsExitStatusAsANumberInAFullReport(t *testing.T) {
	notVolumes := map[string]any{"exitStatus": float64(0), "stderr": "talosctl exited 0, and its output is not the volume statuses this report reads"}
	cases := map[string]struct {
		fakes map[string]string
		key   string
		want  any
	}{
		"the CloudNativePG read exits 1": {map[string]string{"kubectl": failingCloudNativePGKubectl}, "cloudNativePG",
			map[string]any{"exitStatus": float64(1), "stderr": "error: the server does not have a resource type \"clusters\"\n"}},
		"the CloudNativePG read times out": {map[string]string{"kubectl": kubectlAnswering(map[string]string{cloudNativePGRead: "sleep 10"})},
			"cloudNativePG", map[string]any{"exitStatus": float64(124), "stderr": ""}},
		"the volume read exits 1": {map[string]string{"talosctl": talosctlAnswering(`echo 'rpc error: 10.0.0.7 is unreachable' >&2; exit 1`)},
			"systemVolumes", map[string]any{"exitStatus": float64(1), "stderr": "rpc error: 10.0.0.7 is unreachable\n"}},
		"the volume read times out": {map[string]string{"talosctl": talosctlAnswering("sleep 10")}, "systemVolumes",
			map[string]any{"exitStatus": float64(124), "stderr": ""}},
		"the volume read prints no JSON":   {map[string]string{"talosctl": talosctlAnswering("echo 'not json'")}, "systemVolumes", notVolumes},
		"the volume read prints no volume": {map[string]string{"talosctl": talosctlAnswering("true")}, "systemVolumes", notVolumes},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, tc.fakes, "REPORT_READ_SECONDS=1")

			require.NoError(t, err, out)
			assert.Len(t, posted.body["nodes"], 3)
			assert.Len(t, testutil.Field(t, posted.body, "argocd", "applications"), 3)
			assert.Equal(t, map[string]any{"contract": "node-report/v1", "names": []any{}}, posted.body["checks"])
			assert.Equal(t, tc.want, posted.body[tc.key])
		})
	}
}

func TestReport_ThatCannotReadPostsOneReportNamingTheReadAndExitsNonZero(t *testing.T) {
	kubectlFailing := func(read, answer string) map[string]string {
		return map[string]string{"kubectl": kubectlAnswering(map[string]string{read: answer})}
	}
	cases := map[string]struct {
		fakes map[string]string
		want  map[string]any
	}{
		"the nodes read fails":        {kubectlFailing(nodesRead, failedRead), map[string]any{"read": "kubectl get nodes", "exitStatus": float64(3)}},
		"the applications read fails": {kubectlFailing(applicationsRead, failedRead), map[string]any{"read": "kubectl get applications", "exitStatus": float64(3)}},
		"the key set read fails":      {kubectlFailing(keySetRead, failedRead), map[string]any{"read": "kubectl get --raw /openid/v1/jwks", "exitStatus": float64(3)}},
		"the etcd members read fails": {map[string]string{"talosctl": failedRead}, map[string]any{"read": "talosctl etcd members", "exitStatus": float64(3)}},
		"the key set read times out":  {kubectlFailing(keySetRead, "sleep 10"), map[string]any{"read": "kubectl get --raw /openid/v1/jwks", "timedOut": true}},
		"jq cannot build the body":    {kubectlFailing(nodesRead, "echo 'not json'"), map[string]any{"read": "body", "exitStatus": float64(2)}},
		"the token request fails":     {kubectlFailing(tokenRequest, failedRead), map[string]any{"read": "kubectl create token", "exitStatus": float64(3)}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, tc.fakes, "REPORT_READ_SECONDS=1")

			require.Error(t, err, "the report exits non-zero once it has posted the error")
			require.Equal(t, 1, posted.posts, out)
			assert.Equal(t, "Bearer "+reportToken, posted.authorization)
			assert.Empty(t, posted.proof, "an error report proves nothing, so it goes unsigned")
			assert.Equal(t, []string{"error", "reportedAt", "subscriptionId"}, slices.Sorted(maps.Keys(posted.body)))
			stderr := testutil.Field(t, posted.body, "error", "stderr").(string)
			delete(posted.body["error"].(map[string]any), "stderr")
			assert.Equal(t, tc.want, posted.body["error"])
			assert.NotRegexp(t, reportToken+`|Bearer|eyJ|\\u0000`, string(posted.raw), "no token or NUL reaches the platform")
			if tc.want["exitStatus"] == float64(3) {
				assert.Len(t, stderr, 1024, "the error carries the last 1 KiB of the read's stderr")
				assert.True(t, strings.HasSuffix(stderr, "error: the server is gone\n"), "stderr ends with the read's message: %q", stderr)
			}
		})
	}
}

func TestReport_ThatReadsEverythingPostsAHealthyBodyByteForByte(t *testing.T) {
	_, posted, out, err := runReport(t, map[string]string{"date": "echo 2026-10-23T15:45:00Z"})

	require.NoError(t, err, out)
	testutil.Golden(t, filepath.Join("testdata", "golden", "healthy-body.json"), posted.raw)
}

func TestManifests_LetsTheReportListEachKindItReadsAndCloudNativePGsObjectsAndMintATokenForItsOwnServiceAccountAloneAndGrantsNoVerbOnSecrets(t *testing.T) {
	rendered, err := report.Manifests(report.Inputs{URL: "https://api.mitstack.dev/v1/cluster-reports", SubscriptionID: "sub_0123456789ab", ToolsImage: testutil.ToolsImage})
	require.NoError(t, err)

	docs := testutil.Documents(t, rendered)

	roles, tokenRoles, bindings := testutil.Kind(docs, "ClusterRole"), testutil.Kind(docs, "Role"), testutil.Kind(docs, "RoleBinding")

	require.Len(t, tokenRoles, 1)
	require.Len(t, bindings, 1)
	assert.Equal(t, "mitstack-node", testutil.Field(t, tokenRoles[0], "metadata", "namespace"))
	assert.Equal(t, []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"serviceaccounts/token"},
		"resourceNames": []any{"mitstack-node-report"}, "verbs": []any{"create"}}}, testutil.Field(t, tokenRoles[0], "rules"))
	assert.Equal(t, testutil.Field(t, tokenRoles[0], "metadata", "name"), testutil.Field(t, bindings[0], "roleRef", "name"))
	assert.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "mitstack-node-report", "namespace": "mitstack-node"}},
		testutil.Field(t, bindings[0], "subjects"))
	require.Len(t, roles, 1)
	rules := testutil.Field(t, roles[0], "rules").([]any)
	assert.ElementsMatch(t, []any{
		map[string]any{"apiGroups": []any{""}, "resources": []any{"nodes"}, "verbs": []any{"get", "list"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"namespaces", "serviceaccounts", "services", "resourcequotas", "limitranges"},
			"verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"apps"}, "resources": []any{"deployments"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"networking.k8s.io"}, "resources": []any{"ingresses", "networkpolicies"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"rbac.authorization.k8s.io"}, "resources": []any{"roles", "rolebindings"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"argoproj.io"}, "resources": []any{"applications"}, "verbs": []any{"get", "list"}},
		map[string]any{"apiGroups": []any{"argoproj.io"}, "resources": []any{"appprojects"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"external-secrets.io"}, "resources": []any{"externalsecrets"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"postgresql.cnpg.io"}, "resources": []any{"clusters", "databases", "databaseroles"},
			"verbs": []any{"get", "list"}},
	}, rules)
	for _, rule := range rules {
		resources := testutil.Field(t, rule, "resources").([]any)
		assert.NotContains(t, resources, "secrets", "rule %v grants a verb on secrets", rule)
		assert.NotContains(t, resources, "*", "rule %v grants a verb on every resource, secrets among them", rule)
	}
}

func TestManifests_RefusesAReportURLThatIsNotHTTPS(t *testing.T) {
	_, err := report.Manifests(report.Inputs{URL: "http://api.mitstack.dev/v1/cluster-reports", SubscriptionID: "sub_0123456789ab", ToolsImage: testutil.ToolsImage})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "http://api.mitstack.dev/v1/cluster-reports")
}
