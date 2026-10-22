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
	in := testutil.HostingInputs(t)
	set, err := components.Hosting(in)
	require.NoError(t, err)

	images, err := mirror.List(set)

	require.NoError(t, err)
	for _, ref := range append(slices.Clone(testutil.HostingImages), in.Init.ToolsImage) {
		assert.Contains(t, string(mirror.Render(images)), ref+"\n")
	}
}
