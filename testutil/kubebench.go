package testutil

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

// KubeBenchConfig lays out kube-bench's config directory as the tools image carries it: the cis-1.10 config of the
// kube-bench scan.env pins, with tools/kube-bench-talos.yaml over that benchmark's settings.
func KubeBenchConfig(t *testing.T) string {
	t.Helper()
	version, sum := tools.Pin("KUBE_BENCH_VERSION"), tools.Pin("KUBE_BENCH_SHA256")
	release := "https://github.com/aquasecurity/kube-bench/releases/download/" + version
	f, err := os.Open(pinnedDownload(t, release+"/kube-bench_"+strings.TrimPrefix(version, "v")+"_linux_amd64.tar.gz", version, sum, "kube-bench.tar.gz"))
	require.NoError(t, err)
	defer f.Close()
	unzipped, err := gzip.NewReader(f)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "cfg", "cis-1.10"), 0o755))
	archive := tar.NewReader(unzipped)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if !filepath.IsLocal(header.Name) {
			t.Errorf("kube-bench's archive names %q, a path outside the directory it is unpacked into", header.Name)
			continue
		}
		name := path.Clean(header.Name)
		benchmarkFile := path.Dir(name) == "cfg/cis-1.10" && name != "cfg/cis-1.10/config.yaml"
		if name != "cfg/config.yaml" && !benchmarkFile {
			continue
		}
		data, err := io.ReadAll(archive)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	}
	_, here, _, _ := runtime.Caller(0)
	talos, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "tools", "kube-bench-talos.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cfg", "cis-1.10", "config.yaml"), talos, 0o644))
	return filepath.Join(dir, "cfg")
}

// KubeBenchAudits maps each check of the cis-1.10 benchmark under a kube-bench config directory to its audit command.
func KubeBenchAudits(t *testing.T, config string) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(config, "cis-1.10", "*.yaml"))
	require.NoError(t, err)
	audits := map[string]string{}
	for _, file := range files {
		var controls struct {
			Groups []struct{ Checks []struct{ ID, Audit string } }
		}
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		require.NoError(t, yaml.Unmarshal(data, &controls), file)
		for _, group := range controls.Groups {
			for _, check := range group.Checks {
				audits[check.ID] = check.Audit
			}
		}
	}
	return audits
}
