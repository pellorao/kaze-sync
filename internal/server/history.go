package server

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"kaze-sync/internal/proto"
)

// The version history of a folder (design 7): listing, restoring and the state at a point in time. The Web UI and
// export share it.

type versionItem struct {
	ID               int64
	Time             int64
	Device           string
	Size             int64
	Current, Deleted bool
}

// versionList returns the current state of a path followed by its older versions, newest first.
func (s *Server) versionList(folder int64, file string) ([]versionItem, error) {
	db, err := s.folderDB(folder)
	if err != nil {
		return nil, err
	}
	var out []versionItem
	var cur fileRow
	if err := cur.scan(db.QueryRow(`SELECT `+fileCols+` FROM files WHERE path=?`, file)); err == nil {
		out = append(out, versionItem{Time: cur.createdAt, Device: cur.device, Size: cur.size, Current: true, Deleted: cur.hash == nil})
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	old, err := queryAll(db, func(rows *sql.Rows, v *versionItem) error {
		return rows.Scan(&v.ID, &v.Time, &v.Device, &v.Size)
	}, `SELECT id, created_at, device, size FROM versions WHERE path=? ORDER BY replaced_at DESC, id DESC`, file)
	return append(out, old...), err
}

var errNoVersion = errors.New("no such version")

// restoreError is a restore the commit refused; msg says why.
type restoreError struct{ msg string }

func (e restoreError) Error() string { return e.msg }

// restoreVersion commits an old version as the new current state, as device "web", and returns its path.
func (s *Server) restoreVersion(folder, version int64) (string, error) {
	db, err := s.folderDB(folder)
	if err != nil {
		return "", err
	}
	var v fileRow
	err = v.scan(db.QueryRow(`SELECT `+fileCols+` FROM versions WHERE id=?`, version))
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNoVersion
	} else if err != nil {
		return "", err
	}
	var base int64 // the current seq of the path, or 0 when it has never existed
	db.QueryRow(`SELECT seq FROM files WHERE path=?`, v.path).Scan(&base)
	e := v.entry()
	res, err := s.commit(folder, "web", []proto.Op{{Path: v.path, BaseSeq: base, Hash: e.Hash, Blocks: e.Blocks, Size: v.size, MtimeNs: v.mtimeNs, Exec: v.exec}})
	if err != nil {
		return "", err
	}
	if res[0].Status != proto.StatusOK {
		return "", restoreError{res[0].Status + " " + res[0].Reason}
	}
	return v.path, nil
}

// ParseTime reads a time given in UTC, or with an explicit zone, as the Web UI and export take it.
func ParseTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, strings.TrimSpace(s), time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("time %q: use YYYY-MM-DD HH:MM[:SS] in UTC", s)
}

// stateAt calls visit for every file the folder held at unix time at (0: now) at p or below it (design 7; "" is the
// whole folder): the current rows created by then and the versions whose life spans it. Deleted paths have no row.
func stateAt(db *sql.DB, p string, at int64, visit func(*fileRow) error) error {
	if at == 0 {
		at = math.MaxInt64
	}
	cond, args := "true", []any{}
	if p != "" {
		cond, args = "(path = ? OR path >= ? AND path < ?)", []any{p, p + "/", p + "0"} // "0" is the character after "/"
	}
	rows, err := db.Query(`SELECT `+fileCols+` FROM files WHERE hash IS NOT NULL AND created_at <= ? AND `+cond+
		` UNION ALL SELECT `+fileCols+` FROM versions WHERE created_at <= ? AND replaced_at > ? AND `+cond,
		append(append([]any{at}, args...), append([]any{at, at}, args...)...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f fileRow
		if err := f.scan(rows); err != nil {
			return err
		}
		if err := visit(&f); err != nil {
			return err
		}
	}
	return rows.Err()
}

// restoreDiff calls visit for every file at p or below it whose content at time at differs from the current one:
// old is what it held then, nowGone says that it has no content now (deleted or never there).
func restoreDiff(db *sql.DB, p string, at time.Time, visit func(old, cur *fileRow, nowGone bool) error) error {
	return stateAt(db, p, at.Unix(), func(v *fileRow) error {
		var cur fileRow
		err := cur.scan(db.QueryRow(`SELECT `+fileCols+` FROM files WHERE path=?`, v.path))
		if err == nil && bytes.Equal(cur.hash, v.hash) {
			return nil // already as it was
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return visit(v, &cur, err != nil || cur.hash == nil)
	})
}

// restoreDirAt brings back what a path (a directory, or one file) held at a point in time, as device "web": paths
// that now hold other content, or were deleted, get the old content; paths created since stay (nothing is deleted).
// It returns the numbers of restored and refused paths.
func (s *Server) restoreDirAt(folder int64, p string, at time.Time) (restored, failed int, err error) {
	db, err := s.folderDB(folder)
	if err != nil {
		return 0, 0, err
	}
	var ops []proto.Op
	flush := func() error {
		res, err := s.commit(folder, "web", ops)
		for _, x := range res {
			if x.Status == proto.StatusOK {
				restored++
			} else {
				failed++
			}
		}
		ops = ops[:0]
		return err
	}
	err = restoreDiff(db, p, at, func(v, cur *fileRow, _ bool) error {
		e := v.entry()
		ops = append(ops, proto.Op{Path: v.path, BaseSeq: cur.seq, Hash: e.Hash, Blocks: e.Blocks, Size: v.size, MtimeNs: v.mtimeNs, Exec: v.exec})
		if len(ops) == proto.MaxCommitOps {
			return flush()
		}
		return nil
	})
	if err == nil && len(ops) > 0 {
		err = flush()
	}
	return restored, failed, err
}

// previewLimit is the number of paths the restore preview lists.
const previewLimit = 20

type previewItem struct {
	Path string
	Gone bool // deleted now; otherwise its content differs
}

// restorePreview runs the query of restoreDirAt without committing: the number of files it would restore and the
// first previewLimit of them.
func (s *Server) restorePreview(folder int64, p string, at time.Time) (n int, items []previewItem, err error) {
	db, err := s.folderDB(folder)
	if err != nil {
		return 0, nil, err
	}
	err = restoreDiff(db, p, at, func(v, _ *fileRow, gone bool) error {
		if n++; n <= previewLimit {
			items = append(items, previewItem{v.path, gone})
		}
		return nil
	})
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return n, items, err
}
