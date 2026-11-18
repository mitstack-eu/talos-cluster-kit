package robotcluster

import (
	"crypto/sha256"
	"hash/maphash"
	"strconv"
	"strings"
	"sync"

	"github.com/mitstack-eu/block-kit/capabilities/github/landing"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
)

// renderingsKept bounds the renderings kept however many clusters one operator serves.
const renderingsKept = 64

var renderingSeeds = [2]maphash.Seed{maphash.MakeSeed(), maphash.MakeSeed()}

type renderingID [len(renderingSeeds)]uint64

type renderingCache struct {
	mu    sync.Mutex
	kept  map[renderingID]*Rendering
	order []renderingID
}

// Rendering is a release's blob SHA for each path, with the last answers Retired and ReadBack gave for it.
type Rendering struct {
	blobs map[string]string

	mu     sync.Mutex
	judged *retirement
	read   *reportReading
}

// Blobs is the release's blob SHA for each path it writes.
func (r *Rendering) Blobs() map[string]string { return r.blobs }

type retirement struct {
	listing [sha256.Size]byte
	retired []string
}

type reportReading struct {
	report  [sha256.Size]byte
	reading rollout.Reading
}

// RenderingOf renders and hashes release's files for repository only when no rendering of the same bytes is kept.
func (k *Cluster) RenderingOf(release rollout.Release, repository string) (*Rendering, error) {
	key := renderingKey(release, repository)
	if r := k.renderings.get(key); r != nil {
		return r, nil
	}
	files, err := release.Files(repository)
	if err != nil {
		return nil, err
	}
	r := &Rendering{blobs: make(map[string]string, len(files))}
	for _, f := range files {
		r.blobs[f.Path] = landing.BlobHash(f.Bytes)
	}
	return k.renderings.keep(key, r), nil
}

// renderingKey hashes repository and every field of release and its components, all Files renders from but the kit's
// templates, with two seeded maphash hashes, since every call computes it and only the kit's rendering reaches it.
func renderingKey(release rollout.Release, repository string) renderingID {
	var key renderingID
	for i, seed := range renderingSeeds {
		var h maphash.Hash
		h.SetSeed(seed)
		writeField(&h, []byte(repository))
		writeField(&h, []byte(release.ToolsImage))
		for _, m := range release.Components {
			writeField(&h, []byte(m.Name))
			writeField(&h, []byte(m.Path))
			writeField(&h, m.Bytes)
		}
		key[i] = h.Sum64()
	}
	return key
}

// writeField writes field after its length, so where one field ends and the next begins is never ambiguous.
func writeField(h *maphash.Hash, field []byte) {
	_, _ = h.WriteString(strconv.Itoa(len(field)) + ":")
	_, _ = h.Write(field)
}

func (c *renderingCache) get(key renderingID) *Rendering {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kept[key]
}

func (c *renderingCache) keep(key renderingID, r *Rendering) *Rendering {
	c.mu.Lock()
	defer c.mu.Unlock()
	if kept, ok := c.kept[key]; ok {
		return kept
	}
	if len(c.order) == renderingsKept {
		delete(c.kept, c.order[0])
		c.order = c.order[1:]
	}
	c.kept[key] = r
	c.order = append(c.order, key)
	return r
}

// retiredFrom is what Retired names of held, main's listed paths, asked of release again only for another listing.
func (r *Rendering) retiredFrom(release rollout.Release, held []string) ([]string, error) {
	listing := sha256.Sum256([]byte(strings.Join(held, "\x00")))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.judged != nil && r.judged.listing == listing {
		return r.judged.retired, nil
	}
	retired, err := release.Retired(held)
	if err != nil {
		return nil, err
	}
	r.judged = &retirement{listing: listing, retired: retired}
	return retired, nil
}

// readingOf is what ReadBack reads of report, read again only for another report.
func (r *Rendering) readingOf(release rollout.Release, report []byte) (rollout.Reading, error) {
	digest := sha256.Sum256(report)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.read != nil && r.read.report == digest {
		return r.read.reading, nil
	}
	read, err := release.ReadBack(report)
	if err != nil {
		return rollout.Reading{}, err
	}
	r.read = &reportReading{report: digest, reading: read}
	return read, nil
}
