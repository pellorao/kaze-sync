package client

import (
	"context"
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
	ScanInterval             time.Duration // full scan period (design 5.2)
	RetryAfter               time.Duration // delay before running again after a failure or postponed work
	Debounce                 time.Duration // quiet time after file events before scanning (design 5.2)

	mu       sync.Mutex // guards cfg, runners, their fields below the line, and the poll state
	cfg      proto.Config
	runners  map[int64]*runner
	kick     chan struct{}
	polled   chan struct{} // a cycle finished
	failures int           // consecutive cycles that failed
	cancel   context.CancelFunc
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
	acked      int64 // the latest seq the server announced; stands in for seen while the folder cannot be pulled
	ready      bool  // the last scan found the sync root in order
	dirty      map[string]bool
	full       bool // a full scan is due
	pull       bool
	debounceAt time.Time
	retryAt    time.Time
	nextFull   time.Time
}

func (a *Agent) Run(ctx context.Context) error {
	a.kick, a.polled = make(chan struct{}, 1), make(chan struct{}, 1)
	a.runners = map[int64]*runner{}
	if a.Debounce == 0 {
		a.Debounce = 2 * time.Second
	}
	if err := readJSON(filepath.Join(a.Dir, "config.json"), &a.cfg); err == nil {
		slog.Info("using cached configuration", "version", a.cfg.Version)
	}
	go a.pollLoop(ctx)
	for {
		a.applyConfig()
		a.cycle(ctx)
		a.statusChanged()
		select {
		case <-ctx.Done():
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
		req := proto.PollReq{Cursors: map[int64]int64{}, ConfigVersion: a.cfg.Version, AgentVersion: proto.AgentVersion, OS: a.OS, Hostname: a.Hostname, Folders: a.states()}
		for id, r := range a.runners {
			switch {
			case r.seen >= 0:
				req.Cursors[id] = r.seen
			case !r.ready && r.acked >= 0: // nothing to read until the folder is usable: do not wake for its news
				req.Cursors[id] = r.acked
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
			slog.Warn("poll failed", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, time.Minute)
			continue
		}
		backoff = time.Second
		if !a.handlePoll(resp) {
			continue
		}
		select { // the next poll would return at once: wait until the news is handled
		case <-ctx.Done():
		case <-a.polled:
		case <-time.After(30 * time.Second):
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

// handlePoll takes in a poll answer and reports whether it carried news.
func (a *Agent) handlePoll(resp proto.PollResp) bool {
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
	for id, r := range a.runners {
		if seq, ok := resp.Cursors[id]; ok {
			if !r.ready && r.seen < 0 {
				r.acked = seq // a folder that cannot be pulled has no news for us; it pulls when it becomes ready
				continue
			}
			if seq != r.seen {
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
}

// cycle runs the due work of all folders concurrently.
func (a *Agent) cycle(ctx context.Context) {
	now := time.Now()
	var tasks []task
	a.mu.Lock()
	for _, r := range a.runners {
		t := task{r: r, full: r.full || (!r.nextFull.IsZero() && !now.Before(r.nextFull)), pull: r.pull}
		retry := !r.retryAt.IsZero() && !now.Before(r.retryAt)
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
		r.full, r.pull, r.retryAt, r.busy = false, false, time.Time{}, true
		tasks = append(tasks, t)
	}
	a.mu.Unlock()
	if len(tasks) == 0 {
		return
	}
	a.statusChanged()
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.work(ctx, t)
		}()
	}
	wg.Wait()
	select {
	case a.polled <- struct{}{}:
	default:
	}
	a.statusChanged()
}

func (a *Agent) work(ctx context.Context, t task) {
	r, f := t.r, t.r.f
	var rep Report
	var err error
	if t.scan {
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
		a.failures = map[bool]int{true: a.failures + 1, false: a.failures}[err != nil]
	} else {
		a.failures = 0
	}
	if t.full {
		r.nextFull = now.Add(a.ScanInterval)
		if r.w != nil && r.nextFull.Sub(now) > 0 && r.report.State != "" && !t.scan {
			r.full = true
		}
	}
	if err != nil {
		r.pull = r.pull || t.pull
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
	go func() {
		for {
			select {
			case d := <-r.events:
				a.mu.Lock()
				if r.dirty == nil {
					r.dirty = map[string]bool{}
				}
				r.dirty[d] = true
				r.debounceAt = time.Now().Add(a.Debounce)
				a.mu.Unlock()
				a.wake()
			case <-r.done:
				return
			}
		}
	}()
}

// nextWake is how long the loop may sleep.
func (a *Agent) nextWake() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	next := time.Now().Add(time.Hour)
	for _, r := range a.runners {
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
func (a *Agent) applyConfig() {
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
		delete(a.runners, id)
		a.mu.Unlock()
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
		a.mu.Lock()
		a.runners[id] = &runner{cfg: fc, f: f, dbPath: dbPath, events: make(chan string, 1000), done: make(chan struct{}),
			seen: -1, acked: -1, full: true, pull: true, nextFull: time.Now().Add(a.ScanInterval)}
		a.mu.Unlock()
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
