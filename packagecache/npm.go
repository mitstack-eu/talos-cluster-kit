package packagecache

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

// NPMProxy serves the npm registry protocol in front of Upstream, keeping a tarball only once it matches the digest its
// package's metadata names and passing every other request through, since a publish changes the metadata.
type NPMProxy struct {
	Upstream string
	Store    *Store
	Client   *http.Client
}

func (p NPMProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == healthPath {
		answerHealthy(w)
		return
	}
	name, ok := tarballPackage(r.URL.Path)
	if !ok || r.Method != http.MethodGet {
		passThrough(w, r, p.Upstream, p.Client)
		return
	}
	p.serveChecked(w, r, name)
}

// tarballPackage is the package, scoped or not, whose tarball /<name>/-/<file>.tgz is.
func tarballPackage(path string) (string, bool) {
	name, file, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/-/")
	if !ok || name == "" || strings.HasPrefix(name, "-") || strings.Contains(file, "/") || !strings.HasSuffix(file, ".tgz") {
		return "", false
	}
	return name, true
}

// npmDist is what a version's metadata says of its tarball.
type npmDist struct {
	Tarball   string `json:"tarball"`
	Integrity string `json:"integrity"`
	Shasum    string `json:"shasum"`
}

func (p NPMProxy) serveChecked(w http.ResponseWriter, r *http.Request, name string) {
	const tarballType = "application/octet-stream"
	key := "npm" + r.URL.Path
	if entry, ok := p.Store.Open(key); ok {
		serveEntry(w, r, entry, tarballType)
		return
	}
	refuse := func(err error) {
		msg := fmt.Sprintf("the npm cache refuses the tarball %q of %q: %v", r.URL.Path, name, err)
		log.Print(msg)
		http.Error(w, msg, http.StatusBadGateway)
	}
	dist, answered, err := p.dist(w, r, name)
	switch {
	case answered:
		return
	case err != nil:
		refuse(err)
		return
	}
	resp, err := fetch(r, p.Client, p.Upstream, r.URL.EscapedPath())
	if err != nil {
		refuse(err)
		return
	}
	defer resp.Body.Close()
	if relayed(w, resp) {
		return
	}
	if err := p.Store.Put(key, resp.Body, func(file string) error { return checkTarball(file, dist) }); err != nil {
		refuse(err)
		return
	}
	entry, ok := p.Store.Open(key)
	if !ok {
		refuse(errors.New("the entry it stored is gone"))
		return
	}
	serveEntry(w, r, entry, tarballType)
}

// dist reads the package's metadata for the version whose tarball r asks for, answering w itself when the upstream
// does not answer 200.
func (p NPMProxy) dist(w http.ResponseWriter, r *http.Request, name string) (npmDist, bool, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimSuffix(p.Upstream, "/")+"/"+strings.Replace(name, "/", "%2f", 1), nil)
	if err != nil {
		return npmDist{}, false, err
	}
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8")
	resp, err := p.Client.Do(req)
	if err != nil {
		return npmDist{}, false, fmt.Errorf("its metadata: %w", err)
	}
	defer resp.Body.Close()
	if relayed(w, resp) {
		return npmDist{}, true, nil
	}
	var metadata struct {
		Versions map[string]struct {
			Dist npmDist `json:"dist"`
		} `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return npmDist{}, false, fmt.Errorf("its metadata: %w", err)
	}
	for _, v := range metadata.Versions {
		if u, err := url.Parse(v.Dist.Tarball); err == nil && u.Path == r.URL.Path {
			return v.Dist, false, nil
		}
	}
	return npmDist{}, false, errors.New("no version of its metadata names the tarball")
}

// integrityHashes are the algorithms a Subresource Integrity string names that npm writes, strongest first.
var integrityHashes = []struct {
	name string
	new  func() hash.Hash
}{{"sha512", sha512.New}, {"sha384", sha512.New384}, {"sha256", sha256.New}, {"sha1", sha1.New}}

// checkTarball refuses the tarball at file unless it matches the strongest digest of dist's integrity, or its shasum
// where the metadata names no integrity, as npm itself checks it.
func checkTarball(file string, dist npmDist) error {
	for _, alg := range integrityHashes {
		var want []string
		for _, token := range strings.Fields(dist.Integrity) {
			if digest, ok := strings.CutPrefix(token, alg.name+"-"); ok {
				want = append(want, digest)
			}
		}
		if len(want) == 0 {
			continue
		}
		got, err := fileDigest(file, alg.new())
		if err != nil {
			return err
		}
		if !slices.Contains(want, base64.StdEncoding.EncodeToString(got)) {
			return fmt.Errorf("its %s is not the integrity %s its metadata names", alg.name, dist.Integrity)
		}
		return nil
	}
	if dist.Shasum == "" {
		return errors.New("its metadata names no integrity or shasum for it")
	}
	got, err := fileDigest(file, sha1.New())
	if err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(got), dist.Shasum) {
		return fmt.Errorf("its sha1 is not the shasum %s its metadata names", dist.Shasum)
	}
	return nil
}

func fileDigest(file string, h hash.Hash) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
