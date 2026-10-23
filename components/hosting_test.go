package components_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func hostingSet(t *testing.T, in components.HostingInputs) components.Set {
	t.Helper()
	set, err := components.Hosting(in)
	require.NoError(t, err)
	return set
}

// hclBlock returns the body of the first HCL block that opens with header, matching its braces.
func hclBlock(t *testing.T, config, header string) string {
	t.Helper()
	start := strings.Index(config, header+" {")
	require.GreaterOrEqual(t, start, 0, "the configuration has no %s block", header)
	depth := 0
	for i := start + len(header); i < len(config); i++ {
		switch config[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return config[start+len(header)+2 : i]
			}
		}
	}
	require.Fail(t, "unclosed block", header)
	return ""
}

func hclString(t *testing.T, block, key string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*` + key + `\s*=\s*"([^"]*)"`).FindStringSubmatch(block)
	require.NotNil(t, m, "no %s in %q", key, block)
	return m[1]
}

func TestHosting_ListensOnlyOnTLSKeepsRaftOnTheUserVolumeAndUnsealsWithAStaticKeyFile(t *testing.T) {
	set := hostingSet(t, testutil.HostingInputs(t))

	openbao := testutil.Manifest(t, "openbao", set.Served)

	config := testutil.Field(t, namedDocument(t, openbao, "ConfigMap", "openbao-config"), "data", "extraconfig-from-values.hcl").(string)
	listener := hclBlock(t, config, `listener "tcp"`)
	assert.NotContains(t, config, "tls_disable")
	assert.Equal(t, "/openbao/tls/tls.crt", hclString(t, listener, "tls_cert_file"))
	assert.Equal(t, "/openbao/tls/tls.key", hclString(t, listener, "tls_key_file"))
	seal := hclBlock(t, config, `seal "static"`)
	assert.Equal(t, "file:///openbao/seal/key", hclString(t, seal, "current_key"))
	raft := hclString(t, hclBlock(t, config, `storage "raft"`), "path")

	statefulSet := namedDocument(t, openbao, "StatefulSet", "openbao")
	assert.EqualValues(t, components.OpenBaoMembers, testutil.Field(t, statefulSet, "spec", "replicas"))
	mounts := map[string]string{}
	for _, m := range testutil.Field(t, statefulSet, "spec", "template", "spec", "containers", "0", "volumeMounts").([]any) {
		mounts[testutil.Field(t, m, "name").(string)] = testutil.Field(t, m, "mountPath").(string)
	}
	assert.Equal(t, raft, mounts["data"], "Raft keeps its data outside the claim's mount")
	assert.Equal(t, "/openbao/seal", mounts["seal-key"])
	assert.Equal(t, "openbao-local", testutil.Field(t, statefulSet, "spec", "volumeClaimTemplates", "0", "spec", "storageClassName"))

	volumes := testutil.Kind(testutil.Documents(t, testutil.Manifest(t, "mitstack-openbao-storage", set.Inline).Bytes), "PersistentVolume")
	require.Len(t, volumes, components.OpenBaoMembers)
	for i, pv := range volumes {
		assert.Equal(t, "/var/mnt/openbao", testutil.Field(t, pv, "spec", "local", "path"))
		assert.Equal(t, "openbao-local", testutil.Field(t, pv, "spec", "storageClassName"))
		assert.Equal(t, fmt.Sprintf("data-openbao-%d", i), testutil.Field(t, pv, "spec", "claimRef", "name"))
		node := testutil.Field(t, pv, "spec", "nodeAffinity", "required", "nodeSelectorTerms", "0", "matchExpressions", "0", "values", "0")
		assert.Equal(t, testutil.HostingNodes[i], node)
	}
	assert.Contains(t, set.UserVolumes, components.OpenBaoVolume)
}

// secretForms are the ways a value can appear in a manifest: as it is, base64 and hex, and each line of a PEM body.
func secretForms(value []byte) []string {
	forms := []string{string(value), base64.StdEncoding.EncodeToString(value), hex.EncodeToString(value)}
	for _, line := range strings.Split(string(value), "\n") {
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			forms = append(forms, line)
		}
	}
	return forms
}

// pemPrivateKeys returns the type of each PEM block in data that decodes as a private key, however a document indents it.
func pemPrivateKeys(data []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		lines = append(lines, strings.TrimLeft(line, " \t"))
	}
	rest := []byte(strings.Join(lines, "\n"))
	var keys []string
	for {
		i := bytes.Index(rest, []byte("-----BEGIN "))
		if i < 0 {
			return keys
		}
		block, after := pem.Decode(rest[i:])
		if block == nil {
			rest = rest[i+len("-----BEGIN "):]
			continue
		}
		if strings.HasSuffix(block.Type, "PRIVATE KEY") {
			keys = append(keys, block.Type)
		}
		rest = after
	}
}

func holds(m components.Manifest, value []byte) bool {
	return slices.ContainsFunc(secretForms(value), func(form string) bool { return bytes.Contains(m.Bytes, []byte(form)) })
}

func TestHosting_KeepsEverySecretInlineAndNoServedManifestHoldsOne(t *testing.T) {
	in := testutil.HostingInputs(t)
	sealedValue := regexp.MustCompile(components.SealedAppKeyItem + `: (\S+)`).FindSubmatch(in.SealedAppKey)[1]
	secrets := map[string][]byte{
		"the seal key":            in.SealKey,
		"the Sealed Secrets key":  in.SealedSecretsKeyPEM,
		"the Sealed Secrets cert": in.SealedSecretsCertPEM,
		"the sealed App key":      sealedValue,
		"the App key in plain":    []byte(testutil.AppKey),
	}

	set := hostingSet(t, in)

	for _, m := range set.Served[len(components.Default().Served):] {
		assert.Empty(t, testutil.Kind(testutil.Documents(t, m.Bytes), "Secret"), "served manifest %s holds a Secret", m.Name)
	}
	for _, m := range set.Served {
		for _, secret := range testutil.Kind(testutil.Documents(t, m.Bytes), "Secret") {
			assert.Nil(t, secret["data"], "served manifest %s holds a Secret with data", m.Name)
			assert.Nil(t, secret["stringData"], "served manifest %s holds a Secret with data", m.Name)
		}
		assert.Empty(t, pemPrivateKeys(m.Bytes), "served manifest %s holds a PEM private key", m.Name)
		for what, value := range secrets {
			assert.False(t, holds(m, value), "served manifest %s holds %s", m.Name, what)
		}
	}
	for manifest, values := range map[string][][]byte{
		"mitstack-openbao-seal-key":   {in.SealKey},
		"mitstack-sealed-secrets-key": {in.SealedSecretsKeyPEM, in.SealedSecretsCertPEM},
		"mitstack-sealed-app-key":     {sealedValue},
	} {
		for _, value := range values {
			var holders []string
			for _, m := range set.Inline {
				if holds(m, value) {
					holders = append(holders, m.Name)
				}
			}
			assert.Equal(t, []string{manifest}, holders)
		}
	}
	seal := namedDocument(t, testutil.Manifest(t, "mitstack-openbao-seal-key", set.Inline), "Secret", "openbao-seal-key")
	assert.Equal(t, base64.StdEncoding.EncodeToString(in.SealKey), testutil.Field(t, seal, "data", "key"))
	pair := namedDocument(t, testutil.Manifest(t, "mitstack-sealed-secrets-key", set.Inline), "Secret", "sealed-secrets-boot-key")
	assert.Equal(t, string(in.SealedSecretsKeyPEM), testutil.Field(t, pair, "stringData", "tls.key"))
	assert.Equal(t, string(in.SealedSecretsCertPEM), testutil.Field(t, pair, "stringData", "tls.crt"))
	assert.Equal(t, "active", testutil.Field(t, pair, "metadata", "labels", "sealedsecrets.bitnami.com/sealed-secrets-key"))
	for _, m := range set.Inline {
		assert.False(t, holds(m, []byte(testutil.AppKey)), "inline manifest %s holds the App key in plain", m.Name)
	}
	assert.Equal(t, []string{"RSA PRIVATE KEY"}, pemPrivateKeys(testutil.Manifest(t, "mitstack-sealed-secrets-key", set.Inline).Bytes))
}

// podTemplates returns each pod template of the manifest's workloads, keyed by the workload's namespace and name.
func podTemplates(t *testing.T, m components.Manifest) map[string]map[string]any {
	t.Helper()
	templates := map[string]map[string]any{}
	for _, doc := range testutil.Documents(t, m.Bytes) {
		switch doc["kind"] {
		case "Deployment", "StatefulSet", "DaemonSet", "Job":
			key := testutil.Field(t, doc, "metadata", "namespace").(string) + "/" + testutil.Field(t, doc, "metadata", "name").(string)
			templates[key] = testutil.Field(t, doc, "spec", "template").(map[string]any)
		}
	}
	return templates
}

func TestHosting_LabelsEveryPodTheAppLabelPolicyChecks(t *testing.T) {
	label := testutil.Documents(t, manifest(t, "mitstack-require-app-label").Bytes)[0]
	exempt := testutil.Field(t, label, "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values").([]any)

	set := hostingSet(t, testutil.HostingInputs(t))

	checked := 0
	for _, m := range set.Served[len(components.Default().Served):] {
		for workload, template := range podTemplates(t, m) {
			if slices.Contains(exempt, any(strings.Split(workload, "/")[0])) {
				continue
			}
			checked++
			labels, _ := testutil.Field(t, template, "metadata", "labels").(map[string]any)
			assert.NotEmpty(t, labels["mitstack.dev/app"], "the admission policy refuses the pods of %s", workload)
		}
	}
	assert.GreaterOrEqual(t, checked, 7, "the hosting set runs fewer workloads than it renders")
}

func TestHosting_AddsToTheDefaultSetAndLeavesItAsItIs(t *testing.T) {
	def := components.Default()

	set := hostingSet(t, testutil.HostingInputs(t))

	assert.Equal(t, def.Served, set.Served[:len(def.Served)])
	assert.Equal(t, def.Inline, set.Inline[:len(def.Inline)])
	assert.Equal(t, def.TalosImages, set.TalosImages)
	assert.Equal(t, append(slices.Clone(def.UserVolumes), components.OpenBaoVolume), set.UserVolumes)
	assert.Equal(t, components.Default(), def, "rendering the hosting set changed the default set")
}

func TestHosting_RefusesInputsItCannotBoot(t *testing.T) {
	otherKey, _, otherCert := testutil.SealedSecretsKeyPair(t)
	cases := map[string]struct {
		change func(*components.HostingInputs)
		want   string
	}{
		"a seal key of 31 bytes":       {func(in *components.HostingInputs) { in.SealKey = in.SealKey[:31] }, "the seal key is 31 bytes, want 32"},
		"a certificate of another key": {func(in *components.HostingInputs) { in.SealedSecretsCertPEM = otherCert }, "certifies another key"},
		"an App key sealed to another pair": {func(in *components.HostingInputs) {
			in.SealedAppKey = testutil.SealedAppKey(t, &otherKey.PublicKey, testutil.AppKey)
		}, "not sealed to the Sealed Secrets key pair"},
		"an App key in another namespace": {func(in *components.HostingInputs) {
			in.SealedAppKey = bytes.Replace(in.SealedAppKey, []byte("namespace: "+components.SealedAppKeyNamespace), []byte("namespace: default"), 1)
		}, "want bitnami.com/v1alpha1 SealedSecret mitstack-node/github-app-key"},
		"an init Job image named by tag alone": {func(in *components.HostingInputs) { in.Init.ToolsImage = "ghcr.io/x/tools:v1" }, "OpenBao's init Job: tools image"},
		"an App key file with a second object": {func(in *components.HostingInputs) {
			in.SealedAppKey = append(in.SealedAppKey, []byte("---\napiVersion: v1\nkind: Secret\n")...)
		}, "more than one document"},
		"two nodes":              {func(in *components.HostingInputs) { in.Nodes = in.Nodes[:2] }, "2 nodes are named, want one for each of OpenBao's 3 members"},
		"a node named twice":     {func(in *components.HostingInputs) { in.Nodes = []string{"a", "b", "a"} }, "a node is named twice"},
		"a node name with a dot": {func(in *components.HostingInputs) { in.Nodes = []string{"a", "b", "c.d"} }, `"c.d" is not a DNS-1123 label`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := testutil.HostingInputs(t)
			tc.change(&in)

			_, err := components.Hosting(in)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestHosting_ServesOpenBaoACertificateForEveryAddressItsMembersAndClientsDial(t *testing.T) {
	set := hostingSet(t, testutil.HostingInputs(t))
	tls := testutil.Manifest(t, "mitstack-hosting-tls", set.Served)
	config := testutil.Field(t, namedDocument(t, testutil.Manifest(t, "openbao", set.Served), "ConfigMap", "openbao-config"), "data", "extraconfig-from-values.hcl").(string)

	cert := namedDocument(t, tls, "Certificate", "openbao-tls")

	assert.Equal(t, "openbao-ca", testutil.Field(t, cert, "spec", "issuerRef", "name"))
	var patterns []string
	for _, name := range testutil.Field(t, cert, "spec", "dnsNames").([]any) {
		patterns = append(patterns, "^"+strings.ReplaceAll(regexp.QuoteMeta(name.(string)), `\*`, `[a-z0-9-]+`)+"$")
	}
	_, initEnv := initJob(t, set)
	initAddr, err := url.Parse(initEnv["MITSTACK_OPENBAO_ADDR"])
	require.NoError(t, err)
	hosts := []string{"openbao.openbao.svc", initAddr.Hostname()}
	for _, m := range regexp.MustCompile(`leader_api_addr = "https://([^:"]+):8200"`).FindAllStringSubmatch(config, -1) {
		hosts = append(hosts, m[1])
	}
	require.Len(t, hosts, 2+components.OpenBaoMembers)
	for _, host := range hosts {
		assert.True(t, slices.ContainsFunc(patterns, func(p string) bool { return regexp.MustCompile(p).MatchString(host) }), "the certificate does not name %s", host)
	}
	assert.Contains(t, testutil.Field(t, cert, "spec", "ipAddresses"), "127.0.0.1", "the probes dial OpenBao on the loopback address")
	bundle := namedDocument(t, tls, "Bundle", "openbao-ca")
	assert.Equal(t, map[string]any{"name": "openbao-ca", "key": "tls.crt"}, testutil.Field(t, bundle, "spec", "sources", "0", "secret"))
	assert.Equal(t, "ca.crt", testutil.Field(t, bundle, "spec", "target", "configMap", "key"))
	assert.Contains(t, string(testutil.Manifest(t, "trust-manager", set.Served).Bytes), `"--trust-namespace=openbao"`)
}

// initJob returns the init Job's pod as Cilium labels it, and its container's environment, from the hosting set.
func initJob(t *testing.T, set components.Set) (testutil.CiliumEndpoint, map[string]string) {
	t.Helper()
	job := namedDocument(t, testutil.Manifest(t, "mitstack-openbao-init", set.Inline), "Job", "mitstack-openbao-init")
	labels := map[string]string{}
	for k, v := range testutil.Field(t, job, "spec", "template", "metadata", "labels").(map[string]any) {
		labels[k] = v.(string)
	}
	env := testutil.ContainerEnv(t, testutil.Field(t, job, "spec", "template", "spec", "containers", "0"))
	return testutil.Endpoint(testutil.Field(t, job, "metadata", "namespace").(string), nil, labels), env
}

func TestHosting_LetsTheInitJobReachOnlyClusterDNSOpenBaoAndGitHubsAPIAndOpenBaoTakeItsCalls(t *testing.T) {
	set := hostingSet(t, testutil.HostingInputs(t))
	initPod, env := initJob(t, set)
	github := strings.TrimPrefix(env["MITSTACK_GITHUB_API_URL"], "https://")
	policy := namedDocument(t, testutil.Manifest(t, "mitstack-hosting-flows", set.Served), "CiliumClusterwideNetworkPolicy", "mitstack-hosting-flows")
	pods := map[string]testutil.CiliumEndpoint{
		"cluster DNS":  testutil.Endpoint("kube-system", nil, map[string]string{"k8s-app": "kube-dns"}),
		"OpenBao":      testutil.Endpoint("openbao", nil, map[string]string{"app.kubernetes.io/name": "openbao", "component": "server"}),
		"the init Job": initPod,
	}

	initFlows, openBaoFlows := testutil.Grants(t, policy, pods, "the init Job"), testutil.Grants(t, policy, pods, "OpenBao")

	assert.Equal(t, []string{
		"egress to " + github + " on 443/TCP",
		"egress with OpenBao on 8200/TCP",
		"egress with cluster DNS on 53/UDP 53/TCP through the DNS proxy",
	}, initFlows, "the init Job's flows")
	assert.Contains(t, openBaoFlows, "ingress with the init Job on 8200/TCP", "OpenBao refuses the init Job's calls")
}

func TestHosting_ServesTraefikOnNodePorts30080And30081OnEveryNode(t *testing.T) {
	set := hostingSet(t, testutil.HostingInputs(t))
	traefik := testutil.Manifest(t, "traefik", set.Served)

	service := namedDocument(t, traefik, "Service", "traefik")

	assert.Equal(t, "NodePort", testutil.Field(t, service, "spec", "type"))
	assert.Equal(t, "Local", testutil.Field(t, service, "spec", "externalTrafficPolicy"))
	nodePorts := map[any]any{}
	for _, port := range testutil.Field(t, service, "spec", "ports").([]any) {
		nodePorts[testutil.Field(t, port, "targetPort")] = testutil.Field(t, port, "nodePort")
	}
	assert.Equal(t, map[any]any{"web": 30080, "websecure": 30081}, nodePorts)
	namedDocument(t, traefik, "DaemonSet", "traefik")
}
