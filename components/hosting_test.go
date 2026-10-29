package components_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"maps"
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
	sealedWriteValue := regexp.MustCompile(components.DeployKeyItem + `: (\S+)`).FindSubmatch(in.SealedWriteKey)[1]
	sealedReadValue := regexp.MustCompile(components.RepositoryCredentialItem + `: (\S+)`).FindSubmatch(in.SealedReadKey)[1]
	secrets := map[string][]byte{
		"the seal key":            in.SealKey,
		"the Sealed Secrets key":  in.SealedSecretsKeyPEM,
		"the Sealed Secrets cert": in.SealedSecretsCertPEM,
		"the sealed write key":    sealedWriteValue,
		"the write key in plain":  []byte(testutil.WriteKey(t)),
		"the sealed read key":     sealedReadValue,
		"the read key in plain":   []byte(testutil.ReadKey(t)),
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
		"mitstack-openbao-deploy-key": {sealedWriteValue},
		"mitstack-cluster-repository": {sealedReadValue},
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
		assert.False(t, holds(m, []byte(testutil.WriteKey(t))), "inline manifest %s holds the write key in plain", m.Name)
		assert.False(t, holds(m, []byte(testutil.ReadKey(t))), "inline manifest %s holds the read key in plain", m.Name)
	}
	assert.Equal(t, []string{"RSA PRIVATE KEY"}, pemPrivateKeys(testutil.Manifest(t, "mitstack-sealed-secrets-key", set.Inline).Bytes))
}

// argoCDCredentialWriters returns, keyed "kind namespace/name", every ExternalSecret and External Secrets generator of the set, and each object labelled an Argo CD repository credential.
func argoCDCredentialWriters(t *testing.T, set components.Set) map[string]map[string]any {
	t.Helper()
	writers := map[string]map[string]any{}
	for _, m := range slices.Concat(set.Served, set.Inline) {
		for _, doc := range testutil.Documents(t, m.Bytes) {
			group, _, _ := strings.Cut(fmt.Sprint(doc["apiVersion"]), "/")
			external := group == "generators.external-secrets.io" || group == "external-secrets.io" && strings.HasSuffix(fmt.Sprint(doc["kind"]), "ExternalSecret")
			if metadata, _ := doc["metadata"].(map[string]any); external || slices.Contains([]string{"repository", "repo-creds", "repository-write", "repo-write-creds"}, secretType(doc)) {
				writers[fmt.Sprintf("%s %v/%v", doc["kind"], metadata["namespace"], metadata["name"])] = doc
			}
		}
	}
	return writers
}

// secretType returns the Argo CD secret type an object is labelled with, a SealedSecret's being the one its template gives the Secret it opens to.
func secretType(doc map[string]any) string {
	if doc["kind"] == "SealedSecret" {
		spec, _ := doc["spec"].(map[string]any)
		doc, _ = spec["template"].(map[string]any)
	}
	metadata, _ := doc["metadata"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)
	value, _ := labels["argocd.argoproj.io/secret-type"].(string)
	return value
}

func TestHosting_GivesArgoCDTheClusterRepositoryOverSSHOnPort443FromTheSealedReadKey(t *testing.T) {
	in := testutil.HostingInputs(t)

	set := hostingSet(t, in)

	var repositories []map[string]string
	for _, doc := range argoCDCredentialWriters(t, set) {
		if opened := testutil.Unseal(t, in.SealedSecretsKeyPEM, doc); opened.Namespace == "argocd" && secretType(doc) == "repository" {
			repositories = append(repositories, opened.Data)
		}
	}
	assert.Equal(t, []map[string]string{{
		"type":          "git",
		"url":           "ssh://git@ssh.github.com:443/" + testutil.InitRepository + ".git",
		"sshPrivateKey": testutil.ReadKey(t),
	}}, repositories, "the data of each repository credential in argocd")
	assert.Equal(t, "ssh://git@ssh.github.com:443/"+testutil.InitRepository+".git", components.RepositorySSHURL(testutil.InitRepository))
}

func TestHosting_GivesTheInitJobTheWriteKeyInTheNamespaceItRunsIn(t *testing.T) {
	in := testutil.HostingInputs(t)

	set := hostingSet(t, in)

	sealed := namedDocument(t, testutil.Manifest(t, "mitstack-openbao-deploy-key", set.Inline), "SealedSecret", components.DeployKeySecret)
	opened := testutil.Unseal(t, in.SealedSecretsKeyPEM, sealed)
	assert.Equal(t, components.DeployKeyNamespace, opened.Namespace)
	assert.Equal(t, map[string]string{components.DeployKeyItem: testutil.WriteKey(t)}, opened.Data)
}

func TestHosting_RendersNoExternalSecretGeneratorOrOtherWriterOfArgoCDRepositoryCredentials(t *testing.T) {
	set := hostingSet(t, testutil.HostingInputs(t))

	writers := slices.Sorted(maps.Keys(argoCDCredentialWriters(t, set)))

	assert.Equal(t, []string{"SealedSecret argocd/" + components.RepositoryCredentialName}, writers, "the objects that write an Argo CD repository credential")
}

func TestSeal_SealsAValueOnlyTheKeyPairOpensInTheStrictScopeOfItsSecret(t *testing.T) {
	_, keyPEM, certPEM := testutil.SealedSecretsKeyPair(t)

	file, err := components.Seal(certPEM, components.DeployKeyNamespace, components.DeployKeySecret, components.DeployKeyItem, []byte("a private key"))

	require.NoError(t, err)
	opened := testutil.Unseal(t, keyPEM, testutil.Documents(t, file)[0])
	assert.Equal(t, testutil.OpenedSecret{
		Namespace: components.DeployKeyNamespace, Name: components.DeployKeySecret,
		Data: map[string]string{components.DeployKeyItem: "a private key"},
	}, opened)
	in := testutil.HostingInputs(t)
	in.SealedSecretsKeyPEM, in.SealedSecretsCertPEM, in.SealedWriteKey = keyPEM, certPEM, file
	in.SealedReadKey, err = components.Seal(certPEM, components.RepositoryCredentialNamespace, components.RepositoryCredentialName, components.RepositoryCredentialItem, []byte("another"))
	require.NoError(t, err)
	_, err = components.Hosting(in)
	assert.NoError(t, err, "the hosting set refuses what Seal sealed")
}

func TestSealValue_SealsAValueTheControllerOpensAndOpenReadsOnlyUnderItsSecret(t *testing.T) {
	_, keyPEM, certPEM := testutil.SealedSecretsKeyPair(t)

	sealed, err := components.SealValue(certPEM, "argocd", "app-repository-shop", []byte("a private key"))

	require.NoError(t, err)
	doc := map[string]any{
		"kind":     "SealedSecret",
		"metadata": map[string]any{"namespace": "argocd", "name": "app-repository-shop"},
		"spec":     map[string]any{"encryptedData": map[string]any{"sshPrivateKey": sealed}},
	}
	assert.Equal(t, map[string]string{"sshPrivateKey": "a private key"}, testutil.Unseal(t, keyPEM, doc).Data, "what the controller opens")
	opened, err := components.Open(keyPEM, "argocd", "app-repository-shop", sealed)
	require.NoError(t, err)
	assert.Equal(t, "a private key", string(opened), "what Open reads")
	for _, other := range []struct{ namespace, name string }{{"argocd", "app-repository-other"}, {"mitstack-node", "app-repository-shop"}} {
		_, err := components.Open(keyPEM, other.namespace, other.name, sealed)
		assert.Error(t, err, "the value sealed for argocd/app-repository-shop opened as %s/%s", other.namespace, other.name)
	}
}

func TestNewDeployKey_MintsAFreshEd25519KeyWhosePrivateHalfMatchesThePublicHalfARepositoryLists(t *testing.T) {
	key, err := components.NewDeployKey("mitstack Argo CD")
	require.NoError(t, err)
	again, err := components.NewDeployKey("mitstack Argo CD")
	require.NoError(t, err)

	assert.Regexp(t, `^ssh-ed25519 [A-Za-z0-9+/]+=*$`, key.AuthorizedKey, "the public half GitHub is given")
	assert.Equal(t, key.AuthorizedKey, testutil.AuthorizedKey(t, string(key.PrivateKeyPEM)), "the public half of the OpenSSH private key")
	assert.NotEqual(t, key.AuthorizedKey, again.AuthorizedKey, "two calls minted the same key")
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
	_, _, otherCert := testutil.SealedSecretsKeyPair(t)
	cases := map[string]struct {
		change func(*components.HostingInputs)
		want   string
	}{
		"a seal key of 31 bytes":       {func(in *components.HostingInputs) { in.SealKey = in.SealKey[:31] }, "the seal key is 31 bytes, want 32"},
		"a certificate of another key": {func(in *components.HostingInputs) { in.SealedSecretsCertPEM = otherCert }, "certifies another key"},
		"a write key sealed to another pair": {func(in *components.HostingInputs) {
			in.SealedWriteKey = testutil.SealedWriteKey(t, otherCert, testutil.WriteKey(t))
		}, "the sealed write key is not sealed to the Sealed Secrets key pair"},
		"a write key in another namespace": {func(in *components.HostingInputs) {
			in.SealedWriteKey = bytes.Replace(in.SealedWriteKey, []byte("namespace: "+components.DeployKeyNamespace), []byte("namespace: default"), 1)
		}, "want bitnami.com/v1alpha1 SealedSecret mitstack-node/" + components.DeployKeySecret},
		"a read key sealed to another pair": {func(in *components.HostingInputs) {
			in.SealedReadKey = testutil.SealedReadKey(t, otherCert, testutil.ReadKey(t))
		}, "the sealed read key is not sealed to the Sealed Secrets key pair"},
		"a read key under another name": {func(in *components.HostingInputs) {
			in.SealedReadKey = bytes.Replace(in.SealedReadKey, []byte("name: "+components.RepositoryCredentialName), []byte("name: other"), 1)
		}, "want bitnami.com/v1alpha1 SealedSecret argocd/" + components.RepositoryCredentialName},
		"a read key with a username sealed beside it": {func(in *components.HostingInputs) {
			item := regexp.MustCompile(`(?m)^(\s*)` + components.RepositoryCredentialItem + `:`)
			in.SealedReadKey = item.ReplaceAll(in.SealedReadKey, []byte("${1}username: c2VhbGVk\n${1}"+components.RepositoryCredentialItem+":"))
		}, "the sealed read key's encryptedData holds 2 items, want only " + components.RepositoryCredentialItem},
		"an init Job image named by tag alone": {func(in *components.HostingInputs) { in.Init.ToolsImage = "ghcr.io/x/tools:v1" }, "OpenBao's init Job: tools image"},
		"a write key file with a second object": {func(in *components.HostingInputs) {
			in.SealedWriteKey = append(in.SealedWriteKey, []byte("---\napiVersion: v1\nkind: Secret\n")...)
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

func TestHosting_TheInitJobTrustsTheSSHHostKeysArgoCDTrustsForGitHubOn443(t *testing.T) {
	set := hostingSet(t, testutil.HostingInputs(t))
	argo := namedDocument(t, testutil.Manifest(t, "argocd", set.Served), "ConfigMap", "argocd-ssh-known-hosts-cm")
	init := namedDocument(t, testutil.Manifest(t, "mitstack-openbao-init", set.Inline), "ConfigMap", "mitstack-openbao-init")

	onPort443 := func(knownHosts string) []string {
		var lines []string
		for line := range strings.Lines(knownHosts) {
			if strings.HasPrefix(line, "[ssh.github.com]:443 ") {
				lines = append(lines, strings.TrimSpace(line))
			}
		}
		return lines
	}

	want := onPort443(testutil.Field(t, argo, "data", "ssh_known_hosts").(string))
	require.NotEmpty(t, want, "Argo CD trusts no host key for ssh.github.com on 443")
	assert.Equal(t, want, onPort443(testutil.Field(t, init, "data", "known_hosts").(string)))
}

func TestHosting_LetsTheInitJobReachOnlyClusterDNSOpenBaoAndGitHubsSSHOn443AndOpenBaoTakeItsCalls(t *testing.T) {
	set := hostingSet(t, testutil.HostingInputs(t))
	initPod, env := initJob(t, set)
	repository, err := url.Parse(env["MITSTACK_REPOSITORY_URL"])
	require.NoError(t, err)
	github := repository.Hostname()
	require.Equal(t, "ssh.github.com", github, "the host the init Job pushes to")
	policy := namedDocument(t, testutil.Manifest(t, "mitstack-hosting-flows", set.Served), "CiliumClusterwideNetworkPolicy", "mitstack-hosting-flows")
	pods := map[string]testutil.CiliumEndpoint{
		"cluster DNS":  testutil.Endpoint("kube-system", nil, map[string]string{"k8s-app": "kube-dns"}),
		"OpenBao":      testutil.Endpoint("openbao", nil, map[string]string{"app.kubernetes.io/name": "openbao", "component": "server"}),
		"the init Job": initPod,
	}

	initFlows, openBaoFlows := testutil.Grants(t, policy, pods, "the init Job"), testutil.Grants(t, policy, pods, "OpenBao")

	assert.Equal(t, []string{
		"egress to " + github + " on 443/TCP",
		"egress with OpenBao on 8200/TCP 15008/TCP",
		"egress with cluster DNS on 53/UDP 53/TCP through the DNS proxy",
	}, initFlows, "the init Job's flows")
	assert.Contains(t, openBaoFlows, "ingress with the init Job on 8200/TCP 15008/TCP", "OpenBao refuses the init Job's calls")
}

func TestHosting_KeepsTheMembersAloneOnOpenBaosClusterPortAndEveryoneOffItsOtherPorts(t *testing.T) {
	set := hostingSet(t, testutil.HostingInputs(t))
	policy := namedDocument(t, testutil.Manifest(t, "mitstack-hosting-mesh", set.Served), "AuthorizationPolicy", "mitstack-openbao-ports")
	member, secrets := "cluster.local/ns/openbao/sa/openbao", "cluster.local/ns/external-secrets/sa/external-secrets"

	assert.False(t, testutil.Denies(t, policy, member, 8201), "a member's call to the cluster port")
	assert.False(t, testutil.Denies(t, policy, secrets, 8200), "External Secrets' call to the API")
	assert.True(t, testutil.Denies(t, policy, secrets, 8201), "External Secrets' call to the cluster port")
	assert.True(t, testutil.Denies(t, policy, member, 8202), "a call to a port OpenBao's flows do not name")
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

// toleratesControlPlane is whether a pod template schedules past Talos's NoSchedule taint on the control-plane nodes.
func toleratesControlPlane(t *testing.T, template map[string]any) bool {
	t.Helper()
	tolerations, _ := testutil.Field(t, template, "spec").(map[string]any)["tolerations"].([]any)
	for _, toleration := range tolerations {
		tol := toleration.(map[string]any)
		key, effect := tol["key"], tol["effect"]
		anyKey := key == nil && tol["operator"] == "Exists"
		if (key == "node-role.kubernetes.io/control-plane" || anyKey) && (effect == "NoSchedule" || effect == nil) {
			return true
		}
	}
	return false
}

func TestHosting_RunsOpenBaosMembersOnTheTaintedControlPlaneNodesAndEveryOtherHostingWorkloadOnTheWorkers(t *testing.T) {
	set := hostingSet(t, testutil.HostingInputs(t))

	members := podTemplates(t, testutil.Manifest(t, "openbao", set.Served))["openbao/openbao"]
	require.NotNil(t, members, "the openbao manifest has no openbao/openbao workload")
	assert.True(t, toleratesControlPlane(t, members), "OpenBao's members cannot run on the control-plane nodes their volumes are pinned to")
	for _, name := range []string{"traefik", "cert-manager", "trust-manager", "external-secrets", "sealed-secrets", "argocd", "mitstack-openbao-init"} {
		templates := podTemplates(t, testutil.Manifest(t, name, set.Served, set.Inline))
		assert.NotEmpty(t, templates, "the %s manifest renders no workload", name)
		for workload, template := range templates {
			assert.False(t, toleratesControlPlane(t, template), "%s runs on the control-plane nodes", workload)
		}
	}
}

// ingressPorts are the ports an ingress item opens, nil for one that names none and so opens every port.
func ingressPorts(t *testing.T, item map[string]any) []string {
	t.Helper()
	var ports []string
	for _, rule := range listOf(item["toPorts"]) {
		for _, port := range listOf(rule.(map[string]any)["ports"]) {
			ports = append(ports, fmt.Sprint(port.(map[string]any)["port"]))
		}
	}
	return ports
}

func listOf(v any) []any {
	list, _ := v.([]any)
	return list
}

func admitsEntity(item map[string]any, entity string) bool {
	return slices.Contains(listOf(item["fromEntities"]), any(entity))
}

func admitsControlPlaneNodes(item map[string]any) bool {
	return slices.ContainsFunc(listOf(item["fromNodes"]), func(selector any) bool {
		labels, _ := selector.(map[string]any)["matchLabels"].(map[string]any)
		return len(labels) == 1 && labels["node-role.kubernetes.io/control-plane"] == ""
	})
}

func ciliumSpecs(doc map[string]any) []any {
	if spec, ok := doc["spec"].(map[string]any); ok {
		return []any{spec}
	}
	return listOf(doc["specs"])
}

// apiServerIngressFaults names each port admitted from the API server that the control-plane nodes cannot reach, or that
// another node, a host or the world can.
func apiServerIngressFaults(t *testing.T, set components.Set) (faults []string, items int) {
	t.Helper()
	for _, m := range slices.Concat(set.Served, set.Inline) {
		for _, doc := range testutil.Documents(t, m.Bytes) {
			if doc["kind"] != "CiliumNetworkPolicy" && doc["kind"] != "CiliumClusterwideNetworkPolicy" {
				continue
			}
			for _, spec := range ciliumSpecs(doc) {
				ingress := listOf(spec.(map[string]any)["ingress"])
				for _, raw := range ingress {
					item := raw.(map[string]any)
					if !admitsEntity(item, "kube-apiserver") {
						continue
					}
					items++
					ports := ingressPorts(t, item)
					where := fmt.Sprintf("%v in %s %v", ports, m.Name, spec.(map[string]any)["description"])
					if !slices.ContainsFunc(ingress, func(other any) bool {
						return admitsControlPlaneNodes(other.(map[string]any)) && slices.Equal(ingressPorts(t, other.(map[string]any)), ports)
					}) {
						faults = append(faults, "no fromNodes item admits the control-plane nodes to "+where)
					}
					for _, other := range ingress {
						otherPorts := ingressPorts(t, other.(map[string]any))
						for _, entity := range []string{"remote-node", "host", "world", "cluster", "all"} {
							if admitsEntity(other.(map[string]any), entity) && (otherPorts == nil || slices.ContainsFunc(otherPorts, func(p string) bool { return slices.Contains(ports, p) })) {
								faults = append(faults, fmt.Sprintf("%s reaches %s", entity, where))
							}
						}
					}
				}
			}
		}
	}
	return faults, items
}

func TestHosting_AdmitsTheAPIServerToEveryWebhookPortFromTheControlPlaneNodesAndNoOtherNodeOrTheWorld(t *testing.T) {
	for _, tc := range []struct {
		name         string
		set          components.Set
		apiServerUse bool
	}{
		{"the default set", components.Default(), false},
		{"the hosting set", hostingSet(t, testutil.HostingInputs(t)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := namedDocument(t, testutil.Manifest(t, "cilium", tc.set.Served), "ConfigMap", "cilium-config")

			faults, items := apiServerIngressFaults(t, tc.set)

			assert.Empty(t, faults)
			assert.Equal(t, tc.apiServerUse, items > 0, "ingress items that admit the API server: %d", items)
			assert.Equal(t, "true", testutil.Field(t, config, "data", "enable-node-selector-labels"), "Cilium gives a node an identity by its labels")
			assert.Equal(t, "node-role.kubernetes.io/control-plane", testutil.Field(t, config, "data", "node-labels"), "the node label Cilium's identities carry")
		})
	}
}
