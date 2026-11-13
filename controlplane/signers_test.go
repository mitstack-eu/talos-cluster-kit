package controlplane_test

import (
	mathrand "math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

var kitSigner = components.Signer{
	Repository: "ghcr.io/mitstack-eu/talos-cluster-kit-tools",
	Issuer:     "https://token.actions.githubusercontent.com",
	Subject:    "https://github.com/mitstack-eu/talos-cluster-kit/.github/workflows/ci.yml@refs/heads/main",
}

func signed(h *harness) { h.plane.Signers = []components.Signer{kitSigner} }

// longestSignerList gives the plane as many signers as the kit takes, each field at its limit and of random text.
func longestSignerList(h *harness) {
	random := mathrand.New(mathrand.NewChaCha8([32]byte{1}))
	text := func(prefix string, length int) string {
		const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
		out := []byte(prefix)
		for len(out) < length {
			out = append(out, letters[random.IntN(len(letters))])
		}
		return string(out)
	}
	h.plane.Signers = nil
	for range components.SignerLimit {
		h.plane.Signers = append(h.plane.Signers, components.Signer{
			Repository: text("ghcr.io/", components.SignerRepositoryLimit),
			Issuer:     text("https://", components.SignerIssuerLimit),
			Subject:    text("https://", components.SignerSubjectLimit),
		})
	}
}

func signaturePolicy(t *testing.T, signers []components.Signer) components.Manifest {
	t.Helper()
	manifests, err := components.SignaturePolicy(signers)
	require.NoError(t, err)
	require.Len(t, manifests, 1, "the manifests the signer list renders")
	return manifests[0]
}

func TestSigners_ABuildAppliesTheListsPolicyWhichItsBuilderFetchesFromTheServingServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []func(*harness)
		want []string
	}{
		{"a cluster that hosts the platform", []func(*harness){hosting, signed}, append(slices.Clone(hostingInline), components.SignaturePolicyName)},
		{"a cluster that hosts none", []func(*harness){signed}, []string{components.SignaturePolicyName}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)
			want := signaturePolicy(t, h.plane.Signers)

			require.NoError(t, h.converge())

			served := servingFile(t, h)
			assert.ElementsMatch(t, tc.want, names(served), "the inline manifests the serving server holds")
			at := slices.Index(names(served), components.SignaturePolicyName)
			require.GreaterOrEqual(t, at, 0, "the serving server holds %v", names(served))
			assert.Equal(t, string(want.Bytes), testutil.Kind(served, "KubeInlineManifestConfig")[at]["manifest"], "the policy the builder fetches")
			assert.NotContains(t, names(builderConfiguration(t, h)), components.SignaturePolicyName, "the builder's user data carries the policy, which it has no room for")
		})
	}
}

func TestSigners_ABuildWithNoListAndNoPlatformOrdersNoServingServer(t *testing.T) {
	h := newHarness(t)

	require.NoError(t, h.converge())

	for _, spec := range h.cloud.ServerSpecs {
		assert.NotEqual(t, h.plane.Name(controlplane.RoleServing), spec.Name, "a server that would serve the builder nothing")
	}
}

func TestSigners_EveryReleaseCarriesThePolicyOfThePlanesListAndNoneWithoutAList(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   []func(*harness)
		signed bool
	}{
		{"a cluster that hosts the platform under a signer list", []func(*harness){hosting, signed}, true},
		{"a cluster that hosts no platform under a signer list", []func(*harness){signed}, true},
		{"a cluster that hosts the platform under no list", []func(*harness){hosting}, false},
		{"a cluster that hosts no platform under no list", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)
			h.withLoadBalancers()
			plane, err := h.plane.WithGatewayCA()
			require.NoError(t, err)

			release := h.releaseOf(plane)

			if !tc.signed {
				assert.NotContains(t, manifestNames(release.Components), components.SignaturePolicyName, "a cluster with no list refuses no image for its signature")
				return
			}
			assert.Equal(t, string(signaturePolicy(t, h.plane.Signers).Bytes), string(testutil.Manifest(t, components.SignaturePolicyName, release.Components).Bytes), "the policy the release carries")
		})
	}
}

func TestSigners_ASignerKyvernoCouldNotCheckStopsTheInstallBeforeAnyServerAndTheRelease(t *testing.T) {
	const ecr = "839001574838.dkr.ecr.eu-central-1.amazonaws.com"
	unchecked := func(h *harness) {
		h.plane.Signers = []components.Signer{{Repository: ecr + "/mitstack-api", Issuer: kitSigner.Issuer, Subject: kitSigner.Subject}}
	}
	for name, opts := range map[string][]func(*harness){"a cluster that hosts the platform": {hosting, unchecked}, "a cluster that hosts none": {unchecked}} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, opts...)
			plane, err := h.plane.WithGatewayCA()
			require.NoError(t, err)

			_, releaseErr := plane.Release(h.read())
			installErr := h.converge()

			assert.ErrorContains(t, releaseErr, ecr, "the release's refusal names the registry")
			assert.ErrorContains(t, installErr, ecr, "the install's refusal names the registry")
			assert.Empty(t, h.cloud.ServerSpecs, "servers the refused install created")
		})
	}
}
