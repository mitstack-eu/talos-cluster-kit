// The Go module cache serves a module only once the checksum database holds the hashes of its go.mod and zip.
package packagecache_test

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/dirhash"
	"golang.org/x/mod/sumdb/note"

	"github.com/mitstack-eu/talos-cluster-kit/packagecache"
)

const (
	modulePath    = "example.com/Acme/lib"
	escapedModule = "example.com/!acme/lib"
	moduleVersion = "v1.2.0"
)

// goModule is one module version as the upstream serves it.
type goModule struct{ mod, zip []byte }

func moduleZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(modulePath + "@" + moduleVersion + "/" + name)
		require.NoError(t, err)
		_, err = f.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func newModule(t *testing.T) goModule {
	t.Helper()
	mod := "module " + modulePath + "\n\ngo 1.26\n"
	return goModule{mod: []byte(mod), zip: moduleZip(t, map[string]string{"go.mod": mod, "lib.go": "package lib\n"})}
}

// goSumLines are the lines the checksum database holds for m.
func goSumLines(t *testing.T, m goModule) []byte {
	t.Helper()
	zipFile := filepath.Join(t.TempDir(), "m.zip")
	require.NoError(t, os.WriteFile(zipFile, m.zip, 0o644))
	zipHash, err := dirhash.HashZip(zipFile, dirhash.Hash1)
	require.NoError(t, err)
	modHash, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(m.mod)), nil })
	require.NoError(t, err)
	return fmt.Appendf(nil, "%s %s %s\n%s %s/go.mod %s\n", modulePath, moduleVersion, zipHash, modulePath, moduleVersion, modHash)
}

// goUpstream serves m at the escaped path, counting each request it answers by path.
type goUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	asked map[string]int
}

func newGoUpstream(t *testing.T, m goModule) *goUpstream {
	t.Helper()
	u := &goUpstream{asked: map[string]int{}}
	base := "/" + escapedModule + "/@v/"
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.asked[r.URL.Path]++
		u.mu.Unlock()
		switch r.URL.Path {
		case base + "list":
			fmt.Fprintln(w, moduleVersion)
		case base + moduleVersion + ".info":
			fmt.Fprintf(w, `{"Version":%q,"Time":"2026-11-01T00:00:00Z"}`, moduleVersion)
		case base + moduleVersion + ".mod":
			w.Write(m.mod)
		case base + moduleVersion + ".zip":
			w.Write(m.zip)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *goUpstream) times(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.asked[path]
}

// checksumDB is a checksum database that holds the lines gosum gives, signed by a key of its own.
func checksumDB(t *testing.T, gosum func(path, vers string) ([]byte, error)) (url, verifierKey string) {
	t.Helper()
	signer, verifier, err := note.GenerateKey(rand.Reader, "sum.example.test")
	require.NoError(t, err)
	db := httptest.NewServer(sumdb.NewServer(sumdb.NewTestServer(signer, gosum)))
	t.Cleanup(db.Close)
	return db.URL, verifier
}

// goCache is the Go module cache in front of upstream, whose checksum database holds what gosum gives.
func goCache(t *testing.T, root string, upstream *goUpstream, gosum func(path, vers string) ([]byte, error)) *httptest.Server {
	t.Helper()
	store, err := packagecache.NewStore(root, time.Now)
	require.NoError(t, err)
	dbURL, key := checksumDB(t, gosum)
	cache := httptest.NewServer(packagecache.GoProxy{
		Upstream: upstream.URL,
		Sums:     packagecache.NewChecksumDB(dbURL, key, http.DefaultClient, store),
		Store:    store,
		Client:   http.DefaultClient,
	})
	t.Cleanup(cache.Close)
	return cache
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, body
}

func TestGoProxy_ServesAModuleTheChecksumDatabaseHoldsAndServesItASecondTimeFromItsOwnDisk(t *testing.T) {
	m := newModule(t)
	upstream := newGoUpstream(t, m)
	cache := goCache(t, t.TempDir(), upstream, func(string, string) ([]byte, error) { return goSumLines(t, m), nil })
	base := "/" + escapedModule + "/@v/" + moduleVersion

	for round := 1; round <= 2; round++ {
		for ext, want := range map[string][]byte{".mod": m.mod, ".zip": m.zip} {
			status, body := get(t, cache.URL+base+ext)

			assert.Equal(t, http.StatusOK, status, "GET %s%s on round %d", base, ext, round)
			assert.Equal(t, want, body, "the %s the cache serves on round %d", ext, round)
		}
	}
	assert.Equal(t, 1, upstream.times(base+".mod"), "the upstream fetches of the go.mod")
	assert.Equal(t, 1, upstream.times(base+".zip"), "the upstream fetches of the zip")
}

func TestGoProxy_RefusesAndStoresNothingOfAModuleWhoseHashDiffersFromTheChecksumDatabaseOrThatItDoesNotHold(t *testing.T) {
	genuine := newModule(t)
	altered := goModule{
		mod: []byte("module " + modulePath + "\n\ngo 1.26\n\nrequire evil.example/x v1.0.0\n"),
		zip: moduleZip(t, map[string]string{"go.mod": string(genuine.mod), "lib.go": "package lib\n\nfunc init() { panic(1) }\n"}),
	}
	held := func(string, string) ([]byte, error) { return goSumLines(t, genuine), nil }
	for _, tc := range []struct {
		name  string
		ext   string
		gosum func(path, vers string) ([]byte, error)
	}{
		{"a zip whose hash differs from the database's", ".zip", held},
		{"a go.mod whose hash differs from the database's", ".mod", held},
		{"a zip of a module the database does not hold", ".zip", func(string, string) ([]byte, error) { return nil, os.ErrNotExist }},
		{"a go.mod of a module the database does not hold", ".mod", func(string, string) ([]byte, error) { return nil, os.ErrNotExist }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cache := goCache(t, root, newGoUpstream(t, altered), tc.gosum)
			path := "/" + escapedModule + "/@v/" + moduleVersion + tc.ext

			status, body := get(t, cache.URL+path)

			assert.Equal(t, http.StatusBadGateway, status, "GET %s: %s", path, body)
			assert.Contains(t, string(body), modulePath+"@"+moduleVersion, "the refusal names the module")
			assertHoldsNone(t, root, altered.mod, altered.zip)
			again, _ := get(t, cache.URL+path)
			assert.Equal(t, http.StatusBadGateway, again, "a second GET of a refused module serves nothing from disk")
		})
	}
}

func TestGoProxy_PassesTheVersionListAndInfoThroughToTheUpstreamEachTime(t *testing.T) {
	m := newModule(t)
	upstream := newGoUpstream(t, m)
	cache := goCache(t, t.TempDir(), upstream, func(string, string) ([]byte, error) { return goSumLines(t, m), nil })
	base := "/" + escapedModule + "/@v/"

	for _, path := range []string{base + "list", base + moduleVersion + ".info", base + "list", base + moduleVersion + ".info"} {
		status, _ := get(t, cache.URL+path)
		assert.Equal(t, http.StatusOK, status, "GET %s", path)
	}

	assert.Equal(t, 2, upstream.times(base+"list"), "the version list is asked of the upstream each time, since it changes")
	assert.Equal(t, 2, upstream.times(base+moduleVersion+".info"), "the info is asked of the upstream each time, since no hash covers it")
}

func TestGoProxy_AnswersNotFoundForTheChecksumDatabaseAndAModuleTheUpstreamLacks(t *testing.T) {
	m := newModule(t)
	cache := goCache(t, t.TempDir(), newGoUpstream(t, m), func(string, string) ([]byte, error) { return goSumLines(t, m), nil })

	for _, path := range []string{
		"/sumdb/sum.golang.org/supported",
		"/example.com/missing/@v/v0.1.0.zip",
		"/example.com/missing/@v/list",
	} {
		status, _ := get(t, cache.URL+path)

		assert.Equal(t, http.StatusNotFound, status, "GET %s, which the go command then asks the next proxy or the database for", path)
	}
}

func TestGoProxy_AnswersItsHealthWithoutTheUpstream(t *testing.T) {
	m := newModule(t)
	upstream := newGoUpstream(t, m)
	cache := goCache(t, t.TempDir(), upstream, func(string, string) ([]byte, error) { return goSumLines(t, m), nil })
	upstream.Close()

	status, _ := get(t, cache.URL+"/healthz")

	assert.Equal(t, http.StatusOK, status, "GET /healthz")
}

// assertHoldsNone fails when any file under root holds one of the refused bodies.
func assertHoldsNone(t *testing.T, root string, refused ...[]byte) {
	t.Helper()
	for _, name := range remaining(t, root) {
		data, err := os.ReadFile(filepath.Join(root, name))
		require.NoError(t, err)
		for _, body := range refused {
			assert.NotEqual(t, body, data, "the cache stored %s, which holds a refused body", name)
		}
	}
}
