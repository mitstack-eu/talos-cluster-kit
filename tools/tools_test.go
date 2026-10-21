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
		{name: "TALOS_VERSION", want: "v1.14.4"},
		{name: "KUBERNETES_VERSION", want: "v1.37.1"},
		{name: "TALOSCTL_SHA256", want: "0a69e3f9b682c5cd844b78dae305f6056ceb86c5fcc6913dcea78c527ed97cee"},
		{name: "HELM_VERSION", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tools.Pin(tc.name)

			assert.Equal(t, tc.want, got, "Pin(%q)", tc.name)
		})
	}
}
