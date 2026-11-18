package components_test

// The telemetry Collector a cluster requiring an OTLP backend runs, in a copy on every node and one for the cluster.

import (
	"fmt"
	"net/url"
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
	return collectorDocs(t, components.Default(), export)
}

func collectorDocs(t *testing.T, set components.Set, export components.TelemetryExport) []map[string]any {
	t.Helper()
	manifests, err := set.TelemetryCollector(export, telemetrySubscription)
	require.NoError(t, err)
	require.Len(t, manifests, 1, "the manifests the telemetry Collector renders")
	assert.Equal(t, "mitstack-telemetry", manifests[0].Name)
	return testutil.Documents(t, manifests[0].Bytes)
}

func collectorConfig(t *testing.T, docs []map[string]any, which string) map[string]any {
	t.Helper()
	configMap := testutil.Named(t, docs, "ConfigMap", "mitstack-telemetry-"+which)
	var config map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(testutil.Field(t, configMap, "data", "config.yaml").(string)), &config))
	return config
}

func podOf(t *testing.T, docs []map[string]any, which string) map[string]any {
	t.Helper()
	return testutil.Field(t, podTemplate(t, docs, which), "spec").(map[string]any)
}

func podTemplate(t *testing.T, docs []map[string]any, which string) map[string]any {
	t.Helper()
	kind := map[string]string{"node": "DaemonSet", "cluster": "Deployment"}[which]
	return testutil.Field(t, testutil.Named(t, docs, kind, "mitstack-telemetry-"+which), "spec", "template").(map[string]any)
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
		var statements []any
		for _, name := range []string{"mitstack.app", "k8s.node.name", "k8s.namespace.name", "k8s.persistentvolumeclaim.name", "k8s.pod.name"} {
			statements = append(statements, fmt.Sprintf("set(datapoint.attributes[%q], resource.attributes[%q]) where resource.attributes[%q] != nil", name, name, name))
		}
		statements = append(statements, `set(datapoint.attributes["mitstack.subscription.id"], resource.attributes["mitstack.subscription.id"])`)
		assert.Equal(t, statements, testutil.Field(t, config, "processors", "transform/datapoints", "metric_statements"),
			"the %s copy's tags on each data point, by which the cluster's alert rules group and name what they report", which)
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
	assert.Contains(t, testutil.Field(t, testutil.Named(t, docs, "ClusterRole", "mitstack-telemetry"), "rules"),
		map[string]any{"apiGroups": []any{""}, "resources": []any{"nodes/stats", "nodes/pods"}, "verbs": []any{"get"}},
		"the kubelet answers the stats and, for the volume type, its pods")
}

func TestTelemetryCollector_TheClusterCopyReadsClusterMetricsFromOneReplicaOnAControlPlaneNode(t *testing.T) {
	docs := telemetryDocs(t, testutil.TelemetryExport())

	cluster := collectorConfig(t, docs, "cluster")
	deployment := testutil.Named(t, docs, "Deployment", "mitstack-telemetry-cluster")

	assert.Equal(t, "serviceAccount", testutil.Field(t, cluster, "receivers", "k8s_cluster", "auth_type"))
	assert.Equal(t, []any{"k8s_cluster", "prometheus"}, testutil.Field(t, cluster, "service", "pipelines", "metrics", "receivers"),
		"the node conditions and node list, and the metrics it scrapes")
	assert.EqualValues(t, 1, testutil.Field(t, deployment, "spec", "replicas"), "a second replica would send each event twice")
	assert.Equal(t, "Recreate", testutil.Field(t, deployment, "spec", "strategy", "type"), "a rollout never runs two replicas at once")
	pod := podOf(t, docs, "cluster")
	assert.Equal(t, map[string]any{controlPlaneRole: ""}, pod["nodeSelector"], "a stopped worker never takes the cluster copy down")
	assert.Equal(t, []any{map[string]any{"key": controlPlaneRole, "operator": "Exists", "effect": "NoSchedule"}}, pod["tolerations"])
}

func TestTelemetryCollector_TheClusterCopyScrapesArgoCDsApplicationControllerThroughItsMetricsServiceAndNoOpenBaoWhereNoneRuns(t *testing.T) {
	docs := telemetryDocs(t, testutil.TelemetryExport())
	service := namedDocument(t, testutil.Manifest(t, "argocd", components.Default().Served), "Service", "argocd-application-controller-metrics")

	jobs := testutil.ScrapeJobs(t, docs)

	assert.Equal(t, map[string]any{"app.kubernetes.io/name": "argocd-application-controller", "app.kubernetes.io/instance": "argocd"},
		testutil.Field(t, service, "spec", "selector"), "the Service the cluster copy scrapes selects the application controller")
	assert.Equal(t, "metrics", testutil.Field(t, service, "spec", "ports", "0", "targetPort"))
	target := fmt.Sprintf("argocd-application-controller-metrics.argocd.svc:%v", testutil.Field(t, service, "spec", "ports", "0", "port"))
	assert.Equal(t, []any{map[string]any{"targets": []any{target}}}, testutil.Field(t, jobs["argocd"], "static_configs"))
	assert.Equal(t, keepOnly("argocd_app_info"), testutil.Field(t, jobs["argocd"], "metric_relabel_configs"), "the one series an alert rule reads")
	assert.NotContains(t, jobs, "openbao", "a cluster that runs no OpenBao scrapes none")
	assert.NotContains(t, fmt.Sprint(podOf(t, docs, "cluster")["volumes"]), "openbao-ca")
}

func TestTelemetryCollector_OnAClusterThatHostsThePlatformTheClusterCopyScrapesEachOpenBaoMemberOnItsMetricsListenerAndNamesIt(t *testing.T) {
	set := components.HostingServed()
	docs := collectorDocs(t, set, testutil.TelemetryExport())

	openbao := testutil.ScrapeJobs(t, docs)["openbao"]

	var targets []any
	for i := range components.OpenBaoMembers {
		member := fmt.Sprintf("openbao-%d", i)
		targets = append(targets, map[string]any{"targets": []any{fmt.Sprintf("%s.openbao-internal.openbao.svc:%d", member, openBaoMetricsPort(t, set))},
			"labels": map[string]any{"k8s_pod_name": member}})
	}
	assert.Equal(t, targets, testutil.Field(t, openbao, "static_configs"), "one target for each member, so each sample names the member it came from")
	assert.Equal(t, []any{"https", "/v1/sys/metrics", []any{"prometheus"}},
		[]any{testutil.Field(t, openbao, "scheme"), testutil.Field(t, openbao, "metrics_path"), testutil.Field(t, openbao, "params", "format")})
	pod := podOf(t, docs, "cluster")
	assert.Contains(t, pod["volumes"], map[string]any{"name": "openbao-ca", "configMap": map[string]any{"name": "openbao-ca"}}, "the authority trust-manager publishes in every namespace")
	assert.Contains(t, testutil.Field(t, pod, "containers", "0", "volumeMounts"), map[string]any{"name": "openbao-ca", "mountPath": "/etc/mitstack-telemetry-openbao", "readOnly": true})
	assert.Equal(t, "/etc/mitstack-telemetry-openbao/ca.crt", testutil.Field(t, openbao, "tls_config", "ca_file"), "the members' certificate stays verified")
	assert.Equal(t, keepOnly("vault_core_unsealed"), testutil.Field(t, openbao, "metric_relabel_configs"), "the one series an alert rule reads")
	assert.NotContains(t, fmt.Sprint(podOf(t, docs, "node")["volumes"]), "openbao-ca")
}

func keepOnly(metric string) []any {
	return []any{map[string]any{"source_labels": []any{"__name__"}, "regex": metric, "action": "keep"}}
}

func TestTelemetryCollector_EachCopysPodChangesWithItsConfigurationAndOnlyThenSinceTheCollectorReadsItAtStart(t *testing.T) {
	export := testutil.TelemetryExport()
	moved := export
	moved.MetricsEndpoint = "https://4a5b6c7d.metrics.cockpit.fr-par.scw.cloud/otlp/v1/metrics"

	once, again, changed := telemetryDocs(t, export), telemetryDocs(t, export), telemetryDocs(t, moved)

	for _, which := range collectorCopies {
		assert.Equal(t, podTemplate(t, once, which), podTemplate(t, again, which), "the %s copy's pod for the same configuration", which)
		assert.NotEqual(t, podTemplate(t, once, which), podTemplate(t, changed, which), "the %s copy's pod for another configuration", which)
	}
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

			manifests, err := components.Default().TelemetryCollector(export, telemetrySubscription)

			require.Error(t, err, "TelemetryCollector(%+v) rendered %d manifests", export, len(manifests))
			assert.ErrorContains(t, err, tc.names, "the refusal names what is wrong")
			assert.Empty(t, manifests)
		})
	}
}

func TestTelemetryCollector_RendersNothingWithoutAnExport(t *testing.T) {
	manifests, err := components.Default().TelemetryCollector(components.TelemetryExport{}, telemetrySubscription)

	require.NoError(t, err)
	assert.Empty(t, manifests, "a cluster that requires no OTLP backend runs no Collector, OTLP receiver or receiver Service, since one with no exporter cannot start")
}

func TestTelemetryCollector_TheNodeCopyReceivesOTLPOnItsPodsAddressBehindANodeLocalServiceTheBaseURLNames(t *testing.T) {
	docs := telemetryDocs(t, testutil.TelemetryExport())

	node, cluster := collectorConfig(t, docs, "node"), collectorConfig(t, docs, "cluster")
	service := testutil.Named(t, docs, "Service", "mitstack-telemetry-otlp")

	assert.Equal(t, map[string]any{"protocols": map[string]any{"http": map[string]any{"endpoint": "${env:K8S_POD_IP}:4318"}}},
		testutil.Field(t, node, "receivers", "otlp"), "the node copy takes OTLP over HTTP on its pod's own address")
	assert.Contains(t, testutil.Field(t, podOf(t, docs, "node"), "containers", "0", "env"), map[string]any{"name": "K8S_POD_IP",
		"valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "status.podIP"}}})
	assert.NotContains(t, testutil.Field(t, cluster, "receivers"), "otlp", "the cluster copy receives no push")
	assert.Equal(t, "kube-system", testutil.Field(t, service, "metadata", "namespace"))
	assert.Equal(t, map[string]any{"selector": testutil.Field(t, podTemplate(t, docs, "node"), "metadata", "labels"), "internalTrafficPolicy": "Local",
		"ports": []any{map[string]any{"name": "otlp-http", "port": 4318, "targetPort": 4318, "protocol": "TCP"}}},
		testutil.Field(t, service, "spec"), "the Service sends a pod's push to the node copy on the pod's own node")
	endpoint, err := url.Parse(components.TelemetryOTLPEndpoint)
	require.NoError(t, err, "the receiver's base URL %q", components.TelemetryOTLPEndpoint)
	assert.Equal(t, url.URL{Scheme: "http", Host: "mitstack-telemetry-otlp.kube-system.svc.cluster.local:4318"}, *endpoint,
		"the base URL the block outputs names the Service, to which a sender appends /v1/metrics and /v1/logs")
}
