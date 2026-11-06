package client

import (
	"errors"
	"io/fs"
	"path"
	"strconv"
	"time"
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

// savedKeep is how long a time directory of .syncsaved stays: a discarded .git/index would pile up otherwise.
const savedKeep = 30 * 24 * time.Hour

// cleanSaved removes the time directories of .syncsaved that are older than savedKeep (whole scans only).
func (f *Folder) cleanSaved() {
	ents, err := f.FS.ReadDir(savedName)
	if err != nil {
		return
	}
	for _, e := range ents {
		if t, err := time.ParseInLocation("20060102-150405", e.Name, f.Now().Location()); err == nil && f.Now().Sub(t) > savedKeep {
			f.removeTree(path.Join(savedName, e.Name))
		}
	}
	if left, err := f.FS.ReadDir(savedName); err == nil && len(left) == 0 {
		f.FS.Remove(savedName)
	}
}

// removeTree removes p and everything below it; what cannot be removed stays for the next scan.
func (f *Folder) removeTree(p string) {
	if ents, err := f.FS.ReadDir(p); err == nil {
		for _, e := range ents {
			f.removeTree(path.Join(p, e.Name))
		}
	}
	f.FS.Remove(p)
}
