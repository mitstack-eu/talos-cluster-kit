package report_test

import (
	"cmp"
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
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

const (
	reportToken       = "report-token-fixture"
	nodesRead         = "get nodes -o json --show-managed-fields"
	applicationsRead  = "get applications.argoproj.io -n argocd -o json"
	keySetRead        = "get --raw /openid/v1/jwks"
	cloudNativePGRead = "get clusters.postgresql.cnpg.io,databases.postgresql.cnpg.io,databaseroles.postgresql.cnpg.io -A -o json"
	storageRead       = "get persistentvolumeclaims,storageclasses.storage.k8s.io,services -A -o json"
	objectStoresRead  = "get objectstores.barmancloud.cnpg.io -A -o json"
	podsRead          = "get pods -A -o json"
	workloadsRead     = "get deployments.apps,statefulsets.apps,daemonsets.apps,jobs.batch,cronjobs.batch -A -o json"
	ingressesRead     = "get ingresses.networking.k8s.io -A -o json"
	defaultDenyRead   = "get networkpolicies.networking.k8s.io -A --field-selector metadata.name=mitstack-default-deny -o json"
	membersRead       = "-n 10.0.0.6 etcd members"
	etcdStatusRead    = "-n 10.0.0.6,10.0.0.5,10.0.0.7 etcd status"
	volumesRead       = "-n 10.0.0.5,10.0.0.6,10.0.0.7 get volumestatus -o json"
	meshProbeRead     = "get configmap mitstack-mesh-probe -n kube-system -o json"
	sealedSecretsRead = "get sealedsecrets.bitnami.com -n argocd -o json"
	appProjectsRead   = "get appprojects.argoproj.io -n argocd -o json"
	fixturesRead      = "get configmap admission-fixtures -n mitstack-admission-probe -o json"
	dryRun            = "create --dry-run=server -n mitstack-admission-probe -f "
	scansRead         = "get configmap mitstack-security-scans -n kube-system -o json"
	scanCronJobRead   = "get cronjob.batch mitstack-security-scan -n kube-system -o json"
	scanEventsRead    = "get events -n kube-system -o json"
	// dryRunAnswer refuses the app-label and Pod Security fixtures as Kyverno and the API server word it, and admits the rest.
	dryRunAnswer = `case "$(cat "${@: -1}")" in
*refused-require-app-label*)
  echo 'Error from server: admission webhook "vpol.validate.kyverno.svc-fail" denied the request: mitstack-require-app-label: every pod must carry a non-empty mitstack.dev/app label' >&2
  exit 1 ;;
*pod-security-restricted*)
  echo 'Error from server (Forbidden): pods "pod-security-restricted" is forbidden: violates PodSecurity "restricted:latest": privileged' >&2
  exit 1 ;;
*) echo 'deployment.apps/refused-require-image-digest created (server dry run)' ;;
esac`
	tokenRequest      = "create token mitstack-node-report -n mitstack-node "
	reportProof       = "report-proof-fixture"
	proofScript       = `exec kubectl create token "$4" -n "$3" --for "$2" --body-sha256 "$(sha256sum < "$1" | cut -d ' ' -f 1)"`
	failedRead        = `head -c 2000 /dev/zero | tr '\0' x >&2; printf '\nerror: the server is gone\n' >&2
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

func without(left ...string) []string {
	return slices.DeleteFunc(slices.Clone(subscriptionKinds), func(k string) bool { return slices.Contains(left, k) })
}

var fakeKubectl = kubectlAnswering(nil)

func kubectlWith(read, answer string) map[string]string {
	return map[string]string{"kubectl": kubectlAnswering(map[string]string{read: answer})}
}

var fakeTalosctl = talosctlAnswering("cat testdata/volumes.json")

func talosctlAnswering(volumes string) string {
	return talosctlWith(volumes, "cat testdata/etcd-status.txt")
}

func talosctlWith(volumes, status string) string {
	return "case \"$*\" in\n\"" + membersRead + "\")\ncat testdata/members.txt\n;;\n\"" + volumesRead + "\")\n" + volumes +
		"\n;;\n\"" + etcdStatusRead + "\")\n" + status + "\n;;\n*) exit 1 ;;\nesac\n"
}

func kubectlAnswering(overrides map[string]string) string {
	answers := map[string]string{nodesRead: "cat testdata/nodes.json", applicationsRead: "cat testdata/applications.json",
		keySetRead: "cat testdata/jwks.json", cloudNativePGRead: "cat testdata/cloudnative-pg.json", storageRead: "cat testdata/storage-and-services.json",
		tokenRequest: "echo " + reportProof, meshProbeRead: "cat testdata/mesh-probe.json", podsRead: "cat testdata/pods.json",
		workloadsRead: "cat testdata/workloads.json",
		ingressesRead: `echo '{"apiVersion": "v1", "kind": "List", "items": []}'`, defaultDenyRead: "cat testdata/default-deny.json",
		sealedSecretsRead: "cat testdata/sealed-secrets.json", appProjectsRead: "cat testdata/app-projects.json", objectStoresRead: "cat testdata/object-stores.json",
		fixturesRead: "cat testdata/admission-fixtures.json", dryRun: dryRunAnswer, scansRead: "cat testdata/security-scans.json",
		scanCronJobRead: "cat testdata/security-scan-cronjob.json", scanEventsRead: `echo '{"items": []}'`}
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

func inputs(url string) report.Inputs {
	return report.Inputs{URL: url, SubscriptionID: "sub_0123456789ab", ToolsImage: testutil.ToolsImage, ProofScript: proofScript}
}

func named(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, doc := range testutil.Kind(docs, kind) {
		if testutil.Field(t, doc, "metadata", "name") == name {
			return doc
		}
	}
	require.FailNow(t, "no "+kind+" "+name)
	return nil
}

const reportURL = "https://api.mitstack.dev/v1/cluster-reports"

func manifests(t *testing.T, url string) []byte {
	t.Helper()
	rendered, err := report.Manifests(inputs(url))
	require.NoError(t, err)
	return rendered
}

func stringMap(m any) map[string]string {
	out := map[string]string{}
	for k, v := range m.(map[string]any) {
		out[k] = v.(string)
	}
	return out
}

func cronJob(t *testing.T, rendered []byte, name string) (job map[string]any, labels map[string]string, container any) {
	t.Helper()
	job = named(t, testutil.Documents(t, rendered), "CronJob", name)
	pod := testutil.Field(t, job, "spec", "jobTemplate", "spec", "template")
	return job, stringMap(testutil.Field(t, pod, "metadata", "labels")), testutil.Field(t, pod, "spec", "containers", "0")
}

var imageScripts = map[string]string{"mitstack-node-report": "report.sh", "mitstack-mesh-probe": "mesh-probe.sh",
	"mitstack-security-scan": "security-scan.sh"}

func renderedScript(t *testing.T, url, name string) (script string, env map[string]string) {
	t.Helper()
	_, _, container := cronJob(t, manifests(t, url), name)
	assert.Equal(t, []any{"bash", tools.ScriptDir + "/" + imageScripts[name]}, testutil.Field(t, container, "command"),
		"the CronJob runs the script the tools image carries, so the machine configuration carries no script")
	data, err := os.ReadFile(filepath.Join("scripts", imageScripts[name]))
	require.NoError(t, err)
	return string(data), testutil.ContainerEnv(t, container)
}

func meshTarget(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, components.MeshProbeAnswer)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/"
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

	script, rendered := renderedScript(t, server.URL, "mitstack-node-report")

	ca := writeServerCA(t, server)
	out, err := testutil.RunBash(t, script, tools.Env(append([]string{
		"GATEWAYS=10.0.0.2 10.0.0.248 10.0.0.249", "EGRESS_URL=" + egressTarget(t),
		"REPORT_URL=" + server.URL, "SUBSCRIPTION_ID=sub_0123456789ab", "HOST_IP=10.0.0.6", "REPORT_PROOF_SCRIPT=" + rendered["REPORT_PROOF_SCRIPT"],
		"REPORT_TOKEN_FILE=" + token, "CURL_CA_BUNDLE=" + ca, "MESH_PROBE_URL=" + meshTarget(t),
		"MESH_PROBE_ANSWER=" + rendered["MESH_PROBE_ANSWER"], "MESH_PROBE_IDENTITY=" + rendered["MESH_PROBE_IDENTITY"],
		"MESH_PROBE_CONFIGMAP=" + rendered["MESH_PROBE_CONFIGMAP"], "OPENBAO_MEMBERS=" + openBao(t, healthyOpenBao...),
		"OPENBAO_CA_FILE=" + ca, "ARGOCD_URL=" + argoCD(t, signedOut, http.StatusOK, loginsOff)}, env...)...))
	posted.mu.Lock()
	defer posted.mu.Unlock()
	return tools, posted, out, err
}

const (
	signedOut = `{"loggedIn":false}`
	loginsOff = `{"url":"https://argocd.example","userLoginsDisabled":true,"oidcConfig":null}`
	noSession = `{"error":"no session information","code":16,"message":"no session information"}`
)

// argoCDRoutes are the routes the report asks Argo CD without a session, in its order.
var argoCDRoutes = []string{"/api/v1/session/userinfo", "/api/v1/account", "/api/v1/applications", "/api/v1/applicationsets",
	"/api/v1/certificates", "/api/v1/clusters", "/api/v1/gpgkeys", "/api/v1/notifications/services",
	"/api/v1/notifications/templates", "/api/v1/notifications/triggers", "/api/v1/projects", "/api/v1/repocreds",
	"/api/v1/repositories", "/api/v1/settings", "/api/v1/settings/plugins", "/api/v1/write-repocreds",
	"/api/v1/write-repositories"}

// argoCD fakes the two routes Argo CD answers without a session, userinfo and settings, and refuses every other route.
func argoCD(t *testing.T, userinfo string, settingsStatus int, settings string) string {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.ContentLength > 0 {
			t.Errorf("the report asked %s %s with a credential or a body", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/v1/session/userinfo":
			_, _ = io.WriteString(w, userinfo)
		case "/api/v1/settings":
			w.WriteHeader(settingsStatus)
			_, _ = io.WriteString(w, settings)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, noSession)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// openBaoMember is how one fake OpenBao member answers; httptest's servers share one certificate, so one CA covers all.
type openBaoMember struct {
	sealed, active, down bool
	delay                time.Duration
	seal                 string
}

var healthyOpenBao = []openBaoMember{{active: true}, {}, {}}

func openBao(t *testing.T, members ...openBaoMember) string {
	t.Helper()
	named := make([]string, 0, len(members))
	for i, m := range members {
		if m.down {
			named = append(named, fmt.Sprintf("openbao-%d=https://127.0.0.1:1", i))
			continue
		}
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(m.delay)
			switch r.URL.Path {
			case "/v1/sys/seal-status":
				_, _ = io.WriteString(w, cmp.Or(m.seal, fmt.Sprintf(`{"type":"shamir","initialized":true,"sealed":%t,"t":1,"n":1,"storage_type":"raft"}`, m.sealed)))
			case "/v1/sys/leader":
				_, _ = fmt.Fprintf(w, `{"ha_enabled":true,"is_self":%t,"leader_address":"https://openbao-0.openbao-internal:8200"}`, m.active)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		named = append(named, fmt.Sprintf("openbao-%d=%s", i, server.URL))
	}
	return strings.Join(named, " ")
}

func TestReport_PostsEachNodeWithItsTalosVersionAndAllocatableTheEtcdMembershipAndEachApplicationsHealthComparedRevisionAndAppliedObjectsSignedForItsExactBody(t *testing.T) {
	tools, posted, out, err := runReport(t, map[string]string{"date": reportAt})

	require.NoError(t, err, out)
	testutil.Golden(t, filepath.Join("testdata", "golden", "healthy-body.json"), posted.raw)
	assert.Equal(t, "Bearer report-token-fixture", posted.authorization)
	assert.Equal(t, "sub_0123456789ab", posted.body["subscriptionId"])
	assert.Equal(t, []any{
		map[string]any{"name": "poc-talos4-cp1", "ready": true, "talosVersion": "v1.14.4", "kubeletVersion": "v1.37.1", "internalIP": "10.0.0.5",
			"addresses": []any{map[string]any{"type": "InternalIP", "address": "10.0.0.5"}, map[string]any{"type": "Hostname", "address": "poc-talos4-cp1"}}, "allocatable": map[string]any{"cpu": "3950m", "memory": "7712Mi", "pods": "110"}, "zone": "fsn1", "region": "eu-central",
			"zoneLabelManagers": []any{map[string]any{"manager": "mitstack-node-topology", "operation": "Update", "time": "2026-11-04T08:05:00Z"}},
			"taints":            []any{map[string]any{"key": "node-role.kubernetes.io/control-plane", "value": nil, "effect": "NoSchedule"}}, "unschedulable": false},
		map[string]any{"name": "poc-talos4-cp2", "ready": true, "talosVersion": "v1.14.4", "kubeletVersion": "v1.37.1", "internalIP": "10.0.0.6",
			"addresses": []any{map[string]any{"type": "InternalIP", "address": "10.0.0.6"}}, "allocatable": map[string]any{"cpu": "4", "memory": "15622080Ki", "pods": "110"}, "zone": "nbg1", "region": nil,
			"zoneLabelManagers": []any{map[string]any{"manager": "kubectl-label", "operation": "Update", "time": "2026-11-04T08:07:00Z"}},
			"taints":            []any{}, "unschedulable": false},
		map[string]any{"name": "poc-talos4-cp3", "ready": false, "talosVersion": "v1.14.3", "kubeletVersion": "v1.37.1", "internalIP": "10.0.0.7",
			"addresses": []any{map[string]any{"type": "InternalIP", "address": "10.0.0.7"}}, "allocatable": map[string]any{"cpu": "1950m", "memory": "3615Mi", "pods": "110"}, "zone": nil, "region": nil,
			"zoneLabelManagers": []any{},
			"taints": []any{map[string]any{"key": "node.kubernetes.io/unschedulable", "value": nil, "effect": "NoSchedule"},
				map[string]any{"key": "mitstack.dev/pool", "value": "db", "effect": "NoExecute"}}, "unschedulable": true},
	}, posted.body["nodes"], "each node's addresses with their types, its zone and region labels, every manager of its zone label, each taint without the time it was added, and whether the node is unschedulable")
	assert.Equal(t, []any{}, posted.body["ingresses"], "a cluster with no Ingress reports an empty list")
	assert.Equal(t, readJSON(t, "testdata", "golden", "argocd.json"), posted.body["argocd"], "each Application names the revision Argo CD compared it against or null and its last sync's phase with the message cut to 256 characters, and a Secret is among the applied objects, each with its identity, sync and health alone")
	wantCalls := []string{
		"kubectl " + nodesRead,
		"kubectl get applications.argoproj.io -n argocd -o json",
		"kubectl " + keySetRead,
		"talosctl " + membersRead,
		"talosctl " + volumesRead,
		"kubectl " + cloudNativePGRead,
		"kubectl " + storageRead,
	}
	for _, kind := range subscriptionKinds {
		wantCalls = append(wantCalls, "kubectl "+subscriptionRead(kind))
	}
	calls := tools.Calls(t)
	require.NotEmpty(t, calls)
	beside := []string{"kubectl " + meshProbeRead, "kubectl " + podsRead, "kubectl " + workloadsRead, "kubectl " + ingressesRead, "kubectl " + defaultDenyRead,
		"talosctl " + etcdStatusRead, "kubectl " + sealedSecretsRead, "kubectl " + appProjectsRead, "kubectl " + fixturesRead,
		"kubectl " + objectStoresRead,
		"kubectl " + scansRead, "kubectl " + scanCronJobRead, "kubectl " + scanEventsRead}
	for _, read := range beside {
		assert.Contains(t, calls[:len(calls)-1], read, "a read beside the others ends before the proof")
	}
	calls = slices.DeleteFunc(calls, func(c string) bool {
		return slices.Contains(beside, c) || strings.HasPrefix(c, "date ") || strings.HasPrefix(c, "kubectl "+dryRun)
	})
	assert.Equal(t, append(wantCalls, "kubectl "+proofRequest(posted)), calls, "the proof is minted once the body is final")
	assert.Equal(t, reportProof, posted.proof, "the post carries the proof the block's script printed for its exact body")
}

// etcdMember is the etcd member with each member's health in members.txt order, the leader cp1, and no failed read.
func etcdMember(healthy ...bool) map[string]any {
	members := []any{
		map[string]any{"id": "25afe91c7e8956f1", "hostname": "poc-talos4-cp2", "peerURLs": []any{"https://10.0.0.6:2380"}, "learner": false},
		map[string]any{"id": "a48f107742a8605c", "hostname": "poc-talos4-cp1", "peerURLs": []any{"https://10.0.0.5:2380"}, "learner": false},
		map[string]any{"id": "1948ccd42e0e52cd", "hostname": "poc-talos4-cp4", "peerURLs": []any{"https://10.0.0.7:2380"}, "learner": true},
	}
	for i, member := range members {
		member.(map[string]any)["healthy"] = healthy[i]
	}
	return map[string]any{"members": members, "leader": "a48f107742a8605c", "statusError": nil}
}

func TestReport_NamesEachEtcdMembersHealthTheLeaderAndTheStatusReadThatFailed(t *testing.T) {
	twoAnswer := "head -n 3 testdata/etcd-status.txt; echo 'error from node 10.0.0.7: rpc error: code = Unavailable' >&2; exit 1"
	alarmed := `sed '/^10.0.0.7/s/ *$/     memberID 1948ccd42e0e52cd alarm:NOSPACE/' testdata/etcd-status.txt`
	cases := map[string]struct {
		status      string
		healthy     []bool
		leader      any
		statusError any
	}{
		"every node answers": {"cat testdata/etcd-status.txt", []bool{true, true, true}, "a48f107742a8605c", nil},
		"the learner's node is stopped": {twoAnswer, []bool{true, true, false}, "a48f107742a8605c",
			map[string]any{"exitStatus": float64(1), "stderr": "error from node 10.0.0.7: rpc error: code = Unavailable\n"}},
		"the learner names an alarm": {alarmed, []bool{true, true, false}, "a48f107742a8605c", nil},
		"the status read times out":  {"sleep 10", []bool{false, false, false}, nil, map[string]any{"exitStatus": float64(124), "stderr": ""}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, map[string]string{"talosctl": talosctlWith("cat testdata/volumes.json", tc.status)}, "REPORT_READ_SECONDS=1")

			require.NoError(t, err, out)
			want := etcdMember(tc.healthy...)
			want["leader"], want["statusError"] = tc.leader, tc.statusError
			assert.Equal(t, want, posted.body["etcd"], "a member whose node does not answer reads unhealthy, never healthy")
		})
	}
}

func TestReport_NamesEachOpenBaoMembersSealStateTheActiveOneAndEachReadThatFailed(t *testing.T) {
	member := func(name string, sealed, active bool) map[string]any {
		return map[string]any{"name": name, "initialized": true, "sealed": sealed, "active": active}
	}
	unread := func(name string, status float64) map[string]any {
		return map[string]any{"name": name, "initialized": nil, "sealed": nil, "active": nil,
			"error": map[string]any{"read": "sys/seal-status", "exitStatus": status}}
	}
	cases := map[string]struct {
		members []openBaoMember
		want    []any
		leader  any
	}{
		"every member is unsealed": {healthyOpenBao,
			[]any{member("openbao-0", false, true), member("openbao-1", false, false), member("openbao-2", false, false)}, "openbao-0"},
		"one member is sealed": {[]openBaoMember{{active: true}, {sealed: true}, {}},
			[]any{member("openbao-0", false, true), member("openbao-1", true, false), member("openbao-2", false, false)}, "openbao-0"},
		"the active member does not answer": {[]openBaoMember{{down: true}, {}},
			[]any{unread("openbao-0", 7), member("openbao-1", false, false)}, nil},
		"one member answers too late": {[]openBaoMember{{active: true}, {delay: 3 * time.Second}},
			[]any{member("openbao-0", false, true), unread("openbao-1", 124)}, "openbao-0"},
		"one member answers JSON that is not an object": {[]openBaoMember{{active: true}, {seal: "[1]"}},
			[]any{member("openbao-0", false, true), unread("openbao-1", 0)}, "openbao-0"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, nil, "REPORT_READ_SECONDS=1", "OPENBAO_MEMBERS="+openBao(t, tc.members...))

			require.NoError(t, err, out)
			openbao, ok := posted.body["openbao"].(map[string]any)
			require.True(t, ok, "the report carries no openbao member")
			for _, got := range openbao["members"].([]any) {
				if failed, ok := got.(map[string]any)["error"].(map[string]any); ok {
					assert.IsType(t, "", failed["stderr"], "the failed read carries curl's stderr")
					delete(failed, "stderr")
				}
			}
			assert.Equal(t, map[string]any{"members": tc.want, "leader": tc.leader}, openbao)
		})
	}
}

func TestManifests_PointsTheReportAtEachOpenBaoPodByANameItsCertificateHoldsTrustsItsAuthorityAndOpenBaoTakesItsCallsOnItsAPIPort(t *testing.T) {
	set, err := components.Hosting(testutil.HostingInputs(t))
	require.NoError(t, err)
	tls := testutil.Documents(t, testutil.Manifest(t, "mitstack-hosting-tls", set.Served).Bytes)
	statefulSet := testutil.Kind(testutil.Documents(t, testutil.Manifest(t, "openbao", set.Served).Bytes), "StatefulSet")[0]
	job, labels, container := cronJob(t, manifests(t, reportURL), "mitstack-node-report")
	env := testutil.ContainerEnv(t, container)
	endpoints := map[string]testutil.CiliumEndpoint{
		"OpenBao":    testutil.Endpoint("openbao", nil, map[string]string{"app.kubernetes.io/name": "openbao", "component": "server"}),
		"the report": testutil.Endpoint("mitstack-node", nil, labels),
	}
	policies := testutil.Kind(testutil.Documents(t, testutil.Manifest(t, "mitstack-hosting-flows", set.Served).Bytes), "CiliumClusterwideNetworkPolicy")
	require.Len(t, policies, 1)
	assert.Contains(t, testutil.Grants(t, policies[0], endpoints, "OpenBao"), "ingress with the report on 8200/TCP 15008/TCP")

	domain := fmt.Sprintf("%s.%s.svc", testutil.Field(t, statefulSet, "spec", "serviceName"), testutil.Field(t, statefulSet, "metadata", "namespace"))
	var want []string
	for i := range components.OpenBaoMembers {
		pod := fmt.Sprintf("%s-%d", testutil.Field(t, statefulSet, "metadata", "name"), i)
		want = append(want, fmt.Sprintf("%s=https://%s.%s:8200", pod, pod, domain))
	}
	assert.Equal(t, strings.Join(want, " "), env["OPENBAO_MEMBERS"])
	assert.Contains(t, testutil.Field(t, named(t, tls, "Certificate", "openbao-tls"), "spec", "dnsNames"), "*."+domain)

	bundle := named(t, tls, "Bundle", "openbao-ca")
	assert.Contains(t, testutil.Field(t, job, "spec", "jobTemplate", "spec", "template", "spec", "volumes"),
		map[string]any{"name": "openbao-ca", "configMap": map[string]any{"name": testutil.Field(t, bundle, "metadata", "name"), "optional": true}})
	assert.Contains(t, testutil.Field(t, container, "volumeMounts"),
		map[string]any{"name": "openbao-ca", "mountPath": "/var/run/openbao-ca", "readOnly": true})
	assert.Equal(t, "/var/run/openbao-ca/"+testutil.Field(t, bundle, "spec", "target", "configMap", "key").(string), env["OPENBAO_CA_FILE"],
		"trust-manager names the ConfigMap after its Bundle and writes the authority under the Bundle's key")
}

func TestManifests_PointsTheReportAtArgoCDsServerServiceOnItsHTTPSPort(t *testing.T) {
	_, env := renderedScript(t, reportURL, "mitstack-node-report")
	service := named(t, testutil.Documents(t, testutil.Manifest(t, "argocd", components.Default().Served).Bytes), "Service", "argocd-server")

	assert.Equal(t, fmt.Sprintf("https://argocd-server.%s.svc", testutil.Field(t, service, "metadata", "namespace")), env["ARGOCD_URL"],
		"curl asks port 443 of an https URL that names no port")
	assert.Contains(t, testutil.Field(t, service, "spec", "ports"),
		map[string]any{"name": "https", "protocol": "TCP", "port": 443, "targetPort": 8080})
}

func proofRequest(posted *postedReport) string {
	return fmt.Sprintf("%s--for %s --body-sha256 %x", tokenRequest, posted.origin, sha256.Sum256(posted.raw))
}

func TestReport_LeavesOutEveryApplicationsAppliedObjectsWhenCarryingThemWouldPassThePlatformsLimit(t *testing.T) {
	cases := map[string]struct {
		answers map[string]string
		kinds   []string
		objects int
	}{
		"4000 applied objects": {map[string]string{applicationsRead: `jq '.items[0].status.resources = [range(4000) | {version: "v1", kind: "ConfigMap", namespace: "app-shop", name: "settings-\(.)", status: "Synced"}]' testdata/applications.json`}, without(), 4},
		"1000 applied objects, each with a 256-character message, beside 1000 labelled namespaces": {map[string]string{
			applicationsRead:              `jq '.items[0].status.resources = [range(1000) | {group: "apps", kind: "Deployment", namespace: "app-shop", name: "web-\(.)", status: "Synced", health: {status: "Degraded", message: ("x" * 256)}}]' testdata/applications.json`,
			subscriptionRead("namespace"): `jq -n '{items: [range(1000) | {metadata: {name: "app-\(.)", labels: {"mitstack.dev/subscription": "sub_slice0123456"}}}]}'`,
		}, without("deployment.apps", "appproject.argoproj.io", "externalsecret.external-secrets.io"), 1000},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, map[string]string{"kubectl": kubectlAnswering(tc.answers)})

			require.NoError(t, err, out)
			assert.LessOrEqual(t, len(posted.raw), 256<<10, "the body fits the platform's limit")
			assert.Equal(t, readJSON(t, "testdata", "golden", "argocd-without-applied.json"), posted.body["argocd"], "each Application keeps its revision and carries no list, so no observer reads a left-out object absent")
			assert.Len(t, posted.body["nodes"], 3, "the rest of the report stays whole")
			assert.ElementsMatch(t, tc.kinds, posted.body["subscriptionResourceKinds"], "a kind whose objects would pass 1000 is left out whole")
			assert.Len(t, posted.body["subscriptionResources"], tc.objects, "each kind listed is listed whole")
		})
	}
}

func readJSON(t *testing.T, path ...string) any {
	t.Helper()
	var v any
	data, err := os.ReadFile(filepath.Join(path...))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &v))
	return v
}

func TestReport_ListsTheObjectsOfEachKindItReadWholeAndLeavesOutAKindWhoseReadFailsOrIsCutShort(t *testing.T) {
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
			_, posted, out, err := runReport(t, kubectlWith(tc.read, tc.answer), "REPORT_READ_SECONDS=1")

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

// everyCheck is the checks member naming each of the platform verdict's nine checks, in the order it judges them.
var everyCheck = map[string]any{"contract": "node-report/v1", "names": []any{"routes", "exposure", "credentials", "version",
	"advisories", "admission", "default-deny", "cis-benchmark", "nsa-scan"}}

func TestReport_PostsEachMemberItReadsFromAHealthyCluster(t *testing.T) {
	fakes := kubectlWith(ingressesRead, "cat testdata/ingresses.json")
	fakes["date"] = reportAt
	_, posted, out, err := runReport(t, fakes)
	require.NoError(t, err, out)
	assert.NotContains(t, string(posted.raw), "CIPHERTEXT", "no sealed credential's encrypted data leaves the cluster")
	cases := map[string]struct {
		member, msg string
		want        any
	}{
		"every pod with its namespace, node, phase and containers": {"pods", "", readJSON(t, "testdata", "golden", "pods.json")},
		"each node's state and ephemeral volume with its encryption": {"systemVolumes", "",
			readJSON(t, "testdata", "golden", "system-volumes.json")},
		"the key set the API server signs service account tokens with, under jwks": {"jwks",
			"the report's jwks member is the API server's key set as read", readJSON(t, "testdata", "jwks.json")},
		"CloudNativePG's clusters with their current primary, archiving and last backup, databases and roles": {"cloudNativePG", "", []any{
			map[string]any{"namespace": "app-pg", "kind": "Cluster", "name": "shared", "subscription": "sub_pg0123456789",
				"phase": "Cluster in healthy state", "instances": float64(2), "readyInstances": float64(2), "currentPrimary": "shared-1",
				"continuousArchiving": map[string]any{"status": "True", "reason": "ContinuousArchivingSuccess", "message": "Continuous archiving is working"},
				"lastBackup":          map[string]any{"status": "False", "reason": "LastBackupFailed", "message": "can't execute backup: exit status 4"}},
			map[string]any{"namespace": "app-pg", "kind": "Database", "name": "shop", "subscription": "sub_shop01234567",
				"applied": true, "message": nil},
			map[string]any{"namespace": "app-pg", "kind": "DatabaseRole", "name": "shop", "subscription": nil,
				"applied": false, "message": `role "shop" cannot be created: the cluster is not ready`}}},
		"each plugin CloudNativePG finds by a Service in its own namespace, in name order, and none a Service elsewhere names": {
			"cloudNativePGPlugins", "", []any{"barman-cloud.cloudnative-pg.io", "other.example"}},
		"each ObjectStore with every server's recovery window, and one the plugin has not written a status for": {"cloudNativePGObjectStores", "", []any{
			map[string]any{"namespace": "app-pg", "kind": "ObjectStore", "name": "shared", "subscription": "sub_pg0123456789",
				"recoveryWindows": map[string]any{"shared": map[string]any{"firstRecoverabilityPoint": "2026-11-12T02:00:11Z",
					"lastSuccessfulBackupTime": "2026-11-12T02:00:11Z", "lastFailedBackupTime": nil}}},
			map[string]any{"namespace": "app-pg", "kind": "ObjectStore", "name": "fresh", "subscription": nil, "recoveryWindows": map[string]any{}}}},
		"each claim with its storage class, phase, requested size and bound capacity, and the default storage classes": {"storage", "", map[string]any{
			"claims": []any{
				map[string]any{"namespace": "app-shop", "name": "data-shop-db-1", "storageClass": "local-path", "phase": "Bound", "requestedSize": "10Gi", "capacity": "10Gi"},
				map[string]any{"namespace": "app-shop", "name": "scratch", "storageClass": nil, "phase": "Pending", "requestedSize": "1Gi", "capacity": nil},
			},
			"defaultStorageClasses": []any{"local-path"},
		}},
		"every Service with its type, whether or not a block labelled it": {"services", "", []any{
			map[string]any{"namespace": "default", "name": "kubernetes", "type": "ClusterIP"},
			map[string]any{"namespace": "app-shop", "name": "shop-db-rw", "type": "ClusterIP"},
			map[string]any{"namespace": "app-shop", "name": "edge", "type": "NodePort"},
			map[string]any{"namespace": "argocd", "name": "argocd-server", "type": "ClusterIP"},
			map[string]any{"namespace": "argocd", "name": "argocd-repo-server", "type": "ClusterIP"},
			map[string]any{"namespace": "cnpg-system", "name": "cnpg-webhook-service", "type": "ClusterIP"},
			map[string]any{"namespace": "cnpg-system", "name": "other-plugin", "type": "ClusterIP"},
			map[string]any{"namespace": "cnpg-system", "name": "barman-cloud", "type": "ClusterIP"},
			map[string]any{"namespace": "app-shop", "name": "plugin-lookalike", "type": "ClusterIP"}}},
		"the node-report contract and each of the verdict's nine checks": {"checks", "", everyCheck},
		"Argo CD's Services": {"argocd.services", "", []any{
			map[string]any{"name": "argocd-server", "type": "ClusterIP"},
			map[string]any{"name": "argocd-repo-server", "type": "ClusterIP"}}},
		"Argo CD's Ingresses": {"argocd.ingresses", "", []any{map[string]any{"name": "argocd-server"}}},
		"every Ingress in every namespace with its class and the addresses its status holds, and one of a class Traefik does not serve with none": {
			"ingresses", "", readJSON(t, "testdata", "golden", "ingresses.json")},
		"Argo CD's pods with their images": {"kubernetes", "", map[string]any{"argocdPods": []any{map[string]any{
			"name": "argocd-server-5d9c7b8f4-x2x7q", "containers": []any{map[string]any{
				"image": "quay.io/argoproj/argocd:v3.5.5@sha256:13db7b10db23283b1b36f855046602e18539a8a8204c9f6f0aa4a9a6ff5e4f3c"}}}}}},
		"each default-deny policy with its selector, types and rule counts": {"defaultDenyPolicies", "", []any{
			map[string]any{"namespace": "app-shop", "name": "mitstack-default-deny", "podSelector": map[string]any{},
				"policyTypes": []any{"Ingress", "Egress"}, "ingressRules": float64(0), "egressRules": float64(0), "managedBy": "kyverno"},
			map[string]any{"namespace": "app-pg", "name": "mitstack-default-deny", "podSelector": map[string]any{"matchLabels": map[string]any{"app": "pg"}},
				"policyTypes": []any{"Ingress"}, "ingressRules": float64(1), "egressRules": float64(0), "managedBy": nil}}},
		"each sealed repository credential's name, secret type, project and URL, from its template alone": {"repositorySecrets", "", []any{
			map[string]any{"name": "cluster-repository", "secretType": "repository", "project": "",
				"url": "ssh://git@ssh.github.com:443/mitstack-eu/poc-talos4-cluster.git"},
			map[string]any{"name": "app-repository-shop", "secretType": "repository", "project": "app-shop",
				"url": "ssh://git@ssh.github.com:443/mitstack-eu/shop-gitops.git"},
			map[string]any{"name": "write-back", "secretType": "repo-creds", "project": "", "url": ""}}},
		"each AppProject with its source repositories": {"appProjects", "", []any{
			map[string]any{"name": "app-shop", "sourceRepos": []any{"ssh://git@ssh.github.com:443/mitstack-eu/shop-gitops.git"}},
			map[string]any{"name": "default", "sourceRepos": []any{}}}},
		"each admission fixture named for a rule, dry-run in rule order with its answer": {"admission", "", []any{
			map[string]any{"rule": "mitstack-require-app-label", "exitStatus": float64(1),
				"fixture": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: refused-require-app-label\n",
				"message": "Error from server: admission webhook \"vpol.validate.kyverno.svc-fail\" denied the request: " +
					"mitstack-require-app-label: every pod must carry a non-empty mitstack.dev/app label\n"},
			map[string]any{"rule": "mitstack-require-image-digest", "exitStatus": float64(0),
				"fixture": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: refused-require-image-digest\n",
				"message": "deployment.apps/refused-require-image-digest created (server dry run)\n"},
			map[string]any{"rule": "pod-security-restricted", "exitStatus": float64(1),
				"fixture": "apiVersion: v1\nkind: Pod\nmetadata:\n  name: pod-security-restricted\n",
				"message": "Error from server (Forbidden): pods \"pod-security-restricted\" is forbidden: " +
					"violates PodSecurity \"restricted:latest\": privileged\n"}}},
		"the scan CronJob's last runs and each kept summary": {"securityScans", "", map[string]any{
			"runner": map[string]any{"exitStatus": float64(0),
				"output": "the CronJob kube-system/mitstack-security-scan last started a scan at 2026-11-07T03:17:00Z, and one last succeeded at 2026-11-07T03:19:06Z", "job": nil, "events": []any{}},
			"lastRun": map[string]any{"finishedAt": "2026-11-07T03:19:05Z", "exitStatus": float64(0), "output": "kube-bench: ran; kubescape: ran"},
			"kubeBench": map[string]any{"tool": "kube-bench", "version": "v0.16.0", "startedAt": "2026-11-07T03:17:02Z", "profile": "cis-1.10",
				"totals": map[string]any{"pass": float64(69), "fail": float64(1), "warn": float64(46), "info": float64(14)},
				"failed": []any{map[string]any{"id": "1.2.30", "scored": true,
					"text": "Ensure that the --service-account-extend-token-expiration parameter is set to false (Automated)"}},
				"finishedAt": "2026-11-07T03:17:41Z"},
			"kubescape": map[string]any{"tool": "kubescape", "version": "v4.0.15", "startedAt": "2026-11-07T03:17:41Z", "framework": "NSA",
				"controlsVersion": "v2.0.38", "failedControlsBySeverity": map[string]any{"criticalSeverity": float64(0), "highSeverity": float64(0),
					"mediumSeverity": float64(1), "lowSeverity": float64(0)},
				"failed": []any{map[string]any{"id": "C-0013", "name": "Non-root containers", "severity": "Medium", "failedResources": float64(3),
					"resources": []any{}}},
				"finishedAt": "2026-11-07T03:19:05Z"}}},
		"a call from inside the mesh answered, and a plain call from outside refused while ztunnel presents the target's certificate": {
			"mesh", "", map[string]any{"callFromInside": map[string]any{"passed": true},
				"plainCallFromOutside": map[string]any{"passed": true, "probedAt": "2026-10-23T15:42:31Z", "curlExitStatus": float64(56),
					"stderr": "curl: (56) Recv failure: Connection reset by peer\n"},
				"workloadIdentity": map[string]any{"passed": true, "probedAt": "2026-10-23T15:42:31Z", "identity": components.MeshProbeIdentity,
					"serial": "5E1C0A9F3B2D47E8", "notBefore": "2026-10-23T09:12:00Z", "notAfter": "2026-10-24T09:12:00Z"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, testutil.Field(t, posted.body, strings.Split(tc.member, ".")...), tc.msg)
		})
	}
}

func TestReport_CutsAnIngressClassTakenFromItsAnnotationTo256Characters(t *testing.T) {
	long := strings.Repeat("c", 300)

	_, posted, out, err := runReport(t, kubectlWith(ingressesRead, `echo '{"items": [{"metadata": {"annotations": {"kubernetes.io/ingress.class": "`+long+`"}}}]}'`))

	require.NoError(t, err, out)
	assert.Equal(t, long[:256], testutil.Field(t, posted.body, "ingresses", "0", "class"))
}

func TestReport_CutsAClustersArchivingMessageTo256CharactersAndReadsTheLastBackupItHoldsNoConditionForAsNull(t *testing.T) {
	long := strings.Repeat("m", 300)
	failing := `jq '.items[0].status.conditions = [{type: "ContinuousArchiving", status: "False", reason: "ContinuousArchivingFailing", message: "` + long + `"}]' testdata/cloudnative-pg.json`

	_, posted, out, err := runReport(t, kubectlWith(cloudNativePGRead, failing))

	require.NoError(t, err, out)
	assert.Equal(t, map[string]any{"status": "False", "reason": "ContinuousArchivingFailing", "message": long[:256]},
		testutil.Field(t, posted.body, "cloudNativePG", "0", "continuousArchiving"))
	assert.Nil(t, testutil.Field(t, posted.body, "cloudNativePG", "0", "lastBackup"), "a Cluster that has taken no backup holds no LastBackupSucceeded condition")
}

func TestReport_NamesEachAppliedObjectsArgoCDHealthAndEachContainersStateButNoTerminatedContainersMessage(t *testing.T) {
	_, posted, out, err := runReport(t, nil)

	require.NoError(t, err, out)
	assert.NotContains(t, string(posted.raw), "termination-message-fixture", "a terminated container's message is whatever the app wrote")
	for name, c := range readJSON(t, "testdata", "golden", "object-health.json").(map[string]any) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, testutil.Field(t, c, "want"), testutil.Field(t, posted.body, strings.Split(testutil.Field(t, c, "member").(string), ".")...))
		})
	}
}

func TestReport_CarriesEachFailedContainersMessageCutTo256BytesFromTheNewestFailedPodOfEachJobTheKitRendersAndNoneOfAnAppsJob(t *testing.T) {
	_, posted, out, err := runReport(t, map[string]string{"kubectl": kubectlAnswering(map[string]string{podsRead: "cat testdata/failed-job-pods.json", workloadsRead: "cat testdata/failed-jobs.json"})})

	require.NoError(t, err, out)
	assert.Equal(t, readJSON(t, "testdata", "golden", "failed-job-pods.json"), posted.body["pods"], "the newest failed pod of a kit Job carries the message of each container that exited non-zero, cut to 256 bytes, or null for one killed before it printed, while its older pod, a container that exited 0 and an app's own Job carry none")
	assert.NotContains(t, string(posted.raw), "app-job-message-fixture", "an app's own Job's message is whatever the app wrote")
}

func TestReport_CarriesOneMessageOfAtMost256BytesForEachOf40FailedKitJobsInsideThePlatformsLimit(t *testing.T) {
	_, posted, out, err := runReport(t, map[string]string{"kubectl": kubectlAnswering(map[string]string{workloadsRead: "jq -n -f testdata/forty-kit-jobs.jq", podsRead: "jq -n -f testdata/forty-kit-job-pods.jq"})})

	require.NoError(t, err, out)
	assert.LessOrEqual(t, len(posted.raw), 256<<10, "the body fits the platform's limit")
	carried := 0
	for _, pod := range posted.body["pods"].([]any) {
		if message, ok := testutil.Field(t, pod, "containers", "0", "message").(string); ok {
			carried++
			assert.True(t, len(message) <= 256 && utf8.ValidString(message), "a message is at most 256 bytes of whole characters, not %d", len(message))
		}
	}
	assert.Equal(t, 40, carried, "one message for each Job, from its newest failed pod")
}

func TestReport_AsksEachArgoCDRouteWithoutASessionAndKeepsItsStatusItsSizeAndOneBooleanOfUserinfoAndOfSettings(t *testing.T) {
	signedIn, noJSON := `{"loggedIn":true,"username":"admin","iss":"argocd"}`, "signed out at https://argocd.example"
	cases := map[string]struct {
		userinfo            string
		settingsStatus      int
		settings            string
		loggedIn, loginsOff any
	}{
		"Argo CD refuses each route that needs a session": {signedOut, 200, loginsOff, false, true},
		"userinfo says a caller is signed in":             {signedIn, 200, loginsOff, true, true},
		"userinfo answers no JSON":                        {noJSON, 200, loginsOff, nil, true},
		"the settings say a user can log in":              {signedOut, 200, `{"url":"https://argocd.example","userLoginsDisabled":false}`, false, false},
		"the settings name no userLoginsDisabled":         {signedOut, 200, `{"url":"https://argocd.example","oidcConfig":null}`, false, nil},
		"the settings give userLoginsDisabled as text":    {signedOut, 200, `{"url":"https://argocd.example","userLoginsDisabled":"true"}`, false, nil},
		"the settings answer no JSON":                     {signedOut, 200, noJSON, false, nil},
		"the settings answer is empty":                    {signedOut, 200, "", false, nil},
		"the settings route answers another status":       {signedOut, 502, loginsOff, false, nil},
		"Argo CD does not answer":                         {},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			url, want := "https://127.0.0.1:1", []any{}
			if tc.userinfo != "" {
				url = argoCD(t, tc.userinfo, tc.settingsStatus, tc.settings)
			}
			for _, path := range argoCDRoutes {
				route := map[string]any{"path": path, "status": float64(401), "bytes": float64(len(noSession))}
				switch {
				case tc.userinfo == "":
					route = map[string]any{"path": path, "status": float64(0), "bytes": float64(0), "error": "curl exit 7"}
				case path == argoCDRoutes[0]:
					route = map[string]any{"path": path, "status": float64(200), "bytes": float64(len(tc.userinfo)), "loggedIn": tc.loggedIn}
				case path == "/api/v1/settings":
					route = map[string]any{"path": path, "status": float64(tc.settingsStatus), "bytes": float64(len(tc.settings)), "userLoginsDisabled": tc.loginsOff}
				}
				want = append(want, route)
			}

			_, posted, out, err := runReport(t, nil, "ARGOCD_URL="+url)

			require.NoError(t, err, out)
			assert.Equal(t, want, testutil.Field(t, posted.body, "argocd", "routes"))
			assert.NotContains(t, string(posted.raw), "argocd.example", "the report keeps one boolean of each answer and none of its text")
		})
	}
}

func TestReport_NamesAFailedVolumeCloudNativePGStorageOrServiceReadInlineWithItsExitStatusAsANumberInAFullReport(t *testing.T) {
	failed := func(status float64, stderr string) map[string]any {
		return map[string]any{"exitStatus": status, "stderr": stderr}
	}
	timedOut := failed(124, "")
	storageFailed := failed(1, "error: persistentvolumeclaims is forbidden\n")
	podsFailed := failed(1, "error: pods is forbidden\n")
	notVolumes := failed(0, "talosctl exited 0, and its output is not the volume statuses this report reads")
	notStorage := failed(0, "kubectl exited 0, and its output is not the claims, storage classes and Services this report reads")
	ingressesFailed := failed(1, "error: ingresses is forbidden\n")
	notIngresses := failed(0, "kubectl exited 0, and its output is not the Ingresses this report reads")
	noObjectStores := failed(1, "error: the server doesn't have a resource type \"objectstores\"\n")
	notObjectStores := failed(0, "kubectl exited 0, and its output is not the ObjectStores this report reads")
	cases := map[string]struct {
		fakes map[string]string
		want  map[string]any
	}{
		"the CloudNativePG read exits 1": {kubectlWith(cloudNativePGRead, `echo 'error: the server does not have a resource type "clusters"' >&2; exit 1`),
			map[string]any{"cloudNativePG": failed(1, "error: the server does not have a resource type \"clusters\"\n")}},
		"the CloudNativePG read times out": {kubectlWith(cloudNativePGRead, "sleep 10"), map[string]any{"cloudNativePG": timedOut}},
		"the volume read exits 1": {map[string]string{"talosctl": talosctlAnswering(`echo 'rpc error: 10.0.0.7 is unreachable' >&2; exit 1`)},
			map[string]any{"systemVolumes": failed(1, "rpc error: 10.0.0.7 is unreachable\n")}},
		"the volume read times out":        {map[string]string{"talosctl": talosctlAnswering("sleep 10")}, map[string]any{"systemVolumes": timedOut}},
		"the volume read prints no JSON":   {map[string]string{"talosctl": talosctlAnswering("echo 'not json'")}, map[string]any{"systemVolumes": notVolumes}},
		"the volume read prints no volume": {map[string]string{"talosctl": talosctlAnswering("true")}, map[string]any{"systemVolumes": notVolumes}},
		"the storage and Service read exits 1, for every Service and Argo CD's": {
			kubectlWith(storageRead, `echo 'error: persistentvolumeclaims is forbidden' >&2; exit 1`),
			map[string]any{"storage": storageFailed, "services": storageFailed, "argocd.services": storageFailed, "cloudNativePGPlugins": storageFailed}},
		"the storage read times out": {kubectlWith(storageRead, "sleep 10"), map[string]any{"storage": timedOut}},
		"the storage and Service read prints no JSON": {kubectlWith(storageRead, "echo 'not json'"),
			map[string]any{"storage": notStorage, "services": notStorage, "cloudNativePGPlugins": notStorage}},
		"the pod read exits 1, for every pod and Argo CD's": {kubectlWith(podsRead, `echo 'error: pods is forbidden' >&2; exit 1`),
			map[string]any{"pods": podsFailed, "kubernetes.argocdPods": podsFailed}},
		"the pod read times out": {kubectlWith(podsRead, "sleep 10"), map[string]any{"pods": timedOut}},
		"the Ingress read exits 1, for every Ingress and Argo CD's": {kubectlWith(ingressesRead, `echo 'error: ingresses is forbidden' >&2; exit 1`),
			map[string]any{"ingresses": ingressesFailed, "argocd.ingresses": ingressesFailed}},
		"the Ingress read prints no JSON": {kubectlWith(ingressesRead, "echo 'not json'"),
			map[string]any{"ingresses": notIngresses, "argocd.ingresses": notIngresses}},
		"the default-deny read times out": {kubectlWith(defaultDenyRead, "sleep 10"), map[string]any{"defaultDenyPolicies": timedOut}},
		"a cluster without the Barman Cloud plugin, whose API server knows no ObjectStore and whose operator namespace holds no plugin Service": {
			map[string]string{"kubectl": kubectlAnswering(map[string]string{
				objectStoresRead: `echo "error: the server doesn't have a resource type \"objectstores\"" >&2; exit 1`,
				storageRead:      `jq 'del(.items[] | select(.metadata.labels["cnpg.io/pluginName"]))' testdata/storage-and-services.json`})},
			map[string]any{"cloudNativePGObjectStores": noObjectStores, "cloudNativePGPlugins": []any{}, "cloudNativePG.0.kind": "Cluster"}},
		"the workload read is forbidden, so no CronJob or Job is listed": {kubectlWith(workloadsRead, `printf 'Error from server (Forbidden): cronjobs.batch is forbidden: User cannot list resource "cronjobs"\n' >&2; exit 1`),
			map[string]any{"scheduledJobs": failed(1, "Error from server (Forbidden): cronjobs.batch is forbidden: User cannot list resource \"cronjobs\"\n")}},
		"the ObjectStore read prints no JSON": {kubectlWith(objectStoresRead, "echo 'not json'"), map[string]any{"cloudNativePGObjectStores": notObjectStores}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, tc.fakes, "REPORT_READ_SECONDS=1")

			require.NoError(t, err, out)
			assert.Len(t, posted.body["nodes"], 3)
			assert.Len(t, testutil.Field(t, posted.body, "argocd", "applications"), 3)
			assert.Equal(t, everyCheck, posted.body["checks"])
			for member, want := range tc.want {
				assert.Equal(t, want, testutil.Field(t, posted.body, strings.Split(member, ".")...), member)
			}
		})
	}
}

func TestReport_ListsEachLabelledCronJobWithItsNewestFailureSinceItsLastSuccessAndCutsAMessageAt256BytesWholeCharacters(t *testing.T) {
	_, posted, out, err := runReport(t, kubectlWith(workloadsRead, `jq -s '.[0].items += .[1] | .[0]' testdata/workloads.json testdata/scheduled-jobs.json`))

	require.NoError(t, err, out)
	assert.Equal(t, readJSON(t, "testdata", "golden", "scheduled-jobs.json"), posted.body["scheduledJobs"], "a failure older than or at the last success gives no field, a newer one gives it, a CronJob with no success yet gives its one failure, an older failure shows beside a running Job, a failed counter alone and a same-named CronJob in another namespace name nothing, a tie goes to the greater Job name, and two CronJobs of one namespace keep their own Jobs")
	for _, c := range posted.body["scheduledJobs"].(map[string]any)["cronJobs"].([]any) {
		failure, _ := c.(map[string]any)["failedSinceSuccess"].(map[string]any)
		message, _ := failure["message"].(string)
		assert.True(t, len(message) <= 256 && utf8.ValidString(message), "a message is at most 256 bytes and whole characters")
	}
}

func TestReport_SaysWhenItCutsTheScheduledCronJobsAtTheirBound(t *testing.T) {
	for _, n := range []int{100, 101} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			read := fmt.Sprintf(`jq '.items += [range(%d) | {kind: "CronJob", metadata: {namespace: "app-\(.)", name: "dump", labels: {"mitstack.dev/subscription": "sub_x"}}},
				{kind: "Job", metadata: {namespace: "app-\(.)", name: "dump-run", ownerReferences: [{kind: "CronJob", name: "dump"}]}, status: {conditions: [{type: "Failed", status: "True", message: ("m" * 300)}]}}]' testdata/workloads.json`, n)

			_, posted, out, err := runReport(t, kubectlWith(workloadsRead, read))

			require.NoError(t, err, out)
			jobs := posted.body["scheduledJobs"].(map[string]any)
			assert.LessOrEqual(t, len(posted.raw), 256<<10, "a full list of failures leaves the body inside the platform's limit")
			assert.Equal(t, n > 100, jobs["cut"])
			assert.EqualValues(t, n, jobs["found"])
			assert.Len(t, jobs["cronJobs"], min(n, 100))
		})
	}
}

func TestReport_ThatCannotReadPostsOneReportNamingTheReadAndExitsNonZero(t *testing.T) {
	cases := map[string]struct {
		fakes map[string]string
		want  map[string]any
	}{
		"the nodes read fails":         {kubectlWith(nodesRead, failedRead), map[string]any{"read": "kubectl get nodes", "exitStatus": float64(3)}},
		"the applications read fails":  {kubectlWith(applicationsRead, failedRead), map[string]any{"read": "kubectl get applications", "exitStatus": float64(3)}},
		"the key set read fails":       {kubectlWith(keySetRead, failedRead), map[string]any{"read": "kubectl get --raw /openid/v1/jwks", "exitStatus": float64(3)}},
		"the etcd members read fails":  {map[string]string{"talosctl": failedRead}, map[string]any{"read": "talosctl etcd members", "exitStatus": float64(3)}},
		"the key set read times out":   {kubectlWith(keySetRead, "sleep 10"), map[string]any{"read": "kubectl get --raw /openid/v1/jwks", "timedOut": true}},
		"jq cannot build the body":     {kubectlWith(nodesRead, "echo 'not json'"), map[string]any{"read": "body", "exitStatus": float64(2)}},
		"the token request fails":      {kubectlWith(tokenRequest, failedRead), map[string]any{"read": "kubectl create token", "exitStatus": float64(3)}},
		"the proof script prints none": {kubectlWith(tokenRequest, "true"), map[string]any{"read": "kubectl create token", "exitStatus": float64(0)}},
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

func TestManifests_LetsTheReportListPodsEachKindItReadsAndCloudNativePGsObjectsAndMintATokenForItsOwnServiceAccountAloneAndGrantsNoVerbOnSecrets(t *testing.T) {
	docs := testutil.Documents(t, manifests(t, reportURL))

	roles := testutil.Kind(docs, "ClusterRole")
	tokenRoles, bindings := inNamespace(t, testutil.Kind(docs, "Role"), "mitstack-node"), inNamespace(t, testutil.Kind(docs, "RoleBinding"), "mitstack-node")

	require.Len(t, tokenRoles, 1)
	require.Len(t, bindings, 1)
	assert.Equal(t, "mitstack-node", testutil.Field(t, tokenRoles[0], "metadata", "namespace"))
	assert.Equal(t, []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"serviceaccounts/token"},
		"resourceNames": []any{"mitstack-node-report"}, "verbs": []any{"create"}}}, testutil.Field(t, tokenRoles[0], "rules"))
	assert.Equal(t, testutil.Field(t, tokenRoles[0], "metadata", "name"), testutil.Field(t, bindings[0], "roleRef", "name"))
	assert.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "mitstack-node-report", "namespace": "mitstack-node"}},
		testutil.Field(t, bindings[0], "subjects"))
	rules := testutil.Field(t, named(t, roles, "ClusterRole", "mitstack-node-report"), "rules").([]any)
	assert.ElementsMatch(t, []any{
		map[string]any{"apiGroups": []any{""}, "resources": []any{"nodes"}, "verbs": []any{"get", "list"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"namespaces", "serviceaccounts", "services", "resourcequotas", "limitranges"},
			"verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"apps"}, "resources": []any{"deployments", "statefulsets", "daemonsets"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"batch"}, "resources": []any{"jobs", "cronjobs"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"networking.k8s.io"}, "resources": []any{"ingresses", "networkpolicies"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"rbac.authorization.k8s.io"}, "resources": []any{"roles", "rolebindings"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"argoproj.io"}, "resources": []any{"applications"}, "verbs": []any{"get", "list"}},
		map[string]any{"apiGroups": []any{"argoproj.io"}, "resources": []any{"appprojects"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"external-secrets.io"}, "resources": []any{"externalsecrets"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"postgresql.cnpg.io"}, "resources": []any{"clusters", "databases", "databaseroles"},
			"verbs": []any{"get", "list"}},
		map[string]any{"apiGroups": []any{"barmancloud.cnpg.io"}, "resources": []any{"objectstores"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"persistentvolumeclaims"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{"storage.k8s.io"}, "resources": []any{"storageclasses"}, "verbs": []any{"list"}},
	}, rules)
	for _, rule := range rules {
		resources := testutil.Field(t, rule, "resources").([]any)
		assert.NotContains(t, resources, "secrets", "rule %v grants a verb on secrets", rule)
		assert.NotContains(t, resources, "*", "rule %v grants a verb on every resource, secrets among them", rule)
	}
}

func TestManifests_RefusesAReportURLThatIsNotHTTPS(t *testing.T) {
	_, err := report.Manifests(inputs("http://api.mitstack.dev/v1/cluster-reports"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "http://api.mitstack.dev/v1/cluster-reports")
}

func TestManifests_RefusesInputsWithoutAProofScript(t *testing.T) {
	_, err := report.Manifests(report.Inputs{URL: "https://api.mitstack.dev/v1/cluster-reports", SubscriptionID: "sub_0123456789ab", ToolsImage: testutil.ToolsImage})

	require.ErrorContains(t, err, "proof script")
}

func TestManifests_RefusesAProofScriptOneByteOverItsLimit(t *testing.T) {
	in := inputs("https://api.mitstack.dev/v1/cluster-reports")
	in.ProofScript = strings.Repeat("x", report.ProofScriptLimit+1)

	_, err := report.Manifests(in)

	require.ErrorContains(t, err, fmt.Sprintf("the proof script is %d bytes, over the %d a report takes", len(in.ProofScript), report.ProofScriptLimit))
}
