package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/client/watch"
	"kaze-sync/internal/proto"
)

// statusEvery is the shortest time between two restarts of the long poll to report a changed status (design 3.6).
const statusEvery = 10 * time.Second

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
	AuthRetry                time.Duration   // delay before trying again after the server rejected the device token (design 2.5)

	mu       sync.Mutex // guards cfg, runners, their fields below the line, and the poll state
	cfg      proto.Config
	runners  map[int64]*runner
	kick     chan struct{}
	applied  chan struct{} // signalled when applyConfig has run
	failures int           // consecutive cycles that failed
	cancel   context.CancelFunc
	wg       sync.WaitGroup // the folder tasks running
	epoch    string         // the server's epoch from the last poll
	pollAt   time.Time
	sig      string    // status sent with the running poll
	statusAt time.Time // when to try again to report a changed status
}

type runner struct {
	cfg    proto.FolderConfig
	f      *Folder
	dbPath string
	w      *watch.Watcher
	events chan string
	done   chan struct{}

	// Guarded by Agent.mu.
	report     Report
	lastSynced int64
	scanned    bool
	busy       bool
	seen       int64 // the server's latest seq this runner has read up to; -1 unknown
	acked      int64 // the latest seq the server announced; stands in for seen while that news is handled or cannot be
	ready      bool  // the last scan found the sync root in order
	dirty      map[string]bool
	full       bool // a full scan is due
	pull       bool
	reset      bool // the server went back in time: forget its seqs
	rewatch    bool // the watcher reported lost events: start a new one
	debounceAt time.Time
	retryAt    time.Time
	slowTries  int // retries for files in use so far
	nextFull   time.Time
}

func (a *Agent) Run(ctx context.Context) error {
	a.kick, a.applied = make(chan struct{}, 1), make(chan struct{}, 1)
	a.runners = map[int64]*runner{}
	if a.Debounce == 0 {
		a.Debounce = 2 * time.Second
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

func (a *Agent) pollLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		a.mu.Lock()
		wait := a.unplanned()
		a.mu.Unlock()
		if wait { // the server answers at once for a folder without a cursor: give the loop a moment to create its runner
			select {
			case <-ctx.Done():
			case <-a.applied:
			case <-time.After(time.Second):
			}
		}
		a.mu.Lock()
		req := proto.PollReq{Cursors: map[int64]int64{}, ConfigVersion: a.cfg.Version, AgentVersion: proto.AgentVersion, OS: a.OS, Hostname: a.Hostname, Folders: a.states()}
		for id, r := range a.runners {
			cur := r.seen
			if (r.busy || r.pull || !r.ready) && r.acked >= 0 {
				cur = max(cur, r.acked) // news that is being handled, or cannot be until the folder is usable, must not wake the poll again
			}
			if cur >= 0 {
				req.Cursors[id] = cur
			}
		}
		pctx, cancel := context.WithCancel(ctx)
		a.cancel, a.pollAt, a.sig, a.statusAt = cancel, time.Now(), fmt.Sprint(req.Folders), time.Time{}
		a.mu.Unlock()
		resp, err := a.API.Poll(pctx, req)
		cancelled := pctx.Err() != nil
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case cancelled: // restarted to report a changed status
			continue
		case err != nil:
			wait := backoff
			if ue := (*UpgradeError)(nil); errors.As(err, &ue) {
				slog.Error("client too old, sync is stopped until kaze-syncc is upgraded", "err", err, "retry_in", wait)
			} else if errors.Is(err, ErrUnauthorized) { // revoked: asking often only adds load (design 2.5)
				wait = a.AuthRetry
				slog.Error("sync is stopped, the server does not accept this device", "err", err, "retry_in", wait)
			} else {
				slog.Warn("poll failed", "err", err, "retry_in", wait)
			}
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			backoff = min(2*backoff, time.Minute)
			continue
		}
		backoff = time.Second
		if !a.handlePoll(req, resp) {
			continue
		}
		a.mu.Lock()
		n := a.failures
		a.mu.Unlock()
		if n > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(min(time.Second<<min(n, 6), time.Minute)):
			}
		}
	}
}

// handlePoll takes in the answer to a poll request and reports whether it carried news.
func (a *Agent) handlePoll(req proto.PollReq, resp proto.PollResp) bool {
	news := false
	if resp.Config != nil {
		if err := writeJSON(filepath.Join(a.Dir, "config.json"), resp.Config); err != nil {
			slog.Error("cannot cache configuration", "err", err)
		}
		a.mu.Lock()
		a.cfg = *resp.Config
		a.mu.Unlock()
		news = true
	}
	a.mu.Lock()
	if resp.Epoch != a.epoch { // another server history: every folder reads again (Folder.checkEpoch decides)
		a.epoch = resp.Epoch
		for _, r := range a.runners {
			r.pull = true
		}
		news = true
	}
	for id, r := range a.runners {
		if seq, ok := resp.Cursors[id]; ok {
			if have, ok := req.Cursors[id]; ok && seq < have { // the server has less than we knew: a restored backup
				slog.Warn("the server's latest seq is behind ours; reading the folder again", "folder", id, "server", seq, "ours", have)
				r.reset, r.pull, r.seen, r.acked, news = true, true, -1, seq, true // the next poll carries the server's seq, not the stale one
				continue
			}
			if seq == r.seen {
				continue
			}
			r.acked = seq
			if r.ready || r.seen >= 0 { // a folder that cannot be pulled has no news for us; it pulls when it becomes ready
				r.pull, news = true, true
			}
		}
	}
	a.mu.Unlock()
	if news {
		a.wake()
	}
	return news
}

// states is the status report of every folder; the caller holds a.mu.
func (a *Agent) states() []proto.FolderState {
	var out []proto.FolderState
	for id, r := range a.runners {
		st := proto.FolderState{ID: id, State: "idle", LastSyncedAt: r.lastSynced}
		if r.scanned {
			st.State, st.PendingUp, st.PendingDown, st.Errors = r.report.State, r.report.PendingUp, r.report.PendingDown, r.report.Errors
		}
		if r.busy {
			st.State = "syncing"
		}
		out = append(out, st)
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
	r          *runner
	dirs       []string
	full, scan bool
	pull       bool
	reset      bool
	retry      bool // run because a retry was due
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
		t := task{r: r, full: r.full || (!r.nextFull.IsZero() && !now.Before(r.nextFull)), pull: r.pull, reset: r.reset}
		retry := !r.retryAt.IsZero() && !now.Before(r.retryAt)
		t.retry = retry
		if len(r.dirty) > 0 && !now.Before(r.debounceAt) {
			for d := range r.dirty {
				t.dirs = append(t.dirs, d)
			}
			r.dirty = nil
			t.scan = true
		}
		if t.full || retry {
			t.scan, t.pull = true, true
		}
		if !t.scan && !t.pull {
			continue
		}
		if t.full {
			t.dirs = nil
		}
		r.full, r.pull, r.reset, r.retryAt, r.busy = false, false, false, time.Time{}, true
		tasks = append(tasks, t)
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
	if t.reset {
		if err = f.Reset(); err != nil {
			slog.Warn("cannot reset the folder state", "folder", r.cfg.ID, "err", err)
		}
	}
	a.mu.Lock()
	old := r.w
	if r.rewatch && old != nil { // lost events, or a watcher that stopped (Windows, after the root went away): a new one, before the full scan
		r.w = nil
	} else {
		old = nil
	}
	r.rewatch = false
	a.mu.Unlock()
	if old != nil {
		old.Close()
		if f.Ready() {
			a.startWatch(r)
		}
	}
	if t.scan && err == nil {
		if rep, err = f.Scan(ctx, t.dirs...); err != nil && ctx.Err() == nil {
			slog.Warn("scan failed", "folder", r.cfg.ID, "err", err)
		}
	}
	if err == nil && (t.pull || f.retry || f.Seen() < 0) && f.Ready() {
		if rep, err = f.Pull(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("pull failed", "folder", r.cfg.ID, "err", err)
		}
	}
	if r.w == nil && f.Ready() && err == nil {
		a.startWatch(r) // the first scan has created the root; scan again so that every directory gets watched
		t.full = true
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	r.busy, r.scanned = false, true
	if t.scan || t.pull {
		r.report = rep
	}
	r.seen, r.ready = f.Seen(), f.Ready()
	if err == nil && len(rep.Errors) == 0 && rep.PendingUp == 0 && rep.PendingDown == 0 {
		r.lastSynced = now.Unix()
	}
	if err != nil || rep.Retry {
		r.retryAt = now.Add(a.RetryAfter)
		if errors.Is(err, ErrUnauthorized) {
			r.retryAt = now.Add(a.AuthRetry)
		}
		a.failures = map[bool]int{true: a.failures + 1, false: a.failures}[err != nil]
	} else {
		a.failures = 0
	}
	if !rep.Slow {
		r.slowTries = 0
	} else if err == nil && !rep.Retry {
		if t.retry {
			r.slowTries++
		}
		if r.slowTries < len(a.SlowRetry) {
			r.retryAt = now.Add(a.SlowRetry[r.slowTries])
		}
	}
	if t.full {
		r.nextFull = now.Add(a.ScanInterval)
		if r.w != nil && r.nextFull.Sub(now) > 0 && r.report.State != "" && !t.scan {
			r.full = true
		}
	}
	if err != nil {
		r.pull, r.reset = r.pull || t.pull, r.reset || t.reset
	}
}

func (a *Agent) startWatch(r *runner) {
	w, err := watch.Start(r.cfg.Path, r.events)
	if err != nil {
		slog.Warn("cannot watch the folder; relying on periodic scans", "folder", r.cfg.ID, "err", err)
		return
	}
	a.mu.Lock()
	r.w = w
	r.full = true
	a.mu.Unlock()
	r.f.OnDir = func(d string) {
		if d == "" {
			d = "."
		}
		w.Add(d) // fails when the inotify limit is reached: the hourly scan still covers it
	}
}

// consume turns the watcher's hints into the runner's dirty set until the runner stops.
func (a *Agent) consume(r *runner) {
	for {
		select {
		case d := <-r.events:
			a.mu.Lock()
			if r.dirty == nil {
				r.dirty = map[string]bool{}
			}
			r.dirty[d] = true
			r.rewatch = r.rewatch || d == ""
			r.debounceAt = time.Now().Add(a.Debounce)
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
		if r.busy { // its task wakes the loop when it ends
			continue
		}
		for _, t := range []time.Time{r.retryAt, r.nextFull} {
			if !t.IsZero() && t.Before(next) {
				next = t
			}
		}
		if len(r.dirty) > 0 && r.debounceAt.Before(next) {
			next = r.debounceAt
		}
	}
	if !a.statusAt.IsZero() && a.statusAt.Before(next) {
		next = a.statusAt
	}
	return max(time.Until(next), 0)
}

// applyConfig starts, restarts and stops folder runners to match the configuration (design 3.5).
// unplanned reports whether a configured folder has no runner yet; the caller holds a.mu.
func (a *Agent) unplanned() bool {
	for _, fc := range a.cfg.Folders {
		if a.runners[fc.ID] == nil {
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
		dbPath := filepath.Join(a.Dir, "folder-"+itoa(id)+".db")
		db, err := openLocal(dbPath)
		if err != nil {
			slog.Error("cannot open folder state", "folder", id, "err", err)
			continue
		}
		f := NewFolder(fc, a.NewFS(fc.Path), db, a.API, a.OS == "windows")
		f.OS, f.Stable, f.DeviceName = a.OS, a.Stable, a.DeviceName
		r := &runner{cfg: fc, f: f, dbPath: dbPath, events: make(chan string, 1000), done: make(chan struct{}),
			seen: -1, acked: -1, full: true, pull: true, nextFull: time.Now().Add(a.ScanInterval)}
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
