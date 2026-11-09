//go:build apiserver

// The outside probe and the security scan write their ConfigMaps from the tools image, as their CronJobs run them,
// through an API server at the Kubernetes release the kit pins that enforces the RBAC report.Manifests renders.
package talosclusterkit

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	apiServerPort = "6443"
	// serviceAccountDir is where a pod finds its service account's token, CA and namespace, as kubectl reads them.
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	// otherConfigMap stands for each ConfigMap in kube-system that neither script writes.
	otherConfigMap = "mitstack-apiserver-check-other"
)

func TestAPIServer_TheOutsideProbeAndTheSecurityScanWriteTheirOwnConfigMapAndNoOtherUnderTheRenderedRBAC(t *testing.T) {
	rendered, err := report.Manifests(report.Inputs{URL: "https://api.mitstack.dev/v1/cluster-reports", SubscriptionID: "sub_0123456789ab",
		ToolsImage: testutil.ToolsImage, ProofScript: "echo report-proof\n"})
	require.NoError(t, err)
	server := startAPIServer(t)
	server.admin(t, builtInObjects(t, rendered), "apply", "-f", "-")
	server.admin(t, nil, "create", "configmap", otherConfigMap, "-n", "kube-system")

	t.Run("the outside probe writes its result into mitstack-mesh-probe", func(t *testing.T) {
		out, err := server.runCronJob(t, rendered, "mitstack-mesh-probe")

		require.NoError(t, err, "the outside probe printed:\n%s", out)
		var result map[string]any
		require.NoError(t, json.Unmarshal([]byte(server.configMapData(t, "mitstack-mesh-probe")["result"]), &result))
		assert.ElementsMatch(t, []string{"probedAt", "plainCall", "handshake", "gateways"}, slices.Collect(maps.Keys(result)))
	})

	t.Run("the security scan writes both summaries and its outcome into mitstack-security-scans when kube-bench left none", func(t *testing.T) {
		out, err := server.runCronJob(t, rendered, "mitstack-security-scan")

		data := server.configMapData(t, "mitstack-security-scans")
		require.Contains(t, data, "last-run", "the security scan printed:\n%s", out)
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit, "the security scan printed:\n%s", out)
		assert.Equal(t, 1, exit.ExitCode(), "the scan's exit status, which makes the Job run it again")
		assert.Equal(t, "the kube-bench step left no summary", summary(t, data, "kube-bench")["error"])
		assert.Equal(t, "kubescape", summary(t, data, "kubescape")["tool"])
		assert.Equal(t, float64(1), summary(t, data, "last-run")["exitStatus"])
		assert.Regexp(t, `^kube-bench: failed; kubescape: (ran|failed)$`, summary(t, data, "last-run")["output"])
	})

	get := func(name string) []string { return []string{"get", "configmap", name, "-n", "kube-system"} }
	patch := func(name string) []string {
		return []string{"patch", "configmap", name, "-n", "kube-system", "--type", "merge", "-p", `{"data":{"written":"yes"}}`}
	}
	list := []string{"get", "configmaps", "-n", "kube-system"}
	for _, tc := range []struct {
		name, account string
		request       []string
	}{
		{"the outside probe lists kube-system's ConfigMaps", "mitstack-mesh-probe", list},
		{"the outside probe reads another ConfigMap", "mitstack-mesh-probe", get(otherConfigMap)},
		{"the outside probe patches another ConfigMap", "mitstack-mesh-probe", patch(otherConfigMap)},
		{"the outside probe patches the security scan's ConfigMap", "mitstack-mesh-probe", patch("mitstack-security-scans")},
		{"the security scan, which may read every ConfigMap for kubescape, patches another", "mitstack-security-scan", patch(otherConfigMap)},
		{"the security scan patches the outside probe's ConfigMap", "mitstack-security-scan", patch("mitstack-mesh-probe")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := server.as(t, tc.account, nil, append([]string{"kubectl"}, tc.request...)...)

			require.Error(t, err, "kubectl %v as %s printed:\n%s", tc.request, tc.account, out)
			assert.Contains(t, out, "(Forbidden)", "kubectl %v as %s", tc.request, tc.account)
		})
	}
}

func summary(t *testing.T, data map[string]string, key string) map[string]any {
	t.Helper()
	var s map[string]any
	require.NoError(t, json.Unmarshal([]byte(data[key]), &s), "the %s summary: %s", key, data[key])
	return s
}

// apiServer is an etcd and a kube-apiserver from the images the kit pins for Talos, the API server in etcd's network
// namespace, which every container the test runs joins, so each reaches the API server at 127.0.0.1.
type apiServer struct {
	toolsImage string
	etcd       string
	keys       string
}

func startAPIServer(t *testing.T) *apiServer {
	t.Helper()
	toolsImage := os.Getenv("TOOLS_IMAGE")
	require.NotEmpty(t, toolsImage, "TOOLS_IMAGE names the tools image to run, as the tools image CI job builds it")
	s := &apiServer{toolsImage: toolsImage, etcd: "mitstack-apiserver-check-" + strings.ToLower(rand.Text()[:8]), keys: sharedDir(t)}
	s.writeKeys(t)
	apiServer := s.etcd + "-apiserver"
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", apiServer, s.etcd).Run() })

	docker(t, "run", "-d", "--rm", "--name", s.etcd, "--tmpfs", "/var/lib/etcd", talosImage(t, "registry.k8s.io/etcd"),
		"/usr/local/bin/etcd", "--data-dir", "/var/lib/etcd",
		"--listen-client-urls", "http://127.0.0.1:2379", "--advertise-client-urls", "http://127.0.0.1:2379")
	docker(t, "run", "-d", "--rm", "--name", apiServer, "--network", "container:"+s.etcd, "-v", s.keys+":/keys:ro",
		"--entrypoint", "/usr/local/bin/kube-apiserver", talosImage(t, "registry.k8s.io/kube-apiserver"),
		"--etcd-servers", "http://127.0.0.1:2379", "--secure-port", apiServerPort, "--authorization-mode", "RBAC",
		"--token-auth-file", "/keys/tokens.csv", "--tls-cert-file", "/keys/serving.crt", "--tls-private-key-file", "/keys/serving.key",
		"--service-account-issuer", "https://kubernetes.default.svc", "--service-account-key-file", "/keys/serving.key",
		"--service-account-signing-key-file", "/keys/serving.key", "--service-cluster-ip-range", "10.96.0.0/16")
	ready := func() bool { return s.adminCommand(nil, "get", "--raw", "/readyz").Run() == nil }
	if !assert.Eventually(t, ready, 2*time.Minute, 2*time.Second) {
		logs, _ := exec.Command("docker", "logs", apiServer).CombinedOutput()
		require.FailNow(t, "the API server never answered /readyz", "%s", logs)
	}
	return s
}

// writeKeys writes a CA, the API server's certificate for 127.0.0.1, whose key also signs service account tokens, and
// the kubeconfig of an admin the API server knows by a static token.
func (s *apiServer) writeKeys(t *testing.T) {
	t.Helper()
	loopback := netip.MustParseAddr("127.0.0.1")
	ca, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{loopback})
	require.NoError(t, err)
	serving, err := ca.Issue(loopback, time.Now(), time.Hour)
	require.NoError(t, err)
	token := rand.Text()
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters: [{name: check, cluster: {server: "https://127.0.0.1:%s", certificate-authority: /keys/ca.crt}}]
users: [{name: admin, user: {token: %s}}]
contexts: [{name: check, context: {cluster: check, user: admin}}]
current-context: check
`, apiServerPort, token)
	writeFiles(t, s.keys, map[string][]byte{"ca.crt": ca.Cert, "serving.crt": serving.Cert, "serving.key": serving.Key,
		"tokens.csv": []byte(token + ",admin,admin,system:masters\n"), "admin.kubeconfig": []byte(kubeconfig)})
}

func (s *apiServer) adminCommand(stdin []byte, args ...string) *exec.Cmd {
	cmd := exec.Command("docker", append([]string{"run", "-i", "--rm", "--network", "container:" + s.etcd,
		"-v", s.keys + ":/keys:ro", s.toolsImage, "kubectl", "--kubeconfig", "/keys/admin.kubeconfig"}, args...)...)
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd
}

// admin runs kubectl in the tools image as the cluster's admin and returns what it printed on stdout.
func (s *apiServer) admin(t *testing.T, stdin []byte, args ...string) []byte {
	t.Helper()
	var stderr bytes.Buffer
	cmd := s.adminCommand(stdin, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "kubectl %v as the admin: %s", args, stderr.String())
	return out
}

func (s *apiServer) configMapData(t *testing.T, name string) map[string]string {
	t.Helper()
	var configMap struct{ Data map[string]string }
	require.NoError(t, json.Unmarshal(s.admin(t, nil, "get", "configmap", name, "-n", "kube-system", "-o", "json"), &configMap))
	return configMap.Data
}

// as runs command in the tools image as account in kube-system, whose token sits where a pod finds it.
func (s *apiServer) as(t *testing.T, account string, dockerArgs []string, command ...string) (string, error) {
	t.Helper()
	args := []string{"run", "--rm", "--network", "container:" + s.etcd, "-v", s.serviceAccount(t, account) + ":" + serviceAccountDir + ":ro",
		"-e", "KUBERNETES_SERVICE_HOST=127.0.0.1", "-e", "KUBERNETES_SERVICE_PORT=" + apiServerPort}
	args = append(append(append(args, dockerArgs...), s.toolsImage), command...)
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// serviceAccount is a directory holding a token the API server issues to account in kube-system, its CA and namespace.
func (s *apiServer) serviceAccount(t *testing.T, account string) string {
	t.Helper()
	ca, err := os.ReadFile(filepath.Join(s.keys, "ca.crt"))
	require.NoError(t, err)
	dir := sharedDir(t)
	writeFiles(t, dir, map[string][]byte{"token": s.admin(t, nil, "create", "token", account, "-n", "kube-system"),
		"ca.crt": ca, "namespace": []byte("kube-system")})
	return dir
}

// runCronJob runs the CronJob name's first container as its pod runs it: as its user, on a read-only root with a tmpfs
// at each emptyDir it mounts, with its environment and command and its service account's token.
func (s *apiServer) runCronJob(t *testing.T, rendered []byte, name string) (string, error) {
	t.Helper()
	jobs := testutil.Kind(testutil.Documents(t, rendered), "CronJob")
	i := slices.IndexFunc(jobs, func(job map[string]any) bool { return testutil.Field(t, job, "metadata", "name") == name })
	require.GreaterOrEqual(t, i, 0, "no CronJob %s", name)
	job := jobs[i]
	pod := testutil.Field(t, job, "spec", "jobTemplate", "spec", "template", "spec")
	container := testutil.Field(t, pod, "containers", "0")
	user := testutil.Field(t, container, "securityContext", "runAsUser")
	if user == nil {
		user = testutil.Field(t, pod, "securityContext", "runAsUser")
	}
	args := []string{"--user", fmt.Sprint(user)}
	if testutil.Field(t, container, "securityContext", "readOnlyRootFilesystem") == true {
		args = append(args, "--read-only")
	}
	for _, path := range emptyDirMounts(t, pod, container) {
		args = append(args, "--tmpfs", path)
	}
	for _, env := range testutil.Field(t, container, "env").([]any) {
		args = append(args, "-e", envVariable(t, env, testutil.Field(t, job, "metadata", "namespace").(string)))
	}
	var command []string
	for _, word := range testutil.Field(t, container, "command").([]any) {
		command = append(command, word.(string))
	}
	return s.as(t, testutil.Field(t, pod, "serviceAccountName").(string), args, command...)
}

func emptyDirMounts(t *testing.T, pod, container any) []string {
	t.Helper()
	emptyDirs := map[any]bool{}
	for _, volume := range testutil.Field(t, pod, "volumes").([]any) {
		_, isEmptyDir := volume.(map[string]any)["emptyDir"]
		emptyDirs[testutil.Field(t, volume, "name")] = isEmptyDir
	}
	var paths []string
	for _, mount := range testutil.Field(t, container, "volumeMounts").([]any) {
		if emptyDirs[testutil.Field(t, mount, "name")] {
			paths = append(paths, testutil.Field(t, mount, "mountPath").(string))
		}
	}
	return paths
}

// envVariable is one variable of a container's environment as the kubelet sets it, reading a field reference from the
// pod's namespace, the one field the kit's CronJobs read.
func envVariable(t *testing.T, env any, namespace string) string {
	t.Helper()
	name := testutil.Field(t, env, "name").(string)
	if _, isReference := env.(map[string]any)["valueFrom"]; isReference {
		require.Equal(t, "metadata.namespace", testutil.Field(t, env, "valueFrom", "fieldRef", "fieldPath"), "the field %s reads", name)
		return name + "=" + namespace
	}
	return name + "=" + fmt.Sprint(testutil.Field(t, env, "value"))
}

// builtInObjects are the objects of rendered in a built-in API group, in Talos's order, which leaves out each kind whose
// CRD the bare API server lacks, after a Namespace for each namespace they name that another manifest creates.
func builtInObjects(t *testing.T, rendered []byte) []byte {
	t.Helper()
	objects := testutil.TalosOrder(t, map[string][]byte{"mitstack-node-report": rendered})
	created := map[string]bool{}
	for _, o := range objects {
		if o.Doc["kind"] == "Namespace" {
			created[testutil.Field(t, o.Doc, "metadata", "name").(string)] = true
		}
	}
	var out bytes.Buffer
	write := func(doc map[string]any) {
		data, err := yaml.Marshal(doc)
		require.NoError(t, err)
		out.WriteString("---\n")
		out.Write(data)
	}
	for _, o := range objects {
		if namespace, _ := o.Doc["metadata"].(map[string]any)["namespace"].(string); namespace != "" && !created[namespace] {
			created[namespace] = true
			write(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace}})
		}
	}
	for _, o := range objects {
		if group, _, _ := strings.Cut(o.Doc["apiVersion"].(string), "/"); !strings.Contains(group, ".") || strings.HasSuffix(group, ".k8s.io") {
			write(o.Doc)
		}
	}
	return out.Bytes()
}

// talosImage is the image of name that the kit pins for Talos to run.
func talosImage(t *testing.T, name string) string {
	t.Helper()
	images := components.Default().TalosImages
	i := slices.IndexFunc(images, func(image components.Image) bool { return image.Name == name })
	require.GreaterOrEqual(t, i, 0, "the kit pins no %s", name)
	return images[i].Ref()
}

func docker(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	require.NoError(t, err, "docker %v: %s", args, out)
}

// sharedDir is a directory that every container can read, whatever user it runs as.
func sharedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o755))
	return dir
}

func writeFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	}
}
