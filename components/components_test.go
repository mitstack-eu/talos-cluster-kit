package components_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

func TestImages_NamesEveryImageOfTheDefaultSetOnceByTagAndDigest(t *testing.T) {
	set := components.Default()

	images, err := set.Images()

	require.NoError(t, err)
	var refs []string
	for _, image := range images {
		parsed, err := components.ParseImage(image.Ref())
		require.NoError(t, err, "image %s", image.Ref())
		assert.Equal(t, image, parsed)
		refs = append(refs, image.Ref())
	}
	assert.True(t, slices.IsSorted(refs), "images out of order: %v", refs)
	assert.Equal(t, refs, slices.Compact(slices.Clone(refs)), "an image is listed twice")
	for _, name := range []string{"quay.io/cilium/cilium", "quay.io/argoproj/argocd", "ghcr.io/siderolabs/kubelet", "docker.io/otel/opentelemetry-collector-contrib"} {
		assert.True(t, slices.ContainsFunc(images, func(i components.Image) bool { return i.Name == name }), "the set pulls no %s", name)
	}
}

func TestDefault_PinsTheReleasesToolsEnvInstalls(t *testing.T) {
	set := components.Default()

	versions := set.Versions

	assert.Equal(t, tools.Pin("TALOS_VERSION"), versions.Talos)
	assert.Equal(t, tools.Pin("KUBERNETES_VERSION"), "v"+versions.Kubernetes)
	for _, image := range set.TalosImages {
		if strings.HasPrefix(image.Name, "ghcr.io/siderolabs/kubelet") || strings.HasPrefix(image.Name, "registry.k8s.io/kube-") {
			assert.Equal(t, "v"+versions.Kubernetes, image.Tag, "image %s", image.Name)
		}
	}
}

func TestImages_RefusesAManifestImageWithoutADigest(t *testing.T) {
	set := components.Set{Inline: []components.Manifest{{Name: "tagged", Bytes: []byte("spec:\n  image: quay.io/cilium/cilium:v1.19.9\n")}}}

	_, err := set.Images()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "quay.io/cilium/cilium:v1.19.9")
}

func TestLogCollector_ListensOnLoopbackAndVerifiesTheReceiver(t *testing.T) {
	export := testutil.LogExport(t)

	m, err := components.LogCollector(export)

	require.NoError(t, err)
	docs := testutil.Documents(t, m.Bytes)
	configMap := testutil.Kind(docs, "ConfigMap")[0]
	assert.Equal(t, string(export.CA), testutil.Field(t, configMap, "data", "ca.crt"))
	config := testutil.Documents(t, []byte(testutil.Field(t, configMap, "data", "config.yaml").(string)))[0]
	assert.Equal(t, "127.0.0.1:5170", testutil.Field(t, config, "receivers", "tcplog/service", "listen_address"))
	assert.Equal(t, "127.0.0.1:5171", testutil.Field(t, config, "receivers", "tcplog/kernel", "listen_address"))
	assert.Equal(t, map[string]any{
		"endpoint": "otlp.mitstack.dev:4317",
		"tls":      map[string]any{"ca_file": "/etc/mitstack-node-logs/ca.crt"},
	}, testutil.Field(t, config, "exporters", "otlp_grpc"))
	daemonSet := testutil.Kind(docs, "DaemonSet")[0]
	assert.Equal(t, true, testutil.Field(t, daemonSet, "spec", "template", "spec", "hostNetwork"))
}

func TestLogCollector_RefusesAnEndpointWithoutAPort(t *testing.T) {
	export := testutil.LogExport(t)
	export.Endpoint = "otlp.mitstack.dev"

	_, err := components.LogCollector(export)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"otlp.mitstack.dev" is not host:port`)
}
