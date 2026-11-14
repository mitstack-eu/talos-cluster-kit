//go:build apiserver && collector

// The pinned Collector, run on the node copy's rendered configuration against a real API server, masks and tags a
// container's log line and sends it under the push token.
package talosclusterkit

import (
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	collectorSubscription = "sub_0123456789ab"
	shopPodLogs           = "app-shop_shop-web-7d9f4b6c8-x2kqp_8f2b1c4e-0d6a-4f7e-9b3c-2a1d5e6f7a8b/web"
	backendCADir          = "/etc/mitstack-collector-check"
	appShopNamespace      = "{apiVersion: v1, kind: Namespace, metadata: {name: app-shop, labels: {mitstack.dev/app: shop}}}"
)

// shipped is a log record the stand-in backend received, with its resource's attributes and its request's headers.
type shipped struct {
	body       string
	attributes map[string]string
	header     http.Header
	pushToken  string
}

func TestCollector_MasksAFixtureTokenInAContainerLogLineBeforeTheExporterSendsIt(t *testing.T) {
	token, password, email := "ghp_"+rand.Text(), rand.Text(), "jane.doe@example.com"

	got := shipContainerLogLine(t, "paid order 4711 for "+email+" with "+token+" password="+password)

	for _, secret := range []string{token, password, email} {
		assert.NotContains(t, got.body, secret, "the exporter sent a value the redaction masks")
	}
	assert.Contains(t, got.body, "****", "the mask")
	assert.Contains(t, got.body, "paid order 4711 for", "the rest of the line")
}

func TestCollector_TagsAContainerLogLineWithItsNamespacesAppAndSendsThePushTokenUnderTheHeader(t *testing.T) {
	got := shipContainerLogLine(t, "GET /basket 200")

	assert.Equal(t, "shop", got.attributes["mitstack.app"], "the app its namespace's mitstack.dev/app label names")
	assert.Equal(t, "app-shop", got.attributes["k8s.namespace.name"])
	assert.Equal(t, collectorSubscription, got.attributes["mitstack.subscription.id"])
	assert.Equal(t, got.pushToken, got.header.Get(testutil.TelemetryExport().TokenHeader), "the push token, bare, under the resource's header")
	assert.Empty(t, got.header.Get("Authorization"))
}

func TestCollector_ThePinnedCollectorAcceptsEachCopysRenderedConfiguration(t *testing.T) {
	docs := testutil.Documents(t, renderedCollector(t, testutil.TelemetryExport()))
	image := testutil.Field(t, collectorPod(t, docs), "containers", "0", "image").(string)
	ca, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{netip.MustParseAddr("10.0.0.4")})
	require.NoError(t, err)
	account := sharedDir(t)
	writeFiles(t, account, map[string][]byte{"ca.crt": ca.Cert, "token": []byte(rand.Text()), "namespace": []byte("kube-system")})
	for _, which := range []string{"node", "cluster"} {
		t.Run(which, func(t *testing.T) {
			dir := sharedDir(t)
			writeFiles(t, dir, map[string][]byte{"config.yaml": []byte(collectorConfig(t, docs, which))})

			out, err := exec.Command("docker", "run", "--rm", "-e", "K8S_NODE_IP=10.0.0.4", "-e", "K8S_NODE_NAME=shop-node-fsn1",
				"-v", account+":"+serviceAccountDir+":ro", "-v", dir+":/conf:ro", image, "validate", "--config=/conf/config.yaml").CombinedOutput()

			require.NoError(t, err, "the %s copy's configuration:\n%s", which, out)
		})
	}
}

// shipContainerLogLine runs the node copy as its DaemonSet runs it, writes message as a containerd log line of a pod in
// app-shop, and returns the record the stand-in backend received for it.
func shipContainerLogLine(t *testing.T, message string) shipped {
	t.Helper()
	server := startAPIServer(t)
	url, caDir, received := startOTLPBackend(t, bridgeGateway(t, server.etcd))
	export := testutil.TelemetryExport()
	export.MetricsEndpoint, export.LogsEndpoint = url+"/otlp/v1/metrics", url+"/otlp/v1/logs"
	rendered := renderedCollector(t, export)
	server.admin(t, builtInObjects(t, rendered), "apply", "-f", "-")
	server.admin(t, []byte(appShopNamespace), "apply", "-f", "-")

	logs := sharedDir(t)
	podLogs := filepath.Join(logs, shopPodLogs)
	require.NoError(t, os.MkdirAll(podLogs, 0o755))
	logFile := filepath.Join(podLogs, "0.log")
	require.NoError(t, os.WriteFile(logFile, []byte(containerdLine("a line the Collector finds when it starts")), 0o644))
	pushToken := rand.Text()
	collector := startCollector(t, server, testutil.Documents(t, rendered), logs, caDir, pushToken)
	collector.await(t, time.Minute, "Started watching file", "/var/log/pods/"+shopPodLogs+"/0.log")

	marker := rand.Text()
	file, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString(containerdLine(message + " line=" + marker))
	require.NoError(t, errors.Join(err, file.Close()))

	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	for {
		select {
		case got := <-received:
			if strings.Contains(got.body, marker) {
				got.pushToken = pushToken
				return got
			}
		case <-deadline.C:
			require.FailNow(t, "the backend received no record of the line in a minute", "the Collector printed:\n%s", collector.log())
		}
	}
}

func containerdLine(text string) string {
	return time.Now().UTC().Format(time.RFC3339Nano) + " stdout F " + text + "\n"
}

func renderedCollector(t *testing.T, export components.TelemetryExport) []byte {
	t.Helper()
	manifests, err := components.TelemetryCollector(export, collectorSubscription)
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	return manifests[0].Bytes
}

func renderedObject(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	of := testutil.Kind(docs, kind)
	i := slices.IndexFunc(of, func(doc map[string]any) bool { return testutil.Field(t, doc, "metadata", "name") == name })
	require.GreaterOrEqual(t, i, 0, "no %s %s", kind, name)
	return of[i]
}

func collectorPod(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	return testutil.Field(t, renderedObject(t, docs, "DaemonSet", "mitstack-telemetry-node"), "spec", "template", "spec").(map[string]any)
}

func collectorConfig(t *testing.T, docs []map[string]any, which string) string {
	t.Helper()
	return testutil.Field(t, renderedObject(t, docs, "ConfigMap", "mitstack-telemetry-"+which), "data", "config.yaml").(string)
}

// bridgeGateway is the address at which a container on container's network reaches this host.
func bridgeGateway(t *testing.T, container string) netip.Addr {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}", container).CombinedOutput()
	require.NoError(t, err, "%s", out)
	address, err := netip.ParseAddr(strings.TrimSpace(string(out)))
	require.NoError(t, err, "the gateway of %s's network", container)
	return address
}

// startOTLPBackend serves OTLP over HTTPS at address, and returns its URL, a directory holding the ca.crt that signed its
// certificate, and each log record it receives.
func startOTLPBackend(t *testing.T, address netip.Addr) (url, caDir string, records <-chan shipped) {
	t.Helper()
	ca, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{address})
	require.NoError(t, err)
	serving, err := ca.Issue(address, time.Now(), time.Hour)
	require.NoError(t, err)
	certificate, err := tls.X509KeyPair(serving.Cert, serving.Key)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", netip.AddrPortFrom(address, 0).String())
	require.NoError(t, err)
	received := make(chan shipped, 256)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/otlp/v1/logs" {
			sendRecords(t, r, received)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener = listener
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	caDir = sharedDir(t)
	writeFiles(t, caDir, map[string][]byte{"ca.crt": ca.Cert})
	return srv.URL, caDir, received
}

func sendRecords(t *testing.T, r *http.Request, received chan<- shipped) {
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		unzipped, err := gzip.NewReader(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		body = unzipped
	}
	data, err := io.ReadAll(body)
	var request collogspb.ExportLogsServiceRequest
	if !assert.NoError(t, err) || !assert.NoError(t, proto.Unmarshal(data, &request)) {
		return
	}
	for _, resourceLogs := range request.GetResourceLogs() {
		attributes := map[string]string{}
		for _, kv := range resourceLogs.GetResource().GetAttributes() {
			attributes[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			for _, record := range scopeLogs.GetLogRecords() {
				select {
				case received <- shipped{body: record.GetBody().GetStringValue(), attributes: attributes, header: r.Header.Clone()}:
				default:
				}
			}
		}
	}
}

// runningCollector is the node copy's container and everything it has printed.
type runningCollector struct {
	mu      sync.Mutex
	output  []string
	printed chan struct{}
	stopped chan struct{}
}

// startCollector runs the node copy's container as its DaemonSet runs it, with logs as the node's pod logs, pushToken
// where its token Secret mounts and the backend's CA from caDir.
func startCollector(t *testing.T, server *apiServer, docs []map[string]any, logs, caDir, pushToken string) *runningCollector {
	t.Helper()
	pod := collectorPod(t, docs)
	container := testutil.Field(t, pod, "containers", "0")
	config, token := sharedDir(t), sharedDir(t)
	writeFiles(t, config, map[string][]byte{"config.yaml": []byte(collectorConfig(t, docs, "node"))})
	writeFiles(t, token, map[string][]byte{"token": []byte(pushToken)})
	sources := map[string]string{"config": config, "pods": logs, "token": token}
	name := server.etcd + "-collector"
	args := []string{"run", "-d", "--name", name, "--network", "container:" + server.etcd,
		"-v", server.serviceAccount(t, "kube-system", testutil.Field(t, pod, "serviceAccountName").(string)) + ":" + serviceAccountDir + ":ro",
		"-v", caDir + ":" + backendCADir + ":ro", "-e", "SSL_CERT_FILE=" + backendCADir + "/ca.crt",
		"-e", "KUBERNETES_SERVICE_HOST=127.0.0.1", "-e", "KUBERNETES_SERVICE_PORT=" + apiServerPort,
		"-e", "K8S_NODE_NAME=shop-node-fsn1", "-e", "K8S_NODE_IP=127.0.0.1"}
	for _, mount := range testutil.Field(t, container, "volumeMounts").([]any) {
		volume := testutil.Field(t, mount, "name").(string)
		source, ok := sources[volume]
		require.True(t, ok, "the node copy mounts the volume %s, which this check has no stand-in for", volume)
		args = append(args, "-v", source+":"+testutil.Field(t, mount, "mountPath").(string)+":ro")
	}
	args = append(args, testutil.Field(t, container, "image").(string))
	for _, arg := range testutil.Field(t, container, "args").([]any) {
		args = append(args, arg.(string))
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	docker(t, args...)

	c := &runningCollector{printed: make(chan struct{}, 1), stopped: make(chan struct{})}
	follow := exec.Command("docker", "logs", "--follow", name)
	reader, writer := io.Pipe()
	follow.Stdout, follow.Stderr = writer, writer
	require.NoError(t, follow.Start())
	t.Cleanup(func() { _ = follow.Process.Kill() })
	go func() { _ = writer.CloseWithError(follow.Wait()) }()
	go func() {
		defer close(c.stopped)
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			c.mu.Lock()
			c.output = append(c.output, scanner.Text())
			c.mu.Unlock()
			select {
			case c.printed <- struct{}{}:
			default:
			}
		}
	}()
	return c
}

// await returns once the Collector prints a line holding every one of texts, and fails if it stops or within passes first.
func (c *runningCollector) await(t *testing.T, within time.Duration, texts ...string) {
	t.Helper()
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	for {
		if c.hasPrinted(texts) {
			return
		}
		select {
		case <-c.printed:
		case <-c.stopped:
			if !c.hasPrinted(texts) {
				require.FailNow(t, "the Collector stopped", "waiting for %q it printed:\n%s", texts, c.log())
			}
			return
		case <-deadline.C:
			require.FailNow(t, "the Collector never printed what was awaited", "waiting %s for %q it printed:\n%s", within, texts, c.log())
		}
	}
}

func (c *runningCollector) hasPrinted(texts []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.ContainsFunc(c.output, func(line string) bool {
		return !slices.ContainsFunc(texts, func(text string) bool { return !strings.Contains(line, text) })
	})
}

func (c *runningCollector) log() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.output, "\n")
}
