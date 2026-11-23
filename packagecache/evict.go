package packagecache

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"time"
)

// Limits bound a cache's directory: the bytes it may hold, and how long an entry may sit unused.
type Limits struct {
	MaxBytes int64
	MaxAge   time.Duration
}

// Swept is what one sweep removed, and the bytes the directory holds after it.
type Swept struct {
	Removed int
	Kept    int64
}

type sweptFile struct {
	path     string
	size     int64
	lastUsed time.Time
}

// Evict removes each file under dir unused for longer than the age limit or larger than the size limit, then the least
// recently used of the rest until the bytes they hold fit the size limit.
func Evict(dir string, limits Limits, now time.Time) (Swept, error) {
	if limits.MaxBytes <= 0 || limits.MaxAge <= 0 {
		return Swept{}, fmt.Errorf("the sweep of %s needs a size and an age limit above zero, got %d bytes and %s", dir, limits.MaxBytes, limits.MaxAge)
	}
	var files []sweptFile
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil || d.IsDir():
			return err
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files = append(files, sweptFile{path: path, size: info.Size(), lastUsed: lastUsed(info)})
		}
		return nil
	})
	if err != nil {
		return Swept{}, fmt.Errorf("the sweep of %s: %w", dir, err)
	}
	slices.SortFunc(files, func(a, b sweptFile) int { return cmp.Compare(b.lastUsed.UnixNano(), a.lastUsed.UnixNano()) })
	var swept Swept
	full := false
	for _, f := range files {
		full = full || (f.size <= limits.MaxBytes && swept.Kept+f.size > limits.MaxBytes)
		if !full && f.size <= limits.MaxBytes && now.Sub(f.lastUsed) <= limits.MaxAge {
			swept.Kept += f.size
			continue
		}
		if err := removeMissing(f.path); err != nil {
			return swept, fmt.Errorf("the sweep of %s: %w", dir, err)
		}
		swept.Removed++
	}
	return swept, nil
}
