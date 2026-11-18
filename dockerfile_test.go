// The tools image builds machine-config with the Go release the module's go.mod names.
package talosclusterkit

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolsDockerfile_BuildsMachineConfigWithTheGoReleaseTheModuleNamesPinnedByDigest(t *testing.T) {
	mod, err := os.ReadFile("go.mod")
	require.NoError(t, err)
	dockerfile, err := os.ReadFile("tools/Dockerfile")
	require.NoError(t, err)
	goDirective := regexp.MustCompile(`(?m)^go (\S+)$`).FindSubmatch(mod)
	require.NotNil(t, goDirective, "go.mod names its Go release")

	builder := regexp.MustCompile(`(?m)^FROM docker\.io/library/golang:(\S+)-alpine@sha256:[0-9a-f]{64} AS machine-config$`).FindSubmatch(dockerfile)

	require.NotNil(t, builder, "tools/Dockerfile builds machine-config in a golang stage pinned by its digest")
	assert.Equal(t, string(goDirective[1]), string(builder[1]), "the golang stage runs the Go release go.mod names")
}
