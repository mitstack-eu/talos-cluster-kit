// Package packagecache holds the runner plane's shared Go module and npm caches, which keep only what its digest
// proves, and the sweep that holds each cache's directory under its size and age limits.
package packagecache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Store keeps each entry in a file of its own under Dir, named by its key's hash, and an entry's last use in the file's
// times, which the sweep reads.
type Store struct {
	entries, incoming string
	now               func() time.Time
}

// NewStore keeps entries under root, which it creates, writing each to root's incoming directory until its check
// passes, so a refused or half-written entry never sits beside the entries. It empties the incoming directory, since
// what an earlier process left there was never checked.
func NewStore(root string, now func() time.Time) (*Store, error) {
	s := &Store{entries: filepath.Join(root, "entries"), incoming: filepath.Join(root, "incoming"), now: now}
	if err := os.RemoveAll(s.incoming); err != nil {
		return nil, fmt.Errorf("the cache's incoming directory: %w", err)
	}
	for _, dir := range []string{s.entries, s.incoming} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("the cache's directory: %w", err)
		}
	}
	return s, nil
}

// Dir is the directory the entries sit in, which the sweep evicts from.
func (s *Store) Dir() string { return s.entries }

func (s *Store) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:])
	return filepath.Join(s.entries, name[:2], name)
}

// Open opens the entry under key and marks it used now, or answers false when the store holds none.
func (s *Store) Open(key string) (*os.File, bool) {
	path := s.path(key)
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	now := s.now()
	_ = os.Chtimes(path, now, now) // a failed mark leaves the entry's older use, so the sweep may take it sooner
	return f, true
}

// Put writes r to an incoming file, hands its path to check, and stores it under key only once check passes, answering
// check's refusal otherwise.
func (s *Store) Put(key string, r io.Reader, check func(path string) error) (err error) {
	f, err := os.CreateTemp(s.incoming, "entry-")
	if err != nil {
		return fmt.Errorf("the cache's incoming file: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeMissing(f.Name()))
		}
	}()
	_, err = io.Copy(f, r)
	if err = errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("the cache's incoming file: %w", err)
	}
	if err = check(f.Name()); err != nil {
		return err
	}
	now := s.now()
	if err = os.Chtimes(f.Name(), now, now); err != nil {
		return fmt.Errorf("the cache's incoming file: %w", err)
	}
	path := s.path(key)
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("the cache's directory: %w", err)
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("the cache's entry: %w", err)
	}
	return nil
}

// removeMissing removes path, which a sweep or an earlier removal may already have taken.
func removeMissing(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
