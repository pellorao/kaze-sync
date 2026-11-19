package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/ignore"
	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

const (
	markerName  = ".syncroot"
	sqliteMagic = "SQLite format 3\x00"
	maxErrors   = 100
	maxRounds   = 4
)

// packetTarget is the size at which an upload packet is sent; each packet is one /blocks request.
var packetTarget = 4 << 20

// hashWorkers is how many files a scan hashes at once.
var hashWorkers = 8

var (
	errInUse      = errors.New("SQLite database in use (non-empty -wal or -journal)")
	errChanged    = errors.New("file changed while it was read")
	errLinkTarget = errors.New("symbolic link target is empty or longer than 4096 bytes: not synced")
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

	Epoch      string       // the server's epoch from the last poll, "" while unknown (design 5.5)
	DeviceName string       // used in conflict copy names
	CaseFold   bool         // the file system ignores case: Pull merges case-only renames
	OnDir      func(string) // called for every directory a scan visits (the watcher adds it)

	errs, pullErrs []proto.FileError
	scope          func(string) bool    // what the last scan covered: nil for the whole root
	youngFiles     map[string]time.Time // while scanning: the too-fresh files and when each becomes stable
	unstableFiles  map[string]time.Time // the same after a scan, kept across scans and pulls (see Unstable)
	retryDirs      map[string]bool      // directories whose files a retry must look at again (see RetryDirs)
	changedDirs    map[string]bool      // the part of retryDirs where a file changed while read: its change is unsent
	sink           *[]proto.FileError
	errSince       map[string]int64
	ready          bool // the sync root was checked by the last Scan
	retry          bool // something was postponed: run again soon
	pendingUp      int
	pendingDown    int
	changed        bool                 // Pull changed the tree or the base since rejected was filled
	rejected       map[string]rejection // files the server refused, until they change or something else changes
	seen           int64                // latest server seq the last Pull read up to
	epoch          string               // the epoch the local state belongs to
	watched        map[string]bool      // the root's directories a scan has entered, so that a watcher has them
	savedTS        string               // the time directory in .syncsaved of the running scan or pull
	wc             []wcRule             // the write-control settings, the root first (design 2.8.2)
	frozen         map[string]int64     // the frozen scopes and the cursor their pulls stop at (design 2.8.5)
	heldLive       map[string]bool      // the held deletions by path: true once the server is known to still have the path (settleHeld)
	heldSig        string
	heldErrAt      int64  // when the report of the held changes began to fail
	heldErr        string // why the last report of the held changes failed, "" when it did not
	heldSent       string
	heldNote       map[string]string // why the last decision on a held path was not carried out (HeldItem.Note)
	unsentAll      bool              // changes may be unsent anywhere: until a whole scan is clean (after a restart or a failed scan)
	unsentDirs     map[string]bool   // directories where a scan left changes unsent: a clean scan covering one clears it
	unsentFiles    map[string]bool   // unsent files that have a setting of their own (a scope of one file), cleared like unsentDirs
	linkProbe      int               // whether this machine can create symbolic links, probed once per Pull: 0 unknown, 1 yes, -1 no
}

type rejection struct {
	size, mtime int64
	reason      string
}

// Ready reports whether the last Scan found the sync root in order, so that Pull may run.
func (f *Folder) Ready() bool { return f.ready }

// Unstable is when the youngest file that scans left alone for being modified less than Stable ago becomes
// stable, zero when there is none. A scan of the whole root replaces the record, a partial scan replaces what it
// covered, and nothing else changes it: once it is due the caller scans UnstableDirs.
func (f *Folder) Unstable() time.Time {
	var t time.Time
	for _, end := range f.unstableFiles {
		if t.IsZero() || end.Before(t) {
			t = end
		}
	}
	return t
}

// UnstableDirs are the directories holding the files Unstable is about, sorted: the only places a retry for
// them has to scan.
func (f *Folder) UnstableDirs() []string { return dirsOf(f.unstableFiles) }

// RetryDirs are the directories the last Scan or Pull wants scanned again (a file changed while it was read
// or placed, or is in use), sorted. A retry without any (downloads waiting) needs a pull only.
func (f *Folder) RetryDirs() []string { return slices.Sorted(maps.Keys(f.retryDirs)) }

func dirsOf[V any](m map[string]V) []string {
	set := map[string]bool{}
	for p := range m {
		set[path.Dir(p)] = true
	}
	return slices.Sorted(maps.Keys(set))
}

// retryFile asks for a retry that scans the directory of p again, for a file that changed while it was read or
// placed. When it is during a Scan, that change is unsent until the retry (changedDirs, read by endScan); from a Pull the
// next Scan clears changedDirs first, so only the retry directory counts (the per-file base check catches the rest).
// Unlike a file in use, which is retryDir only.
func (f *Folder) retryFile(p string) {
	f.retry = true
	f.retryDir(p)
	if f.changedDirs == nil {
		f.changedDirs = map[string]bool{}
	}
	f.changedDirs[path.Dir(p)] = true
}

// retryDir records the directory of p for the next retry, quick or slow. A file in use only asks for the slow
// retry: it is a standing warning, not an unsent change (design 2.8.6).
func (f *Folder) retryDir(p string) {
	if f.retryDirs == nil {
		f.retryDirs = map[string]bool{}
	}
	f.retryDirs[path.Dir(p)] = true
}

// Seen is the latest server seq the last successful Pull read.
func (f *Folder) Seen() int64 { return f.seen }

// NewFolder builds the engine for a configured folder; caseFold selects the matcher mode.
func NewFolder(fc proto.FolderConfig, fsys cfs.FS, db *localDB, api *API, caseFold bool) *Folder {
	m, _ := ignore.Compile(fc.Rules, caseFold) // the server validated the rules
	if rules := strings.Join(fc.Rules, "\n"); db.metaString("rules") != rules {
		// New rules (also while the agent was off): read the whole server state again so that what they
		// release is reconciled; what they newly exclude is forgotten by the next scan (design 8.3).
		if err := db.setMeta("cursor", 0); err == nil {
			db.setMeta("rules", rules)
		}
	}
	f := &Folder{ID: fc.ID, Adopt: fc.Adopt, FS: fsys, DB: db, API: api, Matcher: m, Now: time.Now, Stable: 5 * time.Second, CaseFold: caseFold, seen: -1, epoch: db.metaString("epoch"), watched: map[string]bool{},
		wc: buildWC(fc.Writectl), frozen: db.metaPrefix("frozen:")}
	f.unsentAll = len(db.metaPrefix("dirty:")) > 0
	f.Requeue() // a change of the settings while the agent was off: the refused writes go again under the new ones
	return f
}

// Reset forgets the server seqs this folder knows: the next Pull reads everything again (design 5.5).
func (f *Folder) Reset() error {
	f.seen = -1
	for id := range f.DB.metaPrefix("dirty:") { // the seqs mean something else now: no scope has an old since (design 2.8.6)
		f.DB.delMeta("dirty:" + id)
	}
	f.DB.setMeta("scanc", 0)
	f.heldLive = nil // what was known of the server's files is of the old history
	if err := f.DB.forgetSeqs(); err != nil {
		return err
	}
	f.lowered(0)
	return nil
}

// checkEpoch resets the folder when the server's epoch is not the one the local state belongs to: the server was
// restored or rebuilt, so its seqs mean something else. A state without an epoch (from before epochs) adopts the first.
func (f *Folder) checkEpoch() error {
	if f.Epoch == "" || f.Epoch == f.epoch {
		return nil
	}
	if f.epoch != "" {
		if err := f.Reset(); err != nil {
			return err
		}
	}
	if err := f.DB.setMeta("epoch", f.Epoch); err != nil {
		return err
	}
	f.epoch = f.Epoch
	return nil
}

// Synced reports whether the folder is in step with the server for last_synced_at (design 3.6): nothing waits to be
// transferred or decided, and no real error is open. Skipped entries (a symbolic link Windows cannot create, a
// junction, a name that is not UTF-8) are standing warnings the user has accepted and do not count.
func (r Report) Synced() bool {
	if r.PendingUp != 0 || r.PendingDown != 0 || r.Held != 0 {
		return false
	}
	for _, e := range r.Errors {
		if e.Kind != proto.KindSkipped && e.Kind != proto.KindReport {
			return false
		}
	}
	return true
}

// Report is the outcome of one Scan.
type Report struct {
	State       string // "idle" or "error"
	Errors      []proto.FileError
	PendingUp   int  // changes found and not yet on the server
	PendingDown int  // server changes not applied yet
	Retry       bool // something was postponed (also files too fresh to upload, see Folder.Unstable): run again soon; derived from the Folder's state
	Slow        bool // files are in use or locked: retry on the slow schedule (design 5.7)
	Committed   int  // commits the server accepted as changes
	Held        int  // changes waiting for the user's decision (design 2.8.6)
}

func (f *Folder) note(p, kind, msg string) {
	if f.sink == nil {
		f.sink = &f.errs
	}
	*f.sink = append(*f.sink, proto.FileError{Path: p, Kind: kind, Message: msg})
}

// Scan finds local changes and commits them to the server (design 5.2, 5.4).
// With dirs, only the trees below them are examined (a watcher's hint); without, the whole root.
func (f *Folder) Scan(ctx context.Context, dirs ...string) (Report, error) {
	var rep Report
	old := f.errs
	f.savedTS = ""
	f.errs, f.sink, f.retry, f.pendingUp, f.scope = nil, &f.errs, false, 0, nil
	f.retryDirs, f.changedDirs, f.youngFiles = nil, nil, nil
	pd, rootOnly := pruneDirs(slices.Clone(dirs))
	whole := len(pd) == 0 && !rootOnly
	ok, err := f.checkRoot()
	f.ready = ok && err == nil
	if err == nil {
		err = f.checkEpoch()
	}
	if f.ready && err == nil {
		err = f.scan(ctx, dirs, &rep)
		if whole {
			f.cleanSaved()
		}
		if f.scope != nil { // a partial scan: errors outside what it covered still stand (design 3.6)
			var kept []proto.FileError
			for _, e := range old {
				if e.Kind != proto.KindFolder && !f.scope(e.Path) {
					kept = append(kept, e)
				}
			}
			f.errs = append(kept, f.errs...)
		}
	}
	if whole {
		f.unstableFiles = f.youngFiles
	} else {
		for p := range f.unstableFiles {
			if f.scope == nil || f.scope(p) {
				delete(f.unstableFiles, p)
			}
		}
		if f.unstableFiles == nil && f.youngFiles != nil {
			f.unstableFiles = map[string]time.Time{}
		}
		maps.Copy(f.unstableFiles, f.youngFiles)
	}
	f.finish(&rep)
	return rep, err
}

// finish completes a report from the folder's current errors and counters.
func (f *Folder) finish(rep *Report) {
	all := append(append([]proto.FileError{}, f.errs...), f.pullErrs...)
	if rows, err := f.DB.unlinked(); err == nil { // links this machine could not create: a state, not an event
		for _, r := range rows {
			all = append(all, proto.FileError{Path: r.Path, Kind: proto.KindSkipped, Message: r.Message})
		}
	}
	since := map[string]int64{}
	now := f.Now().Unix()
	for i := range all {
		e := &all[i]
		k := e.Kind + "\x00" + e.Path
		if s, ok := f.errSince[k]; ok {
			e.Since = s
		} else {
			e.Since = now
		}
		since[k] = e.Since
		if e.Kind == proto.KindInUse {
			rep.Slow = true
		}
		if e.Kind != proto.KindInUse && e.Kind != proto.KindSkipped {
			rep.State = "error"
		}
	}
	f.errSince = since
	if rep.Held = f.DB.heldCount(); rep.Held > 0 {
		rep.State = "error"
	}
	if rep.State == "" {
		rep.State = "idle"
	}
	rep.Errors = all[:min(len(all), maxErrors)]
	rep.PendingUp, rep.PendingDown = f.pendingUp, f.pendingDown
	rep.Retry = f.retry || len(f.unstableFiles) > 0
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
	link bool // a symbolic link (design 6.7)
}

// walk lists the files to sync below dir. Directories that cannot be read go
// to failed, so nothing below them is taken for deleted. Directories for which skip says true are not entered.
func (f *Folder) walk(dir string, files map[string]found, failed map[string]bool, skip func(string) bool) {
	if f.OnDir != nil {
		f.OnDir(dir)
	}
	entries, err := f.FS.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return // gone: everything below it counts as deleted; a directory of that name created later is a new one, not watched
	}
	if dir != "" && !strings.Contains(dir, "/") {
		f.watched[dir] = true
	}
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
		if builtinExcluded(e.Name, names) || dir == "" && e.Name == savedName || f.Matcher.Excluded(p, e.IsDir) {
			if isPart(e) && f.Now().UnixNano()-e.MtimeNs > int64(staleDownloadPart) {
				f.FS.Remove(p) // a download part nobody resumed for a day is leftover
			}
			continue
		}
		switch {
		case !utf8.ValidString(e.Name):
			f.note(p, proto.KindSkipped, "file name is not valid UTF-8")
		case e.IsLink:
			files[p] = found{e: e, link: true}
		case e.IsSymlink:
			f.note(p, proto.KindSkipped, "not a regular file, directory or symbolic link (a Windows junction?): not synced")
		case e.IsDir:
			if skip == nil || !skip(p) {
				f.walk(p, files, failed, skip)
			}
		default:
			busy := names[e.Name+"-wal"].Size > 0 || names[e.Name+"-journal"].Size > 0
			files[p] = found{e: e, busy: busy}
		}
	}
}

// staleDownloadPart is how long a temporary download file may lie untouched before the scan removes it.
const staleDownloadPart = 24 * time.Hour

func isPart(e cfs.Entry) bool {
	return !e.IsDir && strings.HasPrefix(e.Name, ".syncpart-")
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
func (f *Folder) hashFile(p string, fd found, buf []byte) (h hashed, err error) {
	busy := fd.busy
	if fd.link { // the content of a link is its target, one block
		target, err := f.FS.Readlink(p)
		if err != nil {
			return h, err
		}
		if len(target) < 1 || len(target) > proto.MaxLinkTarget {
			return h, errLinkTarget
		}
		h.size = int64(len(target))
		h.whole = sha256.Sum256([]byte(target))
		h.blocks = []proto.Hash{h.whole}
		return h, nil
	}
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
			if !(first && (err == io.EOF || err == io.ErrUnexpectedEOF)) { // a file of one block: its hash is the whole's
				whole.Write(buf[:n])
			}
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
	if len(h.blocks) == 1 {
		h.whole = h.blocks[0]
	} else {
		copy(h.whole[:], whole.Sum(nil))
	}
	return h, nil
}

// hashFiles hashes the files with hashWorkers at a time: opening a small file costs a wait (on Windows, the
// virus scanner reads it first), which overlaps this way. The results are in the order of paths.
func (f *Folder) hashFiles(paths []string, files map[string]found) []struct {
	h   hashed
	err error
} {
	out := make([]struct {
		h   hashed
		err error
	}, len(paths))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var panicked any
	next := make(chan int)
	for w := 0; w < min(hashWorkers, len(paths)); w++ {
		wg.Add(1)
		go func() {
			defer func() {
				if p := recover(); p != nil { // raised again below, in the caller's goroutine
					mu.Lock()
					panicked = p
					mu.Unlock()
					for range next { // let the sender finish
					}
				}
				wg.Done()
			}()
			buf := make([]byte, proto.BlockSize)
			for i := range next {
				out[i].h, out[i].err = f.hashFile(paths[i], files[paths[i]], buf)
			}
		}()
	}
	for i := range paths {
		next <- i
	}
	close(next)
	wg.Wait()
	if panicked != nil {
		panic(panicked)
	}
	return out
}

// item is one pending change.
type item struct {
	path string
	del  bool
	op   proto.Op
	row  localRow // what to store once the server accepts it
	done bool
	// own is set once a conflict with the device's own earlier commit has been committed on top of; a second one is a real conflict.
	own bool
	// reason is why the item is held, when it is: "conflict", "type" or "reset" (design 2.8.6); decided marks the
	// commit of a decision, which is not held again.
	reason  string
	decided bool
	reset   bool // the base was forgotten by an epoch reset
	// stage is the id of this item's block list on the server when it has more than MaxCommitBlocks blocks (design 2.4).
	stage string
}

func (f *Folder) scan(ctx context.Context, dirs []string, rep *Report) error {
	files, failed := map[string]found{}, map[string]bool{}
	var local map[string]localRow
	var err error
	dirs, rootOnly := pruneDirs(dirs)
	if len(dirs) == 0 && !rootOnly {
		f.walk("", files, failed, nil)
		local, err = f.DB.all()
	} else {
		local = map[string]localRow{}
		var top map[string]bool // the root's directories a root hint walked
		if rootOnly {
			var err error
			if top, err = f.walkRoot(files, failed, local); err != nil {
				return err
			}
		}
		f.scope = func(p string) bool {
			if rootOnly && (!strings.Contains(p, "/") || top[p[:strings.Index(p, "/")]]) {
				return true
			}
			for _, d := range dirs {
				if p == d || strings.HasPrefix(p, d+"/") {
					return true
				}
			}
			return false
		}
		for _, d := range dirs {
			if !f.Matcher.ExcludedPath(d, true) && !builtinPath(d) {
				f.walk(d, files, failed, nil)
			}
			rows, err := f.DB.under(d)
			if err != nil {
				return err
			}
			maps.Copy(local, rows)
		}
	}
	if err != nil {
		return err
	}
	var items []*item
	var puts []localRow
	var drop []string
	reverted := map[string]bool{} // files whose content is the base again
	whole := len(dirs) == 0 && !rootOnly
	now := f.Now().UnixNano()

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	var cands []string // the files to hash
	for _, p := range paths {
		e := files[p].e
		row, have := local[p]
		if have && row.Size == e.Size && row.MtimeNs == e.MtimeNs && row.Link == e.IsLink && f.execKept(row, e) {
			continue
		}
		if age := now - e.MtimeNs; age >= 0 && age < int64(f.Stable) {
			end := time.Unix(0, e.MtimeNs).Add(f.Stable)
			if f.youngFiles == nil {
				f.youngFiles = map[string]time.Time{}
			}
			f.youngFiles[p] = end
			continue
		}
		if rj, ok := f.rejected[p]; ok && rj.size == e.Size && rj.mtime == e.MtimeNs {
			f.note(p, proto.KindRejected, rj.reason) // refused before and unchanged: not asking again
			continue
		}
		cands = append(cands, p)
	}
	hashes := f.hashFiles(cands, files)
	for i, p := range cands {
		e := files[p].e
		row, have := local[p]
		h, err := hashes[i].h, hashes[i].err
		switch {
		case errors.Is(err, errInUse), errors.Is(err, cfs.ErrLocked):
			f.note(p, proto.KindInUse, err.Error())
			f.retryDir(p) // the slow schedule scans its directory again
			continue
		case errors.Is(err, errLinkTarget):
			f.note(p, proto.KindSkipped, err.Error())
			continue
		case err != nil:
			f.note(p, proto.KindIO, err.Error())
			continue
		}
		if st, err := f.FS.Stat(p); err != nil || (!e.IsLink && h.size != e.Size) || st.IsLink != e.IsLink || st.Size != e.Size || st.MtimeNs != e.MtimeNs {
			f.retryFile(p) // written while we read it
			continue
		}
		exec := f.execOf(row, e)
		r := localRow{Path: p, Key: pp.Key(p), Hash: h.whole[:], Blocks: proto.JoinHashes(h.blocks), Size: e.Size, MtimeNs: e.MtimeNs, Exec: exec, Link: e.IsLink}
		if have && bytes.Equal(row.Hash, r.Hash) && row.Link == r.Link && row.Exec == exec {
			r.Seq = row.Seq
			puts = append(puts, r) // only the stat fields changed
			reverted[p] = true
			continue
		}
		wh := h.whole
		items = append(items, &item{path: p, row: r, reset: have && row.Seq == 0, op: proto.Op{
			Path: p, BaseSeq: row.Seq, Hash: &wh, Blocks: h.blocks, Size: h.size, MtimeNs: e.MtimeNs, Exec: exec, Link: e.IsLink}})
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
		items = append(items, &item{path: p, del: true, reset: local[p].Seq == 0, op: proto.Op{Path: p, BaseSeq: local[p].Seq}})
	}
	if err := f.DB.apply(puts, drop); err != nil {
		// Only stat fields and forgotten rows: the next scan finds them again. Failing here would stop the commit
		// and the Pull of every cycle without a word on the Web UI; say it there and go on.
		slog.Warn("cannot store the scan's state rows", "folder", f.ID, "err", err)
		f.note("", proto.KindFolder, "cannot write the state database: "+err.Error())
	}
	f.cleanHeld(files, local, reverted, failed)
	// A frozen scope sends nothing: its changes join the held ones (design 2.8.5).
	var live, frozen []*item
	for _, it := range items {
		if f.frozenPath(it.path) {
			frozen = append(frozen, it)
		} else {
			live = append(live, it)
		}
	}
	if err := f.hold(frozen, "owner"); err != nil {
		return err
	}
	f.setSince(live)
	err = f.commitAll(ctx, live, rep)
	var refused []*item // the scopes frozen by this commit: the rest of their changes wait too
	for _, it := range live {
		switch {
		case it.done:
		case f.frozenPath(it.path):
			refused = append(refused, it)
		default:
			f.pendingUp++
		}
	}
	if herr := f.hold(refused, "owner"); herr != nil && err == nil {
		err = herr
	}
	// Frozen by a rejection whose change has gone since, or left frozen by a crash after its last held change went:
	// nothing to wait for.
	f.thaw(false, slices.Collect(maps.Keys(f.frozen))...)
	f.endScan(items, whole, err)
	return err
}

// unwatch forgets a root directory this client removed itself (a pull, a cleared directory): a directory created
// later under that name is a new one with no watch, and only a root hint can find it (newdir).
func (f *Folder) unwatch(d string) {
	if !strings.Contains(d, "/") {
		delete(f.watched, d)
	}
}

// walkRoot examines the root for a hint "." (a change directly in it): its files, and the directories no scan has
// entered yet (a new directory may hold files and has no watch yet). A change inside an entered directory comes
// with that directory's own hint; the hourly scan covers a directory that cannot be watched. The rows in scope go
// to local: the root's files and those below the directories walked or gone.
func (f *Folder) walkRoot(files map[string]found, failed map[string]bool, local map[string]localRow) (map[string]bool, error) {
	listed, err := f.FS.ReadDir("") // exact names: on a case-insensitive file system a case-only rename is a vanished directory too
	if err != nil {                 // do not take the watched directories for gone: the scan fails and is retried
		return nil, err
	}
	isDir, scope := map[string]bool{}, map[string]bool{}
	for _, e := range listed {
		isDir[e.Name] = e.IsDir
		if e.IsDir && !f.watched[e.Name] {
			scope[e.Name] = true // walked now: its rows are in scope
		}
	}
	f.walk("", files, failed, func(p string) bool { return f.watched[p] })
	rows, err := f.DB.rootFiles()
	if err != nil {
		return nil, err
	}
	maps.Copy(local, rows)
	gone, err := f.DB.topDirs()
	if err != nil {
		return nil, err
	}
	for d := range f.watched {
		gone[d] = true
	}
	for d := range gone {
		if isDir[d] {
			continue
		}
		delete(f.watched, d) // a directory of that name created later is a new one
		scope[d] = true
	}
	for d := range scope {
		rows, err := f.DB.under(d)
		if err != nil {
			return nil, err
		}
		maps.Copy(local, rows)
	}
	return scope, nil
}

// execOf is the execute bit to commit for a local entry; Windows does not know the bit and keeps the base's.
func (f *Folder) execOf(row localRow, e cfs.Entry) bool {
	if f.OS == "windows" {
		return row.Exec
	}
	return e.Exec && !e.IsLink
}

// execKept reports whether the entry's execute bit is the base's, so that a chmod alone is not skipped as unchanged.
func (f *Folder) execKept(row localRow, e cfs.Entry) bool { return f.execOf(row, e) == row.Exec }

// pruneDirs sorts dirs and drops the ones below another. "" (events may have been lost) or "/" means everything,
// returned as nil. A "." (a change directly in the root) is not that: it is reported as rootOnly (see walkRoot).
func pruneDirs(dirs []string) (out []string, rootOnly bool) {
	slices.Sort(dirs)
	for _, d := range dirs {
		switch d {
		case "", "/":
			return nil, false
		case ".", "..":
			rootOnly = true // ".." is an event on the root itself, whose parent is outside the folder
		default:
			if strings.HasPrefix(d, "../") {
				rootOnly = true
				continue
			}
			if n := len(out); n == 0 || !strings.HasPrefix(d, out[n-1]+"/") {
				out = append(out, d)
			}
		}
	}
	return out, rootOnly
}

func builtinPath(p string) bool {
	if p == savedName || strings.HasPrefix(p, savedName+"/") {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == markerName || strings.HasPrefix(seg, ".syncpart-") {
			return true
		}
	}
	return false
}
