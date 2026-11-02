package server

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kaze-sync/internal/blobstore"
)

// ExportOptions selects what Export writes: Folder is a name or id ("": every folder, each in a
// subdirectory named after it), At the point in time (zero: now).
type ExportOptions struct {
	Data, Dir, Folder string
	At                time.Time
}

// Export writes the tree of folders as plain files (design 7, disaster recovery and moving out). It
// reads the databases and blocks directly, so it works with the service stopped and on a restored backup.
// A file whose blocks are missing or corrupt is reported and skipped; the others are still written.
func (in *Installer) Export(o ExportOptions) error {
	state := filepath.Join(o.Data, VarDir)
	db, err := OpenDB(filepath.Join(state, DBFile), false)
	if err != nil {
		return err
	}
	defer db.Close()
	blobs, err := blobstore.Open(state)
	if err != nil {
		return err
	}
	type folder struct {
		id   int64
		name string
	}
	all, err := queryAll(db, func(rows *sql.Rows, f *folder) error { return rows.Scan(&f.id, &f.name) }, `SELECT id, name FROM folders ORDER BY id`)
	if err != nil {
		return err
	}
	var at int64
	if !o.At.IsZero() {
		at = o.At.Unix()
	}
	bad, written, matched := 0, 0, false
	for _, f := range all {
		if o.Folder != "" && o.Folder != f.name && o.Folder != strconv.FormatInt(f.id, 10) {
			continue
		}
		matched = true
		root := o.Dir
		if o.Folder == "" {
			root = filepath.Join(o.Dir, f.name)
			if !filepath.IsLocal(f.name) || strings.ContainsAny(f.name, `/\`) {
				root = filepath.Join(o.Dir, "folder-"+strconv.FormatInt(f.id, 10))
			}
		}
		fdb, err := OpenDB(FolderDBPath(state, f.id), false)
		if err != nil {
			return err
		}
		err = stateAt(fdb, "", at, func(r *fileRow) error {
			if err := exportFile(blobs, root, r); err != nil {
				slog.Error("export", "folder", f.name, "path", r.path, "err", err)
				bad++
			} else {
				written++
			}
			return nil
		})
		fdb.Close()
		if err != nil {
			return err
		}
	}
	if o.Folder != "" && !matched {
		return fmt.Errorf("no folder %q", o.Folder)
	}
	if _, _, err := in.Lookup(serviceName); err == nil {
		in.chownData(o.Data) // the databases opened by root may have left -wal and -shm files behind
	}
	in.say("exported %d files to %s", written, o.Dir)
	if bad > 0 {
		return fmt.Errorf("%d files could not be exported (see above)", bad)
	}
	return nil
}

// exportFile writes one file from its blocks and checks its hash.
func exportFile(blobs *blobstore.Store, root string, r *fileRow) (err error) {
	if !filepath.IsLocal(filepath.FromSlash(r.path)) {
		return fmt.Errorf("unsafe path")
	}
	dst := filepath.Join(root, filepath.FromSlash(r.path))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if r.exec {
		mode = 0o755
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(dst)
		}
	}()
	sum := sha256.New()
	w := io.MultiWriter(f, sum)
	for _, b := range splitBlocks(r.blocks) {
		src, err := blobs.Open(blobstore.Hash(b))
		if err != nil {
			f.Close()
			return err
		}
		_, err = io.Copy(w, src)
		src.Close()
		if err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if string(sum.Sum(nil)) != string(r.hash) {
		return fmt.Errorf("content does not match its hash")
	}
	if err := os.Chmod(dst, mode); err != nil {
		return err
	}
	return os.Chtimes(dst, time.Unix(0, r.mtimeNs), time.Unix(0, r.mtimeNs))
}
