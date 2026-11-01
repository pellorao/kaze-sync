package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/ignore"
	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

const (
	markerName   = ".syncroot"
	sqliteMagic  = "SQLite format 3\x00"
	maxErrors    = 100
	maxRounds    = 4
	packetTarget = 16 << 20
)

var (
	errInUse   = errors.New("SQLite database in use (non-empty -wal or -journal)")
	errChanged = errors.New("file changed while it was read")
)

// Folder is the engine for one mapped folder. It has no timers or goroutines:
// callers drive it by calling Scan.
type Folder struct {
	ID      int64
	Adopt   bool
	FS      cfs.FS
	DB      *localDB
	API     *API
	Matcher *ignore.Matcher
	Now     func() time.Time
	Stable  time.Duration // files modified more recently are left for the next scan
	OS      string        // "windows" keeps the execute bit of the base

	errs     []proto.FileError
	errSince map[string]int64
}

// NewFolder builds the engine for a configured folder; caseFold selects the matcher mode.
func NewFolder(fc proto.FolderConfig, fsys cfs.FS, db *localDB, api *API, caseFold bool) *Folder {
	m, _ := ignore.Compile(fc.Rules, caseFold) // the server validated the rules
	return &Folder{ID: fc.ID, Adopt: fc.Adopt, FS: fsys, DB: db, API: api, Matcher: m, Now: time.Now, Stable: 5 * time.Second}
}

// Report is the outcome of one Scan.
type Report struct {
	State     string // "idle" or "error"
	Errors    []proto.FileError
	PendingUp int  // changes found and not yet on the server
	Retry     bool // unstable files were skipped: scan again soon
	Committed int  // commits the server accepted as changes
}

func (f *Folder) note(p, kind, msg string) {
	f.errs = append(f.errs, proto.FileError{Path: p, Kind: kind, Message: msg})
}

// Scan finds local changes and commits them to the server (design 5.2, 5.4).
func (f *Folder) Scan(ctx context.Context) (Report, error) {
	var rep Report
	f.errs = nil
	ok, err := f.checkRoot()
	if ok && err == nil {
		err = f.scan(ctx, &rep)
	}
	since := map[string]int64{}
	now := f.Now().Unix()
	for i := range f.errs {
		e := &f.errs[i]
		k := e.Kind + "\x00" + e.Path
		if s, ok := f.errSince[k]; ok {
			e.Since = s
		} else {
			e.Since = now
		}
		since[k] = e.Since
		if e.Kind != proto.KindInUse && e.Kind != proto.KindSkipped {
			rep.State = "error"
		}
	}
	f.errSince = since
	if rep.State == "" {
		rep.State = "idle"
	}
	rep.Errors = f.errs[:min(len(f.errs), maxErrors)]
	return rep, err
}

// checkRoot makes sure the sync root is the one we set up (design 3.5). It
// reports false, with a folder error noted, when the folder must not be synced.
func (f *Folder) checkRoot() (bool, error) {
	_, err := f.FS.Stat(markerName)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		f.note("", proto.KindFolder, err.Error())
		return false, nil
	}
	if f.DB.metaString("initialised") != "" {
		f.note("", proto.KindFolder, "marker file "+markerName+" is missing (disk not mounted or directory moved); not syncing")
		return false, nil
	}
	entries, err := f.FS.ReadDir("")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		err = f.FS.MkdirAll("")
	case err == nil && len(entries) > 0 && !f.Adopt:
		f.note("", proto.KindFolder, "directory is not empty; allow adopting it on the device page")
		return false, nil
	}
	if err == nil {
		var w io.WriteCloser
		if w, _, err = f.FS.OpenAppend(markerName); err == nil {
			_, err = io.WriteString(w, "kaze-sync sync root\n")
			if cerr := w.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err == nil {
		err = f.DB.setMeta("initialised", 1)
	}
	if err != nil {
		f.note("", proto.KindFolder, err.Error())
		return false, nil
	}
	return true, nil
}

type found struct {
	e    cfs.Entry
	busy bool // a non-empty -wal or -journal sits next to it
}

// walk lists the files to sync below dir. Directories that cannot be read go
// to failed, so nothing below them is taken for deleted.
func (f *Folder) walk(dir string, files map[string]found, failed map[string]bool) {
	entries, err := f.FS.ReadDir(dir)
	if err != nil {
		f.note(dir, proto.KindIO, err.Error())
		failed[dir] = true
		return
	}
	names := map[string]cfs.Entry{}
	for _, e := range entries {
		names[e.Name] = e
	}
	for _, e := range entries {
		p := path.Join(dir, e.Name)
		if builtinExcluded(e.Name, names) || f.Matcher.Excluded(p, e.IsDir) {
			continue
		}
		switch {
		case !utf8.ValidString(e.Name):
			f.note(p, proto.KindSkipped, "file name is not valid UTF-8")
		case e.IsSymlink:
			f.note(p, proto.KindSkipped, "symbolic links are not synced")
		case e.IsDir:
			f.walk(p, files, failed)
		default:
			busy := names[e.Name+"-wal"].Size > 0 || names[e.Name+"-journal"].Size > 0
			files[p] = found{e, busy}
		}
	}
}

// builtinExcluded: the marker, temporary download files and SQLite side files (design 8.1, 5.6).
func builtinExcluded(name string, siblings map[string]cfs.Entry) bool {
	if name == markerName || strings.HasPrefix(name, ".syncpart-") {
		return true
	}
	for _, sfx := range []string{"-wal", "-shm", "-journal"} {
		if base, ok := strings.CutSuffix(name, sfx); ok {
			if _, has := siblings[base]; has {
				return true
			}
		}
	}
	return false
}

func underFailed(failed map[string]bool, p string) bool {
	for d := path.Dir(p); ; d = path.Dir(d) {
		if d == "." {
			d = ""
		}
		if failed[d] {
			return true
		}
		if d == "" {
			return false
		}
	}
}

type hashed struct {
	blocks []proto.Hash
	whole  proto.Hash
	size   int64
}

// hashFile reads p once and computes the block hashes and the whole-file hash.
func (f *Folder) hashFile(p string, busy bool, buf []byte) (h hashed, err error) {
	r, err := f.FS.Open(p)
	if err != nil {
		return h, err
	}
	defer r.Close()
	whole := sha256.New()
	for first := true; ; first = false {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if first && busy && string(buf[:min(n, len(sqliteMagic))]) == sqliteMagic {
				return h, errInUse
			}
			whole.Write(buf[:n])
			h.blocks = append(h.blocks, sha256.Sum256(buf[:n]))
			h.size += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return h, err
		}
	}
	copy(h.whole[:], whole.Sum(nil))
	return h, nil
}

// item is one pending change.
type item struct {
	path string
	del  bool
	op   proto.Op
	row  localRow // what to store once the server accepts it
	done bool
}

func (f *Folder) scan(ctx context.Context, rep *Report) error {
	files, failed := map[string]found{}, map[string]bool{}
	f.walk("", files, failed)
	local, err := f.DB.all()
	if err != nil {
		return err
	}
	var items []*item
	var puts []localRow
	var drop []string
	buf := make([]byte, proto.BlockSize)
	now := f.Now().UnixNano()

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		e := files[p].e
		row, have := local[p]
		if have && row.Size == e.Size && row.MtimeNs == e.MtimeNs {
			continue
		}
		if age := now - e.MtimeNs; age >= 0 && age < int64(f.Stable) {
			rep.Retry = true
			continue
		}
		h, err := f.hashFile(p, files[p].busy, buf)
		switch {
		case errors.Is(err, errInUse), errors.Is(err, cfs.ErrLocked):
			f.note(p, proto.KindInUse, err.Error())
			continue
		case err != nil:
			f.note(p, proto.KindIO, err.Error())
			continue
		}
		if st, err := f.FS.Stat(p); err != nil || h.size != e.Size || st.Size != e.Size || st.MtimeNs != e.MtimeNs {
			rep.Retry = true // written while we read it
			continue
		}
		exec := e.Exec
		if f.OS == "windows" {
			exec = row.Exec // Windows does not know the bit: keep the base's
		}
		r := localRow{Path: p, Key: pp.Key(p), Hash: h.whole[:], Blocks: joinHashes(h.blocks), Size: e.Size, MtimeNs: e.MtimeNs, Exec: exec}
		if have && bytes.Equal(row.Hash, r.Hash) {
			r.Seq = row.Seq
			puts = append(puts, r) // only the stat fields changed
			continue
		}
		wh := h.whole
		items = append(items, &item{path: p, row: r, op: proto.Op{
			Path: p, BaseSeq: row.Seq, Hash: &wh, Blocks: h.blocks, Size: e.Size, MtimeNs: e.MtimeNs, Exec: exec}})
	}

	lpaths := make([]string, 0, len(local))
	for p := range local {
		lpaths = append(lpaths, p)
	}
	slices.Sort(lpaths)
	for _, p := range lpaths {
		if _, ok := files[p]; ok {
			continue
		}
		if f.Matcher.ExcludedPath(p, false) || builtinPath(p) {
			drop = append(drop, p) // newly excluded: forget it, delete nothing (design 8.3)
			continue
		}
		if underFailed(failed, p) {
			continue
		}
		items = append(items, &item{path: p, del: true, op: proto.Op{Path: p, BaseSeq: local[p].Seq}})
	}
	if err := f.DB.apply(puts, drop); err != nil {
		return err
	}
	err = f.commitAll(ctx, items, rep)
	for _, it := range items {
		if !it.done {
			rep.PendingUp++
		}
	}
	return err
}

func builtinPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == markerName || strings.HasPrefix(seg, ".syncpart-") {
			return true
		}
	}
	return false
}

func joinHashes(hs []proto.Hash) []byte {
	b := make([]byte, 0, 32*len(hs))
	for _, h := range hs {
		b = append(b, h[:]...)
	}
	return b
}
