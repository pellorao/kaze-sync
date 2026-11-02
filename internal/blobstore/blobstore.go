// Package blobstore stores file contents addressed by their SHA-256.
package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// refreshAge is the age from which Use refreshes a block's modification time. It must be well below
// the minAge of SweepBlobs: a block that is older is refreshed before it is referenced again.
const refreshAge = 10 * time.Minute

var ErrHashMismatch = errors.New("blobstore: content does not match hash")

type Hash [32]byte

// ParseHash parses 64 lowercase hexadecimal digits.
func ParseHash(s string) (Hash, error) {
	var h Hash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(h) || s != hex.EncodeToString(b) {
		return h, fmt.Errorf("blobstore: invalid hash %q", s)
	}
	copy(h[:], b)
	return h, nil
}

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

func HashReader(r io.Reader) (Hash, int64, error) {
	var h Hash
	d := sha256.New()
	n, err := io.Copy(d, r)
	d.Sum(h[:0])
	return h, n, err
}

type Store struct {
	dir string
	mu  sync.RWMutex // Put and Use hold it shared; SweepBlobs holds it exclusively while it removes one blob
}

func Open(dir string) (*Store, error) {
	for _, sub := range []string{"blobs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir}, nil
}

func (s *Store) blobPath(h Hash) string {
	x := h.String()
	return filepath.Join(s.dir, "blobs", x[:2], x[2:4], x)
}

func (s *Store) Has(h Hash) (bool, error) {
	_, err := os.Stat(s.blobPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Use reports whether a block exists and, when it is old, refreshes its modification time, so that a
// sweep cannot remove it between now and the commit that references it (the grace period of SweepBlobs).
func (s *Store) Use(h Hash) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.blobPath(h)
	fi, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err == nil && time.Since(fi.ModTime()) > refreshAge {
		now := time.Now()
		err = os.Chtimes(p, now, now)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
	}
	return err == nil, err
}

// Put reads r to the end, verifies it hashes to h, and stores it read-only.
// Concurrent Puts of the same hash all succeed and leave one copy.
func (s *Store) Put(h Hash, r io.Reader) error {
	f, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "put-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	d := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, d), r); err != nil {
		return err
	}
	if Hash(d.Sum(nil)) != h {
		return ErrHashMismatch
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o444); err != nil {
		return err
	}
	dst := s.blobPath(h)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	s.mu.RLock() // a sweep must not remove the old copy of dst after it was judged, and thus the new one
	defer s.mu.RUnlock()
	if err := os.Rename(f.Name(), dst); err != nil {
		// Windows cannot replace the read-only copy a concurrent Put just stored.
		if ok, _ := s.Has(h); ok {
			return nil
		}
		return err
	}
	return nil
}

func (s *Store) Open(h Hash) (*os.File, error) { return os.Open(s.blobPath(h)) }

// SweepBlobs removes the blobs whose first hash byte is in [lo, hi), for which
// keep returns false and that are older than minAge. It returns the number removed.
// Splitting the hash space lets a caller hold only part of the referenced hashes
// in memory at a time.
func (s *Store) SweepBlobs(lo, hi int, keep func(Hash) bool, minAge time.Duration) (removed int, err error) {
	now := time.Now()
	for b := lo; b < hi; b++ {
		root := filepath.Join(s.dir, "blobs", fmt.Sprintf("%02x", b))
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == root && errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			if h, perr := ParseHash(d.Name()); perr != nil || now.Sub(fi.ModTime()) <= minAge || keep(h) {
				return nil
			}
			s.mu.Lock() // look again: a Put or Use may have renewed the blob since it was judged
			defer s.mu.Unlock()
			if fi, err = os.Lstat(p); err != nil || time.Since(fi.ModTime()) <= minAge {
				return nil
			}
			removed++
			return os.Remove(p)
		})
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// SweepTmp removes temp files of interrupted Puts older than maxAge.
func (s *Store) SweepTmp(maxAge time.Duration) (removed int, err error) {
	ents, err := os.ReadDir(filepath.Join(s.dir, "tmp"))
	if err != nil {
		return 0, err
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && !e.IsDir() && time.Since(fi.ModTime()) > maxAge {
			if err := os.Remove(filepath.Join(s.dir, "tmp", e.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}
