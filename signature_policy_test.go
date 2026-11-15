package talosclusterkit

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	githubIssuer    = "https://token.actions.githubusercontent.com"
	kitPipeline     = "https://github.com/mitstack-eu/talos-cluster-kit/.github/workflows/ci.yml@refs/heads/main"
	signedTests     = "components/admission/tests/mitstack-verify-image-signatures/"
	privateTests    = "components/admission/tests/mitstack-verify-image-signatures-private-registry/"
	privateRegistry = "123456789012.dkr.ecr.eu-central-1.amazonaws.com"
)

// testedSigners is the list the Kyverno tests judge: the kit's own tools image, and Kyverno's image under an identity
// that did not sign it.
var testedSigners = []components.Signer{
	{Repository: "ghcr.io/mitstack-eu/talos-cluster-kit-tools", Issuer: githubIssuer, Subject: kitPipeline},
	{Repository: "ghcr.io/kyverno/kyverno", Issuer: githubIssuer, Subject: kitPipeline},
}

var shopLogin = components.SecretRef{Namespace: "app-shop", Name: "registry-login"}

// privateSigners adds two repositories of a made-up private ECR registry, both read with one pull Secret.
var privateSigners = append(slices.Clone(testedSigners),
	components.Signer{Repository: privateRegistry + "/shop-api", Issuer: githubIssuer, Subject: kitPipeline, PullSecret: shopLogin},
	components.Signer{Repository: privateRegistry + "/shop-web", Issuer: githubIssuer, Subject: kitPipeline, PullSecret: shopLogin})

// signatureDocuments renders the list and splits its one manifest into the image policies, the login copies and the
// egress limit.
func signatureDocuments(t *testing.T, signers []components.Signer) (policy, logins, egress []byte) {
	t.Helper()
	manifests, err := components.SignaturePolicy(signers)
	require.NoError(t, err)
	require.Len(t, manifests, 1, "the manifests a signer list renders")
	require.Equal(t, components.SignaturePolicyName, manifests[0].Name)
	var policies, copies, limits []string
	for _, doc := range strings.Split(string(manifests[0].Bytes), "\n---\n") {
		switch testutil.Documents(t, []byte(doc))[0]["kind"] {
		case "ImageValidatingPolicy":
			policies = append(policies, doc)
		case "CiliumClusterwideNetworkPolicy":
			limits = append(limits, doc)
		default:
			copies = append(copies, doc)
		}
	}
	require.Len(t, limits, 1, "the manifest holds one egress limit")
	joined := func(docs []string) []byte { return []byte(strings.Join(docs, "\n---\n") + "\n") }
	return joined(policies), joined(copies), []byte(limits[0])
}

func TestSignaturePolicy_AnEmptySignerListRendersNoPolicyAndNoEgressLimit(t *testing.T) {
	manifests, err := components.SignaturePolicy(nil)

	require.NoError(t, err)
	assert.Empty(t, manifests, "every image passes on its digest alone, and Kyverno reaches no registry")
}

func TestSignaturePolicy_RendersThePolicyAndTheEgressLimitTheKyvernoTestsRead(t *testing.T) {
	policy, _, egress := signatureDocuments(t, testedSigners)

	testutil.Golden(t, signedTests+"policy.yaml", policy)
	testutil.Golden(t, signedTests+"egress.yaml", egress)
}

func TestSignaturePolicy_RendersForAPrivateRegistryThePolicyLoginCopiesAndEgressLimitTheKyvernoTestsRead(t *testing.T) {
	policy, logins, egress := signatureDocuments(t, privateSigners)

	testutil.Golden(t, privateTests+"policy.yaml", policy)
	testutil.Golden(t, privateTests+"logins.yaml", logins)
	testutil.Golden(t, privateTests+"egress.yaml", egress)
}

func TestSignaturePolicy_ReadsAPrivateRegistryWithACopyInKyvernosNamespaceOfTheOnePullSecretItsEntriesName(t *testing.T) {
	policy, logins, _ := signatureDocuments(t, privateSigners)
	copies := testutil.Documents(t, logins)

	require.Len(t, copies, 3, "the reader, and one store and one copy for the pull Secret both private entries name")
	reader, store, login := copies[0], copies[1], copies[2]
	for _, p := range testutil.Documents(t, policy) {
		assert.Equal(t, []any{testutil.Field(t, login, "spec", "target", "name")}, testutil.Field(t, p, "spec", "credentials", "secrets"),
			"the login %s reads", p["metadata"].(map[string]any)["name"])
	}
	assert.Equal(t, []any{"ServiceAccount", components.SignatureLoginReader, components.SignatureLoginNamespace},
		[]any{reader["kind"], testutil.Field(t, reader, "metadata", "name"), testutil.Field(t, reader, "metadata", "namespace")})
	assert.Equal(t, []any{shopLogin.Namespace, components.SignatureLoginReader}, []any{
		testutil.Field(t, store, "spec", "provider", "kubernetes", "remoteNamespace"),
		testutil.Field(t, store, "spec", "provider", "kubernetes", "auth", "serviceAccount", "name")}, "where the store reads and as whom")
	assert.Equal(t, []any{testutil.Field(t, store, "metadata", "name"), shopLogin.Name, "kubernetes.io/dockerconfigjson"}, []any{
		testutil.Field(t, login, "spec", "secretStoreRef", "name"), testutil.Field(t, login, "spec", "data", "0", "remoteRef", "key"),
		testutil.Field(t, login, "spec", "target", "template", "type")}, "what the copy reads and what it writes")
}

func TestSignaturePolicy_JudgesEveryNamespaceTheLabelPolicyJudgesAndNamesItselfInEachRefusal(t *testing.T) {
	rendered, _, _ := signatureDocuments(t, testedSigners)
	policy := testutil.Documents(t, rendered)[0]

	assert.Equal(t, "ImageValidatingPolicy", policy["kind"])
	assert.Equal(t, []any{"Deny"}, testutil.Field(t, policy, "spec", "validationActions"))
	assert.Equal(t, policySkips(t, podPolicy(t, "mitstack-require-app-label")), policySkips(t, policy), "the namespaces the signature policy skips")
	validations := testutil.Field(t, policy, "spec", "validations").([]any)
	require.Len(t, validations, len(testedSigners), "one validation per signer")
	for i, v := range validations {
		message := testutil.Field(t, v, "message").(string)
		assert.True(t, strings.HasPrefix(message, components.SignaturePolicyName+": "), "the refusal for %s does not open with the policy's name", testedSigners[i].Repository)
		assert.Contains(t, message, testedSigners[i].Repository, "the refusal names the repository")
		assert.Contains(t, message, testedSigners[i].Subject, "the refusal names the identity whose signature it wants")
	}
}

func TestSignaturePolicy_JudgesTheUpdateKubectlDebugAddsAnEphemeralContainerThroughAndKeepsTheRuleAutogenNeeds(t *testing.T) {
	rendered, _, _ := signatureDocuments(t, privateSigners)
	docs := testutil.Documents(t, rendered)

	require.Len(t, docs, 2, "the image policy and its twin")
	judgesAsItsTwin(t, docs[0], docs[1], "validationActions", "failurePolicy", "evaluation", "validationConfigurations", "credentials", "matchImageReferences", "attestors", "variables", "validations")
}

func TestSignaturePolicy_LimitsKyvernosAdmissionControllerToTheAPIServerDNSAndTheHostsItsChecksTake(t *testing.T) {
	kyverno := testutil.Documents(t, testutil.Manifest(t, "kyverno", components.Default().Served).Bytes)
	podLabels := func(deployment string) map[string]string {
		labels := map[string]string{}
		for _, doc := range testutil.Kind(kyverno, "Deployment") {
			if testutil.Field(t, doc, "metadata", "name") == deployment {
				for k, v := range testutil.Field(t, doc, "spec", "template", "metadata", "labels").(map[string]any) {
					labels[k] = v.(string)
				}
			}
		}
		require.NotEmpty(t, labels, "the Kyverno manifest holds no Deployment %s", deployment)
		return labels
	}
	pods := map[string]testutil.CiliumEndpoint{
		"the admission controller":  testutil.Endpoint("kyverno", nil, podLabels("kyverno-admission-controller")),
		"the background controller": testutil.Endpoint("kyverno", nil, podLabels("kyverno-background-controller")),
		"kube-dns":                  testutil.Endpoint("kube-system", nil, map[string]string{"k8s-app": "kube-dns"}),
		"an app pod":                testutil.Endpoint("app-shop", nil, map[string]string{"app.kubernetes.io/component": "admission-controller"}),
	}

	publicHosts := []string{
		"egress to entity kube-apiserver",
		"egress to ghcr.io on 443/TCP",
		"egress to pkg-containers.githubusercontent.com on 443/TCP",
		"egress to tuf-repo-cdn.sigstore.dev on 443/TCP",
		"egress with kube-dns on 53/UDP 53/TCP through the DNS proxy",
	}
	for _, tc := range []struct {
		name    string
		signers []components.Signer
		want    []string
	}{
		{"a list in ghcr.io", testedSigners, publicHosts},
		{"a list that also names a private ECR registry's repositories", privateSigners, append([]string{
			"egress to " + privateRegistry + " on 443/TCP",
			"egress to prod-eu-central-1-starport-layer-bucket.s3.eu-central-1.amazonaws.com on 443/TCP",
		}, publicHosts...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, rendered := signatureDocuments(t, tc.signers)
			egress := testutil.Documents(t, rendered)[0]

			assert.Equal(t, "CiliumClusterwideNetworkPolicy", egress["kind"])
			assert.ElementsMatch(t, tc.want, testutil.Grants(t, egress, pods, "the admission controller"))
			for _, other := range []string{"the background controller", "kube-dns", "an app pod"} {
				assert.Empty(t, testutil.Grants(t, egress, pods, other), "the policy selects %s", other)
			}
		})
	}
}

func TestSignaturePolicy_RefusesASignerWhoseImagesKyvernoCouldNotCheck(t *testing.T) {
	changed := func(s components.Signer, change func(*components.Signer)) []components.Signer {
		change(&s)
		return []components.Signer{s}
	}
	signer := func(change func(*components.Signer)) []components.Signer { return changed(testedSigners[0], change) }
	private := func(change func(*components.Signer)) []components.Signer { return changed(privateSigners[2], change) }
	var tooMany, tooManyLogins []components.Signer
	for i := range components.SignerLimit + 1 {
		tooMany = append(tooMany, signer(func(s *components.Signer) { s.Repository += fmt.Sprint("-", i) })[0])
	}
	for i := range components.SignerLoginLimit + 1 {
		tooManyLogins = append(tooManyLogins, private(func(s *components.Signer) {
			s.Repository += fmt.Sprint("-", i)
			s.PullSecret.Name += fmt.Sprint("-", i)
		})[0])
	}
	for _, tc := range []struct {
		name    string
		signers []components.Signer
		want    string
	}{
		{"no repository", signer(func(s *components.Signer) { s.Repository = "" }), "repository"},
		{"a repository with a tag", signer(func(s *components.Signer) { s.Repository += ":v1" }), "ghcr.io/mitstack-eu/talos-cluster-kit-tools:v1"},
		{"a repository with a digest", signer(func(s *components.Signer) { s.Repository += "@sha256:00" }), "@sha256:00"},
		{"a repository pattern", signer(func(s *components.Signer) { s.Repository = "ghcr.io/mitstack-eu/*" }), "ghcr.io/mitstack-eu/*"},
		{"a registry with no repository", signer(func(s *components.Signer) { s.Repository = "ghcr.io" }), "ghcr.io"},
		{"a repository that would close the policy's quoted string", signer(func(s *components.Signer) { s.Repository = "ghcr.io/mitstack-eu/a'b" }), "a'b"},
		{"a registry whose hosts the kit does not hold", signer(func(s *components.Signer) { s.Repository = "quay.io/mitstack-eu/tools" }), "quay.io"},
		{"a private ECR repository with no pull Secret", private(func(s *components.Signer) { s.PullSecret = components.SecretRef{} }), privateRegistry + "/shop-api"},
		{"a pull Secret for a registry read with no login", signer(func(s *components.Signer) { s.PullSecret = shopLogin }), "app-shop/registry-login"},
		{"a pull Secret with no namespace", private(func(s *components.Signer) { s.PullSecret.Namespace = "" }), "namespace"},
		{"a pull Secret name that is no Kubernetes name", private(func(s *components.Signer) { s.PullSecret.Name = "Registry_Login" }), "Registry_Login"},
		{"a pull Secret name longer than its limit", private(func(s *components.Signer) {
			s.PullSecret.Name = strings.Repeat("a", components.SignerSecretLimit+1)
		}), fmt.Sprint(components.SignerSecretLimit)},
		{"more pull Secrets than the kit takes", tooManyLogins, fmt.Sprint(components.SignerLoginLimit, " at most")},
		{"no issuer", signer(func(s *components.Signer) { s.Issuer = "" }), "issuer"},
		{"an issuer that is no https URL", signer(func(s *components.Signer) { s.Issuer = "token.actions.githubusercontent.com" }), "issuer"},
		{"no subject", signer(func(s *components.Signer) { s.Subject = "" }), "subject"},
		{"a subject that would close the policy's quoted string", signer(func(s *components.Signer) { s.Subject += `"` }), "subject"},
		{"a subject over two lines", signer(func(s *components.Signer) { s.Subject += "\nspec: {}" }), "subject"},
		{"a repository named twice", []components.Signer{testedSigners[0], testedSigners[0]}, "twice"},
		{"a repository longer than its limit", signer(func(s *components.Signer) {
			s.Repository += "/" + strings.Repeat("a", components.SignerRepositoryLimit)
		}), fmt.Sprint(components.SignerRepositoryLimit)},
		{"an issuer longer than its limit", signer(func(s *components.Signer) { s.Issuer += "/" + strings.Repeat("a", components.SignerIssuerLimit) }), fmt.Sprint(components.SignerIssuerLimit)},
		{"a subject longer than its limit", signer(func(s *components.Signer) { s.Subject += strings.Repeat("a", components.SignerSubjectLimit) }), fmt.Sprint(components.SignerSubjectLimit)},
		{"more signers than the kit takes", tooMany, fmt.Sprint(components.SignerLimit, " signers")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifests, err := components.SignaturePolicy(tc.signers)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want, "what the refusal names")
			assert.Empty(t, manifests)
		})
	}
}
