package report_test

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const cloudNativePGRead = "get clusters.postgresql.cnpg.io,databases.postgresql.cnpg.io,databaseroles.postgresql.cnpg.io -A -o json"

// fakeTalosctl and fakeKubectl answer the report's reads from the fixture cluster in testdata/, and
// failingCloudNativePGKubectl refuses only the CloudNativePG read, as a cluster without its CRDs would.
const (
	fakeTalosctl = `cat testdata/members.txt`
	fakeKubectl  = `
case "$*" in
  "get nodes -o json") cat testdata/nodes.json ;;
  "get applications.argoproj.io -n argocd -o json") cat testdata/applications.json ;;
  "` + cloudNativePGRead + `") cat testdata/cloudnative-pg.json ;;
  *) exit 1 ;;
esac
`
	failingCloudNativePGKubectl = `
case "$*" in
  "get nodes -o json") cat testdata/nodes.json ;;
  "get applications.argoproj.io -n argocd -o json") cat testdata/applications.json ;;
  "` + cloudNativePGRead + `") echo 'error: the server does not have a resource type "clusters"' >&2; exit 1 ;;
  *) exit 1 ;;
esac
`
)

// postedReport is what the fake platform received.
type postedReport struct {
	authorization string
	body          map[string]any
}

func fakePlatform(t *testing.T) (*httptest.Server, *postedReport) {
	t.Helper()
	posted := &postedReport{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted.authorization = r.Header.Get("Authorization")
		data, err := io.ReadAll(r.Body)
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

// runReport runs the rendered report against kubectl and returns what the fake platform received.
func runReport(t *testing.T, kubectl string) (*testutil.Tools, *postedReport, string, error) {
	t.Helper()
	server, posted := fakePlatform(t)
	tools := testutil.NewTools(t, map[string]string{"talosctl": fakeTalosctl, "kubectl": kubectl})
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("report-token-fixture"), 0o600))

	out, err := testutil.RunBash(t, renderedScript(t, server.URL), tools.Env(
		"REPORT_URL="+server.URL, "SUBSCRIPTION_ID=sub_0123456789ab", "HOST_IP=10.0.0.6",
		"REPORT_TOKEN_FILE="+token, "CURL_CA_BUNDLE="+writeServerCA(t, server)))
	return tools, posted, out, err
}

func TestReport_PostsEachNodeWithItsTalosVersionTheEtcdMembershipAndArgoCDsHealth(t *testing.T) {
	tools, posted, out, err := runReport(t, fakeKubectl)

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
	assert.Equal(t, []string{
		"kubectl get nodes -o json",
		"kubectl get applications.argoproj.io -n argocd -o json",
		"talosctl -n 10.0.0.6 etcd members",
		"kubectl " + cloudNativePGRead,
	}, tools.Calls(t))
}

func TestReport_ListsCloudNativePGsClustersDatabasesAndRoles(t *testing.T) {
	_, posted, out, err := runReport(t, fakeKubectl)

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

func TestReport_WhoseCloudNativePGReadFailsStillCarriesItsNodesAndApplications(t *testing.T) {
	_, posted, out, err := runReport(t, failingCloudNativePGKubectl)

	require.NoError(t, err, out)
	assert.Len(t, posted.body["nodes"], 3)
	assert.Len(t, testutil.Field(t, posted.body, "argocd", "applications"), 2)
	assert.Equal(t, map[string]any{
		"exitStatus": float64(1),
		"stderr":     "error: the server does not have a resource type \"clusters\"\n",
	}, posted.body["cloudNativePG"])
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

func TestManifests_RefusesAReportURLThatIsNotHTTPS(t *testing.T) {
	_, err := report.Manifests(report.Inputs{URL: "http://api.mitstack.dev/v1/cluster-reports", SubscriptionID: "sub_0123456789ab", ToolsImage: testutil.ToolsImage})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "http://api.mitstack.dev/v1/cluster-reports")
}
