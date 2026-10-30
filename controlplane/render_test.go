package controlplane_test

// The builder embeds the kit's configuration with the plane's own manifests, and the gateway runs the kit's cloud-config.

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/image"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

type mergeRule struct {
	Name     string   `yaml:"name"`
	Settings []string `yaml:"settings"`
}

type renderedCloudConfig struct {
	MergeHow   []mergeRule    `yaml:"merge_how"`
	WriteFiles []renderedFile `yaml:"write_files"`
	RunCmd     [][]string     `yaml:"runcmd"`
}

// renderedFile is a write_files entry as cloud-init reads it, its content still in the encoding it names.
type renderedFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
	Encoding    string `yaml:"encoding"`
}

func decodeCloudConfig(t *testing.T, data string) renderedCloudConfig {
	t.Helper()
	require.True(t, strings.HasPrefix(data, "#cloud-config\n"), "a cloud-config opens with its header")
	var c renderedCloudConfig
	require.NoError(t, yaml.Unmarshal([]byte(data), &c))
	return c
}

// file returns the file cloud-init writes at path, its content decoded as cloud-init's write_files decodes it.
func (c renderedCloudConfig) file(t *testing.T, path string) renderedFile {
	t.Helper()
	for _, f := range c.WriteFiles {
		if f.Path != path {
			continue
		}
		switch f.Encoding {
		case "":
		case "gz+b64":
			zipped, err := base64.StdEncoding.DecodeString(f.Content)
			require.NoError(t, err, "the base64 of %s", path)
			r, err := gzip.NewReader(bytes.NewReader(zipped))
			require.NoError(t, err, "the gzip of %s", path)
			plain, err := io.ReadAll(r)
			require.NoError(t, err, "the gzip of %s", path)
			f.Content, f.Encoding = string(plain), ""
		default:
			require.Failf(t, "unknown encoding", "%s is in the encoding %q, which this test does not decode", path, f.Encoding)
		}
		return f
	}
	require.Failf(t, "no such file", "the cloud-config writes no %s", path)
	return renderedFile{}
}

// gatewayParts splits the gateway's MIME user data into its cloud-config parts, in order.
func gatewayParts(t *testing.T, data string) []string {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(data))
	require.NoError(t, err)
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/mixed", mediaType)
	reader := multipart.NewReader(msg.Body, params["boundary"])
	var parts []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return parts
		}
		require.NoError(t, err)
		assert.Equal(t, `text/cloud-config; charset="us-ascii"`, part.Header.Get("Content-Type"))
		body, err := io.ReadAll(part)
		require.NoError(t, err)
		parts = append(parts, string(body))
	}
}

func installedOnce(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	require.NoError(t, h.converge())
	return h
}

func (h *harness) gatewayKitPart() renderedCloudConfig {
	h.t.Helper()
	return decodeCloudConfig(h.t, gatewayParts(h.t, h.cloud.UserData[h.byRole(controlplane.RoleGateway)[0].ID])[0])
}

// builderConfiguration reads the builder's create request, so it holds once the builder is deleted.
func builderConfiguration(t *testing.T, h *harness) []map[string]any {
	t.Helper()
	config := decodeCloudConfig(t, h.cloud.SpecFor(t, controlplane.RoleBuilder).UserData).file(t, controlplane.BuilderConfigPath)
	return testutil.Documents(t, []byte(config.Content))
}

func inlineManifests(t *testing.T, config []map[string]any) map[string][]map[string]any {
	t.Helper()
	manifests := map[string][]map[string]any{}
	for _, doc := range testutil.Kind(config, "KubeInlineManifestConfig") {
		manifests[testutil.ManifestName(doc["name"].(string))] = testutil.Documents(t, []byte(doc["manifest"].(string)))
	}
	return manifests
}

func TestBuilder_EmbedsAConfigurationThatCarriesTheReportTheCredentialsAndTheApplication(t *testing.T) {
	h := installedOnce(t)

	c := decodeCloudConfig(t, h.cloud.UserData[h.byRole(controlplane.RoleBuilder)[0].ID])

	config := c.file(t, controlplane.BuilderConfigPath)
	assert.Equal(t, "0600", config.Permissions, "the configuration's permissions")
	docs := testutil.Documents(t, []byte(config.Content))
	var inline []string
	for _, doc := range testutil.Kind(docs, "KubeInlineManifestConfig") {
		inline = append(inline, testutil.ManifestName(doc["name"].(string)))
	}
	assert.Subset(t, inline, []string{"mitstack-node-report", "mitstack-cluster-tokens", "mitstack-cluster-application"})
	clusterDocs := testutil.Kind(docs, "KubeClusterConfig")
	require.Len(t, clusterDocs, 1, "KubeClusterConfig documents")
	endpoint := testutil.Field(t, clusterDocs[0], "endpoint")
	assert.Equal(t, "https://"+h.loadBalancer(controlplane.RoleAPI).PrivateIPs[h.network().ID]+":6443", endpoint, "the control-plane endpoint")
	assert.Equal(t, []string{controlplane.BuilderScriptPath, image.PinnedImager, controlplane.BuilderConfigPath, controlplane.RouterAddress,
		builderMirrorURL, builderGatewayCA}, c.RunCmd[0], "the build command")
}

// The builder asks the gateway's mirror, at the gateway's fixed address, under the CA its user data carries.
const (
	builderMirrorURL = "https://" + controlplane.GatewayAddress + "/v2/"
	builderGatewayCA = "/root/mitstack-build/gateway-ca.crt"
)

func TestBuilder_BothBuildersWaitForTheGatewaysMirrorUnderTheGatewaysOwnCA(t *testing.T) {
	h := installedOnce(t)
	require.Len(t, h.installed, 1, "installs")

	for _, r := range []controlplane.Role{controlplane.RoleBuilder, controlplane.RoleWorkerBuilder} {
		c := decodeCloudConfig(t, h.cloud.SpecFor(t, r).UserData)

		require.Len(t, c.RunCmd, 1, "the %s's commands", r)
		require.GreaterOrEqual(t, len(c.RunCmd[0]), 6, "the %s's build command: %v", r, c.RunCmd[0])
		assert.Equal(t, []string{builderMirrorURL, builderGatewayCA}, c.RunCmd[0][4:6], "where the %s asks for the mirror, and the CA it trusts", r)
		ca := c.file(t, builderGatewayCA)
		assert.Equal(t, string(h.installed[0].GatewayCA), ca.Content, "the CA the %s trusts for the mirror", r)
		assert.Equal(t, "0644", ca.Permissions, "the %s's gateway CA's permissions", r)
	}
}

func TestBuilder_TheAPIServersCertificateNamesTheAPILoadBalancer(t *testing.T) {
	h := installedOnce(t)
	want := []any{h.loadBalancer(controlplane.RoleAPI).PrivateIPs[h.network().ID]}

	docs := builderConfiguration(t, h)

	assert.Equal(t, want, testutil.Field(t, docs[0], "machine", "certSANs"), "the Talos API's certificate names")
	apiServer := testutil.Kind(docs, "KubeAPIServerConfig")
	require.Len(t, apiServer, 1, "KubeAPIServerConfig documents")
	assert.Equal(t, want, testutil.Field(t, apiServer[0], "certExtraSANs"), "the Kubernetes API's certificate names")
}

func TestBuilder_TheApplicationSyncsTheClusterRepositoryTheEnrolmentNamesThePlatformAndTheReportRunsTheBlocksProofScript(t *testing.T) {
	h := installedOnce(t)

	manifests := inlineManifests(t, builderConfiguration(t, h))

	apps := testutil.Kind(manifests["mitstack-cluster-application"], "Application")
	require.Len(t, apps, 2, "Applications")
	for _, app := range apps {
		assert.Equal(t, testRepoURL, testutil.Field(t, app, "spec", "source", "repoURL"), "Application %v's repository", app["metadata"])
	}
	jobs := testutil.Kind(manifests["mitstack-cluster-tokens"], "Job")
	require.Len(t, jobs, 1, "enrolment Jobs")
	container := testutil.Field(t, jobs[0], "spec", "template", "spec", "containers", "0")
	assert.Equal(t, tools.Image, testutil.Field(t, container, "image"), "the enrolment's image")
	assert.Contains(t, testutil.Field(t, container, "env"), map[string]any{"name": "PLATFORM_URL", "value": platformURL})
	cronJobs := testutil.Kind(manifests["mitstack-node-report"], "CronJob")
	require.Len(t, cronJobs, 1, "node report CronJobs")
	report := testutil.Field(t, cronJobs[0], "spec", "jobTemplate", "spec", "template", "spec", "containers", "0")
	assert.Equal(t, tools.Image, testutil.Field(t, report, "image"), "the node report's image")
	assert.Equal(t, proofScript, testutil.ContainerEnv(t, report)["REPORT_PROOF_SCRIPT"], "the script that signs each report")
}

func TestGateway_RunsTheKitsCloudConfigAndThenBootstrapsTheFirstNodeWithAShortLivedAdminTalosconfig(t *testing.T) {
	h := installedOnce(t)
	gw := h.byRole(controlplane.RoleGateway)[0]

	parts := gatewayParts(t, h.cloud.UserData[gw.ID])

	require.Len(t, parts, 2, "user data parts")
	decodeCloudConfig(t, parts[0]).file(t, "/usr/local/bin/mitstack-talos-bootstrap")
	bootstrap := decodeCloudConfig(t, parts[1])
	assert.Equal(t, []mergeRule{{Name: "list", Settings: []string{"append"}}, {Name: "dict", Settings: []string{"no_replace", "recurse_list"}}}, bootstrap.MergeHow)
	talosconfig := bootstrap.file(t, controlplane.BootstrapTalosconfigPath)
	assert.Equal(t, "0600", talosconfig.Permissions)
	cert := adminCertificate(t, talosconfig.Content)
	assert.WithinDuration(t, testStart.Add(controlplane.AdminTalosconfigTTL), cert.NotAfter, time.Minute, "the admin certificate's expiry")
	assert.Equal(t, []string{"systemctl", "start", "--no-block", controlplane.BootstrapUnit}, bootstrap.RunCmd[0])
	assert.LessOrEqual(t, len(h.cloud.UserData[gw.ID]), cloudtest.UserDataLimit, "Hetzner's user data limit")
	assert.Contains(t, h.cloud.UserData[gw.ID], `boundary="mitstack-test-boundary"`, "the boundary the plane is given")
}

func TestGateway_StartsTheMirrorBesideItsFirewallAndPassesPullsThroughIt(t *testing.T) {
	kit := installedOnce(t).gatewayKitPart()

	assert.Contains(t, kit.file(t, "/etc/mitstack-gateway/Caddyfile").Content,
		"  route /v2/* {\n    reverse_proxy @pull 127.0.0.1:5000\n    respond 405\n  }\n", "the mirror tries the registry for a pull before refusing")
	assert.JSONEq(t, `{"iptables": false, "ip6tables": false, "bridge": "none"}`, kit.file(t, "/etc/docker/daemon.json").Content,
		"Docker writes iptables rules the gateway's nftables deletes, so the mirror never starts")
}

func TestGateway_ForwardsPort80ToTheIngressLoadBalancersPublicAddressAlone(t *testing.T) {
	h := installedOnce(t)
	ingress := h.loadBalancer(controlplane.RoleIngress)

	nftables := h.gatewayKitPart().file(t, "/etc/nftables.conf").Content

	require.NotEmpty(t, ingress.PublicIPv4, "the ingress load balancer's public address")
	assert.Contains(t, nftables, " ip daddr "+ingress.PublicIPv4+" tcp dport 80 accept\n", "cert-manager's HTTP-01 self-check to an app's hostname")
	assert.Len(t, regexp.MustCompile(`\b80\b`).FindAllString(nftables, -1), 1, "the gateway's rules naming port 80")
}

func TestBuilder_TheNodeRoutesThroughTheNetworksRouterAndKeepsItsDHCPAddress(t *testing.T) {
	docs := builderConfiguration(t, installedOnce(t))

	assert.Equal(t, []map[string]any{{"apiVersion": "v1alpha1", "kind": "LinkConfig", "name": "eth0",
		"routes": []any{map[string]any{"gateway": controlplane.RouterAddress}}}}, testutil.Kind(docs, "LinkConfig"), "the node's default route")
	assert.Equal(t, []map[string]any{{"apiVersion": "v1alpha1", "kind": "DHCPv4Config", "name": "eth0"}}, testutil.Kind(docs, "DHCPv4Config"),
		"the node's DHCP, which its LinkConfig would otherwise stop")
}

func TestBuilder_TheClusterRunsKyvernoAndCloudNativePGFromManifestsTheGatewayServes(t *testing.T) {
	config := builderConfiguration(t, installedOnce(t))

	served := map[string]map[string]any{}
	for _, doc := range testutil.Kind(config, "KubeExternalManifestConfig") {
		served[testutil.ManifestName(doc["name"].(string))] = doc
	}
	for _, name := range []string{"kyverno", "cloudnative-pg"} {
		require.Contains(t, served, name, "the served manifests the node applies")
		assert.Equal(t, "https://"+controlplane.GatewayAddress+"/manifests/"+served[name]["name"].(string)+".yaml", served[name]["url"], "where the node fetches the %s manifest", name)
	}
}

func TestBuilder_TheClusterAdmitsOnlyLabelledPodsIsolatesEachAppNamespaceAndAdmitsDatabaseSlices(t *testing.T) {
	manifests := inlineManifests(t, builderConfiguration(t, installedOnce(t)))

	for _, want := range []struct{ manifest, kind, name string }{
		{"mitstack-require-app-label", "ValidatingPolicy", "mitstack-require-app-label"},
		{"mitstack-generate-default-deny", "GeneratingPolicy", "mitstack-generate-default-deny"},
		{"cloudnative-pg-namespace", "Namespace", "cnpg-system"},
		{"mitstack-database-slices", "CiliumClusterwideNetworkPolicy", "mitstack-database-slices"},
	} {
		docs := testutil.Kind(manifests[want.manifest], want.kind)
		if assert.Len(t, docs, 1, "%s documents in the inline manifest %s", want.kind, want.manifest) {
			assert.Equal(t, want.name, testutil.Field(t, docs[0], "metadata", "name"), "the %s in %s", want.kind, want.manifest)
		}
	}
}

type workload struct {
	namespace, name string
	podLabels       map[string]any
}

func workloads(t *testing.T, docs []map[string]any) []workload {
	t.Helper()
	var found []workload
	for _, doc := range docs {
		var template any
		switch doc["kind"] {
		case "Job", "Deployment", "DaemonSet", "StatefulSet":
			template = testutil.Field(t, doc, "spec", "template")
		case "CronJob":
			template = testutil.Field(t, doc, "spec", "jobTemplate", "spec", "template")
		default:
			continue
		}
		metadata, _ := template.(map[string]any)["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		found = append(found, workload{
			namespace: testutil.Field(t, doc, "metadata", "namespace").(string),
			name:      testutil.Field(t, doc, "metadata", "name").(string),
			podLabels: labels,
		})
	}
	return found
}

// exemptNamespaces are the namespaces a policy's namespaceSelector leaves out by name.
func exemptNamespaces(t *testing.T, policy map[string]any) []string {
	t.Helper()
	var exempt []string
	for _, expr := range testutil.Field(t, policy, "spec", "matchConstraints", "namespaceSelector", "matchExpressions").([]any) {
		if m := expr.(map[string]any); m["key"] == "kubernetes.io/metadata.name" && m["operator"] == "NotIn" {
			for _, ns := range m["values"].([]any) {
				exempt = append(exempt, ns.(string))
			}
		}
	}
	return exempt
}

func TestBuilder_ThePlanesOwnPodsPassTheAppLabelPolicyAndKeepTheirEgress(t *testing.T) {
	manifests := inlineManifests(t, builderConfiguration(t, installedOnce(t)))
	labelPolicies := testutil.Kind(manifests["mitstack-require-app-label"], "ValidatingPolicy")
	require.Len(t, labelPolicies, 1, "the app label policy")
	denyPolicies := testutil.Kind(manifests["mitstack-generate-default-deny"], "GeneratingPolicy")
	require.Len(t, denyPolicies, 1, "the default-deny policy")

	exempt := exemptNamespaces(t, labelPolicies[0])
	denyCondition := testutil.Field(t, denyPolicies[0], "spec", "matchConditions", "0", "expression").(string)
	own := append(workloads(t, manifests["mitstack-node-report"]), workloads(t, manifests["mitstack-cluster-tokens"])...)

	require.NotEmpty(t, own, "the plane's workloads")
	for _, w := range own {
		if w.podLabels["mitstack.dev/app"] == nil {
			assert.Contains(t, exempt, w.namespace, "%s/%s's pods carry no mitstack.dev/app label, so their namespace must be exempt", w.namespace, w.name)
		}
		assert.Contains(t, denyCondition, "'"+w.namespace+"'", "%s/%s reaches the platform, so its namespace must get no default-deny", w.namespace, w.name)
	}
}

func mirroredImages(t *testing.T, kit renderedCloudConfig) map[string]string {
	t.Helper()
	mirrored := map[string]string{}
	for _, ref := range strings.Fields(kit.file(t, "/etc/mitstack-gateway/images.txt").Content) {
		image, err := components.ParseImage(ref)
		require.NoError(t, err, "the mirror's image list")
		mirrored[image.Name] = image.Digest
	}
	return mirrored
}

func TestGateway_MirrorsTheClustersImagesByDigestAndFetchesTheirManifestsFromTheKitRelease(t *testing.T) {
	release, err := controlplane.KitRelease()
	require.NoError(t, err)

	kit := installedOnce(t).gatewayKitPart()

	mirrored := mirroredImages(t, kit)
	for _, repo := range []string{
		"ghcr.io/cloudnative-pg/cloudnative-pg", "ghcr.io/cloudnative-pg/postgresql",
		"ghcr.io/kyverno/kyverno", "ghcr.io/kyverno/kyvernopre", "ghcr.io/kyverno/background-controller",
		"docker.io/rancher/local-path-provisioner", "docker.io/library/busybox",
		"docker.io/istio/pilot", "docker.io/istio/install-cni", "docker.io/istio/ztunnel",
	} {
		assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, mirrored[repo], "the mirror's digest for %s", repo)
	}
	fetched := strings.Fields(kit.file(t, "/etc/mitstack-gateway/manifests.txt").Content)
	for _, path := range []string{
		"kyverno/kyverno", "cloudnative-pg/cloudnative-pg", "local-path-provisioner/local-path-provisioner",
		"istio/istio-base", "istio/istiod", "istio/istio-cni", "istio/ztunnel",
	} {
		assert.Contains(t, fetched, gateway.ReleaseURL(release)+"/components/"+path+".yaml", "where the gateway fetches %s", path)
	}
}

func TestCluster_PutsEveryPodInIstiosAmbientMeshWithStrictMTLS(t *testing.T) {
	manifests := inlineManifests(t, builderConfiguration(t, installedOnce(t)))

	strict := testutil.Kind(manifests["mitstack-strict-mtls"], "PeerAuthentication")
	require.Len(t, strict, 1, "the mesh-wide policy")
	assert.Equal(t, "STRICT", testutil.Field(t, strict[0], "spec", "mtls", "mode"))
	for _, policy := range []string{"mitstack-require-mesh", "mitstack-require-strict-mtls"} {
		assert.Len(t, testutil.Kind(manifests[policy], "ValidatingPolicy"), 1, "the admission policy %s", policy)
	}
}

func adminCertificate(t *testing.T, talosconfig string) *x509.Certificate {
	t.Helper()
	var cfg struct {
		Contexts map[string]struct {
			Crt string `yaml:"crt"`
		} `yaml:"contexts"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(talosconfig), &cfg))
	raw, err := base64.StdEncoding.DecodeString(cfg.Contexts["admin"].Crt)
	require.NoError(t, err)
	block, _ := pem.Decode(raw)
	require.NotNil(t, block, "the admin certificate is PEM")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

func TestPlane_KeepsTheClustersKeysOutOfTheOutputsAndTheProblems(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.converge())
	machine := builderConfiguration(t, h)[0]
	keys := []string{
		testutil.Field(t, machine, "machine", "token").(string),
		testutil.Field(t, machine, "machine", "ca", "key").(string),
		testutil.Field(t, machine, "cluster", "token").(string),
	}
	h.built()

	outputs, detail := h.plane.Outputs(h.read())

	for _, key := range keys {
		for name, value := range outputs {
			assert.NotContains(t, value, key, "output %s carries a cluster key", name)
		}
		assert.NotContains(t, detail, key, "the detail carries a cluster key")
	}
}

func TestBuilder_UserDataIsOneCloudConfigDocumentWithinHetznersLimit(t *testing.T) {
	h := installedOnce(t)
	data := h.cloud.UserData[h.byRole(controlplane.RoleBuilder)[0].ID]

	decoder := yaml.NewDecoder(strings.NewReader(data))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		docs = append(docs, doc)
	}

	t.Logf("the builder's user data is %d bytes", len(data))
	assert.LessOrEqual(t, len(data), cloudtest.UserDataLimit, "the builder's user data is %d bytes; Hetzner accepts %d", len(data), cloudtest.UserDataLimit)
	assert.Len(t, docs, 1, "YAML documents in the builder's user data")
}

func TestBuilder_UserDataOverHetznersLimitFailsTheRender(t *testing.T) {
	incompressible := make([]byte, 2*cloudtest.UserDataLimit)
	_, err := rand.Read(incompressible)
	require.NoError(t, err)

	_, err = controlplane.BuilderUserData(incompressible)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "over Hetzner's 32768")
}

// recordedToolsImage stands in for tools.Image, which every kit release pins anew.
const recordedToolsImage = "ghcr.io/mitstack-eu/talos-cluster-kit-tools:recorded@sha256:0000000000000000000000000000000000000000000000000000000000000000"

func TestCluster_RendersTheRecordedComponentsMirrorAndBuilderApartFromTheNodeReport(t *testing.T) {
	controlplane.SwapToolsImage(t, recordedToolsImage)

	got := defaultClusterRecord(t)

	testutil.Golden(t, "testdata/default-cluster.golden", got)
}

// defaultClusterRecord lists the component set, the gateway's mirror and the builder's cloud-config and documents, one per line.
func defaultClusterRecord(t *testing.T) []byte {
	t.Helper()
	h := newHarness(t, sequential)
	require.NoError(t, h.converge())
	set, err := h.plane.ClusterComponents("enrolment-token")
	require.NoError(t, err)
	var out strings.Builder
	record := func(fields ...string) {
		out.WriteString(strings.Join(slices.DeleteFunc(fields, func(f string) bool { return f == "" }), " ") + "\n")
	}
	record("versions", set.Versions.Talos, set.Versions.Kubernetes)
	for _, m := range set.Served {
		record("served", m.Name, m.Path, m.SHA256())
	}
	for _, m := range set.Inline {
		digest := m.SHA256()
		if m.Name == "mitstack-node-report" {
			digest = "unrecorded"
		}
		record("inline", m.Name, m.Path, digest)
	}
	for _, ref := range strings.Fields(h.gatewayKitPart().file(t, "/etc/mitstack-gateway/images.txt").Content) {
		record("mirror", ref)
	}
	builder := decodeCloudConfig(t, h.cloud.UserData[h.byRole(controlplane.RoleBuilder)[0].ID])
	for _, f := range builder.WriteFiles {
		record("builder-file", f.Path, f.Permissions, f.Encoding)
	}
	for _, cmd := range builder.RunCmd {
		record(append([]string{"builder-run"}, cmd...)...)
	}
	for _, doc := range builderConfiguration(t, h) {
		kind, _ := doc["kind"].(string)
		version, _ := doc["version"].(string)
		name, _ := doc["name"].(string)
		record("document", cmp.Or(kind, version), name)
	}
	return []byte(out.String())
}

// appliedManifests are the manifests a control-plane node of h's cluster applies, keyed by their name in its
// configuration, a served one read from served at the URL the node fetches it from.
func appliedManifests(t *testing.T, h *harness, served []components.Manifest) map[string][]byte {
	t.Helper()
	config := builderConfiguration(t, h)
	if h.plane.Hosting != nil {
		config = append(config, servingFile(t, h)...)
	}
	manifests := map[string][]byte{}
	for _, doc := range testutil.Kind(config, "KubeInlineManifestConfig") {
		manifests[doc["name"].(string)] = []byte(doc["manifest"].(string))
	}
	for _, doc := range testutil.Kind(config, "KubeExternalManifestConfig") {
		i := slices.IndexFunc(served, func(m components.Manifest) bool { return manifestURL(m) == doc["url"] })
		require.GreaterOrEqual(t, i, 0, "no served manifest at %v", doc["url"])
		manifests[doc["name"].(string)] = served[i].Bytes
	}
	return manifests
}

func manifestURL(m components.Manifest) string {
	return nodeconfig.ManifestURL(netip.MustParseAddr(controlplane.GatewayAddress), m).String()
}

// webhookOrderFaults names each object a webhook intercepts that Talos creates before the webhook's configuration, the
// Service it calls, ztunnel and istio-cni have all applied, or before the node report and cluster tokens have.
func webhookOrderFaults(t *testing.T, order []testutil.AppliedObject) []string {
	t.Helper()
	last := map[string]int{}
	services := map[string]string{}
	for i, o := range order {
		last[testutil.ManifestName(o.Manifest)] = i
		if o.Doc["kind"] == "Service" {
			services[testutil.Field(t, o.Doc, "metadata", "namespace").(string)+"/"+testutil.Field(t, o.Doc, "metadata", "name").(string)] = o.Manifest
		}
	}
	plurals := testutil.Plurals(t, order)
	var faults []string
	for _, w := range testutil.Webhooks(t, order) {
		needs := []string{testutil.ManifestName(w.Configuration.Manifest), "ztunnel", "istio-cni"}
		if w.Service != "" {
			require.Contains(t, services, w.Service, "the Service the webhook %s calls", w.Name)
			needs = append(needs, testutil.ManifestName(services[w.Service]))
		}
		needs = slices.Compact(slices.Sorted(slices.Values(needs)))
		for i, o := range order {
			if !w.Intercepts(o, plurals) {
				continue
			}
			for _, needed := range needs {
				require.Contains(t, last, needed, "a manifest the webhook %s needs", w.Name)
				if last[needed] > i {
					faults = append(faults, fmt.Sprintf("%s applies before %s, which the webhook %s needs", o, needed, w.Name))
				}
			}
			for _, reporter := range []string{"mitstack-node-report", "mitstack-cluster-tokens"} {
				require.Contains(t, last, reporter, "the manifests the node applies")
				if last[reporter] > i {
					faults = append(faults, fmt.Sprintf("%s applies after %s, which the webhook %s can refuse", reporter, o, w.Name))
				}
			}
		}
	}
	return faults
}

// Every webhook counts whatever it does when it cannot answer, since istiod turns its validators from Ignore to Fail.
func TestBuilder_TalosAppliesEveryObjectAWebhookInterceptsAfterTheWebhookTheMeshAndTheNodeReport(t *testing.T) {
	hostingSet, err := components.Hosting(testutil.HostingInputs(t))
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		opts   []func(*harness)
		served []components.Manifest
	}{
		{"the default set", nil, components.Default().Served},
		{"the hosting set", []func(*harness){hosting}, hostingSet.Served},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)
			require.NoError(t, h.converge())

			order := testutil.TalosOrder(t, appliedManifests(t, h, tc.served))

			assert.Empty(t, webhookOrderFaults(t, order), "objects Talos creates before a webhook that intercepts them can answer")
		})
	}
}

func TestBuilder_TheNodeFetchesEachServedManifestUnderItsNameWhereTheGatewayServesIt(t *testing.T) {
	h := installedOnce(t)

	external := testutil.Kind(builderConfiguration(t, h), "KubeExternalManifestConfig")

	served := strings.Fields(h.gatewayKitPart().file(t, "/etc/mitstack-gateway/manifests.txt").Content)
	require.Len(t, external, len(components.Default().Served), "the served manifests the node applies")
	for _, doc := range external {
		name := doc["name"].(string)
		m := testutil.Manifest(t, testutil.ManifestName(name), components.Default().Served)
		assert.Equal(t, manifestURL(m), doc["url"], "where the node fetches %s", name)
		assert.Equal(t, "https://"+controlplane.GatewayAddress+"/manifests/"+name+".yaml", doc["url"], "the path the node fetches %s at", name)
		assert.Contains(t, served, name+".yaml", "the file the gateway serves %s from", name)
	}
}
