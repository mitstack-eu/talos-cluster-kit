package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

// AddImageJq puts the jq the tools image ships first on the fakes' PATH, fetched once per machine by the pin in jq.env,
// since the runner's own jq parses numbers differently.
func (tools *Tools) AddImageJq(t *testing.T) {
	t.Helper()
	require.NoError(t, os.Symlink(imageJq(t), filepath.Join(tools.Dir, "jq")))
}

func imageJq(t *testing.T) string {
	t.Helper()
	version, sum := tools.Pin("JQ_VERSION"), tools.Pin("JQ_SHA256")
	require.NotEmpty(t, version, "jq.env pins no JQ_VERSION")
	require.Equal(t, "linux/amd64", runtime.GOOS+"/"+runtime.GOARCH, "the tools image's jq is a linux/amd64 binary")

	cache, err := os.UserCacheDir()
	require.NoError(t, err)
	dir := filepath.Join(cache, "talos-cluster-kit", version+"-"+sum)
	path := filepath.Join(dir, "jq")
	if fileSHA256(path) == sum {
		return path
	}

	require.NoError(t, os.MkdirAll(dir, 0o755))
	resp, err := http.Get("https://github.com/jqlang/jq/releases/download/" + version + "/jq-linux-amd64")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "fetching %s", version)
	tmp, err := os.CreateTemp(dir, "jq-*")
	require.NoError(t, err)
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, resp.Body)
	require.NoError(t, err)
	require.NoError(t, tmp.Close())
	require.Equal(t, sum, fileSHA256(tmp.Name()), "the fetched %s does not match jq.env's JQ_SHA256", version)
	require.NoError(t, os.Chmod(tmp.Name(), 0o755))
	require.NoError(t, os.Rename(tmp.Name(), path))
	return path
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
