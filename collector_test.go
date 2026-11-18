//go:build apiserver && collector

// The pinned Collector, run on its rendered configuration against a real API server, masks and tags a container's log
// line and what a pod pushes, sends it under the push token and reports each target it cannot scrape, and an OpenBao
// member answers its scrape.
package talosclusterkit

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
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
	appOtherNamespace     = "{apiVersion: v1, kind: Namespace, metadata: {name: app-other, labels: {mitstack.dev/app: other}}}"
	// shopPod is bound to the node the node copy serves, where no kubelet runs, so the check patches its address in.
	shopPod = `{apiVersion: v1, kind: Pod, metadata: {name: shop-web, namespace: app-shop}, spec: {nodeName: shop-node-fsn1,
  automountServiceAccountToken: false, containers: [{name: web, image: registry.example/shop:1}]}}`
	pushedMetric = "shop.orders.paid"
	pushedEmail  = "jane.doe@example.com"
)

// scraped is one sample of a scrape's up series the backend received, with its job and data point attributes.
type scraped struct {
	job        string
	attributes map[string]string
	value      float64
}

type shipped struct {
	body             string
	attributes       map[string]string
	recordAttributes map[string]string
	header           http.Header
	pushToken        string
}

// pushedPoint is one data point of pushedMetric the backend received, with its resource and its own attributes.
type pushedPoint struct {
	resource, attributes map[string]string
}

// otherApp is what a sender writes to pass its records off as another app's, on the resource and on each record.
var otherApp = map[string]string{"mitstack.app": "other", "mitstack_app": "other", "mitstack-app": "other", "k8s.namespace.name": "app-other",
	"k8s.pod.name": "other-web"}

func TestCollector_TagsAMetricAndALogRecordAPodPushesWithItsOwnNamespacesAppWhateverAppTheyClaimAndMasksThem(t *testing.T) {
	rig := startPushRig(t, true)
	marker := rand.Text()

	push(t, rig.receiver+"/v1/logs", pushedLogRecord(marker))
	push(t, rig.receiver+"/v1/metrics", pushedDataPoint())
	record, point := awaitRecord(t, rig.backend, rig.collector, marker), awaitPoint(t, rig.backend, rig.collector)

	for _, got := range []map[string]string{record.attributes, point.resource} {
		assert.Equal(t, "shop", got["mitstack.app"], "the app of the namespace the pushing pod runs in")
		assert.Equal(t, "app-shop", got["k8s.namespace.name"])
		assert.Equal(t, "shop-web", got["k8s.pod.name"])
		assert.Equal(t, collectorSubscription, got["mitstack.subscription.id"])
	}
	for _, got := range []map[string]string{record.attributes, record.recordAttributes, point.resource, point.attributes} {
		for _, claimed := range []string{"mitstack_app", "mitstack-app"} {
			assert.NotContains(t, got, claimed, "an app claimed under a name a backend stores as the label mitstack.app becomes")
		}
	}
	assert.NotContains(t, record.recordAttributes, "mitstack.app", "the log record keeps no app it claimed")
	assert.NotContains(t, record.recordAttributes, "k8s.namespace.name")
	assert.NotContains(t, record.body, pushedEmail, "a pushed record is masked like a container's log line")
	assert.Equal(t, "shop", point.attributes["mitstack.app"], "the data point carries the app its resource was tagged with")
	assert.Equal(t, "app-shop", point.attributes["k8s.namespace.name"])
	assert.Equal(t, "paid", point.attributes["outcome"], "an attribute the sender owns")
}

func TestCollector_DropsWhatAnAddressNoPodOnTheNodeHoldsPushesSoNoRecordLeavesWithoutAnApp(t *testing.T) {
	rig := startPushRig(t, false)
	marker := rand.Text()

	push(t, rig.receiver+"/v1/logs", pushedLogRecord(rand.Text()))
	push(t, rig.receiver+"/v1/metrics", pushedDataPoint())
	rig.pushFromTheNode(t, "/v1/logs", pushedLogRecord(marker))
	rig.pushFromTheNode(t, "/v1/metrics", pushedDataPoint())
	records := receivedUntil(t, rig.backend.records, rig.collector, func(got shipped) bool { return strings.Contains(got.body, marker) })
	points := receivedUntil(t, rig.backend.points, rig.collector, func(got pushedPoint) bool { return got.resource["mitstack.app"] == "shop" })

	for _, got := range records {
		assert.Equal(t, "shop", got.attributes["mitstack.app"], "every log record the backend received, of %q", got.body)
	}
	for _, got := range points {
		assert.Equal(t, "shop", got.resource["mitstack.app"], "every point the backend received")
	}
}

type pushRig struct {
	server    *apiServer
	backend   otlpBackend
	collector *followedContainer
	receiver  string
}

// startPushRig starts a node copy and its backend, with shop-web at this host's address when shopIsTheHost, and
// otherwise at the node copy's own address, which only a push from inside its network comes from.
func startPushRig(t *testing.T, shopIsTheHost bool) pushRig {
	t.Helper()
	server := startAPIServer(t)
	rig := pushRig{server: server}
	host, node := bridgeGateway(t, server.etcd), containerAddress(t, server.etcd)
	rig.backend = startOTLPBackend(t, host)
	export := testutil.TelemetryExport()
	export.MetricsEndpoint, export.LogsEndpoint = rig.backend.url+"/otlp/v1/metrics", rig.backend.url+"/otlp/v1/logs"
	rendered := renderedCollector(t, components.Default(), export)
	server.admin(t, builtInObjects(t, rendered), "apply", "-f", "-")
	server.admin(t, []byte(appShopNamespace+"\n---\n"+appOtherNamespace), "apply", "-f", "-")
	server.admin(t, nil, "create", "serviceaccount", "default", "-n", "app-shop")
	server.admin(t, []byte(shopPod), "apply", "-f", "-")
	shop := node
	if shopIsTheHost {
		shop = host
	}
	server.admin(t, nil, "patch", "pod", "shop-web", "-n", "app-shop", "--subresource=status", "--type=merge",
		"-p", `{"status":{"phase":"Running","podIP":"`+shop.String()+`","podIPs":[{"ip":"`+shop.String()+`"}]}}`)
	rig.collector = startCollector(t, server, testutil.Documents(t, rendered), "node", map[string]string{"pods": sharedDir(t)}, rig.backend.caDir, rand.Text())
	rig.collector.await(t, time.Minute, "Everything is ready")
	rig.receiver = "http://" + node.String() + ":4318"
	return rig
}

// pushFromTheNode pushes message to the receiver from a container sharing the node copy's network, so from its address.
func (r pushRig) pushFromTheNode(t *testing.T, path string, message proto.Message) {
	t.Helper()
	body, err := proto.Marshal(message)
	require.NoError(t, err)
	cmd := exec.Command("docker", "run", "-i", "--rm", "--network", "container:"+r.server.etcd, r.server.toolsImage,
		"curl", "-fsS", "-H", "Content-Type: application/x-protobuf", "--data-binary", "@-", r.receiver+path)
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the push to %s from the node copy's network: %s", path, out)
}

// receivedUntil returns what from delivers up to and including the first item last accepts.
func receivedUntil[T any](t *testing.T, from <-chan T, collector *followedContainer, last func(T) bool) []T {
	t.Helper()
	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	var got []T
	for {
		select {
		case item := <-from:
			if got = append(got, item); last(item) {
				return got
			}
		case <-deadline.C:
			require.FailNow(t, "the backend received no push from shop-web in a minute", "it received %d before; the Collector printed:\n%s", len(got), collector.log())
		}
	}
}

// containerAddress is container's own address on its network, which a pod sharing its network listens on.
func containerAddress(t *testing.T, container string) netip.Addr {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", container).CombinedOutput()
	require.NoError(t, err, "%s", out)
	address, err := netip.ParseAddr(strings.TrimSpace(string(out)))
	require.NoError(t, err, "the address of %s", container)
	return address
}

func claims(extra map[string]string) []*commonpb.KeyValue {
	var attributes []*commonpb.KeyValue
	for _, key := range slices.Sorted(maps.Keys(otherApp)) {
		attributes = append(attributes, stringAttribute(key, otherApp[key]))
	}
	for _, key := range slices.Sorted(maps.Keys(extra)) {
		attributes = append(attributes, stringAttribute(key, extra[key]))
	}
	return attributes
}

func stringAttribute(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func pushedLogRecord(marker string) proto.Message {
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: claims(map[string]string{"service.name": "shop-web"})},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: uint64(time.Now().UnixNano()),
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "order paid by " + pushedEmail + " line=" + marker}},
			Attributes:   claims(nil),
		}}}},
	}}}
}

func pushedDataPoint() proto.Message {
	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: claims(map[string]string{"service.name": "shop-web"})},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Name: pushedMetric,
			Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
				TimeUnixNano: uint64(time.Now().UnixNano()),
				Value:        &metricspb.NumberDataPoint_AsInt{AsInt: 1},
				Attributes:   claims(map[string]string{"outcome": "paid"}),
			}}}},
		}}}},
	}}}
}

func push(t *testing.T, address string, message proto.Message) {
	t.Helper()
	body, err := proto.Marshal(message)
	require.NoError(t, err)
	response, err := http.Post(address, "application/x-protobuf", bytes.NewReader(body))
	require.NoError(t, err, "the push to %s", address)
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, "the push to %s answered %s", address, answer)
}

func awaitRecord(t *testing.T, backend otlpBackend, collector *followedContainer, marker string) shipped {
	t.Helper()
	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	for {
		select {
		case got := <-backend.records:
			if strings.Contains(got.body, marker) {
				return got
			}
		case <-deadline.C:
			require.FailNow(t, "the backend received no record of the pushed line in a minute", "the Collector printed:\n%s", collector.log())
		}
	}
}

func awaitPoint(t *testing.T, backend otlpBackend, collector *followedContainer) pushedPoint {
	t.Helper()
	select {
	case got := <-backend.points:
		return got
	case <-time.After(time.Minute):
		require.FailNow(t, "the backend received no point of the pushed metric in a minute", "the Collector printed:\n%s", collector.log())
		return pushedPoint{}
	}
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
	ca, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{netip.MustParseAddr("10.0.0.4")})
	require.NoError(t, err)
	account := sharedDir(t)
	writeFiles(t, account, map[string][]byte{"ca.crt": ca.Cert, "token": []byte(rand.Text()), "namespace": []byte("kube-system")})
	for name, set := range map[string]components.Set{"default": components.Default(), "hosting": components.HostingServed()} {
		docs := testutil.Documents(t, renderedCollector(t, set, testutil.TelemetryExport()))
		image := testutil.Field(t, collectorPod(t, docs, "node"), "containers", "0", "image").(string)
		for _, which := range []string{"node", "cluster"} {
			t.Run(name+"/"+which, func(t *testing.T) {
				dir := sharedDir(t)
				writeFiles(t, dir, map[string][]byte{"config.yaml": []byte(collectorConfig(t, docs, which))})

				out, err := exec.Command("docker", "run", "--rm", "-e", "K8S_NODE_IP=10.0.0.4", "-e", "K8S_NODE_NAME=shop-node-fsn1", "-e", "K8S_POD_IP=10.244.0.7",
					"-v", account+":"+serviceAccountDir+":ro", "-v", dir+":/conf:ro", image, "validate", "--config=/conf/config.yaml").CombinedOutput()

				require.NoError(t, err, "the %s set's %s copy's configuration:\n%s", name, which, out)
			})
		}
	}
}

func TestCollector_AnOpenBaoMemberAnswersTheClusterCopysScrapeWithItsSealStateWithoutATokenAndServesNothingElseThere(t *testing.T) {
	job := testutil.ScrapeJobs(t, testutil.Documents(t, renderedCollector(t, components.HostingServed(), testutil.TelemetryExport())))["openbao"]
	require.NotNil(t, job, "the cluster copy's openbao scrape job")
	_, port, err := net.SplitHostPort(testutil.Field(t, job, "static_configs", "0", "targets", "0").(string))
	require.NoError(t, err)
	address, client := startOpenBaoMember(t, port)
	query := url.Values{}
	for key, values := range testutil.Field(t, job, "params").(map[string]any) {
		for _, value := range values.([]any) {
			query.Add(key, value.(string))
		}
	}
	scrape := url.URL{Scheme: testutil.Field(t, job, "scheme").(string), Host: address, Path: testutil.Field(t, job, "metrics_path").(string), RawQuery: query.Encode()}
	health := url.URL{Scheme: scrape.Scheme, Host: address, Path: "/v1/sys/health"}

	scraped, metrics := get(t, client, scrape.String())
	other, _ := get(t, client, health.String())

	assert.Equal(t, http.StatusOK, scraped, "the scrape, sent without a token:\n%s", metrics)
	assert.Regexp(t, `(?m)^vault_core_unsealed(\{[^}]*\})? 0$`, metrics, "the seal state of a member not yet initialised, which is sealed")
	assert.Equal(t, http.StatusNotFound, other, "the API, asked on the metrics listener")
}

func TestCollector_TheClusterCopySendsUpAtZeroForEachTargetItCannotScrapeThoughItsJobsKeepOneSeriesEach(t *testing.T) {
	server := startAPIServer(t)
	backend := startOTLPBackend(t, bridgeGateway(t, server.etcd))
	export := testutil.TelemetryExport()
	export.MetricsEndpoint, export.LogsEndpoint = backend.url+"/otlp/v1/metrics", backend.url+"/otlp/v1/logs"
	rendered := renderedCollector(t, components.HostingServed(), export)
	server.admin(t, builtInObjects(t, rendered), "apply", "-f", "-")
	authority, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	require.NoError(t, err)
	openBaoCA := sharedDir(t)
	writeFiles(t, openBaoCA, map[string][]byte{"ca.crt": authority.Cert})
	collector := startCollector(t, server, testutil.Documents(t, rendered), "cluster", map[string]string{"openbao-ca": openBaoCA}, backend.caDir, rand.Text())

	down := map[string]scraped{}
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	for len(down) < 1+components.OpenBaoMembers {
		select {
		case up := <-backend.ups:
			if up.value == 0 {
				down[up.job+" "+up.attributes["k8s_pod_name"]] = up
			}
		case <-deadline.C:
			require.FailNow(t, "the backend received no up at 0 for every target", "it received %v, and the Collector printed:\n%s", slices.Collect(maps.Keys(down)), collector.log())
		}
	}

	assert.ElementsMatch(t, []string{"argocd ", "openbao openbao-0", "openbao openbao-1", "openbao openbao-2"}, slices.Collect(maps.Keys(down)),
		"no stand-in serves the targets, so each reads up at 0 with the member it names")
	for target, up := range down {
		assert.Equal(t, collectorSubscription, up.attributes["mitstack.subscription.id"], "the up series of %s", target)
	}
}

// shipContainerLogLine runs the node copy as its DaemonSet runs it, writes message as a containerd log line of a pod in
// app-shop, and returns the record the stand-in backend received for it.
func shipContainerLogLine(t *testing.T, message string) shipped {
	t.Helper()
	server := startAPIServer(t)
	backend := startOTLPBackend(t, bridgeGateway(t, server.etcd))
	export := testutil.TelemetryExport()
	export.MetricsEndpoint, export.LogsEndpoint = backend.url+"/otlp/v1/metrics", backend.url+"/otlp/v1/logs"
	rendered := renderedCollector(t, components.Default(), export)
	server.admin(t, builtInObjects(t, rendered), "apply", "-f", "-")
	server.admin(t, []byte(appShopNamespace), "apply", "-f", "-")

	logs := sharedDir(t)
	podLogs := filepath.Join(logs, shopPodLogs)
	require.NoError(t, os.MkdirAll(podLogs, 0o755))
	logFile := filepath.Join(podLogs, "0.log")
	require.NoError(t, os.WriteFile(logFile, []byte(containerdLine("a line the Collector finds when it starts")), 0o644))
	pushToken := rand.Text()
	collector := startCollector(t, server, testutil.Documents(t, rendered), "node", map[string]string{"pods": logs}, backend.caDir, pushToken)
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
		case got := <-backend.records:
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

func renderedCollector(t *testing.T, set components.Set, export components.TelemetryExport) []byte {
	t.Helper()
	manifests, err := set.TelemetryCollector(export, collectorSubscription)
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	return manifests[0].Bytes
}

func collectorPod(t *testing.T, docs []map[string]any, which string) map[string]any {
	t.Helper()
	kind := map[string]string{"node": "DaemonSet", "cluster": "Deployment"}[which]
	return testutil.Field(t, testutil.Named(t, docs, kind, "mitstack-telemetry-"+which), "spec", "template", "spec").(map[string]any)
}

func collectorConfig(t *testing.T, docs []map[string]any, which string) string {
	t.Helper()
	return testutil.Field(t, testutil.Named(t, docs, "ConfigMap", "mitstack-telemetry-"+which), "data", "config.yaml").(string)
}

// startOpenBaoMember runs the hosting set's OpenBao container on its rendered configuration, less the Kubernetes service
// registration a container outside a cluster cannot make, and returns the loopback address it publishes port at and a
// client that trusts the member's certificate.
func startOpenBaoMember(t *testing.T, port string) (string, *http.Client) {
	t.Helper()
	served := components.HostingServed().Served
	i := slices.IndexFunc(served, func(m components.Manifest) bool { return m.Name == "openbao" })
	require.GreaterOrEqual(t, i, 0, "the hosting set serves no OpenBao")
	docs := testutil.Documents(t, served[i].Bytes)
	container := testutil.Field(t, testutil.Named(t, docs, "StatefulSet", "openbao"), "spec", "template", "spec", "containers", "0")
	config := map[string][]byte{}
	for file, text := range testutil.Field(t, testutil.Named(t, docs, "ConfigMap", "openbao-config"), "data").(map[string]any) {
		config[file] = []byte(strings.Replace(text.(string), `service_registration "kubernetes" {}`, "", 1))
	}
	loopback := netip.MustParseAddr("127.0.0.1")
	ca, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{loopback})
	require.NoError(t, err)
	serving, err := ca.Issue(loopback, time.Now(), time.Hour)
	require.NoError(t, err)
	sealKey := make([]byte, 32)
	_, err = rand.Read(sealKey)
	require.NoError(t, err)
	sources := map[string]map[string][]byte{
		"config":   config,
		"tls":      {"tls.crt": serving.Cert, "tls.key": serving.Key, "ca.crt": ca.Cert},
		"seal-key": {"key": sealKey},
		"data":     {},
		"home":     {},
	}
	name := "mitstack-openbao-check-" + strings.ToLower(rand.Text()[:8])
	args := []string{"run", "-d", "--name", name, "--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"-p", "127.0.0.1::" + port, "-e", "BAO_API_ADDR=https://127.0.0.1:8200", "-e", "BAO_CLUSTER_ADDR=https://127.0.0.1:8201"}
	configDir := ""
	for _, mount := range testutil.Field(t, container, "volumeMounts").([]any) {
		volume, path := testutil.Field(t, mount, "name").(string), testutil.Field(t, mount, "mountPath").(string)
		files, ok := sources[volume]
		require.True(t, ok, "OpenBao mounts the volume %s, which this check has no stand-in for", volume)
		dir := sharedDir(t)
		writeFiles(t, dir, files)
		args = append(args, "-v", dir+":"+path)
		if volume == "config" {
			configDir = path
		}
	}
	require.NotEmpty(t, configDir, "OpenBao mounts no configuration")
	args = append(args, "--entrypoint", "bao", testutil.Field(t, container, "image").(string), "server", "-config="+configDir)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	docker(t, args...)
	followLogs(t, name).await(t, time.Minute, "OpenBao server started")

	out, err := exec.Command("docker", "port", name, port+"/tcp").CombinedOutput()
	require.NoError(t, err, "%s", out)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca.Cert))
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	return strings.Fields(string(out))[0], client
}

func get(t *testing.T, client *http.Client, address string) (int, string) {
	t.Helper()
	response, err := client.Get(address)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(body)
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

type otlpBackend struct {
	url, caDir string
	records    <-chan shipped
	ups        <-chan scraped
	points     <-chan pushedPoint
}

// startOTLPBackend serves OTLP over HTTPS at address and hands on each log record, each sample of up and each point
// of pushedMetric it gets.
func startOTLPBackend(t *testing.T, address netip.Addr) otlpBackend {
	t.Helper()
	ca, err := gateway.NewCA(time.Now(), time.Hour, []netip.Addr{address})
	require.NoError(t, err)
	serving, err := ca.Issue(address, time.Now(), time.Hour)
	require.NoError(t, err)
	certificate, err := tls.X509KeyPair(serving.Cert, serving.Key)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", netip.AddrPortFrom(address, 0).String())
	require.NoError(t, err)
	received, ups, points := make(chan shipped, 256), make(chan scraped, 256), make(chan pushedPoint, 256)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/otlp/v1/logs":
			sendRecords(t, r, received)
		case "/otlp/v1/metrics":
			sendMetrics(t, r, ups, points)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener = listener
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	caDir := sharedDir(t)
	writeFiles(t, caDir, map[string][]byte{"ca.crt": ca.Cert})
	return otlpBackend{url: srv.URL, caDir: caDir, records: received, ups: ups, points: points}
}

// decode reads the OTLP request r carries into message, and reports whether it could.
func decode(t *testing.T, r *http.Request, message proto.Message) bool {
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		unzipped, err := gzip.NewReader(r.Body)
		if !assert.NoError(t, err) {
			return false
		}
		body = unzipped
	}
	data, err := io.ReadAll(body)
	return assert.NoError(t, err) && assert.NoError(t, proto.Unmarshal(data, message))
}

func attributesOf(kvs []*commonpb.KeyValue) map[string]string {
	attributes := map[string]string{}
	for _, kv := range kvs {
		attributes[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	return attributes
}

func sendMetrics(t *testing.T, r *http.Request, ups chan<- scraped, points chan<- pushedPoint) {
	var request colmetricspb.ExportMetricsServiceRequest
	if !decode(t, r, &request) {
		return
	}
	for _, resourceMetrics := range request.GetResourceMetrics() {
		resource := attributesOf(resourceMetrics.GetResource().GetAttributes())
		for _, scopeMetrics := range resourceMetrics.GetScopeMetrics() {
			for _, metric := range scopeMetrics.GetMetrics() {
				for _, point := range metric.GetGauge().GetDataPoints() {
					switch metric.GetName() {
					case "up":
						select {
						case ups <- scraped{job: resource["service.name"], attributes: attributesOf(point.GetAttributes()), value: point.GetAsDouble()}:
						default:
						}
					case pushedMetric:
						select {
						case points <- pushedPoint{resource: resource, attributes: attributesOf(point.GetAttributes())}:
						default:
						}
					}
				}
			}
		}
	}
}

func sendRecords(t *testing.T, r *http.Request, received chan<- shipped) {
	var request collogspb.ExportLogsServiceRequest
	if !decode(t, r, &request) {
		return
	}
	for _, resourceLogs := range request.GetResourceLogs() {
		attributes := attributesOf(resourceLogs.GetResource().GetAttributes())
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			for _, record := range scopeLogs.GetLogRecords() {
				select {
				case received <- shipped{body: record.GetBody().GetStringValue(), attributes: attributes,
					recordAttributes: attributesOf(record.GetAttributes()), header: r.Header.Clone()}:
				default:
				}
			}
		}
	}
}

// followedContainer holds each line a running container prints.
type followedContainer struct {
	mu      sync.Mutex
	output  []string
	printed chan struct{}
	stopped chan struct{}
}

// startCollector runs the which copy's container as Kubernetes runs it, with standIns as the directories of the volumes
// beside its configuration and token, pushToken where its token Secret mounts and the backend's CA from caDir.
func startCollector(t *testing.T, server *apiServer, docs []map[string]any, which string, standIns map[string]string, caDir, pushToken string) *followedContainer {
	t.Helper()
	pod := collectorPod(t, docs, which)
	container := testutil.Field(t, pod, "containers", "0")
	config, token := sharedDir(t), sharedDir(t)
	writeFiles(t, config, map[string][]byte{"config.yaml": []byte(collectorConfig(t, docs, which))})
	writeFiles(t, token, map[string][]byte{"token": []byte(pushToken)})
	sources := maps.Clone(standIns)
	sources["config"], sources["token"] = config, token
	name := server.etcd + "-collector-" + which
	args := []string{"run", "-d", "--name", name, "--network", "container:" + server.etcd,
		"-v", server.serviceAccount(t, "kube-system", testutil.Field(t, pod, "serviceAccountName").(string)) + ":" + serviceAccountDir + ":ro",
		"-v", caDir + ":" + backendCADir + ":ro", "-e", "SSL_CERT_FILE=" + backendCADir + "/ca.crt",
		"-e", "KUBERNETES_SERVICE_HOST=127.0.0.1", "-e", "KUBERNETES_SERVICE_PORT=" + apiServerPort,
		"-e", "K8S_NODE_NAME=shop-node-fsn1", "-e", "K8S_NODE_IP=127.0.0.1", "-e", "K8S_POD_IP=" + containerAddress(t, server.etcd).String()}
	for _, mount := range testutil.Field(t, container, "volumeMounts").([]any) {
		volume := testutil.Field(t, mount, "name").(string)
		source, ok := sources[volume]
		require.True(t, ok, "the %s copy mounts the volume %s, which this check has no stand-in for", which, volume)
		args = append(args, "-v", source+":"+testutil.Field(t, mount, "mountPath").(string)+":ro")
	}
	args = append(args, testutil.Field(t, container, "image").(string))
	for _, arg := range testutil.Field(t, container, "args").([]any) {
		args = append(args, arg.(string))
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	docker(t, args...)
	return followLogs(t, name)
}

// followLogs follows what the running container name prints until the test ends.
func followLogs(t *testing.T, name string) *followedContainer {
	t.Helper()
	c := &followedContainer{printed: make(chan struct{}, 1), stopped: make(chan struct{})}
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

// await returns once the container prints a line holding every one of texts, and fails if it stops or within passes first.
func (c *followedContainer) await(t *testing.T, within time.Duration, texts ...string) {
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
				require.FailNow(t, "the container stopped", "waiting for %q it printed:\n%s", texts, c.log())
			}
			return
		case <-deadline.C:
			require.FailNow(t, "the container never printed what was awaited", "waiting %s for %q it printed:\n%s", within, texts, c.log())
		}
	}
}

func (c *followedContainer) hasPrinted(texts []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.ContainsFunc(c.output, func(line string) bool {
		return !slices.ContainsFunc(texts, func(text string) bool { return !strings.Contains(line, text) })
	})
}

func (c *followedContainer) log() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.output, "\n")
}
