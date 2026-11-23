//go:build toolsimage

// The tools image's package-cache sweeps a cache's directory as the image cache's pod runs it, as the image's own user.
package talosclusterkit

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolsImage_PackageCacheSweepRemovesTheEntryPastItsAgeAndKeepsTheRest(t *testing.T) {
	built := os.Getenv("TOOLS_IMAGE")
	require.NotEmpty(t, built, "TOOLS_IMAGE names the tools image to run, as the tools image CI job builds it")
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o777))
	for name, idle := range map[string]time.Duration{"stale": 72 * time.Hour, "fresh": time.Minute} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(name), 0o666))
		used := time.Now().Add(-idle)
		require.NoError(t, os.Chtimes(path, used, used))
	}
	cmd := exec.Command("docker", "run", "--rm", "-v", dir+":/cache", built,
		"timeout", "5", "package-cache", "sweep", "--dir=/cache", "--max-size=1Mi", "--max-age=48h")

	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit), "the sweep runs until timeout stops it: %v\n%s", err, out)
	assert.Equal(t, 124, exit.ExitCode(), "timeout's exit once the sweep outlives it, so the sweep ran on: %s", out)
	assert.NoFileExists(t, filepath.Join(dir, "stale"), "the entry unused past the age limit")
	assert.FileExists(t, filepath.Join(dir, "fresh"), "the entry used a minute ago")
}
