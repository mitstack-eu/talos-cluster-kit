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

func TestList_RefusesAnImageFromARegistryNoNodeSendsToTheMirrorOrADockerHubNameWithoutItsNamespace(t *testing.T) {
	for name, want := range map[string]string{"registry.gitlab.com/acme/tool": "registry.gitlab.com", "docker.io/traefik": "docker.io/library/traefik"} {
		_, err := mirror.List(components.Set{}, components.Image{Name: name, Tag: "1", Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
		assert.ErrorContains(t, err, want, name)
	}
}

func TestList_MirrorsThePostgres16ImagePostgresDBKubePinsAsCloudNativePGsDefault(t *testing.T) {
	const postgres16 = "ghcr.io/cloudnative-pg/postgresql:16.15-system-trixie@sha256:8ca5699981c25db72e549e9fa69561bac233df15b9b1e3a6282847b0629bb5ea"
	set := components.Default()
	operator := testutil.Manifest(t, "cloudnative-pg", set.Served, set.Inline)

	images, err := mirror.List(set)

	require.NoError(t, err)
	assert.Contains(t, string(operator.Bytes), "\n  POSTGRES_IMAGE_NAME: "+postgres16+"\n")
	assert.Contains(t, string(mirror.Render(images)), postgres16+"\n")
	assert.NotContains(t, string(mirror.Render(images)), "postgresql:18.6")
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
