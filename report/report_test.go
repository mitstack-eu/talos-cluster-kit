package report_test

import (
	"encoding/json"
	"encoding/pem"
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
	// failedRead writes 2000 bytes of noise, its message, the report token, a bearer header, a JWT and a NUL to stderr.
	failedRead = `head -c 2000 /dev/zero | tr '\0' x >&2; printf '\nerror: the server is gone\n' >&2
printf 'token ` + reportToken + `\nAuthorization: Bearer abc.def\nsigned eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln\n\0' >&2
exit 3`
)

// subscriptionKinds are the kinds whose labelled objects the report lists, each as kubectl --show-kind names it, in the
// order it reads them.
var subscriptionKinds = []string{"namespace", "serviceaccount", "service", "resourcequota", "limitrange", "deployment.apps",
	"ingress.networking.k8s.io", "networkpolicy.networking.k8s.io", "role.rbac.authorization.k8s.io",
	"rolebinding.rbac.authorization.k8s.io", "application.argoproj.io", "appproject.argoproj.io",
	"externalsecret.external-secrets.io"}

func subscriptionRead(kind string) string {
	return "get " + kind + " -A -l mitstack.dev/subscription -o json"
}

// fakeKubectl answers the report's reads from the fixture cluster in testdata/, and failingCloudNativePGKubectl
// refuses only the CloudNativePG read, as a cluster without its CRDs would.
var fakeKubectl, failingCloudNativePGKubectl = kubectlAnswering(nil), kubectlAnswering(map[string]string{
	cloudNativePGRead: `echo 'error: the server does not have a resource type "clusters"' >&2; exit 1`})

// fakeTalosctl answers the etcd members and every node's volume statuses from testdata/.
var fakeTalosctl = talosctlAnswering("cat testdata/volumes.json")

func talosctlAnswering(volumes string) string {
	return "case \"$*\" in\n\"" + membersRead + "\")\ncat testdata/members.txt\n;;\n\"" + volumesRead + "\")\n" + volumes + "\n;;\n*) exit 1 ;;\nesac\n"
}

// kubectlAnswering answers each kubectl read from the fixture cluster, except those overrides replaces.
func kubectlAnswering(overrides map[string]string) string {
	answers := map[string]string{nodesRead: "cat testdata/nodes.json", applicationsRead: "cat testdata/applications.json",
		keySetRead: "cat testdata/jwks.json", cloudNativePGRead: "cat testdata/cloudnative-pg.json"}
	for _, kind := range subscriptionKinds {
		fixture := filepath.Join("testdata", "subscription-resources", kind+".json")
		answers[subscriptionRead(kind)] = `echo '{"apiVersion": "v1", "kind": "List", "items": []}'`
		if _, err := os.Stat(fixture); err == nil {
			answers[subscriptionRead(kind)] = "cat " + fixture
		}
	}
	maps.Copy(answers, overrides)
	script := "case \"$*\" in\n"
	for read, answer := range answers {
		script += "\"" + read + "\")\n" + answer + "\n;;\n"
	}
	return script + "*) exit 1 ;;\nesac\n"
}

// postedReport is what the fake platform last received, and how many reports it took.
type postedReport struct {
	mu            sync.Mutex
	posts         int
	authorization string
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

func renderedScript(t *testing.T, url string) string {
	t.Helper()
	rendered, err := report.Manifests(report.Inputs{URL: url, SubscriptionID: "sub_0123456789ab", ToolsImage: testutil.ToolsImage})
	require.NoError(t, err)
	cronJobs := testutil.Kind(testutil.Documents(t, rendered), "CronJob")
	require.Len(t, cronJobs, 1)
	return testutil.Field(t, cronJobs[0], "spec", "jobTemplate", "spec", "template", "spec", "containers", "0", "command", "2").(string)
}

// runReport runs the rendered report with the tools image's jq against the fixture fakes, each of fakes replacing its
// namesake, with env added, and returns what the fake platform received.
func runReport(t *testing.T, fakes map[string]string, env ...string) (*testutil.Tools, *postedReport, string, error) {
	t.Helper()
	server, posted := fakePlatform(t)
	all := map[string]string{"talosctl": fakeTalosctl, "kubectl": fakeKubectl}
	maps.Copy(all, fakes)
	tools := testutil.NewTools(t, all)
	tools.AddImageJq(t)
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte(reportToken), 0o600))

	out, err := testutil.RunBash(t, renderedScript(t, server.URL), tools.Env(append([]string{
		"REPORT_URL=" + server.URL, "SUBSCRIPTION_ID=sub_0123456789ab", "HOST_IP=10.0.0.6",
		"REPORT_TOKEN_FILE=" + token, "CURL_CA_BUNDLE=" + writeServerCA(t, server)}, env...)...))
	posted.mu.Lock()
	defer posted.mu.Unlock()
	return tools, posted, out, err
}

func TestReport_PostsEachNodeWithItsTalosVersionTheEtcdMembershipAndArgoCDsHealth(t *testing.T) {
	tools, posted, out, err := runReport(t, nil)

	require.NoError(t, err, out)
	assert.Equal(t, "Bearer report-token-fixture", posted.authorization)
	assert.Equal(t, "sub_0123456789ab", posted.body["subscriptionId"])
	assert.Equal(t, []any{
		map[string]any{"name": "poc-talos4-cp1", "ready": true, "talosVersion": "v1.14.4", "kubeletVersion": "v1.37.1", "internalIP": "10.0.0.5"},
		map[string]any{"name": "poc-talos4-cp2", "ready": true, "talosVersion": "v1.14.4", "kubeletVersion": "v1.37.1", "internalIP": "10.0.0.6"},
		map[string]any{"name": "poc-talos4-cp3", "ready": false, "talosVersion": "v1.14.3", "kubeletVersion": "v1.37.1", "internalIP": "10.0.0.7"},
	}, posted.body["nodes"])
	assert.Equal(t, map[string]any{"members": []any{
		map[string]any{"id": "25afe91c7e8956f1", "hostname": "poc-talos4-cp2", "peerURLs": []any{"https://10.0.0.6:2380"}, "learner": false},
		map[string]any{"id": "a48f107742a8605c", "hostname": "poc-talos4-cp1", "peerURLs": []any{"https://10.0.0.5:2380"}, "learner": false},
		map[string]any{"id": "1948ccd42e0e52cd", "hostname": "poc-talos4-cp4", "peerURLs": []any{"https://10.0.0.7:2380"}, "learner": true},
	}}, posted.body["etcd"])
	assert.Equal(t, map[string]any{"applications": []any{
		map[string]any{"name": "cluster-root", "health": "Healthy", "sync": "Synced"},
		map[string]any{"name": "shop", "health": "Degraded", "sync": "OutOfSync"},
	}}, posted.body["argocd"])
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
	assert.Equal(t, wantCalls, tools.Calls(t))
}

func TestReport_NamesEachNodesStateAndEphemeralVolumeWithItsEncryption(t *testing.T) {
	volume := func(node, name, provider string, keys []any, lockedToState bool) map[string]any {
		return map[string]any{"node": node, "name": name, "phase": "ready", "encryptionProvider": provider,
			"encryptionKeys": keys, "lockedToState": lockedToState}
	}
	nodeID := []any{"nodeID"}

	_, posted, out, err := runReport(t, nil)

	require.NoError(t, err, out)
	assert.Equal(t, []any{
		volume("10.0.0.5", "STATE", "luks2", nodeID, false),
		volume("10.0.0.5", "EPHEMERAL", "luks2", nodeID, true),
		volume("10.0.0.6", "STATE", "luks2", nodeID, false),
		volume("10.0.0.6", "EPHEMERAL", "luks2", nodeID, true),
		volume("10.0.0.7", "STATE", "none", []any{}, false),
		volume("10.0.0.7", "EPHEMERAL", "none", []any{}, false),
	}, posted.body["systemVolumes"])
}

func TestReport_WhoseVolumeReadFailsStillCarriesItsNodesAndNamesTheFailure(t *testing.T) {
	cases := map[string]struct {
		answer string
		want   map[string]any
	}{
		"the read exits 1": {`echo 'rpc error: code = Unavailable desc = 10.0.0.7 is unreachable' >&2; exit 1`,
			map[string]any{"exitStatus": float64(1), "stderr": "rpc error: code = Unavailable desc = 10.0.0.7 is unreachable\n"}},
		"the read times out": {"sleep 10", map[string]any{"exitStatus": float64(124), "stderr": ""}},
		"the read prints no volume statuses": {"echo 'not json'",
			map[string]any{"exitStatus": float64(0), "stderr": "talosctl exited 0, and its output is not the volume statuses this report reads"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, map[string]string{"talosctl": talosctlAnswering(tc.answer)}, "REPORT_READ_SECONDS=1")

			require.NoError(t, err, out)
			assert.NotContains(t, posted.body, "error", "a failed volume read still leaves a full report")
			assert.Len(t, posted.body["nodes"], 3)
			assert.Equal(t, tc.want, posted.body["systemVolumes"])
		})
	}
}

// fixtureObjects are the labelled objects of testdata/subscription-resources, as the report lists them.
var fixtureObjects = map[string]any{
	"namespace": map[string]any{"namespace": nil, "kind": "namespace", "name": "app-shop", "subscription": "sub_slice0123456"},
	"deployment.apps": map[string]any{"namespace": "app-shop", "kind": "deployment.apps", "name": "shop",
		"subscription": "sub_deploy012345"},
	"appproject.argoproj.io": map[string]any{"namespace": "argocd", "kind": "appproject.argoproj.io", "name": "app-shop",
		"subscription": "sub_slice0123456"},
	"externalsecret.external-secrets.io": map[string]any{"namespace": "argocd", "kind": "externalsecret.external-secrets.io",
		"name": "repo-app-shop", "subscription": "sub_slice0123456"},
}

// listedOf returns the fixture objects of kinds, in the order the report reads their kinds.
func listedOf(kinds []string) []any {
	listed := []any{}
	for _, kind := range kinds {
		if object, ok := fixtureObjects[kind]; ok {
			listed = append(listed, object)
		}
	}
	return listed
}

func without(kinds []string, left string) []string {
	return slices.DeleteFunc(slices.Clone(kinds), func(kind string) bool { return kind == left })
}

func asAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func TestReport_ListsTheSubscriptionsLabelledObjectsAndNamesEachKindItReadWhole(t *testing.T) {
	_, posted, out, err := runReport(t, nil)

	require.NoError(t, err, out)
	assert.Equal(t, listedOf(subscriptionKinds), posted.body["subscriptionResources"])
	assert.Equal(t, asAny(subscriptionKinds), posted.body["subscriptionResourceKinds"])
	assert.NotContains(t, posted.body, "subscriptionResourcesComplete", "no whole-list flag lets an observer read an unread kind absent")
	madeAtText, ok := posted.body["madeAt"].(string)
	require.True(t, ok, "madeAt = %v; want the time the reads began", posted.body["madeAt"])
	madeAt, err := time.Parse(time.RFC3339, madeAtText)
	require.NoError(t, err)
	reportedAt, err := time.Parse(time.RFC3339, posted.body["reportedAt"].(string))
	require.NoError(t, err)
	assert.False(t, madeAt.After(reportedAt), "madeAt %s, when the reads began, is after reportedAt %s", madeAt, reportedAt)
}

func TestReport_LeavesOutOfSubscriptionResourceKindsAKindWhoseReadFailsOrIsCutShort(t *testing.T) {
	deployments := subscriptionRead("deployment.apps")
	beforeDeployments := subscriptionKinds[:slices.Index(subscriptionKinds, "deployment.apps")]
	cases := map[string]struct {
		read, answer string
		wantKinds    []string
	}{
		"the deployment read fails": {deployments, `echo 'error: deployments.apps is forbidden' >&2; exit 1`,
			without(subscriptionKinds, "deployment.apps")},
		"the deployment read prints no list":                 {deployments, `echo 'not json'`, without(subscriptionKinds, "deployment.apps")},
		"the deployment read is cut short by the time limit": {deployments, "sleep 10", beforeDeployments},
		"the namespace read holds more objects than one report carries": {subscriptionRead("namespace"),
			`jq -n '{apiVersion: "v1", kind: "List", items: [range(1001) | {metadata: {name: "app-\(.)", labels: {"mitstack.dev/subscription": "sub_slice0123456"}}}]}'`,
			without(subscriptionKinds, "namespace")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			kubectl := kubectlAnswering(map[string]string{tc.read: tc.answer})

			_, posted, out, err := runReport(t, map[string]string{"kubectl": kubectl}, "REPORT_READ_SECONDS=1")

			require.NoError(t, err, out)
			assert.NotContains(t, posted.body, "error", "a kind left out still leaves a full report")
			assert.Len(t, posted.body["nodes"], 3)
			assert.Equal(t, asAny(tc.wantKinds), posted.body["subscriptionResourceKinds"])
			assert.Equal(t, listedOf(tc.wantKinds), posted.body["subscriptionResources"],
				"the list holds exactly the objects of the kinds it names")
		})
	}
}

func TestReport_CarriesTheKeySetTheAPIServerSignsServiceAccountTokensWithUnderJwks(t *testing.T) {
	var want map[string]any
	data, err := os.ReadFile(filepath.Join("testdata", "jwks.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &want))

	_, posted, out, err := runReport(t, nil)

	require.NoError(t, err, out)
	assert.Equal(t, want, posted.body["jwks"], "the report's jwks member is the API server's key set as read")
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
	for name, kubectl := range map[string]string{"every read answers": fakeKubectl, "the CloudNativePG read fails": failingCloudNativePGKubectl} {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, map[string]string{"kubectl": kubectl})

			require.NoError(t, err, out)
			assert.Equal(t, map[string]any{"contract": "node-report/v1", "names": []any{}}, posted.body["checks"])
		})
	}
}

func TestReport_WhoseCloudNativePGReadFailsStillCarriesItsNodesAndApplications(t *testing.T) {
	_, posted, out, err := runReport(t, map[string]string{"kubectl": failingCloudNativePGKubectl})

	require.NoError(t, err, out)
	assert.Len(t, posted.body["nodes"], 3)
	assert.Len(t, testutil.Field(t, posted.body, "argocd", "applications"), 2)
	assert.Equal(t, map[string]any{
		"exitStatus": float64(1),
		"stderr":     "error: the server does not have a resource type \"clusters\"\n",
	}, posted.body["cloudNativePG"])
}

func TestReport_HandsTheCloudNativePGExitStatusToTheToolsImagesJqAsANumber(t *testing.T) {
	cases := map[string]struct {
		kubectl string
		want    func(t *testing.T, read any)
	}{
		"the read exits 0": {fakeKubectl, func(t *testing.T, read any) { assert.Len(t, read, 3) }},
		"the read exits 1": {failingCloudNativePGKubectl, func(t *testing.T, read any) { assert.Equal(t, float64(1), testutil.Field(t, read, "exitStatus")) }},
		"the read times out": {kubectlAnswering(map[string]string{cloudNativePGRead: "sleep 10"}),
			func(t *testing.T, read any) { assert.Equal(t, float64(124), testutil.Field(t, read, "exitStatus")) }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, map[string]string{"kubectl": tc.kubectl}, "REPORT_READ_SECONDS=1")

			require.NoError(t, err, out)
			tc.want(t, posted.body["cloudNativePG"])
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
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, tc.fakes, "REPORT_READ_SECONDS=1")

			require.Error(t, err, "the report exits non-zero once it has posted the error")
			require.Equal(t, 1, posted.posts, out)
			assert.Equal(t, "Bearer "+reportToken, posted.authorization)
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

func TestManifests_LetsTheReportListCloudNativePGsClustersDatabasesAndRoles(t *testing.T) {
	rendered, err := report.Manifests(report.Inputs{URL: "https://api.mitstack.dev/v1/cluster-reports", SubscriptionID: "sub_0123456789ab", ToolsImage: testutil.ToolsImage})
	require.NoError(t, err)

	roles := testutil.Kind(testutil.Documents(t, rendered), "ClusterRole")

	require.Len(t, roles, 1)
	assert.Contains(t, testutil.Field(t, roles[0], "rules"), map[string]any{
		"apiGroups": []any{"postgresql.cnpg.io"},
		"resources": []any{"clusters", "databases", "databaseroles"},
		"verbs":     []any{"get", "list"},
	})
}

func TestManifests_LetsTheReportListEachKindItReadsAndGrantsNoVerbOnSecrets(t *testing.T) {
	rendered, err := report.Manifests(report.Inputs{URL: "https://api.mitstack.dev/v1/cluster-reports", SubscriptionID: "sub_0123456789ab", ToolsImage: testutil.ToolsImage})
	require.NoError(t, err)

	roles := testutil.Kind(testutil.Documents(t, rendered), "ClusterRole")

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
