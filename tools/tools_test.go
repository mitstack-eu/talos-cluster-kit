package tools_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

func TestPin_ReadsEachBinarysPinAndNothingForAnUnknownName(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{name: "TALOS_VERSION", want: `^v\d+\.\d+\.\d+$`},
		{name: "KUBERNETES_VERSION", want: `^v\d+\.\d+\.\d+$`},
		{name: "TALOSCTL_SHA256", want: `^[0-9a-f]{64}$`},
		{name: "KUBECTL_SHA256", want: `^[0-9a-f]{64}$`},
		{name: "HELM_VERSION", want: `^$`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tools.Pin(tc.name)

			assert.Regexp(t, tc.want, got, "Pin(%q)", tc.name)
		})
	}
}

func TestImage_NamesTheKitsToolsPackageByTagAndDigest(t *testing.T) {
	ref := tools.Image

	assert.Regexp(t, `^ghcr\.io/mitstack-eu/talos-cluster-kit-tools:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`, ref)
}
