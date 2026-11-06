package client

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// savedName is the directory in the sync root that holds the local changes refused in a read-only range (design 4).
// It is never synced.
const savedName = ".syncsaved"

// rangeOf is the read-only range that holds p, and whether there is one.
func (f *Folder) rangeOf(p string) (string, bool) {
	k := pp.Key(p)
	for _, r := range f.Readonly {
		if rk := pp.Key(r); rk == "" || k == rk || strings.HasPrefix(k, rk+"/") {
			return r, true
		}
	}
	return "", false
}

// savedPath is where the local file p goes: .syncsaved/<time of the scan>/<p>, a number added when taken.
func (f *Folder) savedPath(p string) string {
	if f.savedTS == "" {
		f.savedTS = f.Now().Format("20060102-150405")
	}
	base := path.Join(savedName, f.savedTS, p)
	for i := 0; ; i++ {
		q := base
		if i > 0 {
			q += "." + strconv.Itoa(i)
		}
		if _, err := f.guard.FS.Stat(q); errors.Is(err, fs.ErrNotExist) {
			return q
		}
	}
}

// saveAway moves the local file p to .syncsaved (the same volume: a rename) and counts it for its range. It works
// on the unguarded file system: .syncsaved is not protected, and the directory p leaves is made writable for the move.
func (f *Folder) saveAway(p string) error {
	in := f.guard.FS
	dst := f.savedPath(p)
	if err := in.MkdirAll(path.Dir(dst)); err != nil {
		return err
	}
	dir := path.Dir(p)
	if dir == "." {
		dir = ""
	}
	if st, err := in.Stat(dir); err == nil && st.ReadOnly {
		in.SetWritable(dir, true)
		defer in.SetWritable(dir, false)
	}
	in.SetWritable(p, true)
	if err := in.Rename(p, dst); err != nil {
		return err
	}
	if r, ok := f.rangeOf(p); ok {
		f.savedN[r]++
	}
	return nil
}

// rescue handles the local changes the server refuses to take, or would overwrite, in a read-only range (design 4):
// the new or changed file goes to .syncsaved, and the server's version comes back with the next Pull. A deletion
// needs no move. Nothing is touched while the server cannot be reached.
func (f *Folder) rescue(ctx context.Context, items []*item) error {
	if len(items) == 0 {
		return nil
	}
	if _, err := f.API.Changes(ctx, f.ID, f.DB.metaInt("cursor")); err != nil {
		return err
	}
	var forget []string
	for _, it := range items {
		if !it.del {
			if err := f.saveAway(it.path); err != nil {
				kind := proto.KindIO
				if errors.Is(err, cfs.ErrLocked) {
					kind = proto.KindInUse
				}
				f.note(it.path, kind, err.Error())
				f.retry = true
				continue
			}
		}
		it.done = true
		forget = append(forget, it.path)
	}
	if len(forget) == 0 {
		return nil
	}
	if err := f.DB.apply(nil, forget); err != nil {
		return err
	}
	f.retry = true
	f.seen = -1
	return f.DB.setMeta("cursor", 0) // the server's versions come again through Pull
}

// countSaved recounts the files in .syncsaved per range; only a whole scan does it.
func (f *Folder) countSaved() {
	f.savedN = map[string]int{}
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		ents, err := f.guard.FS.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			q, r := path.Join(dir, e.Name), path.Join(rel, e.Name)
			switch {
			case e.IsDir && !e.IsLink:
				walk(q, r)
			case rel != "": // not the time directory itself
				if rg, ok := f.rangeOf(r); ok {
					f.savedN[rg]++
				}
			}
		}
	}
	ents, err := f.guard.FS.ReadDir(savedName)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir {
			walk(path.Join(savedName, e.Name), "")
		}
	}
}

// lockedErrors is the one error per range that has refused changes waiting in .syncsaved (design 4).
func (f *Folder) lockedErrors() []proto.FileError {
	var out []proto.FileError
	for _, r := range f.Readonly {
		if n := f.savedN[r]; n > 0 {
			out = append(out, proto.FileError{Path: r, Kind: proto.KindRejected, Message: "locked: " + strconv.Itoa(n) + " refused changes are in " + savedName, N: n})
		}
	}
	slices.SortFunc(out, func(a, b proto.FileError) int { return strings.Compare(a.Path, b.Path) })
	return out
}
