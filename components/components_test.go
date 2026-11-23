package components_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/mirror"
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

func TestDefault_RunsKyvernoWithBothPoliciesAndCloudNativePGWithTheDatabaseSlicePolicy(t *testing.T) {
	images, err := mirror.List(components.Default())

	require.NoError(t, err)
	for _, name := range []string{"kyverno", "mitstack-require-app-label", "mitstack-generate-default-deny", "cloudnative-pg", "cloudnative-pg-namespace", "mitstack-database-slices"} {
		manifest(t, name)
	}
	for _, name := range []string{
		"ghcr.io/kyverno/kyverno", "ghcr.io/kyverno/kyvernopre", "ghcr.io/kyverno/background-controller",
		"ghcr.io/kyverno/cleanup-controller", "ghcr.io/kyverno/reports-controller",
		"ghcr.io/cloudnative-pg/cloudnative-pg", "ghcr.io/cloudnative-pg/postgresql", "ghcr.io/cloudnative-pg/pgbouncer",
	} {
		assert.True(t, slices.ContainsFunc(images, func(i components.Image) bool { return i.Name == name }), "the mirror list holds no %s", name)
	}
}

func TestImages_RefusesAnOperatorsDefaultImageAndAPluginsSidecarImageWithoutADigest(t *testing.T) {
	for setting, ref := range map[string]string{"PGBOUNCER_IMAGE_NAME": "ghcr.io/cloudnative-pg/pgbouncer:1.25.2", "SIDECAR_IMAGE": `"ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar:v0.16.0"`} {
		_, err := components.Set{Inline: []components.Manifest{{Name: "operator", Bytes: []byte("data:\n  " + setting + ": " + ref + "\n")}}}.Images()

		require.Error(t, err, setting)
		assert.Contains(t, err.Error(), strings.Trim(ref, `"`))
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

func TestImages_ScansAManifestOnceAndAnswersTheSameImagesTwice(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	set := components.Set{Inline: []components.Manifest{{Name: "repeat", Bytes: []byte("spec:\n  image: quay.io/cilium/cilium:v1.19.9@" + digest + "\n")}}}

	first, err := set.Images()
	require.NoError(t, err)
	second, err := set.Images()

	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestImages_DoesNotRememberAScanThatFailed(t *testing.T) {
	bad := components.Manifest{Name: "refused", Bytes: []byte("spec:\n  image: quay.io/cilium/cilium:v1.19.9\n")}
	before := components.ManifestImageSlots()

	for range 2 {
		_, err := components.Set{Inline: []components.Manifest{bad}}.Images()
		require.Error(t, err)
	}

	assert.Equal(t, before, components.ManifestImageSlots())
}

func TestImages_RescansAManifestWhoseBytesChangedUnderTheSameName(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	read := func(tag string) []components.Image {
		set := components.Set{Inline: []components.Manifest{{Name: "changing", Bytes: []byte("spec:\n  image: quay.io/cilium/cilium:" + tag + "@" + digest + "\n")}}}
		images, err := set.Images()
		require.NoError(t, err)
		return images
	}

	old, current := read("v1.19.8"), read("v1.19.9")

	assert.NotEqual(t, old, current)
	assert.Equal(t, old, read("v1.19.8"))
}

func TestImages_KeepsOneSlotForAManifestWhateverBytesItHasHad(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	scan := func(token string) {
		set := components.Set{Inline: []components.Manifest{{Name: "tokens", Bytes: []byte("data:\n  token: " + token + "\nspec:\n  image: quay.io/cilium/cilium:v1.19.9@" + digest + "\n")}}}
		_, err := set.Images()
		require.NoError(t, err)
	}
	scan("first")
	before := components.ManifestImageSlots()

	for i := range 50 {
		scan(fmt.Sprintf("install-%d", i))
	}

	assert.Equal(t, before, components.ManifestImageSlots())
}

func TestCoreDNS_RunsTalossDeploymentWithOneReplicaInEachLocationAndLeavesAClusterInOneLocationToTalos(t *testing.T) {
	for _, tc := range []struct {
		name, golden string
		locations    []string
	}{
		{"a cluster in one location keeps the replicas Talos sets", "", []string{"fsn1"}},
		{"a cluster in two locations runs two replicas", "coredns-2-locations.yaml", []string{"fsn1", "nbg1"}},
		{"a cluster whose nodes name three locations, one twice, runs one in each, replaced one at a time", "coredns-3-locations.yaml", []string{"hel1", "fsn1", "nbg1", "fsn1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifests, err := components.CoreDNS(tc.locations)

			require.NoError(t, err)
			if tc.golden == "" {
				assert.Empty(t, manifests, "a cluster in %v keeps the Deployment Talos creates", tc.locations)
				return
			}
			require.Len(t, manifests, 1, "the CoreDNS of a cluster in %v", tc.locations)
			testutil.Golden(t, filepath.Join("testdata", "golden", tc.golden), manifests[0].Bytes)
		})
	}
}

// databaseOperator is one Deployment the spread runs twice, with the update strategy it is replaced under.
type databaseOperator struct {
	manifest, deployment string
	strategy             map[string]any
}

// The plugin keeps its chart's Recreate strategy, which its installation guide keeps so no two versions serve at once.
var databaseOperators = []databaseOperator{
	{"cloudnative-pg", "cloudnative-pg", map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": 0, "maxUnavailable": 1}}},
	{"plugin-barman-cloud", "plugin-barman-cloud", map[string]any{"type": "Recreate"}},
}

func TestDatabaseOperatorsAcrossZones_RunTwoReplicasInDistinctZonesUnderADisruptionBudgetOfOneAndLeaveOneZoneAsBuilt(t *testing.T) {
	built := components.HostingServed()
	for _, tc := range []struct {
		name    string
		zones   []string
		spreads bool
	}{
		{"workers in three zones run two replicas of each, no two in one zone", []string{"fsn1", "nbg1", "hel1"}, true},
		{"workers in two zones, one named twice, run two replicas of each, one in each zone", []string{"nbg1", "fsn1", "nbg1"}, true},
		{"workers in one zone keep one replica of each and no disruption budget", []string{"hel1", "hel1"}, false},
		{"a cluster that names no worker zone keeps the set as built", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, err := built.DatabaseOperatorsAcrossZones(tc.zones)

			require.NoError(t, err)
			if !tc.spreads {
				assert.Equal(t, built, set, "the set of a cluster whose workers sit in %v", tc.zones)
				return
			}
			for _, op := range databaseOperators {
				assertSpread(t, op, testutil.Manifest(t, op.manifest, built.Served), testutil.Manifest(t, op.manifest, set.Served))
			}
		})
	}
}

func assertSpread(t *testing.T, op databaseOperator, built, spread components.Manifest) {
	t.Helper()
	builtDocs, spreadDocs := testutil.Documents(t, built.Bytes), testutil.Documents(t, spread.Bytes)
	before := testutil.Named(t, builtDocs, "Deployment", op.deployment)
	after := testutil.Named(t, spreadDocs, "Deployment", op.deployment)
	selector := testutil.Field(t, before, "spec", "selector")
	affinity := testutil.Field(t, after, "spec", "template", "spec", "affinity")

	assert.Equal(t, 2, testutil.Field(t, after, "spec", "replicas"), "the replicas of %s", op.deployment)
	assert.Equal(t, []any{map[string]any{"labelSelector": selector, "topologyKey": "topology.kubernetes.io/zone"}},
		testutil.Field(t, affinity, "podAntiAffinity", "requiredDuringSchedulingIgnoredDuringExecution"), "the zone rule of %s", op.deployment)
	assert.Equal(t, withZoneLabel(t, testutil.Field(t, before, "spec", "template", "spec", "affinity", "nodeAffinity")), testutil.Field(t, affinity, "nodeAffinity"),
		"the node rule of %s, which keeps it off burst members and off a node the labeller has not given a zone yet", op.deployment)
	assert.Equal(t, op.strategy, testutil.Field(t, after, "spec", "strategy"), "the update strategy of %s", op.deployment)
	budget := testutil.Named(t, spreadDocs, "PodDisruptionBudget", op.deployment)
	assert.Equal(t, "cnpg-system", testutil.Field(t, budget, "metadata", "namespace"), "the namespace of %s's disruption budget", op.deployment)
	assert.Equal(t, map[string]any{"maxUnavailable": 1, "selector": selector}, budget["spec"], "the disruption budget of %s", op.deployment)
	assert.Equal(t, withoutDeployment(builtDocs, op.deployment), withoutDeployment(withoutKind(spreadDocs, "PodDisruptionBudget"), op.deployment),
		"every other document of %s as built", op.manifest)
}

// withZoneLabel is nodeAffinity's one node term with a zone label required, since the scheduler skips a zone rule on a
// node without one.
func withZoneLabel(t *testing.T, nodeAffinity any) map[string]any {
	t.Helper()
	terms, ok := testutil.Field(t, nodeAffinity, "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms").([]any)
	require.True(t, ok && len(terms) == 1, "the chart's node rule holds one term: %v", nodeAffinity)
	expressions, ok := testutil.Field(t, terms[0], "matchExpressions").([]any)
	require.True(t, ok, "the chart's node term matches expressions: %v", terms[0])
	zoned := append(slices.Clone(expressions), map[string]any{"key": "topology.kubernetes.io/zone", "operator": "Exists"})
	return map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
		"nodeSelectorTerms": []any{map[string]any{"matchExpressions": zoned}}}}
}

func TestDatabaseOperatorsAcrossZones_RefusesASetThatLacksAnOperatorItMustSpread(t *testing.T) {
	for _, tc := range []struct {
		name    string
		set     components.Set
		missing string
	}{
		{"the default set without its CloudNativePG operator", components.Default(), "cloudnative-pg"},
		{"the hosting set without its CloudNativePG operator", components.HostingServed(), "cloudnative-pg"},
		{"the hosting set, which serves cert-manager, without its Barman Cloud plugin", components.HostingServed(), "plugin-barman-cloud"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := tc.set
			set.Served = slices.DeleteFunc(slices.Clone(set.Served), func(m components.Manifest) bool { return m.Name == tc.missing })

			_, err := set.DatabaseOperatorsAcrossZones([]string{"fsn1", "nbg1"})

			assert.ErrorContains(t, err, tc.missing, "the spread of %s over two zones", tc.name)
		})
	}
}

func withoutKind(docs []map[string]any, kind string) []map[string]any {
	return slices.DeleteFunc(slices.Clone(docs), func(doc map[string]any) bool { return doc["kind"] == kind })
}

func withoutDeployment(docs []map[string]any, name string) []map[string]any {
	return slices.DeleteFunc(slices.Clone(docs), func(doc map[string]any) bool {
		meta, _ := doc["metadata"].(map[string]any)
		return doc["kind"] == "Deployment" && meta["name"] == name
	})
}

func manifestSums(set components.Set) []byte {
	var out strings.Builder
	for _, m := range set.Served {
		fmt.Fprintf(&out, "served %s %s\n", m.Name, m.SHA256())
	}
	for _, m := range set.Inline {
		fmt.Fprintf(&out, "inline %s %s\n", m.Name, m.SHA256())
	}
	return []byte(out.String())
}

func TestSets_RenderEveryManifestOfTheDefaultAndHostingServedSetsByteForByteAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    components.Set
		golden string
	}{
		{"the default set a cluster with no runner services runs", components.Default(), "default-manifests.txt"},
		{"the served half of the hosting set the gateway of a hosting cluster serves", components.HostingServed(), "hosting-served-manifests.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Golden(t, filepath.Join("testdata", "golden", tc.golden), manifestSums(tc.set))
		})
	}
}
