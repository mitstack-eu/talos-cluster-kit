package mirror_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/mirror"
)

func TestList_AddsTheExtraImagesOnceInOrder(t *testing.T) {
	kubelet := components.Image{Name: "ghcr.io/siderolabs/kubelet", Tag: "v1.37.1", Digest: "sha256:3777bdaaa352e1f425dff9d8868a970923c66d0629bad16f935c41018a83d9d8"}
	tools := components.Image{Name: "ghcr.io/mitstack-eu/talos-tools", Tag: "v0.1.0", Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}

	images, err := mirror.List(components.Set{TalosImages: []components.Image{kubelet}}, tools, kubelet)

	require.NoError(t, err)
	assert.Equal(t, "docker.io/otel/opentelemetry-collector-contrib:0.163.1@sha256:1175b03003053962012f5a231f5567f66a3e8fdb960c7835c0e48f7aa1555806\n"+
		tools.Ref()+"\n"+kubelet.Ref()+"\n", string(mirror.Render(images)))
}

func TestList_RefusesAnImageFromARegistryNoNodeSendsToTheMirror(t *testing.T) {
	other := components.Image{Name: "registry.gitlab.com/acme/tool", Tag: "1", Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}

	_, err := mirror.List(components.Set{}, other)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "registry.gitlab.com")
}
