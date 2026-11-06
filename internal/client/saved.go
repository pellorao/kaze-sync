package client

import (
	"errors"
	"io/fs"
	"path"
	"strconv"
)

// savedName is the directory in the sync root that holds the local changes that were set aside (design 2.8).
// It is never synced.
const savedName = ".syncsaved"

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
		if _, err := f.FS.Stat(q); errors.Is(err, fs.ErrNotExist) {
			return q
		}
	}
}

// saveAway moves the local file p to .syncsaved (the same volume: a rename).
func (f *Folder) saveAway(p string) error {
	dst := f.savedPath(p)
	if err := f.FS.MkdirAll(path.Dir(dst)); err != nil {
		return err
	}
	return f.FS.Rename(p, dst)
}
