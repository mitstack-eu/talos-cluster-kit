//go:build toolsimage

// The tools image's machine-config prints the configuration a release gives the running node on its stdin.
package talosclusterkit

import (
	"bytes"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func TestToolsImage_MachineConfigPrintsTheConfigurationAReleaseGivesTheRunningNodeOnItsStdin(t *testing.T) {
	built := os.Getenv("TOOLS_IMAGE")
	require.NotEmpty(t, built, "TOOLS_IMAGE names the tools image to run, as the tools image CI job builds it")
	const release = "sha256:5f0c1b2a3d4e5f60718293a4b5c6d7e8f90123456789abcdef0123456789abcd"
	cluster := testutil.NewCluster(t)
	running, err := nodeconfig.Render(cluster, nodeconfig.Node{})
	require.NoError(t, err)
	cluster.Release = release
	want, err := nodeconfig.Render(cluster, nodeconfig.Node{})
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", "run", "--rm", "-i", built, "machine-config", "--release", release)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(running), &stdout, &stderr

	err = cmd.Run()

	require.NoError(t, err, stderr.String())
	assert.Equal(t, testutil.Documents(t, want), testutil.Documents(t, stdout.Bytes()))
}
