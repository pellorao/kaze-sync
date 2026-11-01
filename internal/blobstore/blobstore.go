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
	"time"
)

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

type Store struct{ dir string }

func Open(dir string) (*Store, error) {
	for _, sub := range []string{"blobs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{dir}, nil
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

// Sweep removes blobs for which keep returns false and that are older than
// minAge, and temp files of interrupted Puts older than partMaxAge. It returns the number removed.
func (s *Store) Sweep(keep func(Hash) bool, minAge, partMaxAge time.Duration) (removed int, err error) {
	now := time.Now()
	for _, sub := range []string{"blobs", "tmp"} {
		err = filepath.WalkDir(filepath.Join(s.dir, sub), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			age := now.Sub(fi.ModTime())
			if sub == "tmp" {
				if age <= partMaxAge {
					return nil
				}
			} else if h, perr := ParseHash(d.Name()); perr != nil || age <= minAge || keep(h) {
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
