package mirror_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/mirror"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func TestList_AddsTheExtraImagesOnceInOrder(t *testing.T) {
	kubelet := components.Image{Name: "ghcr.io/siderolabs/kubelet", Tag: "v1.37.1", Digest: "sha256:3777bdaaa352e1f425dff9d8868a970923c66d0629bad16f935c41018a83d9d8"}
	tools := components.Image{Name: "ghcr.io/mitstack-eu/talos-tools", Tag: "v0.1.0", Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}

	set := components.Set{TalosImages: []components.Image{kubelet}}
	setImages, err := set.Images()
	require.NoError(t, err)

	images, err := mirror.List(set, tools, kubelet)

	require.NoError(t, err)
	var want []string
	for _, image := range append(setImages, tools) {
		want = append(want, image.Ref())
	}
	slices.Sort(want)
	assert.Equal(t, strings.Join(want, "\n")+"\n", string(mirror.Render(images)))
}

func TestList_RefusesAnImageFromARegistryNoNodeSendsToTheMirror(t *testing.T) {
	other := components.Image{Name: "registry.gitlab.com/acme/tool", Tag: "1", Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}

	_, err := mirror.List(components.Set{}, other)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "registry.gitlab.com")
}

func TestList_HoldsEveryImageTheHostingSetPullsByDigest(t *testing.T) {
	set, err := components.Hosting(testutil.HostingInputs(t))
	require.NoError(t, err)
	defaultImages, err := mirror.List(components.Default())
	require.NoError(t, err)

	images, err := mirror.List(set)

	require.NoError(t, err)
	hosting := []string{
		"quay.io/openbao/openbao:2.7.1@sha256:6d2b93856e3fcf7b18ad855a0b51eaba474dc8b79cf554379ea32034797d2acf",
		"quay.io/jetstack/cert-manager-controller:v1.21.2@sha256:70f532fd9cfde0b09d55687200942399d89838bc2d5d5b45152eb799a15912b8",
		"quay.io/jetstack/cert-manager-webhook:v1.21.2@sha256:a60e2dac46dbb8a7f3df95c54ce941012f54c2fe022f0ee55aaa1ab40ed957ae",
		"quay.io/jetstack/cert-manager-cainjector:v1.21.2@sha256:c85268c64f2e0e76684bf5fe8906caff34b82523561c6affe0fae3546bd87562",
		"quay.io/jetstack/cert-manager-acmesolver:v1.21.2@sha256:699b40d622211ab7accad8a21b04c5fbaa1841ef7a12621e8de492dbe27b2503",
		"quay.io/jetstack/trust-manager:v0.25.0@sha256:0521921c2a46d14d0417336b20bcde69215ef3164f2977cc939d53c6e5224a17",
		"ghcr.io/external-secrets/external-secrets:v2.11.0@sha256:66fb710878cbf3eba4a927e35c5d75e29a202c9bab238aad64bfb23b786b962b",
		"docker.io/bitnami/sealed-secrets-controller:0.40.0@sha256:b1ff382e9300dc9e74991f3177b18cafc06ce43b5c21349c86adbdd1d1c177d3",
		"docker.io/traefik:v3.7.13@sha256:24841fe2de7304c149343d877d2923b4c8800a38ba015dea9174c23b20e344a0",
	}
	var want []string
	for _, image := range defaultImages {
		want = append(want, image.Ref())
	}
	want = append(want, hosting...)
	slices.Sort(want)
	assert.Equal(t, strings.Join(want, "\n")+"\n", string(mirror.Render(images)))
}
