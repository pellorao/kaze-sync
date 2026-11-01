package client

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// Agent runs the sync loop: it keeps the configuration current with long
// polls and scans every mapped folder on start, on configuration changes and
// periodically. (File watching and pulling arrive with two-way sync.)
type Agent struct {
	Dir          string // state directory
	API          *API
	NewFS        func(root string) cfs.FS
	OS, Hostname string
	Stable       time.Duration
	ScanInterval time.Duration // full scan period (design 5.2)
	RetryAfter   time.Duration // rescan delay when files were still being written

	mu      sync.Mutex // guards cfg, cursors and the runners' reports
	cfg     proto.Config
	cursors map[int64]int64
	runners map[int64]*runner
	kick    chan struct{}
}

type runner struct {
	cfg        proto.FolderConfig
	f          *Folder
	dbPath     string
	report     Report
	lastSynced int64
	scanned    bool
}

func (a *Agent) Run(ctx context.Context) error {
	a.kick = make(chan struct{}, 1)
	a.runners = map[int64]*runner{}
	if err := readJSON(filepath.Join(a.Dir, "config.json"), &a.cfg); err == nil {
		slog.Info("using cached configuration", "version", a.cfg.Version)
	}
	go a.pollLoop(ctx)
	var wait time.Duration
	for {
		select {
		case <-ctx.Done():
			a.closeAll()
			return nil
		case <-a.kick:
		case <-time.After(wait):
		}
		wait = a.cycle(ctx)
	}
}

func (a *Agent) pollLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		a.mu.Lock()
		req := proto.PollReq{Cursors: a.cursors, ConfigVersion: a.cfg.Version, AgentVersion: proto.AgentVersion, OS: a.OS, Hostname: a.Hostname, Folders: a.states()}
		a.mu.Unlock()
		resp, err := a.API.Poll(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("poll failed", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, time.Minute)
			continue
		}
		backoff = time.Second
		a.mu.Lock()
		a.cursors = resp.Cursors
		a.mu.Unlock()
		if resp.Config != nil {
			if err := writeJSON(filepath.Join(a.Dir, "config.json"), resp.Config); err != nil {
				slog.Error("cannot cache configuration", "err", err)
			}
			a.mu.Lock()
			a.cfg = *resp.Config
			a.mu.Unlock()
			select {
			case a.kick <- struct{}{}:
			default:
			}
		}
	}
}

// states is the status report of every folder; the caller holds a.mu.
func (a *Agent) states() []proto.FolderState {
	var out []proto.FolderState
	for id, r := range a.runners {
		st := proto.FolderState{ID: id, State: "idle", LastSyncedAt: r.lastSynced}
		if r.scanned {
			st.State, st.PendingUp, st.Errors = r.report.State, r.report.PendingUp, r.report.Errors
		}
		out = append(out, st)
	}
	return out
}

// cycle applies the configuration, scans all folders concurrently and returns the delay until the next cycle.
func (a *Agent) cycle(ctx context.Context) time.Duration {
	a.applyConfig()
	a.mu.Lock()
	var rs []*runner
	for _, r := range a.runners {
		rs = append(rs, r)
	}
	a.mu.Unlock()
	var wg sync.WaitGroup
	var retryMu sync.Mutex
	retry := false
	for _, r := range rs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, err := r.f.Scan(ctx)
			if err != nil && ctx.Err() == nil {
				slog.Warn("scan failed", "folder", r.cfg.ID, "err", err)
			}
			a.mu.Lock()
			r.report, r.scanned = rep, true
			if err == nil && len(rep.Errors) == 0 && rep.PendingUp == 0 {
				r.lastSynced = time.Now().Unix()
			}
			a.mu.Unlock()
			retryMu.Lock()
			retry = retry || rep.Retry || err != nil
			retryMu.Unlock()
		}()
	}
	wg.Wait()
	if retry {
		return a.RetryAfter
	}
	return a.ScanInterval
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
		f.OS, f.Stable = a.OS, a.Stable
		a.mu.Lock()
		a.runners[id] = &runner{cfg: fc, f: f, dbPath: dbPath}
		a.mu.Unlock()
	}
}

// stop closes a runner; with forget it also removes the state database and the marker file, keeping the local files.
func (a *Agent) stop(r *runner, forget bool) {
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
