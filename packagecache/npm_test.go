// The npm package cache serves a tarball only once it matches the integrity its registry's metadata names.
package packagecache_test

import (
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/packagecache"
)

// npmPackage is one package version as the registry serves it: its tarball, and the dist fields its metadata gives.
type npmPackage struct {
	name, version, file string
	tarball             []byte
	dist                map[string]string
}

func integrity(body []byte) string {
	sum := sha512.Sum512(body)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func shasum(body []byte) string {
	sum := sha1.Sum(body)
	return hex.EncodeToString(sum[:])
}

func newPackage(name, file string, tarball []byte) npmPackage {
	return npmPackage{name: name, version: "4.17.21", file: file, tarball: tarball,
		dist: map[string]string{"integrity": integrity(tarball), "shasum": shasum(tarball)}}
}

// npmUpstream serves each package's metadata and the tarball it holds, which may differ from what the metadata names,
// counting each request by its unescaped path.
type npmUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	asked map[string]int
}

func newNPMUpstream(t *testing.T, packages ...npmPackage) *npmUpstream {
	t.Helper()
	u := &npmUpstream{asked: map[string]int{}}
	bodies := map[string][]byte{}
	for _, p := range packages {
		tarballPath := "/" + p.name + "/-/" + p.file
		dist := map[string]any{"tarball": "https://registry.npmjs.org" + tarballPath}
		for k, v := range p.dist {
			dist[k] = v
		}
		metadata, err := json.Marshal(map[string]any{"name": p.name, "versions": map[string]any{p.version: map[string]any{"version": p.version, "dist": dist}}})
		require.NoError(t, err)
		bodies[tarballPath], bodies["/"+p.name] = p.tarball, metadata
	}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.asked[r.URL.Path]++
		u.mu.Unlock()
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *npmUpstream) times(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.asked[path]
}

func npmCache(t *testing.T, root string, upstream *npmUpstream) *httptest.Server {
	t.Helper()
	store, err := packagecache.NewStore(root, time.Now)
	require.NoError(t, err)
	cache := httptest.NewServer(packagecache.NPMProxy{Upstream: upstream.URL, Store: store, Client: http.DefaultClient})
	t.Cleanup(cache.Close)
	return cache
}

func TestNPMProxy_ServesATarballMatchingItsIntegrityAndServesItASecondTimeFromItsOwnDisk(t *testing.T) {
	for _, p := range []npmPackage{
		newPackage("lodash", "lodash-4.17.21.tgz", []byte("lodash tarball")),
		newPackage("@types/node", "node-4.17.21.tgz", []byte("scoped tarball")),
	} {
		t.Run(p.name, func(t *testing.T) {
			upstream := newNPMUpstream(t, p)
			cache := npmCache(t, t.TempDir(), upstream)
			path := "/" + p.name + "/-/" + p.file

			for round := 1; round <= 2; round++ {
				status, body := get(t, cache.URL+path)

				assert.Equal(t, http.StatusOK, status, "GET %s on round %d: %s", path, round, body)
				assert.Equal(t, p.tarball, body, "the tarball the cache serves on round %d", round)
			}
			assert.Equal(t, 1, upstream.times(path), "the upstream fetches of the tarball")
		})
	}
}

func TestNPMProxy_RefusesAndStoresNothingOfATarballThatDiffersFromItsMetadataOrThatItsMetadataDoesNotName(t *testing.T) {
	genuine := []byte("genuine tarball")
	altered := []byte("altered tarball")
	for _, tc := range []struct {
		name string
		pkg  npmPackage
		path string
	}{
		{"a tarball whose sha512 differs from its integrity",
			npmPackage{name: "left-pad", version: "4.17.21", file: "left-pad-4.17.21.tgz", tarball: altered,
				dist: map[string]string{"integrity": integrity(genuine), "shasum": shasum(altered)}}, "/left-pad/-/left-pad-4.17.21.tgz"},
		{"a tarball whose sha1 differs from the shasum of metadata naming no integrity",
			npmPackage{name: "left-pad", version: "4.17.21", file: "left-pad-4.17.21.tgz", tarball: altered,
				dist: map[string]string{"shasum": shasum(genuine)}}, "/left-pad/-/left-pad-4.17.21.tgz"},
		{"a tarball of metadata naming neither digest",
			npmPackage{name: "left-pad", version: "4.17.21", file: "left-pad-4.17.21.tgz", tarball: altered,
				dist: map[string]string{}}, "/left-pad/-/left-pad-4.17.21.tgz"},
		{"a tarball no version of the package's metadata names",
			newPackage("left-pad", "left-pad-4.17.21.tgz", altered), "/left-pad/-/left-pad-9.9.9.tgz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cache := npmCache(t, root, newNPMUpstream(t, tc.pkg))

			status, body := get(t, cache.URL+tc.path)

			assert.Equal(t, http.StatusBadGateway, status, "GET %s: %s", tc.path, body)
			assert.Contains(t, string(body), tc.pkg.name, "the refusal names the package")
			assertHoldsNone(t, root, altered)
			again, _ := get(t, cache.URL+tc.path)
			assert.Equal(t, http.StatusBadGateway, again, "a second GET of a refused tarball serves nothing from disk")
		})
	}
}

func TestNPMProxy_PassesThePackageMetadataThroughUnchangedEachTime(t *testing.T) {
	p := newPackage("@types/node", "node-4.17.21.tgz", []byte("scoped tarball"))
	upstream := newNPMUpstream(t, p)
	cache := npmCache(t, t.TempDir(), upstream)
	_, want := get(t, upstream.URL+"/@types%2fnode")

	for round := 1; round <= 2; round++ {
		status, body := get(t, cache.URL+"/@types%2fnode")

		assert.Equal(t, http.StatusOK, status, "GET /@types%%2fnode on round %d", round)
		assert.JSONEq(t, string(want), string(body), "the metadata the cache passes on round %d", round)
	}
	assert.Equal(t, 3, upstream.times("/@types/node"), "the metadata is asked of the upstream each time, since a publish changes it")
}

func TestNPMProxy_AnswersItsHealthWithoutTheUpstream(t *testing.T) {
	upstream := newNPMUpstream(t)
	cache := npmCache(t, t.TempDir(), upstream)
	upstream.Close()

	status, _ := get(t, cache.URL+"/healthz")

	assert.Equal(t, http.StatusOK, status, "GET /healthz")
}
