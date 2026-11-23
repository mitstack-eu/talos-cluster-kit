// package-cache runs the Go or npm cache, or the sweep of a directory another server fills, each evicting by its limits.
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_SweepRemovesWhatIsPastTheLimitsOnceAtStartAndStopsWithItsContext(t *testing.T) {
	dir := t.TempDir()
	for name, idle := range map[string]time.Duration{"stale": 72 * time.Hour, "fresh": time.Minute} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(name), 0o644))
		used := time.Now().Add(-idle)
		require.NoError(t, os.Chtimes(path, used, used))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var log bytes.Buffer

	err := run(ctx, []string{"sweep", "--dir=" + dir, "--max-size=1Mi", "--max-age=48h"}, &log)

	require.NoError(t, err, log.String())
	assert.NoFileExists(t, filepath.Join(dir, "stale"), "the entry unused past the age limit")
	assert.FileExists(t, filepath.Join(dir, "fresh"), "the entry used a minute ago")
	assert.Contains(t, log.String(), dir, "the sweep logs what it swept")
}

func TestRun_RefusesWhatItCannotRunWithNamingIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", nil, "go, npm or sweep"},
		{"an unknown subcommand", []string{"pip"}, "go, npm or sweep"},
		{"a size that is no quantity", []string{"go", "--root=" + t.TempDir(), "--max-size=lots", "--max-age=336h"}, "max-size"},
		{"no size", []string{"npm", "--root=" + t.TempDir(), "--max-age=336h"}, "max-size"},
		{"an age that is no duration", []string{"sweep", "--dir=" + t.TempDir(), "--max-size=1Gi", "--max-age=two weeks"}, "max-age"},
		{"a sweep of no directory", []string{"sweep", "--max-size=1Gi", "--max-age=336h"}, "dir"},
		{"a cache with no root", []string{"go", "--max-size=1Gi", "--max-age=336h"}, "root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := run(ctx, tc.args, &bytes.Buffer{})

			assert.ErrorContains(t, err, tc.want, "run(%q)", tc.args)
		})
	}
}
