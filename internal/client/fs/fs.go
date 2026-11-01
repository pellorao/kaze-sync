// Package fs is the file system the sync engine works on: the real one (OS),
// and an in-memory double with fault injection for tests (Mem).
package fs

import (
	"errors"
	"io"
)

// ErrLocked is wrapped by errors for files another process holds open without
// sharing (Windows sharing and lock violations).
var ErrLocked = errors.New("file is locked by another process")

type Entry struct {
	Name      string // exact name as stored; for Stat, the last element of the argument
	IsDir     bool
	IsSymlink bool // symbolic link or Windows junction
	Size      int64
	MtimeNs   int64
	Exec      bool
}

// FS paths are relative to the root, separated by "/"; "" is the root.
type FS interface {
	ReadDir(dir string) ([]Entry, error) // exact names, sorted
	Stat(p string) (Entry, error)        // does not follow symlinks
	Open(p string) (io.ReadCloser, error)
	OpenAppend(p string) (io.WriteCloser, int64, error) // creates p; returns the existing length
	Sync(p string) error
	Rename(oldp, newp string) error // replaces an existing target
	Remove(p string) error          // a file or an empty directory
	MkdirAll(p string) error
	SetMeta(p string, mtimeNs int64, exec bool) error
}
