package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path"
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
	res, err := s.commit(folder, "web", []proto.Op{{Path: v.path, BaseSeq: base, Hash: e.Hash, Blocks: e.Blocks, Size: v.size, MtimeNs: v.mtimeNs, Exec: v.exec, Link: v.link}})
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
	cond, args := scopeCond(p)
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

// scopeCond is the condition that selects the rows at p or below it ("" is the whole folder).
func scopeCond(p string) (string, []any) {
	if p == "" {
		return "true", nil
	}
	return "(path = ? OR path >= ? AND path < ?)", []any{p, p + "/", p + "0"} // "0" is the character after "/"
}

// earliestRecord is the unix time of the first record at p or below it, 0 when there is none: the history of a
// path starts there (a folder's starts when its first file was uploaded, which can be later than the time a person asks for).
func earliestRecord(db *sql.DB, p string) (int64, error) {
	cond, args := scopeCond(p)
	var first sql.NullInt64
	err := db.QueryRow(`SELECT min(t) FROM (SELECT min(created_at) t FROM files WHERE `+cond+
		` UNION ALL SELECT min(created_at) FROM versions WHERE `+cond+`)`, append(append([]any{}, args...), args...)...).Scan(&first)
	return first.Int64, err
}

// importIdle is how long a folder must go without a new file for the first upload of a device to count as finished
// (seconds). The server is not told when a client's first scan ends, so the end is read off the data: the first
// upload creates files in a burst, whose last file is followed by a quiet time.
const importIdle = 120

// importEnd is the unix time the initial import of a folder finished: the time the last path of the first burst
// first appeared, the paths whose first records follow one another less than importIdle apart. A burst still going
// on (its last path is younger than importIdle) has not finished: 0. Only the first appearance of a path counts, so
// rewrites of the files (churn) do not hide the quiet time that follows the import.
func importEnd(db *sql.DB, now int64) (int64, error) {
	rows, err := db.Query(`SELECT min(created_at) t FROM (SELECT path, created_at FROM files WHERE hash IS NOT NULL
		UNION ALL SELECT path, created_at FROM versions WHERE hash IS NOT NULL) GROUP BY path ORDER BY t`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return 0, err
		}
		if last != 0 && t-last > importIdle {
			return last, nil
		}
		last = t
	}
	if err := rows.Err(); err != nil || last == 0 || now-last <= importIdle {
		return 0, err
	}
	return last, nil
}

// restoreNote says how a time before the history was moved (design 7): to the end of the folder's initial import
// (Import), or to the first record of the path, which is later when the path was created after the import; At is
// that time, 0 when the time was not moved.
type restoreNote struct {
	At     int64
	Import bool
}

// historyStart is the state a time before the first record (earliest, at or below the path asked about) is taken
// as (design 7): the end of the folder's initial import (imp), or earliest itself for a path created after the import.
func historyStart(db *sql.DB, earliest, now int64) (start int64, imp bool, err error) {
	end, err := importEnd(db, now)
	if err != nil {
		return 0, false, err
	}
	if end >= earliest {
		return end, true, nil
	}
	return earliest, false, nil
}

// restoreDiff calls visit for every file at p or below it whose content at time at differs from the current one:
// old is what it held then, nowGone says that it has no content now (deleted or never there). A time before the
// first record is taken as the end of the folder's initial import (design 7): that is the first state the
// folder was complete in, and "no difference" would be wrong. Files created after the import count as absent
// then and are not touched. none says that there is no record at all. A cancelled ctx (the browser dropped the
// request: the line moved on) ends the scan with its error.
func restoreDiff(ctx context.Context, db *sql.DB, p string, at time.Time, now int64, visit func(old, cur *fileRow, nowGone bool) error) (note restoreNote, none bool, err error) {
	earliest, err := earliestRecord(db, p)
	if err != nil || earliest == 0 {
		return restoreNote{}, err == nil, err
	}
	t := at.Unix()
	if t < earliest {
		start, imp, err := historyStart(db, earliest, now)
		if err != nil {
			return restoreNote{}, false, err
		}
		t, note = start, restoreNote{At: start, Import: imp}
	}
	err = stateAt(db, p, t, func(v *fileRow) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var cur fileRow
		err := cur.scan(db.QueryRow(`SELECT `+fileCols+` FROM files WHERE path=?`, v.path))
		if err == nil && bytes.Equal(cur.hash, v.hash) && cur.link == v.link {
			return nil // already as it was
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return visit(v, &cur, err != nil || cur.hash == nil)
	})
	return note, false, err
}

// restoreTicks are the marks of the restore time axis (design 4.6): the times a restore of p can start from, oldest
// first. The first is From, the oldest state a restore can go back to: the end of the initial import or the first
// record of p, or the start of the keep window when that is later. Then every minute with a change at or below p
// (a new version, a deletion), as the end of that minute so that the state at the tick holds the change, and last now.
type restoreTicks struct {
	Now   int64   `json:"now"`
	From  int64   `json:"from"`
	Ticks []int64 `json:"ticks"` // unix seconds, ascending, From first and Now last
	None  bool    `json:"none"`  // p has no history
}

func (s *Server) restoreTicks(folder int64, p string) (restoreTicks, error) {
	db, err := s.folderDB(folder)
	if err != nil {
		return restoreTicks{}, err
	}
	now := s.now().Unix()
	out := restoreTicks{Now: now, Ticks: []int64{}}
	earliest, err := earliestRecord(db, p)
	if err != nil || earliest == 0 {
		out.None = err == nil
		return out, err
	}
	start, _, err := historyStart(db, earliest, now)
	if err != nil {
		return out, err
	}
	out.From = max(start, now-int64(metaInt(s.db, "keep_days"))*86400)
	cond, args := scopeCond(p)
	q := `SELECT t FROM (SELECT (created_at+59)/60*60 t FROM files WHERE created_at > ? AND ` + cond +
		` UNION SELECT (created_at+59)/60*60 FROM versions WHERE created_at > ? AND ` + cond + `) WHERE t < ? ORDER BY t`
	args = append(append(append([]any{out.From}, args...), out.From), args...)
	rows, err := db.Query(q, append(args, now)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Ticks = append(out.Ticks, out.From)
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return out, err
		}
		out.Ticks = append(out.Ticks, t)
	}
	if out.From < now {
		out.Ticks = append(out.Ticks, now)
	}
	return out, rows.Err()
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
	_, _, err = restoreDiff(context.Background(), db, p, at, s.now().Unix(), func(v, cur *fileRow, _ bool) error {
		e := v.entry()
		ops = append(ops, proto.Op{Path: v.path, BaseSeq: cur.seq, Hash: e.Hash, Blocks: e.Blocks, Size: v.size, MtimeNs: v.mtimeNs, Exec: v.exec, Link: v.link})
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

type previewItem struct {
	Path string
	Gone bool // deleted now; otherwise its content differs
}

// restorePreview runs the query of restoreDirAt without committing: the number of files it would restore (n), how
// many of them pass the glob filter (match; the filter sees paths relative to p) and the first previewMax of those,
// in path order; note and none are as for restoreDiff.
func (s *Server) restorePreview(ctx context.Context, folder int64, p string, at time.Time, filter string) (n, match int, items []previewItem, note restoreNote, none bool, err error) {
	db, err := s.folderDB(folder)
	if err != nil {
		return 0, 0, nil, restoreNote{}, false, err
	}
	keep := globFilter(filter)
	first := firstPaths[previewItem]{path: func(i previewItem) string { return i.Path }}
	note, none, err = restoreDiff(ctx, db, p, at, s.now().Unix(), func(v, _ *fileRow, gone bool) error {
		n++
		rel := v.path
		if p != "" && strings.HasPrefix(rel, p+"/") {
			rel = rel[len(p)+1:]
		} else if p != "" {
			rel = path.Base(rel)
		}
		if keep == nil || keep(rel) {
			match++
			first.add(previewItem{rel, gone})
		}
		return nil
	})
	return n, match, first.items, note, none, err
}
