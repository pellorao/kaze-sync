// Package place writes downloaded content into the sync tree atomically.
package place

import (
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"path"

	"kaze-sync/internal/blobstore"
	cfs "kaze-sync/internal/client/fs"
)

var (
	ErrTargetChanged = errors.New("place: target changed")
	ErrHashMismatch  = blobstore.ErrHashMismatch
	ErrLocked        = cfs.ErrLocked
)

// Expect is the state the target must still be in before it is replaced.
type Expect struct {
	Exists        bool
	Size, MtimeNs int64
}

type Meta struct {
	MtimeNs int64
	Exec    bool
}

// Place writes the content with hash h to dst: it appends to (or creates) the
// temporary file .syncpart-<first 16 hex digits> next to dst, verifies the hash,
// syncs, sets mtime and execute bit, checks that dst is still as expected and
// renames the temporary file over it. open(offset) returns the content from
// offset on. After a failure the temporary file is kept for resuming, except
// after ErrHashMismatch.
func Place(fsys cfs.FS, dst string, h blobstore.Hash, open func(offset int64) (io.ReadCloser, error), m Meta, expect Expect) error {
	s, err := Stage(fsys, dst, h, open, m, expect, "", false)
	if err != nil {
		return err
	}
	return s.Finish()
}

// Staged is a downloaded temporary file that still needs its fsync, metadata and rename (Finish). Staging a
// batch of files first and finishing them together lets their fsyncs overlap, which on a disk that takes
// milliseconds per fsync is most of the time of a download of many small files.
type Staged struct {
	fsys     cfs.FS
	tmp, dst string
	h        blobstore.Hash
	m        Meta
	expect   Expect
	hashed   bool // the content was hashed while it was written
}

// Stage is the first half of Place: it writes the content to the temporary file and, when it wrote all of it,
// verifies the hash. A non-empty suffix is added to the temporary name, for two files of one directory with
// the same content that are staged at once (their names would be the same). With fresh, a temporary file left
// by an earlier attempt is dropped and the content always starts at offset 0 (open is called with 0).
func Stage(fsys cfs.FS, dst string, h blobstore.Hash, open func(offset int64) (io.ReadCloser, error), m Meta, expect Expect, suffix string, fresh bool) (*Staged, error) {
	dir := path.Dir(dst)
	if dir != "." {
		if err := fsys.MkdirAll(dir); err != nil {
			return nil, err
		}
	}
	tmp := path.Join(dir, ".syncpart-"+h.String()[:16]+suffix)
	if fresh {
		fsys.Remove(tmp)
	}

	w, offset, err := fsys.OpenAppend(tmp)
	if err != nil {
		return nil, err
	}
	hasher := sha256.New()
	if err := copyFrom(w, open, offset, hasher); err != nil {
		return nil, err
	}
	s := &Staged{fsys: fsys, tmp: tmp, dst: dst, h: h, m: m, expect: expect, hashed: offset == 0}
	if s.hashed && blobstore.Hash(hasher.Sum(nil)) != h {
		fsys.Remove(tmp)
		return nil, ErrHashMismatch
	}
	return s, nil
}

// Finish is the second half of Place: it syncs the temporary file, sets mtime and execute bit, checks that the
// target is still as expected and renames the file over it. After a failure the temporary file is kept, except
// after ErrHashMismatch. Calls for different files may run at the same time.
func (s *Staged) Finish() error {
	fsys, tmp := s.fsys, s.tmp
	if err := fsys.Sync(tmp); err != nil {
		return err
	}
	if !s.hashed { // resumed: the part written before was not hashed
		r, err := fsys.Open(tmp)
		if err != nil {
			return err
		}
		got, _, err := blobstore.HashReader(r)
		r.Close()
		if err != nil {
			return err
		}
		if got != s.h {
			fsys.Remove(tmp)
			return ErrHashMismatch
		}
	}
	if err := fsys.SetMeta(tmp, s.m.MtimeNs, s.m.Exec); err != nil {
		return err
	}

	cur, err := fsys.Stat(s.dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if s.expect.Exists {
			return ErrTargetChanged
		}
	case err != nil:
		return err
	case !s.expect.Exists || cur.Size != s.expect.Size || cur.MtimeNs != s.expect.MtimeNs:
		return ErrTargetChanged
	}
	return fsys.Rename(tmp, s.dst)
}

// copyFrom appends the content from offset on to w, which it closes; from offset 0 the content is also written to h.
func copyFrom(w io.WriteCloser, open func(int64) (io.ReadCloser, error), offset int64, h io.Writer) error {
	r, err := open(offset)
	if err != nil {
		w.Close()
		return err
	}
	var dst io.Writer = w
	if offset == 0 {
		dst = io.MultiWriter(w, h)
	}
	_, err = io.Copy(dst, r)
	r.Close()
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

// PlaceLink makes dst a symbolic link to target: it creates the link under a temporary name next to dst, checks
// that dst is still as expected and renames the link over it (the rename replaces dst itself, a file or another
// link, and never follows it). A machine that cannot create links gets cfs.ErrLinkUnsupported.
func PlaceLink(fsys cfs.FS, dst, target string, h blobstore.Hash, expect Expect) error {
	dir := path.Dir(dst)
	if dir != "." {
		if err := fsys.MkdirAll(dir); err != nil {
			return err
		}
	}
	tmp := path.Join(dir, ".syncpart-"+h.String()[:16]+"-link")
	fsys.Remove(tmp)
	if err := fsys.Symlink(target, tmp); err != nil {
		return err
	}
	cur, err := fsys.Stat(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if expect.Exists {
			err = ErrTargetChanged
		} else {
			err = nil
		}
	case err != nil:
	case !expect.Exists || cur.Size != expect.Size || cur.MtimeNs != expect.MtimeNs:
		err = ErrTargetChanged
	default:
		err = nil
	}
	if err == nil {
		err = fsys.Rename(tmp, dst)
	}
	if err != nil {
		fsys.Remove(tmp)
	}
	return err
}
