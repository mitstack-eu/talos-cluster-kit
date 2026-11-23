// A cache at its size or age limit loses its least recently used entries and keeps the rest.
package packagecache_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/packagecache"
)

var sweptAt = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// entry is a file of size bytes under a cache's directory, last used idle before sweptAt.
type entry struct {
	name string
	size int
	idle time.Duration
}

func writeEntries(t *testing.T, dir string, entries []entry) {
	t.Helper()
	for _, e := range entries {
		path := filepath.Join(dir, e.name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, make([]byte, e.size), 0o644))
		used := sweptAt.Add(-e.idle)
		require.NoError(t, os.Chtimes(path, used, used))
	}
}

func remaining(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		names = append(names, filepath.ToSlash(rel))
		return err
	}))
	slices.Sort(names)
	return names
}

func TestEvict_RemovesTheEntriesPastTheCachesSizeOrAgeOldestUseFirstAndKeepsTheRest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []entry
		limits  packagecache.Limits
		kept    []string
	}{
		{"a cache under both limits keeps every entry",
			[]entry{{"a", 100, time.Hour}, {"b/c", 100, 2 * time.Hour}},
			packagecache.Limits{MaxBytes: 1000, MaxAge: 24 * time.Hour}, []string{"a", "b/c"}},
		{"an entry unused past the age limit goes while the newer entries stay",
			[]entry{{"old", 10, 49 * time.Hour}, {"new", 10, time.Hour}, {"deep/older", 10, 72 * time.Hour}},
			packagecache.Limits{MaxBytes: 1000, MaxAge: 48 * time.Hour}, []string{"new"}},
		{"a cache past its size loses its least recently used entries until it fits",
			[]entry{{"lru", 400, 5 * time.Hour}, {"mid", 400, 3 * time.Hour}, {"mru", 400, time.Minute}},
			packagecache.Limits{MaxBytes: 900, MaxAge: 48 * time.Hour}, []string{"mid", "mru"}},
		{"a cache past its size after the aged entries go loses the oldest of the rest too",
			[]entry{{"aged", 400, 50 * time.Hour}, {"lru", 400, 5 * time.Hour}, {"mid", 400, 3 * time.Hour}, {"mru", 400, time.Minute}},
			packagecache.Limits{MaxBytes: 500, MaxAge: 48 * time.Hour}, []string{"mru"}},
		{"an entry larger than the cache goes while the older entries that fit stay",
			[]entry{{"huge", 2000, time.Minute}, {"small", 100, time.Hour}, {"smaller", 50, 2 * time.Hour}},
			packagecache.Limits{MaxBytes: 1000, MaxAge: 48 * time.Hour}, []string{"small", "smaller"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeEntries(t, dir, tc.entries)

			swept, err := packagecache.Evict(dir, tc.limits, sweptAt)

			require.NoError(t, err)
			assert.Equal(t, tc.kept, remaining(t, dir), "the entries Evict(%+v) keeps", tc.limits)
			assert.Equal(t, len(tc.entries)-len(tc.kept), swept.Removed, "the entries Evict reports removed")
			assert.LessOrEqual(t, swept.Kept, tc.limits.MaxBytes, "the bytes the cache holds after the sweep")
		})
	}
}

func TestEvict_ReadsADirectoryNotYetWrittenAsAnEmptyCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docker", "registry", "v2", "blobs")

	swept, err := packagecache.Evict(dir, packagecache.Limits{MaxBytes: 1, MaxAge: time.Hour}, sweptAt)

	require.NoError(t, err)
	assert.Equal(t, packagecache.Swept{}, swept, "a sweep of a cache that holds nothing yet")
}

func TestEvict_RefusesLimitsThatWouldEmptyOrNeverBoundTheCache(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits packagecache.Limits
	}{
		{"no size limit", packagecache.Limits{MaxAge: time.Hour}},
		{"no age limit", packagecache.Limits{MaxBytes: 1 << 30}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeEntries(t, dir, []entry{{"kept", 10, time.Minute}})

			_, err := packagecache.Evict(dir, tc.limits, sweptAt)

			assert.Error(t, err, "Evict(%+v)", tc.limits)
			assert.Equal(t, []string{"kept"}, remaining(t, dir), "a refused sweep removes nothing")
		})
	}
}

func TestStore_AHitMarksItsEntryUsedSoTheSweepTakesTheUnusedEntryFirst(t *testing.T) {
	now := sweptAt.Add(-time.Hour)
	store, err := packagecache.NewStore(t.TempDir(), func() time.Time { return now })
	require.NoError(t, err)
	for _, key := range []string{"first", "second"} {
		require.NoError(t, store.Put(key, bytes.NewReader(make([]byte, 400)), func(string) error { return nil }))
		now = now.Add(time.Minute)
	}
	now = now.Add(time.Minute)
	hit, ok := store.Open("first")
	require.True(t, ok, "the entry first was stored")
	require.NoError(t, hit.Close())

	_, err = packagecache.Evict(store.Dir(), packagecache.Limits{MaxBytes: 500, MaxAge: 24 * time.Hour}, sweptAt)

	require.NoError(t, err)
	_, firstKept := store.Open("first")
	_, secondKept := store.Open("second")
	assert.True(t, firstKept, "the entry read last stays")
	assert.False(t, secondKept, "the entry unread since its write goes")
}

func TestStore_KeepsNothingOfAnEntryItsCheckRefuses(t *testing.T) {
	root := t.TempDir()
	store, err := packagecache.NewStore(root, time.Now)
	require.NoError(t, err)
	refused := assert.AnError

	err = store.Put("module.zip", bytes.NewReader([]byte("altered")), func(string) error { return refused })

	assert.ErrorIs(t, err, refused, "Put answers the check's refusal")
	_, ok := store.Open("module.zip")
	assert.False(t, ok, "a refused entry is not stored")
	assert.Empty(t, remaining(t, root), "the files the cache holds after a refused write")
}
