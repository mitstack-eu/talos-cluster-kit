package components_test

// The telemetry Collector a cluster requiring an OTLP backend runs, in a copy on every node and one for the cluster.

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	telemetrySubscription = "sub_0123456789ab"
	controlPlaneRole      = "node-role.kubernetes.io/control-plane"
	pushTokenDir          = "/var/run/mitstack-telemetry"
)

var collectorCopies = []string{"node", "cluster"}

func telemetryDocs(t *testing.T, export components.TelemetryExport) []map[string]any {
	t.Helper()
	manifests, err := components.TelemetryCollector(export, telemetrySubscription)
	require.NoError(t, err)
	require.Len(t, manifests, 1, "the manifests the telemetry Collector renders")
	assert.Equal(t, "mitstack-telemetry", manifests[0].Name)
	return testutil.Documents(t, manifests[0].Bytes)
}

func named(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	of := testutil.Kind(docs, kind)
	i := slices.IndexFunc(of, func(doc map[string]any) bool { return testutil.Field(t, doc, "metadata", "name") == name })
	require.GreaterOrEqual(t, i, 0, "no %s %s", kind, name)
	return of[i]
}

func collectorConfig(t *testing.T, docs []map[string]any, which string) map[string]any {
	t.Helper()
	configMap := named(t, docs, "ConfigMap", "mitstack-telemetry-"+which)
	var config map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(testutil.Field(t, configMap, "data", "config.yaml").(string)), &config))
	return config
}

func podOf(t *testing.T, docs []map[string]any, which string) map[string]any {
	t.Helper()
	kind := map[string]string{"node": "DaemonSet", "cluster": "Deployment"}[which]
	return testutil.Field(t, named(t, docs, kind, "mitstack-telemetry-"+which), "spec", "template", "spec").(map[string]any)
}

func TestTelemetryCollector_RendersTheEventsLogsAndKubeletReceivers(t *testing.T) {
	docs := telemetryDocs(t, testutil.TelemetryExport())

	node, cluster := collectorConfig(t, docs, "node"), collectorConfig(t, docs, "cluster")

	logs := testutil.Field(t, node, "receivers", "file_log")
	assert.Equal(t, []any{"/var/log/pods/*/*/*.log"}, testutil.Field(t, logs, "include"), "the container logs the node copy reads")
	assert.Equal(t, []any{"/var/log/pods/kube-system_mitstack-telemetry-*/*/*.log"}, testutil.Field(t, logs, "exclude"), "the Collector's own logs, left out")
	assert.Equal(t, true, testutil.Field(t, logs, "include_file_path"), "the path the container operator reads the namespace and pod from")
	assert.Equal(t, "container", testutil.Field(t, logs, "operators", "0", "type"))
	assert.Equal(t, "https://${env:K8S_NODE_IP}:10250", testutil.Field(t, node, "receivers", "kubelet_stats", "endpoint"), "the node's own kubelet")
	assert.Equal(t, "serviceAccount", testutil.Field(t, node, "receivers", "kubelet_stats", "auth_type"))
	assert.Nil(t, testutil.Field(t, node, "receivers", "kubelet_stats", "insecure_skip_verify"), "the kubelet's certificate stays verified")
	assert.Equal(t, []any{map[string]any{"name": "events", "group": "events.k8s.io", "mode": "watch"}},
		testutil.Field(t, cluster, "receivers", "k8s_objects", "objects"), "the events the cluster copy watches")
	assert.Equal(t, []any{"file_log"}, testutil.Field(t, node, "service", "pipelines", "logs", "receivers"))
	assert.Equal(t, []any{"kubelet_stats"}, testutil.Field(t, node, "service", "pipelines", "metrics", "receivers"))
	assert.Equal(t, []any{"k8s_objects"}, testutil.Field(t, cluster, "service", "pipelines", "logs", "receivers"))
	pod := podOf(t, docs, "node")
	assert.Equal(t, []any{map[string]any{"operator": "Exists"}}, pod["tolerations"], "the node copy runs on every node, control plane included")
	assert.Contains(t, pod["volumes"], map[string]any{"name": "pods", "hostPath": map[string]any{"path": "/var/log/pods", "type": "Directory"}})
	assert.Contains(t, testutil.Field(t, pod, "containers", "0", "volumeMounts"), map[string]any{"name": "pods", "mountPath": "/var/log/pods", "readOnly": true})
	assert.Contains(t, testutil.Field(t, pod, "containers", "0", "env"), map[string]any{"name": "K8S_NODE_IP",
		"valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "status.hostIP"}}})
	assert.Contains(t, testutil.Field(t, pod, "containers", "0", "env"), map[string]any{"name": "K8S_NODE_NAME",
		"valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "spec.nodeName"}}})
}

func TestTelemetryCollector_TagsEveryPipelineWithTheNamespacesAppAndMasksBeforeEachExporter(t *testing.T) {
	docs := telemetryDocs(t, testutil.TelemetryExport())

	for _, which := range collectorCopies {
		config := collectorConfig(t, docs, which)

		assert.Equal(t, []any{map[string]any{"tag_name": "mitstack.app", "key": "mitstack.dev/app", "from": "namespace"}},
			testutil.Field(t, config, "processors", "k8s_attributes", "extract", "labels"), "the %s copy's app tag", which)
		assert.Equal(t, true, testutil.Field(t, config, "processors", "k8s_attributes", "wait_for_metadata"), "the %s copy tags from its first record", which)
		redaction := testutil.Field(t, config, "processors", "redaction").(map[string]any)
		assert.Equal(t, true, redaction["allow_all_keys"], "the %s copy keeps every attribute and masks its value", which)
		assert.Equal(t, true, redaction["redact_all_types"], "the %s copy masks a card number held as a number", which)
		assert.NotEmpty(t, redaction["blocked_values"], "the %s copy's masking patterns", which)
		assert.NotContains(t, redaction, "allowed_values", "the %s copy lets no value that matches a pattern pass unmasked", which)
		assert.Equal(t, []any{map[string]any{"key": "mitstack.subscription.id", "value": telemetrySubscription, "action": "upsert"}},
			testutil.Field(t, config, "processors", "resource/subscription", "attributes"), "the %s copy's subscription tag", which)
		assert.Equal(t, []any{
			`set(datapoint.attributes["mitstack.app"], resource.attributes["mitstack.app"]) where resource.attributes["mitstack.app"] != nil`,
			`set(datapoint.attributes["mitstack.subscription.id"], resource.attributes["mitstack.subscription.id"])`,
		}, testutil.Field(t, config, "processors", "transform/datapoints", "metric_statements"), "the %s copy's tags on each data point", which)
		for _, signal := range []string{"logs", "metrics"} {
			want := []any{"memory_limiter", "k8s_attributes", "redaction", "resource/subscription", "batch"}
			if signal == "metrics" {
				want = []any{"memory_limiter", "k8s_attributes", "redaction", "resource/subscription", "transform/datapoints", "batch"}
			}
			assert.Equal(t, want, testutil.Field(t, config, "service", "pipelines", signal, "processors"),
				"the %s copy's %s pipeline tags, then masks what the tag copied in, before its exporter", which, signal)
		}
	}
}

func TestTelemetryCollector_ExportsEachSignalToTheEndpointAndHeaderTheRequiredResourceNames(t *testing.T) {
	export := testutil.TelemetryExport()

	docs := telemetryDocs(t, export)

	for _, which := range collectorCopies {
		config := collectorConfig(t, docs, which)
		assert.Equal(t, map[string]any{"metrics_endpoint": export.MetricsEndpoint, "auth": map[string]any{"authenticator": "bearertokenauth"}},
			testutil.Field(t, config, "exporters", "otlp_http/metrics"), "the %s copy's metrics exporter, which appends no path to a full address", which)
		assert.Equal(t, map[string]any{"logs_endpoint": export.LogsEndpoint, "auth": map[string]any{"authenticator": "bearertokenauth"}},
			testutil.Field(t, config, "exporters", "otlp_http/logs"), "the %s copy's logs exporter", which)
		assert.Equal(t, map[string]any{"header": export.TokenHeader, "scheme": "", "filename": pushTokenDir + "/token"},
			testutil.Field(t, config, "extensions", "bearertokenauth"), "the %s copy sends the bare push token under the resource's header", which)
		assert.Equal(t, []any{"bearertokenauth"}, testutil.Field(t, config, "service", "extensions"))
		assert.Equal(t, []any{"otlp_http/logs"}, testutil.Field(t, config, "service", "pipelines", "logs", "exporters"))
		assert.Equal(t, []any{"otlp_http/metrics"}, testutil.Field(t, config, "service", "pipelines", "metrics", "exporters"))
		pod := podOf(t, docs, which)
		assert.Contains(t, pod["volumes"], map[string]any{"name": "token", "secret": map[string]any{"secretName": "mitstack-telemetry-token",
			"items": []any{map[string]any{"key": "token", "path": "token"}}}}, "the %s copy reads the push token the lease job writes", which)
		assert.Contains(t, testutil.Field(t, pod, "containers", "0", "volumeMounts"), map[string]any{"name": "token", "mountPath": pushTokenDir, "readOnly": true})
		assert.Equal(t, "mitstack-telemetry", pod["serviceAccountName"], "the %s copy's account, which holds no right on Secrets", which)
	}
}

func TestTelemetryCollector_ReadsTheVolumeAndNodeFilesystemMetrics(t *testing.T) {
	docs := telemetryDocs(t, testutil.TelemetryExport())

	kubelet := testutil.Field(t, collectorConfig(t, docs, "node"), "receivers", "kubelet_stats").(map[string]any)

	assert.Equal(t, []any{"node", "pod", "container", "volume"}, kubelet["metric_groups"], "the node group holds the filesystem metrics and the volume group each volume's")
	assert.Equal(t, []any{"k8s.volume.type"}, kubelet["extra_metadata_labels"], "each volume carries its type beside its claim name")
	assert.Equal(t, true, testutil.Field(t, kubelet, "metrics", "k8s.pod.volume.usage", "enabled"), "each volume's used bytes")
	assert.Contains(t, testutil.Field(t, named(t, docs, "ClusterRole", "mitstack-telemetry"), "rules"),
		map[string]any{"apiGroups": []any{""}, "resources": []any{"nodes/stats", "nodes/pods"}, "verbs": []any{"get"}},
		"the kubelet answers the stats and, for the volume type, its pods")
}

func TestTelemetryCollector_TheClusterCopyReadsClusterMetricsFromOneReplicaOnAControlPlaneNode(t *testing.T) {
	docs := telemetryDocs(t, testutil.TelemetryExport())

	cluster := collectorConfig(t, docs, "cluster")
	deployment := named(t, docs, "Deployment", "mitstack-telemetry-cluster")

	assert.Equal(t, "serviceAccount", testutil.Field(t, cluster, "receivers", "k8s_cluster", "auth_type"))
	assert.Equal(t, []any{"k8s_cluster"}, testutil.Field(t, cluster, "service", "pipelines", "metrics", "receivers"), "the node conditions and node list")
	assert.EqualValues(t, 1, testutil.Field(t, deployment, "spec", "replicas"), "a second replica would send each event twice")
	assert.Equal(t, "Recreate", testutil.Field(t, deployment, "spec", "strategy", "type"), "a rollout never runs two replicas at once")
	pod := podOf(t, docs, "cluster")
	assert.Equal(t, map[string]any{controlPlaneRole: ""}, pod["nodeSelector"], "a stopped worker never takes the cluster copy down")
	assert.Equal(t, []any{map[string]any{"key": controlPlaneRole, "operator": "Exists", "effect": "NoSchedule"}}, pod["tolerations"])
}

func TestTelemetryCollector_RefusesAnExportWithoutAnHTTPSEndpointOrAHeaderName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*components.TelemetryExport)
		names string
	}{
		{"a metrics endpoint over plain http", func(e *components.TelemetryExport) {
			e.MetricsEndpoint = "http://0f1e2d3c.metrics.cockpit.fr-par.scw.cloud/otlp/v1/metrics"
		}, "http://0f1e2d3c.metrics.cockpit.fr-par.scw.cloud/otlp/v1/metrics"},
		{"a logs endpoint with no host", func(e *components.TelemetryExport) { e.LogsEndpoint = "https:///otlp/v1/logs" }, "https:///otlp/v1/logs"},
		{"a logs endpoint that is no address", func(e *components.TelemetryExport) { e.LogsEndpoint = "logs.cockpit" }, "logs.cockpit"},
		{"no header name", func(e *components.TelemetryExport) { e.TokenHeader = "" }, "header"},
		{"a header name holding a space", func(e *components.TelemetryExport) { e.TokenHeader = "X TOKEN" }, "X TOKEN"},
		{"no resource to lease the push token of", func(e *components.TelemetryExport) { e.Resource = "" }, "resource"},
		{"an export naming its header alone", func(e *components.TelemetryExport) { *e = components.TelemetryExport{TokenHeader: "X-TOKEN"} }, "endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			export := testutil.TelemetryExport()
			tc.edit(&export)

			manifests, err := components.TelemetryCollector(export, telemetrySubscription)

			require.Error(t, err, "TelemetryCollector(%+v) rendered %d manifests", export, len(manifests))
			assert.ErrorContains(t, err, tc.names, "the refusal names what is wrong")
			assert.Empty(t, manifests)
		})
	}
}

func TestTelemetryCollector_RendersNothingWithoutAnExport(t *testing.T) {
	manifests, err := components.TelemetryCollector(components.TelemetryExport{}, telemetrySubscription)

	require.NoError(t, err)
	assert.Empty(t, manifests, "a cluster that requires no OTLP backend runs no Collector, since one with no exporter cannot start")
}
