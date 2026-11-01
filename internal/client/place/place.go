// Package place writes downloaded content into the sync tree atomically.
package place

import (
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
	dir := path.Dir(dst)
	if dir != "." {
		if err := fsys.MkdirAll(dir); err != nil {
			return err
		}
	}
	tmp := path.Join(dir, ".syncpart-"+h.String()[:16])

	w, offset, err := fsys.OpenAppend(tmp)
	if err != nil {
		return err
	}
	if err := copyFrom(w, open, offset); err != nil {
		return err
	}
	if err := fsys.Sync(tmp); err != nil {
		return err
	}
	r, err := fsys.Open(tmp)
	if err != nil {
		return err
	}
	got, _, err := blobstore.HashReader(r)
	r.Close()
	if err != nil {
		return err
	}
	if got != h {
		fsys.Remove(tmp)
		return ErrHashMismatch
	}
	if err := fsys.SetMeta(tmp, m.MtimeNs, m.Exec); err != nil {
		return err
	}

	cur, err := fsys.Stat(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if expect.Exists {
			return ErrTargetChanged
		}
	case err != nil:
		return err
	case !expect.Exists || cur.Size != expect.Size || cur.MtimeNs != expect.MtimeNs:
		return ErrTargetChanged
	}
	return fsys.Rename(tmp, dst)
}

// copyFrom appends the content from offset on to w and closes it.
func copyFrom(w io.WriteCloser, open func(int64) (io.ReadCloser, error), offset int64) error {
	r, err := open(offset)
	if err != nil {
		w.Close()
		return err
	}
	_, err = io.Copy(w, r)
	r.Close()
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}
