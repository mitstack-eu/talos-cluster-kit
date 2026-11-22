package nodeconfig_test

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/mirror"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func TestRender_MatchesTheGoldenConfiguration(t *testing.T) {
	cases := []struct {
		name      string
		profile   nodeconfig.Profile
		locations int
		hostnames []string
	}{
		{name: "a Cloud cluster of one control-plane node", profile: nodeconfig.HCloud, locations: 1, hostnames: []string{""}},
		{name: "a Cloud cluster of three control-plane nodes shares one image", profile: nodeconfig.HCloud, locations: 3, hostnames: []string{""}},
		{name: "a dedicated cluster of one control-plane node", profile: nodeconfig.Metal, locations: 1, hostnames: []string{"cp-fsn1"}},
		{name: "a dedicated cluster of three control-plane nodes has one image per node", profile: nodeconfig.Metal, locations: 3, hostnames: []string{"cp-fsn1", "cp-nbg1", "cp-hel1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testutil.NewCluster(t, testutil.WithProfile(tc.profile), testutil.WithLocations(tc.locations))

			for _, hostname := range tc.hostnames {
				got, err := nodeconfig.Render(cluster, nodeconfig.Node{Hostname: hostname})

				require.NoError(t, err)
				testutil.Golden(t, goldenPath(tc.profile, tc.locations, hostname), got)
			}
		})
	}
}

func goldenPath(profile nodeconfig.Profile, locations int, hostname string) string {
	name := fmt.Sprintf("%s-%d-control-plane", profile, locations)
	if hostname != "" {
		name += "-" + hostname
	}
	return filepath.Join("testdata", "golden", name+".yaml")
}

// robotWorker is a dedicated server joining a cluster whose control plane spans three locations.
func robotWorker(t *testing.T) (nodeconfig.Cluster, nodeconfig.Node) {
	t.Helper()
	return testutil.NewCluster(t, testutil.WithProfile(nodeconfig.Metal), testutil.WithLocations(3)), nodeconfig.Node{Hostname: "wk-fsn1"}
}

type diskImage struct{}

func (diskImage) String() string        { return "cloud" }
func (diskImage) RequiresInstall() bool { return false }
func (diskImage) InContainer() bool     { return false }

func TestRenderWorker_IsAWorkerTalosAcceptsThatJoinsThroughThePrivateAPIEndpointAndAppliesNoManifest(t *testing.T) {
	cluster, node := robotWorker(t)

	got, err := nodeconfig.RenderWorker(cluster, node)

	require.NoError(t, err)
	cfg, err := configloader.NewFromBytes(got)
	require.NoError(t, err)
	_, err = cfg.Validate(diskImage{})
	require.NoError(t, err, "Talos's schema refuses the worker configuration")
	assert.Equal(t, machine.TypeWorker, cfg.Machine().Type())
	assert.Equal(t, "https://10.0.0.3:6443", cfg.K8sClusterConfig().ClusterEndpoint().String(), "the first location's private API load balancer")
	docs := testutil.Documents(t, got)
	for _, kind := range []string{"KubeInlineManifestConfig", "KubeExternalManifestConfig", "KubeTalosAPIAccessConfig"} {
		assert.Empty(t, testutil.Kind(docs, kind), "only a control-plane node applies a %s", kind)
	}
	testutil.Golden(t, filepath.Join("testdata", "golden", "metal-3-worker-wk-fsn1.yaml"), got)
}

func TestRenderWorker_HoldsTheGoldenListOfSecretsWithNeitherTheEtcdNorTheServiceAccountSigningKey(t *testing.T) {
	cluster, node := robotWorker(t)
	controlPlane, err := nodeconfig.Render(cluster, nodeconfig.Node{Hostname: "cp-fsn1"})
	require.NoError(t, err)
	require.Subset(t, heldSecrets(t, controlPlane), []string{"certs.etcd.key", "certs.k8sserviceaccount.key"}, "the search misses a key a control-plane node holds")

	got, err := nodeconfig.RenderWorker(cluster, node)

	require.NoError(t, err)
	held := heldSecrets(t, got)
	assert.NotContains(t, held, "certs.etcd.key", "the worker could issue etcd certificates")
	assert.NotContains(t, held, "certs.k8sserviceaccount.key", "the worker could sign service-account tokens")
	testutil.Golden(t, filepath.Join("testdata", "golden", "metal-3-worker-secrets.txt"), []byte(strings.Join(held, "\n")+"\n"))
}

// heldSecrets names each field of the fixture secrets whose value config holds, as is or decoded, at any indentation.
func heldSecrets(t *testing.T, config []byte) []string {
	t.Helper()
	var bundle map[string]any
	require.NoError(t, yaml.Unmarshal(testutil.ReadTestdata(t, "secrets.yaml"), &bundle))
	flat := strings.Join(strings.Fields(string(config)), "")
	var held []string
	var walk func(path string, node any)
	walk = func(path string, node any) {
		if fields, ok := node.(map[string]any); ok {
			for key, child := range fields {
				walk(strings.TrimPrefix(path+"."+key, "."), child)
			}
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(node.(string))
		if strings.Contains(flat, node.(string)) || err == nil && strings.Contains(flat, strings.Join(strings.Fields(string(decoded)), "")) {
			held = append(held, path)
		}
	}
	walk("", bundle)
	slices.Sort(held)
	return held
}

func TestRender_WithNoLogReceiverLeavesOutTheCollectorAndBothLogSinksAndNothingElse(t *testing.T) {
	noReceiver := testutil.NewCluster(t)
	noReceiver.Logs = components.LogExport{}
	withReceiver, err := nodeconfig.Render(testutil.NewCluster(t), nodeconfig.Node{})
	require.NoError(t, err)

	got, err := nodeconfig.Render(noReceiver, nodeconfig.Node{})

	require.NoError(t, err)
	assert.Equal(t, withoutLogParts(testutil.Documents(t, withReceiver)), testutil.Documents(t, got))
}

// withoutLogParts drops the service log destination, the kernel log sink and the Collector's manifest.
func withoutLogParts(docs []map[string]any) []map[string]any {
	var kept []map[string]any
	for _, doc := range docs {
		name, _ := doc["name"].(string)
		if doc["kind"] == "KmsgLogConfig" || testutil.ManifestName(name) == "mitstack-node-logs" {
			continue
		}
		if machine, ok := doc["machine"].(map[string]any); ok {
			delete(machine, "logging")
		}
		kept = append(kept, doc)
	}
	return kept
}

func TestRender_RefusesALogReceiverWithACAButNoEndpoint(t *testing.T) {
	cluster := testutil.NewCluster(t)
	cluster.Logs.Endpoint = ""

	_, err := nodeconfig.Render(cluster, nodeconfig.Node{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `log export endpoint "" is not host:port`)
}

func TestRender_PullsEveryImageOfTheComponentSetFromEveryGatewaysMirror(t *testing.T) {
	cluster := testutil.NewCluster(t, testutil.WithLocations(3))
	images, err := mirror.List(cluster.Components)
	require.NoError(t, err)

	got, err := nodeconfig.Render(cluster, nodeconfig.Node{})

	require.NoError(t, err)
	docs := testutil.Documents(t, got)
	mirrors := map[string]map[string]any{}
	for _, doc := range testutil.Kind(docs, "RegistryMirrorConfig") {
		mirrors[doc["name"].(string)] = doc
	}
	for _, image := range images {
		doc, ok := mirrors[image.Registry()]
		require.True(t, ok, "image %s comes from %s, which has no RegistryMirrorConfig", image.Ref(), image.Registry())
		assert.Equal(t, true, doc["skipFallback"], "registry %s falls back to the public registry", image.Registry())
		assert.Equal(t, gatewayEndpoints(cluster.Gateways, image.Registry()), doc["endpoints"], "registry %s endpoints", image.Registry())
	}
	for _, kind := range []string{"KubeletConfig", "KubeAPIServerConfig", "KubeControllerManagerConfig", "KubeSchedulerConfig"} {
		ref := testutil.Field(t, testutil.Kind(docs, kind)[0], "image").(string)
		assert.True(t, slices.ContainsFunc(images, func(i components.Image) bool { return i.Name+":"+i.Tag == ref }),
			"%s pulls %s, which the mirror list %v lacks", kind, ref, images)
	}
}

func gatewayEndpoints(gateways []netip.Addr, registry string) []any {
	var endpoints []any
	for _, gw := range gateways {
		endpoints = append(endpoints, map[string]any{"url": "https://" + gw.String() + "/v2/" + registry, "overridePath": true})
	}
	return endpoints
}

func TestRender_UsesHetznersResolversAndTimeServersInPlaceOfTalosDefaults(t *testing.T) {
	cluster := testutil.NewCluster(t)

	got, err := nodeconfig.Render(cluster, nodeconfig.Node{})

	require.NoError(t, err)
	docs := testutil.Documents(t, got)
	resolvers := testutil.Kind(docs, "ResolverConfig")
	require.Len(t, resolvers, 1)
	assert.Equal(t, []any{map[string]any{"address": "185.12.64.1"}, map[string]any{"address": "185.12.64.2"}}, resolvers[0]["nameservers"])
	timeSync := testutil.Kind(docs, "TimeSyncConfig")
	require.Len(t, timeSync, 1)
	assert.Equal(t, []any{"ntp1.hetzner.de", "ntp2.hetzner.com", "ntp3.hetzner.net"}, testutil.Field(t, timeSync[0], "ntp", "servers"))
	assert.Empty(t, testutil.Kind(docs, "DiscoveryServiceConfig"), "the configuration still reaches Sidero's discovery service")
}

func TestRender_GivesACloudNodeADefaultRouteThroughHetznersRouterAndKeepsDHCPOnItsLink(t *testing.T) {
	cases := []struct {
		name      string
		profile   nodeconfig.Profile
		hostname  string
		wantLinks []map[string]any
		wantDHCP  []map[string]any
	}{
		{
			name: "a Cloud node, whose only link is on the private network", profile: nodeconfig.HCloud,
			wantLinks: []map[string]any{{"apiVersion": "v1alpha1", "kind": "LinkConfig", "name": "eth0", "routes": []any{map[string]any{"gateway": "10.0.0.1"}}}},
			wantDHCP:  []map[string]any{{"apiVersion": "v1alpha1", "kind": "DHCPv4Config", "name": "eth0"}},
		},
		{name: "a dedicated node, whose addresses its own trial decides", profile: nodeconfig.Metal, hostname: "cp-fsn1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testutil.NewCluster(t, testutil.WithProfile(tc.profile))

			got, err := nodeconfig.Render(cluster, nodeconfig.Node{Hostname: tc.hostname})

			require.NoError(t, err)
			docs := testutil.Documents(t, got)
			assert.Equal(t, tc.wantLinks, testutil.Kind(docs, "LinkConfig"))
			assert.Equal(t, tc.wantDHCP, testutil.Kind(docs, "DHCPv4Config"))
		})
	}
}

func directoryVolume(name string) map[string]any {
	return map[string]any{"apiVersion": "v1alpha1", "kind": "UserVolumeConfig", "name": name, "volumeType": "directory"}
}

func TestRender_GivesAHostingClusterOpenBaosUserVolumeBesideTheLocalPathVolume(t *testing.T) {
	hosting, err := components.Hosting(testutil.HostingInputs(t))
	require.NoError(t, err)
	cluster := testutil.NewCluster(t, testutil.WithLocations(3))
	cluster.Components = hosting

	got, err := nodeconfig.Render(cluster, nodeconfig.Node{})

	require.NoError(t, err)
	docs := testutil.Documents(t, got)
	assert.Equal(t, []map[string]any{directoryVolume(components.LocalPathVolume), directoryVolume(components.OpenBaoVolume)},
		testutil.Kind(docs, "UserVolumeConfig"))
	assert.Subset(t, documentNames(testutil.Kind(docs, "KubeInlineManifestConfig")),
		[]any{"mitstack-openbao-seal-key", "mitstack-sealed-secrets-key", "mitstack-openbao-deploy-key", "mitstack-cluster-repository", "mitstack-openbao-storage"})
}

func TestRender_GivesEveryClusterADefaultStorageClassThatProvisionsOnItsNodesUserVolume(t *testing.T) {
	for name, nodes := range map[string]int{"one node": 1, "three nodes": 3} {
		t.Run(name, func(t *testing.T) {
			cluster := testutil.NewCluster(t, testutil.WithLocations(nodes))
			images, err := mirror.List(cluster.Components)
			require.NoError(t, err)

			got, err := nodeconfig.Render(cluster, nodeconfig.Node{})

			require.NoError(t, err)
			docs := testutil.Documents(t, got)
			assert.Equal(t, []map[string]any{directoryVolume(components.LocalPathVolume)}, testutil.Kind(docs, "UserVolumeConfig"))
			assert.Contains(t, documentNames(testutil.Kind(docs, "KubeExternalManifestConfig")), "local-path-provisioner", "no node fetches the provisioner")
			provisioner := testutil.Documents(t, testutil.Manifest(t, "local-path-provisioner", cluster.Components.Served).Bytes)
			deployment, config := testutil.Kind(provisioner, "Deployment")[0], testutil.Kind(provisioner, "ConfigMap")[0]
			helper := testutil.Documents(t, []byte(testutil.Field(t, config, "data", "helperPod.yaml").(string)))[0]
			classes := defaultStorageClasses(t, cluster.Components)
			require.Len(t, classes, 1, "the set's default storage classes")
			command := testutil.Field(t, deployment, "spec", "template", "spec", "containers", "0", "command").([]any)
			assert.Equal(t, command[slices.Index(command, any("--provisioner-name"))+1], classes[0]["provisioner"], "the default class names another provisioner")
			assert.Equal(t, "WaitForFirstConsumer", classes[0]["volumeBindingMode"])
			assert.Equal(t, "Delete", classes[0]["reclaimPolicy"], "a deleted claim's directory stays on the node")
			assert.Equal(t, map[string]any{
				"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}},
				"readOnlyRootFilesystem": true, "runAsGroup": 65534, "runAsUser": 65534,
			}, testutil.Field(t, deployment, "spec", "template", "spec", "containers", "0", "securityContext"), "the provisioner's container")
			assert.Equal(t, "local", testutil.Field(t, classes[0], "metadata", "annotations", "defaultVolumeType"), "a hostPath volume comes back empty on a node that lost its directory")
			var paths struct{ NodePathMap []struct{ Paths []string } }
			require.NoError(t, json.Unmarshal([]byte(testutil.Field(t, config, "data", "config.json").(string)), &paths))
			require.Len(t, paths.NodePathMap, 1, "the provisioner's node path map")
			assert.Equal(t, []string{components.UserVolumePath(components.LocalPathVolume)}, paths.NodePathMap[0].Paths)
			for _, pod := range []any{testutil.Field(t, deployment, "spec", "template"), helper} {
				image, err := components.ParseImage(testutil.Field(t, pod, "spec", "containers", "0", "image").(string))
				require.NoError(t, err)
				assert.Contains(t, images, image, "the mirror holds no %s", image.Ref())
			}
			assert.Empty(t, credentials(t, append(provisioner, helper)))
		})
	}
}

func TestRender_RunsTheMeshAndItsAdmissionPoliciesOnEveryProfileAndSize(t *testing.T) {
	for _, profile := range []nodeconfig.Profile{nodeconfig.HCloud, nodeconfig.Metal} {
		for _, nodes := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s with %d nodes", profile, nodes), func(t *testing.T) {
				cluster := testutil.NewCluster(t, testutil.WithProfile(profile), testutil.WithLocations(nodes))
				node := nodeconfig.Node{}
				if profile == nodeconfig.Metal {
					node.Hostname = "cp-fsn1"
				}

				got, err := nodeconfig.Render(cluster, node)

				require.NoError(t, err)
				docs := testutil.Documents(t, got)
				assert.Subset(t, documentNames(testutil.Kind(docs, "KubeExternalManifestConfig")),
					[]any{"istio-base", "istiod", "istio-cni", "ztunnel", "mitstack-mesh-probe"}, "no node fetches the mesh or its probe target")
				assert.Subset(t, documentNames(testutil.Kind(docs, "KubeInlineManifestConfig")),
					[]any{"mitstack-strict-mtls", "mitstack-ambient-probes", "mitstack-require-mesh", "mitstack-require-strict-mtls"},
					"the configuration carries no mesh-wide STRICT policy or no admission policy for it")
			})
		}
	}
}

func TestRender_EncryptsStateAndEphemeralWithLUKS2UnderTheNodeIDKeyWithEphemeralsKeyLockedToState(t *testing.T) {
	nodeIDKey := map[string]any{"slot": 0, "nodeID": map[string]any{}}
	want := map[string]map[string]any{
		"STATE": {"apiVersion": "v1alpha1", "kind": "VolumeConfig", "name": "STATE",
			"encryption": map[string]any{"provider": "luks2", "keys": []any{nodeIDKey}}},
		"EPHEMERAL": {"apiVersion": "v1alpha1", "kind": "VolumeConfig", "name": "EPHEMERAL", "mount": map[string]any{"secure": true},
			"encryption": map[string]any{"provider": "luks2", "keys": []any{map[string]any{"slot": 0, "nodeID": map[string]any{}, "lockToState": true}}}},
	}
	for _, profile := range []nodeconfig.Profile{nodeconfig.HCloud, nodeconfig.Metal} {
		for _, nodes := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s with %d nodes", profile, nodes), func(t *testing.T) {
				cluster := testutil.NewCluster(t, testutil.WithProfile(profile), testutil.WithLocations(nodes))
				node := nodeconfig.Node{}
				if profile == nodeconfig.Metal {
					node.Hostname = "cp-fsn1"
				}

				got, err := nodeconfig.Render(cluster, node)

				require.NoError(t, err)
				volumes := map[string]map[string]any{}
				for _, doc := range testutil.Kind(testutil.Documents(t, got), "VolumeConfig") {
					volumes[doc["name"].(string)] = doc
				}
				assert.Equal(t, want, volumes)
			})
		}
	}
}

// meshDocs returns the documents of the named manifests of set, in order.
func meshDocs(t *testing.T, set components.Set, names ...string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for _, name := range names {
		docs = append(docs, testutil.Documents(t, testutil.Manifest(t, name, set.Served, set.Inline).Bytes)...)
	}
	return docs
}

func TestRender_PutsEveryPodButKubeSystemsIstiosAndKyvernosInTheMeshFromTheMirror(t *testing.T) {
	set := components.Default()
	images, err := mirror.List(set)
	require.NoError(t, err)
	cni := testutil.Kind(meshDocs(t, set, "istio-cni"), "ConfigMap")[0]["data"].(map[string]any)
	istio := meshDocs(t, set, "istio-base", "istiod")
	cilium := testutil.Kind(meshDocs(t, set, "cilium"), "ConfigMap")[0]["data"].(map[string]any)
	exempt := testutil.Field(t, meshDocs(t, set, "mitstack-require-mesh")[0], "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values")

	assert.Equal(t, "- namespaceSelector: {}\n  podSelector: {}", cni["AMBIENT_ENABLEMENT_SELECTOR"], "a label could take a pod out of the mesh")
	assert.Equal(t, "kube-system,istio-system,kyverno", cni["EXCLUDE_NAMESPACES"])
	assert.Subset(t, exempt, []any{"kube-system", "istio-system", "kyverno"}, "Kyverno checks a namespace the mesh skips")
	assert.Equal(t, "false", cni["AMBIENT_DNS_CAPTURE"], "ztunnel would answer lookups Cilium's DNS proxy must see")
	for _, name := range []string{"docker.io/istio/pilot", "docker.io/istio/install-cni", "docker.io/istio/ztunnel"} {
		assert.True(t, slices.ContainsFunc(images, func(i components.Image) bool { return i.Name == name }), "the mirror holds no %s", name)
	}
	assert.Equal(t, "privileged", testutil.Field(t, testutil.Kind(istio, "Namespace")[0], "metadata", "labels", "pod-security.kubernetes.io/enforce"))
	for _, cm := range testutil.Kind(istio, "ConfigMap") {
		assert.NotEqual(t, "istio-sidecar-injector", testutil.Field(t, cm, "metadata", "name"), "istiod could give a pod a sidecar, which takes it out of the mesh")
	}
	assert.Equal(t, "false", cilium["cni-exclusive"], "Cilium would delete the CNI configuration Istio chains")
	assert.Equal(t, "true", cilium["bpf-lb-sock-hostns-only"], "Cilium's socket load balancing would hide service calls from ztunnel")
}

func TestRender_RefusesACallWithoutAWorkloadIdentityButOnTheWebhookWebAndScrapedMetricsPorts(t *testing.T) {
	set, err := components.Hosting(testutil.HostingInputs(t))
	require.NoError(t, err)
	var peers []map[string]any
	for _, m := range slices.Concat(set.Served, set.Inline) {
		peers = append(peers, testutil.Kind(testutil.Documents(t, m.Bytes), "PeerAuthentication")...)
	}
	policy := meshDocs(t, set, "mitstack-require-strict-mtls")[0]
	admitted := testutil.Field(t, policy, "spec", "matchConditions", "0", "expression").(string)
	exempt := testutil.Field(t, policy, "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values").([]any)

	got := map[string]string{}
	for _, peer := range peers {
		namespace := testutil.Field(t, peer, "metadata", "namespace")
		key := fmt.Sprintf("%v/%v", namespace, testutil.Field(t, peer, "metadata", "name"))
		assert.Equal(t, "STRICT", testutil.Field(t, peer, "spec", "mtls", "mode"), key)
		got[key] = ""
		if ports, ok := testutil.Field(t, peer, "spec").(map[string]any)["portLevelMtls"]; ok {
			got[key] = fmt.Sprint(ports)
			assert.True(t, slices.Contains(exempt, namespace) || strings.Contains(admitted, `"`+key+`"`), "Kyverno refuses %s", key)
		}
	}
	permissive := func(ports ...int) string {
		m := map[any]any{}
		for _, p := range ports {
			m[p] = map[string]any{"mode": "PERMISSIVE"}
		}
		return fmt.Sprint(m)
	}
	assert.Equal(t, map[string]string{
		"istio-system/default":                               "",
		"cnpg-system/mitstack-cloudnative-pg-webhook":        permissive(9443),
		"cert-manager/mitstack-cert-manager-webhook":         permissive(10250),
		"cert-manager/mitstack-trust-manager-webhook":        permissive(6443),
		"external-secrets/mitstack-external-secrets-webhook": permissive(10250),
		"traefik/mitstack-traefik-web":                       permissive(8000, 8443),
		"argocd/mitstack-argocd-controller-metrics":          permissive(8082),
		"openbao/mitstack-openbao-metrics":                   permissive(9101),
	}, got, "a port takes calls without a workload identity that neither the API server, an outside web client nor the telemetry Collector makes")
}

func TestRender_AdmitsZtunnelsPortWhereverArgoCDsChartAdmitsACallBetweenItsPods(t *testing.T) {
	set := components.Default()
	mesh := testutil.Kind(meshDocs(t, set, "mitstack-mesh-argocd"), "NetworkPolicy")

	var want []string
	for _, policy := range testutil.Kind(meshDocs(t, set, "argocd"), "NetworkPolicy") {
		for _, rule := range testutil.Field(t, policy, "spec", "ingress").([]any) {
			if from, _ := rule.(map[string]any)["from"].([]any); len(from) > 0 && testutil.Field(t, from, "0", "podSelector") != nil {
				want = append(want, fmt.Sprint(testutil.Field(t, policy, "spec", "podSelector"), from, []any{map[string]any{"port": 15008, "protocol": "TCP"}}))
			}
		}
	}
	var got []string
	for _, policy := range mesh {
		got = append(got, fmt.Sprint(testutil.Field(t, policy, "spec", "podSelector"), testutil.Field(t, policy, "spec", "ingress", "0", "from"), testutil.Field(t, policy, "spec", "ingress", "0", "ports")))
	}
	assert.ElementsMatch(t, want, got, "a call between Argo CD's pods inside the mesh arrives on 15008, which the chart's policies do not admit")
}

func documentNames(docs []map[string]any) []any {
	var names []any
	for _, doc := range docs {
		names = append(names, testutil.ManifestName(doc["name"].(string)))
	}
	return names
}

// defaultStorageClasses returns every StorageClass of the set that Kubernetes gives a claim naming no class.
func defaultStorageClasses(t *testing.T, set components.Set) []map[string]any {
	t.Helper()
	var classes []map[string]any
	for _, m := range slices.Concat(set.Served, set.Inline) {
		for _, class := range testutil.Kind(testutil.Documents(t, m.Bytes), "StorageClass") {
			if testutil.Field(t, class, "metadata", "annotations", "storageclass.kubernetes.io/is-default-class") == "true" {
				classes = append(classes, class)
			}
		}
	}
	return classes
}

var credentialName = regexp.MustCompile(`(?i)token|secret|password|key`)

// credentials names each Secret, reference to a Secret, variable named for a credential and RBAC rule on Secrets in docs.
func credentials(t *testing.T, docs []map[string]any) []string {
	t.Helper()
	var found []string
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch v := node.(type) {
		case map[string]any:
			for key, child := range v {
				if child != nil && slices.Contains([]string{"secret", "secretKeyRef", "secretRef", "imagePullSecrets"}, key) {
					found = append(found, path+"."+key)
				}
				walk(path+"."+key, child)
			}
		case []any:
			for i, child := range v {
				if strings.HasSuffix(path, ".env") && credentialName.MatchString(fmt.Sprint(testutil.Field(t, child, "name"))) {
					found = append(found, fmt.Sprintf("%s.%d", path, i))
				}
				walk(fmt.Sprintf("%s.%d", path, i), child)
			}
		}
	}
	for _, doc := range docs {
		name := fmt.Sprintf("%v %v", doc["kind"], testutil.Field(t, doc, "metadata", "name"))
		if doc["kind"] == "Secret" {
			found = append(found, name)
		}
		walk(name, doc)
		rules, _ := doc["rules"].([]any)
		for _, rule := range rules {
			resources, _ := testutil.Field(t, rule, "resources").([]any)
			if slices.Contains(resources, any("secrets")) || slices.Contains(resources, any("*")) {
				found = append(found, name+" grants access to Secrets")
			}
		}
	}
	return found
}

func TestRender_SignsServiceAccountTokensForTheIssuerItIsGiven(t *testing.T) {
	cluster := testutil.NewCluster(t)
	cluster.ServiceAccountIssuer = &url.URL{Scheme: "https", Host: "oidc.example.com", Path: "/clusters/shop"}

	got, err := nodeconfig.Render(cluster, nodeconfig.Node{})

	require.NoError(t, err)
	accounts := testutil.Kind(testutil.Documents(t, got), "KubeServiceAccountConfig")
	require.Len(t, accounts, 1)
	assert.Equal(t, "https://oidc.example.com/clusters/shop", testutil.Field(t, accounts[0], "issuer", "issuerURL"),
		"the issuerURL Talos passes the API server as --service-account-issuer")
}

func TestRender_RefusesAServiceAccountIssuerTheAPIServerServesNoDiscoveryFor(t *testing.T) {
	cases := []struct {
		name   string
		issuer *url.URL
		want   string
	}{
		{name: "no issuer", want: "no service-account issuer"},
		{name: "an http issuer", issuer: &url.URL{Scheme: "http", Host: "oidc.example.com", Path: "/shop"}, want: "http://oidc.example.com/shop is not an https URL"},
		{name: "an issuer with no host", issuer: &url.URL{Scheme: "https", Path: "/shop"}, want: "https:///shop is not an https URL"},
		{name: "an issuer with a query", issuer: &url.URL{Scheme: "https", Host: "oidc.example.com", Path: "/shop", RawQuery: "v=1"}, want: "https://oidc.example.com/shop?v=1 is not an https URL"},
		{name: "an issuer with a fragment", issuer: &url.URL{Scheme: "https", Host: "oidc.example.com", Path: "/shop", Fragment: "keys"}, want: "https://oidc.example.com/shop#keys is not an https URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testutil.NewCluster(t)
			cluster.ServiceAccountIssuer = tc.issuer

			_, err := nodeconfig.Render(cluster, nodeconfig.Node{})

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRender_CarriesNoAdminClientCertificate(t *testing.T) {
	cluster := testutil.NewCluster(t)

	got, err := nodeconfig.Render(cluster, nodeconfig.Node{})

	require.NoError(t, err)
	var certs []*x509.Certificate
	collectCertificates(t, got, &certs)
	for _, doc := range testutil.Documents(t, got) {
		collectBase64Certificates(t, doc, &certs)
	}
	require.NotEmpty(t, certs, "the configuration holds no certificate at all")
	for _, cert := range certs {
		assert.True(t, cert.IsCA, "certificate %q in the configuration is not a CA", cert.Subject)
		assert.NotContains(t, cert.Subject.Organization, "os:admin", "certificate %q is an admin client certificate", cert.Subject)
	}
}

// collectCertificates parses every PEM certificate block in data.
func collectCertificates(t *testing.T, data []byte, certs *[]*x509.Certificate) {
	t.Helper()
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		*certs = append(*certs, cert)
	}
}

// collectBase64Certificates finds the base64-encoded PEM certificates the v1alpha1 document keeps under crt fields.
func collectBase64Certificates(t *testing.T, node any, certs *[]*x509.Certificate) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			if s, ok := child.(string); ok && key == "crt" {
				data, err := base64.StdEncoding.DecodeString(s)
				require.NoError(t, err, "crt field %q", s)
				collectCertificates(t, data, certs)
				continue
			}
			collectBase64Certificates(t, child, certs)
		}
	case []any:
		for _, child := range v {
			collectBase64Certificates(t, child, certs)
		}
	}
}

func TestRender_RefusesANodeItsProfileCannotName(t *testing.T) {
	cases := []struct {
		name    string
		profile nodeconfig.Profile
		node    nodeconfig.Node
		want    string
	}{
		{name: "a Cloud node with a hostname of its own", profile: nodeconfig.HCloud, node: nodeconfig.Node{Hostname: "cp-fsn1"}, want: `hostname "cp-fsn1" is refused`},
		{name: "a dedicated node with no hostname", profile: nodeconfig.Metal, node: nodeconfig.Node{}, want: "needs its own hostname"},
		{name: "a profile imager has no part in", profile: "aws", node: nodeconfig.Node{}, want: `profile "aws"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testutil.NewCluster(t, testutil.WithProfile(tc.profile))

			_, err := nodeconfig.Render(cluster, tc.node)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestAdminTalosconfig_HoldsTheAdminRoleForItsTTLAlone(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	got, err := testutil.Secrets(t).AdminTalosconfig(netip.MustParseAddr("10.0.0.5"), 10*time.Minute, now)

	require.NoError(t, err)
	var cfg struct {
		Contexts map[string]struct {
			Endpoints []string `yaml:"endpoints"`
			Crt       string   `yaml:"crt"`
		} `yaml:"contexts"`
	}
	require.NoError(t, yaml.Unmarshal(got, &cfg))
	ctx := cfg.Contexts["admin"]
	assert.Equal(t, []string{"10.0.0.5"}, ctx.Endpoints)
	der, err := base64.StdEncoding.DecodeString(ctx.Crt)
	require.NoError(t, err)
	block, _ := pem.Decode(der)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, []string{"os:admin"}, cert.Subject.Organization)
	assert.Equal(t, now.Add(10*time.Minute), cert.NotAfter.UTC())
}

func vswitchNode(t *testing.T) nodeconfig.Node {
	t.Helper()
	mac, err := net.ParseMAC("9c:6b:00:55:d8:f5")
	require.NoError(t, err)
	return nodeconfig.Node{Hostname: "wk-hel1", VSwitch: &nodeconfig.VSwitch{MAC: mac, VLAN: 4000, Address: netip.MustParsePrefix("10.0.9.2/24")}}
}

func TestRenderWorker_PutsADedicatedNodeOnItsVSwitchThroughTheCardWithItsMAC(t *testing.T) {
	cluster, _ := robotWorker(t)

	got, err := nodeconfig.RenderWorker(cluster, vswitchNode(t))

	require.NoError(t, err)
	docs := testutil.Documents(t, got)
	assert.Equal(t, []map[string]any{{"apiVersion": "v1alpha1", "kind": "LinkAliasConfig", "name": "pub0",
		"selector": map[string]any{"match": `mac(link.permanent_addr) == "9c:6b:00:55:d8:f5"`}}}, testutil.Kind(docs, "LinkAliasConfig"))
	assert.Equal(t, []map[string]any{{"apiVersion": "v1alpha1", "kind": "LinkConfig", "name": "pub0", "up": true}}, testutil.Kind(docs, "LinkConfig"),
		"a LinkConfig with no address keeps Talos's default DHCP off the public card")
	assert.Equal(t, []map[string]any{{"apiVersion": "v1alpha1", "kind": "VLANConfig", "name": "vsw0", "vlanID": 4000, "parent": "pub0", "mtu": 1400,
		"addresses": []any{map[string]any{"address": "10.0.9.2/24"}}, "routes": []any{map[string]any{"gateway": "10.0.9.1"}}}}, testutil.Kind(docs, "VLANConfig"))
	assert.Empty(t, testutil.Kind(docs, "DHCPv4Config"))
}

func TestRender_RefusesAVSwitchItsProfileOrNetworkCannotCarry(t *testing.T) {
	cases := []struct {
		name    string
		profile nodeconfig.Profile
		edit    func(*nodeconfig.Node)
		want    string
	}{
		{"a Cloud node", nodeconfig.HCloud, func(n *nodeconfig.Node) { n.Hostname = "" }, "a Cloud node has no vSwitch"},
		{"an address outside the cluster network", nodeconfig.Metal, func(n *nodeconfig.Node) { n.VSwitch.Address = netip.MustParsePrefix("192.168.9.2/24") }, "outside the cluster network"},
		{"the vSwitch gateway's own address", nodeconfig.Metal, func(n *nodeconfig.Node) { n.VSwitch.Address = netip.MustParsePrefix("10.0.9.1/24") }, "the vSwitch gateway's"},
		{"VLAN 0", nodeconfig.Metal, func(n *nodeconfig.Node) { n.VSwitch.VLAN = 0 }, "VLAN 0"},
		{"a MAC of eight bytes", nodeconfig.Metal, func(n *nodeconfig.Node) { n.VSwitch.MAC = net.HardwareAddr{1, 2, 3, 4, 5, 6, 7, 8} }, "MAC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testutil.NewCluster(t, testutil.WithProfile(tc.profile))
			node := vswitchNode(t)
			tc.edit(&node)

			_, err := nodeconfig.RenderWorker(cluster, node)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRenderWorker_LabelsTheNodeWithItsOwnLabelsBesideTheSubscription(t *testing.T) {
	cluster, node := robotWorker(t)
	node.Labels = map[string]string{"topology.kubernetes.io/zone": "hel1", "topology.kubernetes.io/region": "eu-central"}

	got, err := nodeconfig.RenderWorker(cluster, node)

	require.NoError(t, err)
	nodeConfigs := testutil.Kind(testutil.Documents(t, got), "KubeNodeConfig")
	require.Len(t, nodeConfigs, 1)
	assert.Equal(t, map[string]any{
		nodeconfig.SubscriptionLabel:    cluster.SubscriptionID,
		"topology.kubernetes.io/zone":   "hel1",
		"topology.kubernetes.io/region": "eu-central",
	}, testutil.Field(t, nodeConfigs[0], "labels"))
}

func TestRender_RefusesNodeLabelsEveryCloudNodeWouldShareThatReplaceTheSubscriptionsOrThatKubernetesRefuses(t *testing.T) {
	cases := []struct {
		name    string
		profile nodeconfig.Profile
		node    nodeconfig.Node
		want    string
	}{
		{name: "a Cloud node, whose configuration every Cloud node boots", profile: nodeconfig.HCloud,
			node: nodeconfig.Node{Labels: map[string]string{"topology.kubernetes.io/zone": "fsn1"}}, want: "every Cloud node"},
		{name: "a label that replaces the subscription's", profile: nodeconfig.Metal,
			node: nodeconfig.Node{Hostname: "wk-hel1", Labels: map[string]string{nodeconfig.SubscriptionLabel: "sub_other"}}, want: nodeconfig.SubscriptionLabel},
		{name: "a value Kubernetes refuses", profile: nodeconfig.Metal,
			node: nodeconfig.Node{Hostname: "wk-hel1", Labels: map[string]string{"topology.kubernetes.io/zone": "hel1 dc1"}}, want: "hel1 dc1"},
		{name: "a key Kubernetes refuses", profile: nodeconfig.Metal,
			node: nodeconfig.Node{Hostname: "wk-hel1", Labels: map[string]string{"mitstack.dev/zone dc": "hel1"}}, want: `"zone dc" is invalid`},
		{name: "a label in Kubernetes's own namespace that NodeRestriction refuses a worker's kubelet", profile: nodeconfig.Metal,
			node: nodeconfig.Node{Hostname: "wk-hel1", Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"}}, want: "NodeRestriction"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testutil.NewCluster(t, testutil.WithProfile(tc.profile))

			_, err := nodeconfig.RenderWorker(cluster, tc.node)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// release names the kit release a test renders, as its tools image's digest names it.
const release = "sha256:5f0c1b2a3d4e5f60718293a4b5c6d7e8f90123456789abcdef0123456789abcd"

func TestRender_AnnotatesTheNodeWithTheReleaseItsConfigurationWasRenderedFrom(t *testing.T) {
	cluster := testutil.NewCluster(t)
	cluster.Release = release

	got, err := nodeconfig.RenderWorker(cluster, nodeconfig.Node{})

	require.NoError(t, err)
	nodeConfigs := testutil.Kind(testutil.Documents(t, got), "KubeNodeConfig")
	require.Len(t, nodeConfigs, 1)
	assert.Equal(t, map[string]any{nodeconfig.MachineConfigAnnotation: release}, testutil.Field(t, nodeConfigs[0], "annotations"))
}

// renderedNode is one kind of node, the cluster it belongs to and how a release renders its configuration.
type renderedNode struct {
	name    string
	cluster func() nodeconfig.Cluster
	render  func(nodeconfig.Cluster) ([]byte, error)
}

func renderedNodes(t *testing.T) []renderedNode {
	t.Helper()
	controlPlane := func(n nodeconfig.Node) func(nodeconfig.Cluster) ([]byte, error) {
		return func(c nodeconfig.Cluster) ([]byte, error) { return nodeconfig.Render(c, n) }
	}
	worker := func(n nodeconfig.Node) func(nodeconfig.Cluster) ([]byte, error) {
		return func(c nodeconfig.Cluster) ([]byte, error) { return nodeconfig.RenderWorker(c, n) }
	}
	dedicated := vswitchNode(t)
	dedicated.Labels = map[string]string{"topology.kubernetes.io/zone": "hel1"}
	cloud := func() nodeconfig.Cluster { return testutil.NewCluster(t) }
	return []renderedNode{
		{name: "a Cloud control-plane node of one location", cluster: cloud, render: controlPlane(nodeconfig.Node{})},
		{name: "a Cloud control-plane node of three locations that keeps apps off it", render: controlPlane(nodeconfig.Node{}), cluster: func() nodeconfig.Cluster {
			c := testutil.NewCluster(t, testutil.WithLocations(3))
			c.AppsOnWorkers = true
			return c
		}},
		{name: "a Cloud control-plane node with no log receiver", render: controlPlane(nodeconfig.Node{}), cluster: func() nodeconfig.Cluster {
			c := testutil.NewCluster(t)
			c.Logs = components.LogExport{}
			return c
		}},
		{name: "a dedicated control-plane node", render: controlPlane(nodeconfig.Node{Hostname: "cp-fsn1"}), cluster: func() nodeconfig.Cluster {
			return testutil.NewCluster(t, testutil.WithProfile(nodeconfig.Metal))
		}},
		{name: "a Cloud worker", cluster: cloud, render: worker(nodeconfig.Node{})},
		{name: "a Cloud control-plane node that evicts at 85% disk", render: controlPlane(nodeconfig.Node{}), cluster: func() nodeconfig.Cluster {
			c := testutil.NewCluster(t)
			c.DiskEviction = true
			return c
		}},
		{name: "a Cloud worker that evicts at 85% disk", render: worker(nodeconfig.Node{}), cluster: func() nodeconfig.Cluster {
			c := testutil.NewCluster(t)
			c.DiskEviction = true
			return c
		}},
		{name: "a dedicated worker on a vSwitch with its own labels", render: worker(dedicated), cluster: func() nodeconfig.Cluster {
			return testutil.NewCluster(t, testutil.WithProfile(nodeconfig.Metal), testutil.WithLocations(3))
		}},
	}
}

func TestRerender_GivesARunningNodeTheConfigurationThisReleaseRendersForItUnderItsOwnKeysAndSettings(t *testing.T) {
	for _, tc := range renderedNodes(t) {
		t.Run(tc.name, func(t *testing.T) {
			cluster := tc.cluster()
			running, err := tc.render(cluster)
			require.NoError(t, err)
			cluster.Release = release
			want, err := tc.render(cluster)
			require.NoError(t, err)

			got, err := nodeconfig.Rerender(running, release)

			require.NoError(t, err)
			assert.Equal(t, testutil.Documents(t, want), testutil.Documents(t, got))
		})
	}
}

func TestRerender_GivesANodeAnEarlierReleaseConfiguredThisReleasesSettingsAndKeepsTheManifestsItApplied(t *testing.T) {
	cluster := testutil.NewCluster(t)
	current, err := nodeconfig.Render(cluster, nodeconfig.Node{})
	require.NoError(t, err)
	running := editDocuments(t, current, func(doc map[string]any) {
		switch doc["kind"] {
		case "TimeSyncConfig":
			doc["ntp"] = map[string]any{"servers": []any{"time.cloudflare.com"}}
		case "KubeInlineManifestConfig":
			doc["manifest"] = "kind: ConfigMap\napiVersion: v1\nmetadata:\n  name: applied-at-install\n"
		}
	})
	cluster.Release = release
	want, err := nodeconfig.Render(cluster, nodeconfig.Node{})
	require.NoError(t, err)

	got, err := nodeconfig.Rerender(running, release)

	require.NoError(t, err)
	gotDocs := testutil.Documents(t, got)
	assert.Equal(t, testutil.Kind(testutil.Documents(t, want), "TimeSyncConfig"), testutil.Kind(gotDocs, "TimeSyncConfig"))
	assert.Equal(t, testutil.Kind(testutil.Documents(t, running), "KubeInlineManifestConfig"), testutil.Kind(gotDocs, "KubeInlineManifestConfig"),
		"the components' manifests reach a running cluster through Argo CD, so the node keeps the ones it applied at install")
}

func TestRerender_RefusesARunningNodeWhoseSettingItTakesOnlyAtRebootTheReleaseChangesNamingTheSetting(t *testing.T) {
	cases := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"an etcd setting", func(doc map[string]any) {
			if doc["version"] == "v1alpha1" {
				etcd := doc["cluster"].(map[string]any)["etcd"].(map[string]any)
				etcd["extraArgs"] = map[string]any{"election-timeout": "5000"}
			}
		}, "cluster.etcd"},
		{"a kernel module", func(doc map[string]any) {
			if doc["version"] == "v1alpha1" {
				doc["machine"].(map[string]any)["kernel"] = map[string]any{"modules": []any{map[string]any{"name": "drbd"}}}
			}
		}, "machine.kernel"},
		{"a system volume's encryption", func(doc map[string]any) {
			if doc["kind"] == "VolumeConfig" && doc["name"] == "EPHEMERAL" {
				delete(doc, "encryption")
			}
		}, "VolumeConfig EPHEMERAL"},
		{"a user volume's kind of volume", func(doc map[string]any) {
			if doc["kind"] == "UserVolumeConfig" && doc["name"] == "local-path-provisioner" {
				doc["volumeType"] = "partition"
				doc["provisioning"] = map[string]any{"diskSelector": map[string]any{"match": "system_disk"}, "minSize": "1GiB"}
			}
		}, "UserVolumeConfig local-path-provisioner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current, err := nodeconfig.Render(testutil.NewCluster(t), nodeconfig.Node{})
			require.NoError(t, err)
			running := editDocuments(t, current, tc.edit)

			got, err := nodeconfig.Rerender(running, release)

			require.ErrorIs(t, err, nodeconfig.ErrRebootOnly)
			assert.Contains(t, err.Error(), tc.want)
			assert.Nil(t, got)
		})
	}
}

func TestRerender_RefusesWhatIsNoRunningNodesConfigurationOrNamesNoRelease(t *testing.T) {
	current, err := nodeconfig.Render(testutil.NewCluster(t), nodeconfig.Node{})
	require.NoError(t, err)
	withoutV1Alpha1 := editDocuments(t, current, func(doc map[string]any) {
		if doc["version"] == "v1alpha1" {
			clear(doc)
		}
	})
	cases := []struct {
		name     string
		running  []byte
		release  string
		wantText string
	}{
		{"bytes that are no configuration", []byte("{not: yaml: at all"), release, "read the running configuration"},
		{"a configuration without its v1alpha1 document", withoutV1Alpha1, release, "v1alpha1"},
		{"no release", current, "", "release"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nodeconfig.Rerender(tc.running, tc.release)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantText)
			assert.Nil(t, got)
		})
	}
}

func TestRerender_RefusesARunningConfigurationItCannotReadBackWholeNamingWhatItCannotRead(t *testing.T) {
	cluster := testutil.NewCluster(t)
	controlPlane, err := nodeconfig.Render(cluster, nodeconfig.Node{})
	require.NoError(t, err)
	worker, err := nodeconfig.RenderWorker(cluster, nodeconfig.Node{})
	require.NoError(t, err)
	edit := func(config []byte, kind string, change func(map[string]any)) []byte {
		return editDocuments(t, config, func(doc map[string]any) {
			if doc["kind"] == kind {
				change(doc)
			}
		})
	}
	drop := func(doc map[string]any) { clear(doc) }
	cases := []struct {
		name     string
		running  []byte
		wantText string
	}{
		{"a document this release does not render", append(slices.Clone(controlPlane),
			"---\napiVersion: v1alpha1\nkind: StaticHostConfig\nname: 10.0.0.9\nhostnames:\n    - added-by-hand\n"...), "StaticHostConfig"},
		{"no cluster document", edit(controlPlane, "KubeClusterConfig", drop), "KubeClusterConfig"},
		{"an API endpoint that names a host in place of an address", edit(controlPlane, "KubeClusterConfig", func(doc map[string]any) {
			doc["endpoint"] = "https://api.shop.example:6443"
		}), "api.shop.example"},
		{"no node document", edit(controlPlane, "KubeNodeConfig", drop), "KubeNodeConfig"},
		{"a kubelet image from another registry", edit(controlPlane, "KubeletConfig", func(doc map[string]any) {
			doc["image"] = "registry.example/kubelet:v1.37.1"
		}), "registry.example/kubelet:v1.37.1"},
		{"a worker that trusts no API server CA", edit(worker, "KubeAPIServerCAConfig", drop), "API server"},
		{"a worker that trusts a second API server CA", edit(worker, "KubeAPIServerCAConfig", func(doc map[string]any) {
			doc["acceptedCAs"] = append(doc["acceptedCAs"].([]any), string(testutil.ReadTestdata(t, "logs-ca.pem")))
		}), "2 API server CAs"},
		{"a kubelet image pinned by its digest", edit(controlPlane, "KubeletConfig", func(doc map[string]any) {
			doc["image"] = "ghcr.io/siderolabs/kubelet:v1.37.1@sha256:" + strings.Repeat("0", 64)
		}), "@sha256"},
		{"a taint beside the control plane's", edit(controlPlane, "KubeNodeConfig", func(doc map[string]any) {
			doc["taints"] = map[string]any{"example.com/dedicated": "apps:NoSchedule"}
		}), "example.com/dedicated"},
		{"a registry the node trusts beside its gateways", withDocument(t, controlPlane, map[string]any{
			"apiVersion": "v1alpha1", "kind": "RegistryTLSConfig", "name": "10.0.5.5", "ca": string(testutil.ReadTestdata(t, "logs-ca.pem")),
		}), "10.0.5.5"},
		{"a second set of trusted roots beside the gateways'", withDocument(t, controlPlane, map[string]any{
			"apiVersion": "v1alpha1", "kind": "TrustedRootsConfig", "name": "corp-proxy", "certificates": string(testutil.ReadTestdata(t, "logs-ca.pem")),
		}), "corp-proxy"},
		{"a worker running the control plane's taint", edit(worker, "KubeNodeConfig", func(doc map[string]any) {
			doc["taints"] = map[string]any{"node-role.kubernetes.io/control-plane": "NoSchedule"}
		}), "worker"},
		{"no gateway roots, with registries that trust no CA", editDocuments(t, controlPlane, func(doc map[string]any) {
			switch doc["kind"] {
			case "TrustedRootsConfig":
				clear(doc)
			case "RegistryTLSConfig":
				delete(doc, "ca")
			}
		}), "mitstack-gateway"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nodeconfig.Rerender(tc.running, release)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantText)
			assert.Nil(t, got)
		})
	}
}

// withDocument adds doc to a multi-document configuration.
func withDocument(t *testing.T, config []byte, doc map[string]any) []byte {
	t.Helper()
	added, err := yaml.Marshal(doc)
	require.NoError(t, err)
	return slices.Concat(config, []byte("---\n"), added)
}

// editDocuments applies edit to each document of a multi-document configuration, dropping one the edit leaves empty.
func editDocuments(t *testing.T, config []byte, edit func(map[string]any)) []byte {
	t.Helper()
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	for _, doc := range testutil.Documents(t, config) {
		edit(doc)
		if len(doc) == 0 {
			continue
		}
		require.NoError(t, encoder.Encode(doc))
	}
	require.NoError(t, encoder.Close())
	return out.Bytes()
}

// kubeletSettings is the KubeletConfiguration keys a node's KubeletConfig document sets.
func kubeletSettings(t *testing.T, config []byte) map[string]any {
	t.Helper()
	docs := testutil.Kind(testutil.Documents(t, config), "KubeletConfig")
	require.Len(t, docs, 1, "the KubeletConfig documents of the node's configuration")
	settings, _ := docs[0]["config"].(map[string]any)
	return settings
}

var diskEvictionSettings = map[string]any{
	"imageGCHighThresholdPercent": 80,
	"imageGCLowThresholdPercent":  70,
	"evictionSoft":                map[string]any{"imagefs.available": "15%", "nodefs.available": "15%"},
	"evictionSoftGracePeriod":     map[string]any{"imagefs.available": "1m", "nodefs.available": "1m"},
}

func TestRender_DiskEvictionCollectsImagesFrom80PercentAndEvictsPodsWhenLessThan15PercentOfADiskIsFree(t *testing.T) {
	for _, tc := range []struct {
		name   string
		render func(nodeconfig.Cluster) ([]byte, error)
	}{
		{"a control-plane node", func(c nodeconfig.Cluster) ([]byte, error) { return nodeconfig.Render(c, nodeconfig.Node{}) }},
		{"a worker", func(c nodeconfig.Cluster) ([]byte, error) { return nodeconfig.RenderWorker(c, nodeconfig.Node{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testutil.NewCluster(t)
			cluster.DiskEviction = true

			got, err := tc.render(cluster)

			require.NoError(t, err)
			assert.Equal(t, diskEvictionSettings, kubeletSettings(t, got), "the kubelet's settings")
		})
	}
}

func TestRender_WithoutDiskEvictionTheKubeletKeepsItsDefaultDiskSettings(t *testing.T) {
	cluster := testutil.NewCluster(t)

	got, err := nodeconfig.Render(cluster, nodeconfig.Node{})

	require.NoError(t, err)
	assert.Empty(t, kubeletSettings(t, got), "the kubelet's settings")
}

func TestRerender_KeepsTheDiskEvictionOfARunningNodeRenderedWithItAndAddsNoneToOneRenderedWithout(t *testing.T) {
	for _, tc := range []struct {
		name     string
		eviction bool
		want     map[string]any
	}{
		{"a node rendered with disk eviction", true, diskEvictionSettings},
		{"a node rendered without it", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testutil.NewCluster(t)
			cluster.DiskEviction = tc.eviction
			running, err := nodeconfig.Render(cluster, nodeconfig.Node{})
			require.NoError(t, err)

			got, err := nodeconfig.Rerender(running, release)

			require.NoError(t, err)
			if tc.want == nil {
				assert.Empty(t, kubeletSettings(t, got), "the kubelet's settings after the rerender")
				return
			}
			assert.Equal(t, tc.want, kubeletSettings(t, got), "the kubelet's settings after the rerender")
		})
	}
}
