package talosclusterkit

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	githubIssuer = "https://token.actions.githubusercontent.com"
	kitPipeline  = "https://github.com/mitstack-eu/talos-cluster-kit/.github/workflows/ci.yml@refs/heads/main"
	signedTests  = "components/admission/tests/mitstack-verify-image-signatures/"
)

// testedSigners is the list the Kyverno tests judge: the kit's own tools image, and Kyverno's image under an identity
// that did not sign it.
var testedSigners = []components.Signer{
	{Repository: "ghcr.io/mitstack-eu/talos-cluster-kit-tools", Issuer: githubIssuer, Subject: kitPipeline},
	{Repository: "ghcr.io/kyverno/kyverno", Issuer: githubIssuer, Subject: kitPipeline},
}

// signatureDocuments renders the list and splits its one manifest into the policy and the egress limit.
func signatureDocuments(t *testing.T, signers []components.Signer) (policy, egress []byte) {
	t.Helper()
	manifests, err := components.SignaturePolicy(signers)
	require.NoError(t, err)
	require.Len(t, manifests, 1, "the manifests a signer list renders")
	require.Equal(t, components.SignaturePolicyName, manifests[0].Name)
	policy, egress, found := bytes.Cut(manifests[0].Bytes, []byte("\n---\n"))
	require.True(t, found, "the manifest holds two documents")
	return append(policy, '\n'), egress
}

func TestSignaturePolicy_AnEmptySignerListRendersNoPolicyAndNoEgressLimit(t *testing.T) {
	manifests, err := components.SignaturePolicy(nil)

	require.NoError(t, err)
	assert.Empty(t, manifests, "every image passes on its digest alone, and Kyverno reaches no registry")
}

func TestSignaturePolicy_RendersThePolicyAndTheEgressLimitTheKyvernoTestsRead(t *testing.T) {
	policy, egress := signatureDocuments(t, testedSigners)

	testutil.Golden(t, signedTests+"policy.yaml", policy)
	testutil.Golden(t, signedTests+"egress.yaml", egress)
}

func TestSignaturePolicy_JudgesEveryNamespaceTheLabelPolicyJudgesAndNamesItselfInEachRefusal(t *testing.T) {
	rendered, _ := signatureDocuments(t, testedSigners)
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

func TestSignaturePolicy_LimitsKyvernosAdmissionControllerToTheAPIServerDNSAndTheHostsItsChecksTake(t *testing.T) {
	_, rendered := signatureDocuments(t, testedSigners)
	egress := testutil.Documents(t, rendered)[0]
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

	assert.Equal(t, "CiliumClusterwideNetworkPolicy", egress["kind"])
	assert.Equal(t, []string{
		"egress to entity kube-apiserver",
		"egress to ghcr.io on 443/TCP",
		"egress to pkg-containers.githubusercontent.com on 443/TCP",
		"egress to tuf-repo-cdn.sigstore.dev on 443/TCP",
		"egress with kube-dns on 53/UDP 53/TCP through the DNS proxy",
	}, testutil.Grants(t, egress, pods, "the admission controller"))
	for _, other := range []string{"the background controller", "kube-dns", "an app pod"} {
		assert.Empty(t, testutil.Grants(t, egress, pods, other), "the policy selects %s", other)
	}
}

func TestSignaturePolicy_RefusesASignerWhoseImagesKyvernoCouldNotCheck(t *testing.T) {
	signer := func(change func(*components.Signer)) []components.Signer {
		s := testedSigners[0]
		change(&s)
		return []components.Signer{s}
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
		{"a registry whose hosts and credentials the kit does not hold", signer(func(s *components.Signer) {
			s.Repository = "839001574838.dkr.ecr.eu-central-1.amazonaws.com/mitstack-api"
		}), "839001574838.dkr.ecr.eu-central-1.amazonaws.com"},
		{"no issuer", signer(func(s *components.Signer) { s.Issuer = "" }), "issuer"},
		{"an issuer that is no https URL", signer(func(s *components.Signer) { s.Issuer = "token.actions.githubusercontent.com" }), "issuer"},
		{"no subject", signer(func(s *components.Signer) { s.Subject = "" }), "subject"},
		{"a subject that would close the policy's quoted string", signer(func(s *components.Signer) { s.Subject += `"` }), "subject"},
		{"a subject over two lines", signer(func(s *components.Signer) { s.Subject += "\nspec: {}" }), "subject"},
		{"a repository named twice", []components.Signer{testedSigners[0], testedSigners[0]}, "twice"},
		{"a repository longer than the serving server's user data has room for", signer(func(s *components.Signer) {
			s.Repository += "/" + strings.Repeat("a", components.SignerRepositoryLimit)
		}), fmt.Sprint(components.SignerRepositoryLimit)},
		{"an issuer longer than its limit", signer(func(s *components.Signer) { s.Issuer += "/" + strings.Repeat("a", components.SignerIssuerLimit) }), fmt.Sprint(components.SignerIssuerLimit)},
		{"a subject longer than its limit", signer(func(s *components.Signer) { s.Subject += strings.Repeat("a", components.SignerSubjectLimit) }), fmt.Sprint(components.SignerSubjectLimit)},
		{"more signers than the kit takes", func() []components.Signer {
			var list []components.Signer
			for i := range components.SignerLimit + 1 {
				list = append(list, signer(func(s *components.Signer) { s.Repository += fmt.Sprint("-", i) })[0])
			}
			return list
		}(), fmt.Sprint(components.SignerLimit, " signers")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifests, err := components.SignaturePolicy(tc.signers)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want, "what the refusal names")
			assert.Empty(t, manifests)
		})
	}
}
