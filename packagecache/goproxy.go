package packagecache

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/dirhash"
)

// GoProxy serves the Go module proxy protocol in front of Upstream. It keeps a go.mod or a zip only once the checksum
// database holds its hash, and passes the version list, a version's info and @latest through, since they change and
// no hash covers them.
type GoProxy struct {
	Upstream string
	Sums     *ChecksumDB
	Store    *Store
	Client   *http.Client
}

var goContentTypes = map[string]string{".mod": "text/plain; charset=utf-8", ".zip": "application/zip"}

func (p GoProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == healthPath {
		answerHealthy(w)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/sumdb/") {
		http.NotFound(w, r) // the go command then asks the checksum database itself
		return
	}
	escaped, file, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/@v/")
	ext := file[strings.LastIndexByte(file, '.')+1:]
	if !ok || r.Method != http.MethodGet || goContentTypes["."+ext] == "" {
		passThrough(w, r, p.Upstream, p.Client)
		return
	}
	path, errPath := module.UnescapePath(escaped)
	version, errVersion := module.UnescapeVersion(strings.TrimSuffix(file, "."+ext))
	if errPath != nil || errVersion != nil {
		http.NotFound(w, r)
		return
	}
	p.serveChecked(w, r, module.Version{Path: path, Version: version}, "."+ext)
}

func (p GoProxy) serveChecked(w http.ResponseWriter, r *http.Request, m module.Version, ext string) {
	key := "go" + r.URL.Path
	if entry, ok := p.Store.Open(key); ok {
		serveEntry(w, r, entry, goContentTypes[ext])
		return
	}
	refuse := func(err error) {
		msg := fmt.Sprintf("the Go module cache refuses the %s of %s: %v", ext, m, err)
		log.Print(msg)
		http.Error(w, msg, http.StatusBadGateway)
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
	if err := p.Store.Put(key, resp.Body, func(file string) error { return p.Sums.check(m, ext, file) }); err != nil {
		refuse(err)
		return
	}
	entry, ok := p.Store.Open(key)
	if !ok {
		refuse(fmt.Errorf("the entry it stored is gone"))
		return
	}
	serveEntry(w, r, entry, goContentTypes[ext])
}

// ChecksumDB asks a Go checksum database for a module's hashes, checking each answer against the database's signed
// tree as the go command does.
type ChecksumDB struct {
	ops    *sumdbOps
	mu     sync.Mutex
	client *sumdb.Client
}

// NewChecksumDB asks the database at url, whose signatures verifierKey checks, keeping its tiles in store.
func NewChecksumDB(url, verifierKey string, client *http.Client, store *Store) *ChecksumDB {
	ops := &sumdbOps{url: strings.TrimSuffix(url, "/"), key: verifierKey, http: client, store: store}
	return &ChecksumDB{ops: ops, client: sumdb.NewClient(ops)}
}

// check refuses the go.mod or zip at file unless its hash is the one the database holds for m.
func (db *ChecksumDB) check(m module.Version, ext, file string) error {
	version, hash, err := m.Version, "", error(nil)
	switch ext {
	case ".zip":
		hash, err = dirhash.HashZip(file, dirhash.Hash1)
	case ".mod":
		version += "/go.mod"
		hash, err = dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) { return os.Open(file) })
	}
	if err != nil {
		return fmt.Errorf("its hash: %w", err)
	}
	lines, err := db.lookup(m.Path, version)
	if err != nil {
		return fmt.Errorf("the checksum database: %w", err)
	}
	if !slices.Contains(lines, m.Path+" "+version+" "+hash) {
		return fmt.Errorf("its hash %s is not the checksum database's %q", hash, lines)
	}
	return nil
}

// lookup starts a fresh client after a failed lookup, since a client keeps each lookup's answer for its life, a
// failure too; the tree the database signed stays in ops, so the fresh client still refuses a forked one.
func (db *ChecksumDB) lookup(path, version string) ([]string, error) {
	db.mu.Lock()
	client := db.client
	db.mu.Unlock()
	lines, err := client.Lookup(path, version)
	if err != nil {
		db.mu.Lock()
		if db.client == client {
			db.client = sumdb.NewClient(db.ops)
		}
		db.mu.Unlock()
	}
	return lines, err
}

// sumdbOps keeps the database's latest signed tree in memory and its tiles in the store.
type sumdbOps struct {
	url, key string
	http     *http.Client
	store    *Store
	mu       sync.Mutex
	latest   []byte
}

func (o *sumdbOps) ReadRemote(path string) ([]byte, error) {
	resp, err := o.http.Get(o.url + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", o.url+path, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func (o *sumdbOps) ReadConfig(file string) ([]byte, error) {
	if file == "key" {
		return []byte(o.key), nil
	}
	if !strings.HasSuffix(file, "/latest") {
		return nil, fmt.Errorf("the checksum database client holds no configuration %s", file)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.latest), nil
}

func (o *sumdbOps) WriteConfig(file string, old, new []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !bytes.Equal(o.latest, old) {
		return sumdb.ErrWriteConflict
	}
	o.latest = slices.Clone(new)
	return nil
}

func (o *sumdbOps) ReadCache(file string) ([]byte, error) {
	entry, ok := o.store.Open("sumdb/" + file)
	if !ok {
		return nil, os.ErrNotExist
	}
	defer entry.Close()
	return io.ReadAll(entry)
}

// WriteCache stores what the client has checked against the signed tree already.
func (o *sumdbOps) WriteCache(file string, data []byte) {
	if err := o.store.Put("sumdb/"+file, bytes.NewReader(data), func(string) error { return nil }); err != nil {
		log.Printf("the Go module cache keeps no copy of the checksum database's %s: %v", file, err)
	}
}

func (o *sumdbOps) Log(msg string) { log.Print(msg) }

func (o *sumdbOps) SecurityError(msg string) { log.Print("the checksum database misbehaves: " + msg) }
