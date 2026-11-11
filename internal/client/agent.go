package client

import (
	"context"
	"errors"
	"fmt"
	"kaze-sync/internal/ntfy"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/client/watch"
	"kaze-sync/internal/proto"
)

// openRetryAfter is how long a folder whose state cannot be opened is left alone before the next try.
const openRetryAfter = time.Minute

// openRetryNewer is the wait after errNewerDB: only an upgrade of the binary (a restart) or a restored database
// helps, so asking every minute only fills the log.
const openRetryNewer = time.Hour

// openFail is a folder without a runner because its state database cannot be opened.
type openFail struct {
	at    time.Time // when to try again
	msg   string    // why, shown on the Web UI as the folder's error
	since int64     // unix seconds of the first failure
	seen  int64     // the server's latest seq for it from a poll, -1 unknown: the poll carries it so that the server does not answer at once
}

// statusEvery is the shortest time between two restarts of the long poll to report a changed status (design 3.6).
const statusEvery = 10 * time.Second

// syncingAfter is how long a task runs before the status says "syncing". A scan after a file event takes a fraction
// of a second; reporting it would restart the poll twice (busy, idle) and show "syncing" for about statusEvery after
// each tiny task (a file rewritten every 30 seconds made the overview say syncing half of the time, 2026-11-05).
const syncingAfter = 3 * time.Second

// Agent runs the sync loop (design 2.1): the long poll keeps the
// configuration and the server's news current, and every mapped folder is
// scanned and pulled on start, after file events (debounced), when the server
// has news, when something was postponed, and periodically.
type Agent struct {
	Dir                      string // state directory
	API                      *API
	NewFS                    func(root string) cfs.FS
	OS, Hostname, DeviceName string
	Stable                   time.Duration
	ScanInterval             time.Duration   // full scan period (design 5.2)
	RetryAfter               time.Duration   // delay before running again after a failure or postponed work
	SlowRetry                []time.Duration // delays of the retries for files in use; after them the periodic scan retries (design 5.7)
	Debounce                 time.Duration   // quiet time after file events before scanning (design 5.2)
	MaxWait                  time.Duration   // longest wait from the first pending event to the scan, however busy the folder (design 5.2)
	AuthRetry                time.Duration   // delay before trying again after the server rejected the device token (design 2.5)
	Notify                   *ntfy.Notifier  // pushes the errors the user must know about (design 4.3); nil is off

	mu       sync.Mutex // guards cfg, runners, their fields below the line, and the poll state
	cfg      proto.Config
	runners  map[int64]*runner
	kick     chan struct{}
	applied  chan struct{} // signalled when applyConfig has run
	failures int           // consecutive cycles that failed
	cancel   context.CancelFunc
	wg       sync.WaitGroup // the folder tasks running
	epoch    string         // the server's epoch from the last poll
	boot     string         // the server's run (proto.PollResp.Boot) from the last poll
	pollAt   time.Time
	sig      string    // status sent with the running poll
	statusAt time.Time // when to try again to report a changed status

	pollDown  atomic.Bool         // polls are failing to connect: folder failures are probably the same outage
	openRetry map[int64]*openFail // folders whose state database cannot be opened
}

type runner struct {
	cfg    proto.FolderConfig
	f      *Folder
	dbPath string
	w      *watch.Watcher
	events chan string
	done   chan struct{}

	// Guarded by Agent.mu.
	cfgVer     int64          // the configuration version the runner last saw
	requeue    bool           // the configuration changed: held refused writes go again (design 2.8.6)
	actions    []proto.Action // the user's decisions on held changes, to carry out in the next task (design 2.8.7)
	resend     bool           // the server restarted: send the held list again (design 2.8.10)
	report     Report
	lastSynced int64
	scanned    bool
	busy       bool
	busyAt     time.Time // when the running task started
	seen       int64     // the server's latest seq this runner has read up to; -1 unknown
	acked      int64     // the latest seq the server announced; stands in for seen while that news is handled or cannot be
	ready      bool      // the last scan found the sync root in order
	s          schedule  // when to scan and pull (schedule.go)
}

func (a *Agent) Run(ctx context.Context) error {
	a.kick, a.applied = make(chan struct{}, 1), make(chan struct{}, 1)
	a.runners, a.openRetry = map[int64]*runner{}, map[int64]*openFail{}
	if a.Debounce == 0 {
		a.Debounce = 2 * time.Second
	}
	if a.MaxWait == 0 {
		a.MaxWait = 10 * time.Second
	}
	if a.AuthRetry == 0 {
		a.AuthRetry = 10 * time.Minute
	}
	if a.SlowRetry == nil {
		a.SlowRetry = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}
	}
	if err := readJSON(filepath.Join(a.Dir, "config.json"), &a.cfg); err == nil {
		slog.Info("using cached configuration", "version", a.cfg.Version)
		a.dropStale()
	}
	go a.pollLoop(ctx)
	for {
		a.applyConfig()
		a.cycle(ctx)
		a.statusChanged()
		select {
		case <-ctx.Done():
			a.wg.Wait()
			a.closeAll()
			return nil
		case <-a.kick:
		case <-time.After(a.nextWake()):
		}
	}
}

func (a *Agent) wake() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// states is the status report of every folder; the caller holds a.mu.
func (a *Agent) states() []proto.FolderState {
	var out []proto.FolderState
	for id, r := range a.runners {
		st := proto.FolderState{ID: id, State: "idle", LastSyncedAt: r.lastSynced}
		if r.scanned {
			st.State, st.PendingUp, st.PendingDown, st.Errors, st.Held = r.report.State, r.report.PendingUp, r.report.PendingDown, r.report.Errors, r.report.Held
		}
		if r.busy && time.Since(r.busyAt) >= syncingAfter {
			st.State = "syncing"
		}
		out = append(out, st)
	}
	for id, o := range a.openRetry { // no runner: the folder state cannot be opened, which the Web UI must show
		out = append(out, proto.FolderState{ID: id, State: "error", Errors: []proto.FileError{{Kind: proto.KindFolder, Message: o.msg, Since: o.since}}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// statusChanged restarts the long poll when the status differs from the one it carries (at most every statusEvery).
func (a *Agent) statusChanged() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.statusAt = time.Time{} // a due retry is consumed here; otherwise nextWake would stay zero
	if a.cancel == nil || fmt.Sprint(a.states()) == a.sig {
		return
	}
	if wait := statusEvery - time.Since(a.pollAt); wait > 0 {
		a.statusAt = time.Now().Add(wait)
		return
	}
	a.cancel()
}

// task is the work of one runner in one cycle.
type task struct {
	r *runner
	plan
}

func (a *Agent) schedConf() schedConf {
	return schedConf{Debounce: a.Debounce, MaxWait: a.MaxWait, RetryAfter: a.RetryAfter, AuthRetry: a.AuthRetry, ScanInterval: a.ScanInterval, SlowRetry: a.SlowRetry}
}

// cycle starts the due work of the folders that are not busy, each as a task of its own: a long upload of one
// folder holds up no other (design 3). A finished task wakes the loop, which then plans again.
func (a *Agent) cycle(ctx context.Context) {
	now := time.Now()
	var tasks []task
	a.mu.Lock()
	for _, r := range a.runners {
		if r.busy {
			continue
		}
		r.f.Epoch = a.epoch
		p, ok := r.s.take(now)
		if !ok {
			continue
		}
		slog.Debug("task planned", "folder", r.cfg.ID, "dirs", p.dirs, "full", p.full, "scan", p.scan, "pull", p.pull, "retry", p.retry)
		r.busy, r.busyAt = true, now
		tasks = append(tasks, task{r, p})
	}
	a.mu.Unlock()
	if len(tasks) == 0 {
		return
	}
	a.statusChanged()
	for _, t := range tasks {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.work(ctx, t)
			a.statusChanged()
			a.wake()
		}()
	}
}

func (a *Agent) work(ctx context.Context, t task) {
	r, f := t.r, t.r.f
	var rep Report
	var err error
	a.mu.Lock()
	requeue, acts, resend := r.requeue, r.actions, r.resend
	r.requeue, r.actions, r.resend = false, nil, false
	a.mu.Unlock()
	if resend {
		f.heldSent = ""
	}
	for _, act := range acts {
		if err := f.Resolve(ctx, act); err != nil {
			slog.Info("the decision was not carried out", "folder", r.cfg.ID, "action", act.Action, "err", err)
		}
	}
	if requeue {
		if err = f.Requeue(); err != nil {
			slog.Warn("cannot send the held changes again", "folder", r.cfg.ID, "err", err)
		}
	}
	if t.reset {
		if err = f.Reset(); err != nil {
			slog.Warn("cannot reset the folder state", "folder", r.cfg.ID, "err", err)
		}
	}
	fresh := false // a watcher started in this task: a scan of the whole folder has to follow it
	a.mu.Lock()
	old := r.w
	if r.s.rewatch && old != nil { // lost events, or a watcher that stopped (Windows, after the root went away): a new one, before the full scan
		r.w = nil
	} else {
		old = nil
	}
	r.s.rewatch = false
	a.mu.Unlock()
	tried := false // a watcher was started (or failed to) in this task: one warning, one attempt
	if old != nil {
		old.Close()
		if f.Ready() {
			tried = true
			fresh = a.startWatch(r)
		}
	}
	if r.w == nil && err == nil && !tried {
		if _, serr := f.FS.Stat(markerName); serr == nil { // the root exists: watch it before the first scan, which then registers every directory
			tried = true
			fresh = a.startWatch(r)
		}
	}
	wholeDone := false
	if t.scan && err == nil {
		wholeDone = wholeScan(t.dirs)
		done := trace("scan", "folder", r.cfg.ID, "dirs", t.dirs)
		rep, err = f.Scan(ctx, t.dirs...)
		done("committed", rep.Committed, "pending_up", rep.PendingUp, "unstable", !f.Unstable().IsZero(), "err", err)
		if err != nil && ctx.Err() == nil {
			slog.Warn("scan failed", "folder", r.cfg.ID, "err", err)
		}
	}
	if !t.scan {
		f.retry, f.retryDirs = false, nil // nothing was scanned: this task's Pull alone says whether a retry is still due
	}
	if err == nil && (t.pull || f.retry || f.Seen() < 0) && f.Ready() {
		done := trace("pull", "folder", r.cfg.ID)
		rep, err = f.Pull(ctx)
		done("pending_down", rep.PendingDown, "err", err)
		if err != nil && ctx.Err() == nil {
			slog.Warn("pull failed", "folder", r.cfg.ID, "err", err)
		}
	}
	if r.w == nil && f.Ready() && err == nil && !tried && a.startWatch(r) {
		fresh = true // the first scan has created the root: scan again so that every directory gets watched
		wholeDone = false
	}
	if fresh && !wholeDone {
		a.mu.Lock()
		r.s.full = true
		a.mu.Unlock()
	}
	if items, cut, changed := f.HeldItems(); changed && ctx.Err() == nil { // the list for the Web UI, only when it differs
		herr := a.API.Held(ctx, proto.HeldReq{Folder: r.cfg.ID, Items: items, Cut: cut})
		f.HeldReportFailed(herr)
		if herr != nil {
			slog.Warn("cannot report the held changes", "folder", r.cfg.ID, "err", herr)
		} else {
			f.HeldSent()
		}
	}
	f.ShowHeldError(&rep)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	r.busy, r.scanned = false, true
	if t.scan || t.pull {
		r.report = rep
	}
	r.seen, r.ready = f.Seen(), f.Ready()
	if err == nil && rep.Synced() {
		r.lastSynced = now.Unix()
	}
	if err != nil || f.retry {
		a.failures = map[bool]int{true: a.failures + 1, false: a.failures}[err != nil]
	} else {
		a.failures = 0
	}
	a.notifyFolder(r, rep, err, t.scan)
	r.s.done(a.schedConf(), now, t.plan, taskResult{err: err, retry: f.retry, slow: rep.Slow, unstableAt: f.Unstable(), unstableDirs: f.UnstableDirs(), retryDirs: f.RetryDirs(),
		unauth: errors.Is(err, ErrUnauthorized)})
}

// wholeScan reports whether a Scan with these dirs covers the whole folder.
func wholeScan(dirs []string) bool {
	pd, rootOnly := pruneDirs(slices.Clone(dirs))
	return len(pd) == 0 && !rootOnly
}

// startWatch starts the folder's watcher; it reports false when that failed. A scan that visits every directory
// after it was started registers them with it.
func (a *Agent) startWatch(r *runner) bool {
	w, err := watch.Start(r.cfg.Path, r.events)
	if err != nil {
		slog.Warn("cannot watch the folder; relying on periodic scans", "folder", r.cfg.ID, "err", err)
		return false
	}
	a.mu.Lock()
	r.w = w
	a.mu.Unlock()
	r.f.OnDir = func(d string) {
		if d == "" {
			d = "."
		}
		w.Add(d) // fails when the inotify limit is reached: the hourly scan still covers it
	}
	return true
}

// consume turns the watcher's hints into the runner's dirty set until the runner stops.
func (a *Agent) consume(r *runner) {
	for {
		select {
		case d := <-r.events:
			slog.Debug("watcher event", "folder", r.cfg.ID, "dir", d)
			a.mu.Lock()
			r.s.event(a.schedConf(), d, time.Now())
			a.mu.Unlock()
			a.wake()
		case <-r.done:
			return
		}
	}
}

// nextWake is how long the loop may sleep.
func (a *Agent) nextWake() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	next := time.Now().Add(time.Hour)
	for _, r := range a.runners {
		if r.busy { // its task wakes the loop when it ends; a long one is reported as syncing
			if t := r.busyAt.Add(syncingAfter); t.After(time.Now()) && t.Before(next) {
				next = t
			}
			continue
		}
		if t := r.s.next(); !t.IsZero() && t.Before(next) {
			next = t
		}
	}
	if !a.statusAt.IsZero() && a.statusAt.Before(next) {
		next = a.statusAt
	}
	for _, o := range a.openRetry {
		if o.at.Before(next) {
			next = o.at
		}
	}
	return max(time.Until(next), 0)
}

// applyConfig starts, restarts and stops folder runners to match the configuration (design 3.5).
// unplanned reports whether a configured folder has no runner yet; the caller holds a.mu.
func (a *Agent) unplanned() bool {
	for _, fc := range a.cfg.Folders {
		if _, failed := a.openRetry[fc.ID]; a.runners[fc.ID] == nil && !failed { // a failed one is not coming soon: waiting for it would make the poll spin
			return true
		}
	}
	return false
}

func (a *Agent) applyConfig() {
	defer func() {
		select {
		case a.applied <- struct{}{}:
		default:
		}
	}()
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	want := map[int64]proto.FolderConfig{}
	for _, fc := range cfg.Folders {
		want[fc.ID] = fc
	}
	for id, r := range a.runners {
		if fc, ok := want[id]; ok && reflect.DeepEqual(fc, r.cfg) {
			a.mu.Lock()
			if r.cfgVer != cfg.Version { // an exclusive may have moved without the client's view changing
				r.cfgVer, r.requeue, r.s.full, r.s.pull = cfg.Version, true, true, true
			}
			a.mu.Unlock()
			continue
		}
		a.mu.Lock()
		busy := r.busy
		if !busy {
			delete(a.runners, id)
		}
		a.mu.Unlock()
		if busy { // its task finishes first and wakes the loop, which comes back here
			continue
		}
		a.stop(r, want[id].Path != r.cfg.Path) // a changed path is a new mapping; the old one is dropped
	}
	for id, fc := range want {
		if _, ok := a.runners[id]; ok {
			continue
		}
		a.mu.Lock()
		fail := a.openRetry[id]
		a.mu.Unlock()
		if fail != nil && time.Now().Before(fail.at) {
			continue
		}
		dbPath := filepath.Join(a.Dir, "folder-"+itoa(id)+".db")
		db, err := openLocal(dbPath)
		if err != nil {
			wait := openRetryAfter
			if errors.Is(err, errNewerDB) {
				wait = openRetryNewer
			}
			slog.Error("cannot open folder state", "folder", id, "err", err, "retry_in", wait)
			a.Notify.Fail("open-"+itoa(id), "Cannot open the state of folder "+itoa(id)+"; sync of it is stopped", 1)
			a.mu.Lock()
			if fail == nil {
				fail = &openFail{seen: -1, since: time.Now().Unix()}
			}
			fail.at, fail.msg = time.Now().Add(wait), "cannot open the state database: "+err.Error()
			a.openRetry[id] = fail
			a.mu.Unlock()
			continue
		}
		a.mu.Lock()
		delete(a.openRetry, id)
		a.mu.Unlock()
		a.Notify.Ok("open-"+itoa(id), "Opened the state of folder "+itoa(id)+" again")
		f := NewFolder(fc, a.NewFS(fc.Path), db, a.API, a.OS == "windows")
		f.OS, f.Stable, f.DeviceName = a.OS, a.Stable, a.DeviceName
		r := &runner{cfg: fc, cfgVer: cfg.Version, f: f, dbPath: dbPath, events: make(chan string, 1000), done: make(chan struct{}),
			seen: -1, acked: -1, s: schedule{full: true, pull: true, nextFull: time.Now().Add(a.ScanInterval)}}
		a.mu.Lock()
		a.runners[id] = r
		a.mu.Unlock()
		go a.consume(r)
	}
}

// dropStale removes the state databases of folders that are not in the configuration: a mapping removed while
// this agent was not running to forget it would leave one, and a later folder must not start from its cursor.
func (a *Agent) dropStale() {
	have := map[string]bool{}
	for _, fc := range a.cfg.Folders {
		have["folder-"+itoa(fc.ID)+".db"] = true
	}
	names, _ := filepath.Glob(filepath.Join(a.Dir, "folder-*.db"))
	for _, p := range names {
		if !have[filepath.Base(p)] {
			slog.Info("removing the state of a folder that is no longer mapped", "file", filepath.Base(p))
			for _, sfx := range []string{"", "-wal", "-shm"} {
				os.Remove(p + sfx)
			}
		}
	}
}

// stop closes a runner; with forget it also removes the state database and the marker file, keeping the local files.
func (a *Agent) stop(r *runner, forget bool) {
	close(r.done)
	if r.w != nil {
		r.w.Close()
	}
	r.f.DB.Close()
	if forget {
		for _, sfx := range []string{"", "-wal", "-shm"} {
			os.Remove(r.dbPath + sfx)
		}
		r.f.FS.Remove(markerName)
	}
}

func (a *Agent) closeAll() {
	for _, r := range a.runners {
		a.stop(r, false)
	}
}

// failuresBeforePush is how many cycles of one folder must fail in a row before the user is told: the retries come
// first (design 4.3).
const failuresBeforePush = 3

// notifyFolder tells the user about a folder that keeps failing or keeps reporting disk and permission errors, and
// that it is fine again. Files in use and skipped files are reminders on the web page, not pushes.
func (a *Agent) notifyFolder(r *runner, rep Report, err error, scanned bool) {
	if a.Notify == nil || errors.Is(err, context.Canceled) {
		return
	}
	id, name := itoa(r.cfg.ID), r.cfg.Name
	if err != nil && a.pollDown.Load() { // the outage is already reported once by "poll"; no second message for it
		return
	}
	if err != nil {
		a.Notify.Fail("sync-"+id, "Sync of folder "+name+" keeps failing", failuresBeforePush)
	} else {
		a.Notify.Ok("sync-"+id, "Sync of folder "+name+" works again")
	}
	if scanned {
		if rep.Held > 0 {
			a.Notify.Fail("held-"+id, itoa(int64(rep.Held))+" changes of folder "+name+" are on hold for your decision (open the Web UI)", 1)
		} else {
			a.Notify.Ok("held-"+id, "Folder "+name+" has no more changes on hold")
		}
	}
	n := 0
	for _, e := range rep.Errors {
		if e.Kind == proto.KindIO || e.Kind == proto.KindFolder || e.Kind == proto.KindRejected {
			n++
		}
	}
	if !scanned {
		return
	}
	if err == nil && n > 0 {
		a.Notify.Fail("files-"+id, itoa(int64(n))+" files of folder "+name+" cannot be synced (disk, permission or server refusal)", failuresBeforePush)
	} else if err == nil {
		a.Notify.Ok("files-"+id, "Folder "+name+" has no more file errors")
	}
}
