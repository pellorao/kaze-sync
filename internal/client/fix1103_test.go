package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

func rowSeq(t *testing.T, g *engine, p string) int64 {
	t.Helper()
	r, ok, err := g.f.DB.get(p)
	if err != nil || !ok {
		t.Fatalf("no row for %s: %v", p, err)
	}
	return r.Seq
}

// A changed epoch (restored or rebuilt server) makes the folder forget the server's seqs and read everything
// again; the first epoch a state sees is adopted without that (design 5.5).
func TestEpochChangeReadsEverythingAgain(t *testing.T) {
	_, _, _, g := setup(t)
	g.put("a.txt", []byte("one"))
	g.scan()
	g.pull()
	seq, cursor := rowSeq(t, g, "a.txt"), g.f.DB.metaInt("cursor")
	if seq == 0 || cursor == 0 {
		t.Fatalf("setup: seq %d cursor %d", seq, cursor)
	}
	g.f.Epoch = "A" // a state from before epochs adopts the first one
	g.scan()
	if g.f.DB.metaString("epoch") != "A" || g.f.DB.metaInt("cursor") != cursor || rowSeq(t, g, "a.txt") != seq {
		t.Fatal("the first epoch reset the state")
	}
	g.f.Epoch = "B"
	g.scan()
	if g.f.DB.metaInt("cursor") != 0 || rowSeq(t, g, "a.txt") != 0 || g.f.Seen() != -1 || g.f.DB.metaString("epoch") != "B" {
		t.Fatalf("not reset: cursor %d seq %d seen %d", g.f.DB.metaInt("cursor"), rowSeq(t, g, "a.txt"), g.f.Seen())
	}
	g.pull()
	if g.f.DB.metaInt("cursor") != cursor || rowSeq(t, g, "a.txt") != seq {
		t.Fatal("the folder did not read the server state again")
	}
	// The epoch survives a restart: the same one is not a change.
	g.restart(g.f.API, proto.FolderConfig{ID: g.f.ID, Rules: []string{"*.tmp"}, Adopt: true}, g.fs)
	g.f.Epoch = "B"
	g.scan()
	if g.f.DB.metaInt("cursor") != cursor {
		t.Fatal("a known epoch reset the state after a restart")
	}
}

// The server's latest seq behind the one the request carried means a restored backup: reset that folder only. An
// answer to an older request cannot trigger it (the comparison is with the request's own cursor).
func TestHandlePollDetectsGoingBackInTime(t *testing.T) {
	a := &Agent{Dir: t.TempDir(), kick: make(chan struct{}, 1), runners: map[int64]*runner{1: {seen: 5, ready: true}, 2: {seen: 5, ready: true}}}
	req := proto.PollReq{Cursors: map[int64]int64{1: 5, 2: 5}}
	if !a.handlePoll(req, proto.PollResp{Cursors: map[int64]int64{1: 3, 2: 5}, Epoch: "A"}) {
		t.Fatal("no news")
	}
	if r := a.runners[1]; !r.reset || !r.pull {
		t.Fatalf("folder 1: %+v", r)
	}
	if r := a.runners[2]; r.reset || r.pull != true { // pull: the first epoch seen is announced to every folder
		t.Fatalf("folder 2: %+v", r)
	}
	a.runners[2].pull = false
	// Same epoch, a seq that is only behind what another request meanwhile made the runner read: nothing.
	a.runners[2].seen = 9
	if a.handlePoll(req, proto.PollResp{Cursors: map[int64]int64{2: 5}, Epoch: "A"}); a.runners[2].reset {
		t.Fatal("a late answer reset the folder")
	}
	// A new epoch wakes every folder.
	a.runners[2].pull = false
	if !a.handlePoll(req, proto.PollResp{Cursors: map[int64]int64{2: 9}, Epoch: "B"}) || !a.runners[2].pull {
		t.Fatal("a new epoch did not wake the folder")
	}
}

// The state of a folder that is not in the cached configuration goes at start; the rest stays.
func TestAgentDropsStateOfUnmappedFolders(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"folder-1.db", "folder-7.db", "folder-7.db-wal", "folder-7.db-shm", "device.json"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
	}
	writeJSON(filepath.Join(dir, "config.json"), proto.Config{Version: 3, Folders: []proto.FolderConfig{{ID: 1, Path: filepath.Join(dir, "root")}}})
	dead := &API{Base: "https://127.0.0.1:1", Token: "t", HC: http.DefaultClient}
	a := &Agent{Dir: dir, API: dead, NewFS: cfs.OS, OS: runtime.GOOS, ScanInterval: time.Hour, RetryAfter: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, "the stale state to go", func() bool {
		_, err := os.Stat(filepath.Join(dir, "folder-7.db"))
		return err != nil
	})
	for _, n := range []string{"folder-7.db-wal", "folder-7.db-shm"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Errorf("%s remains", n)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "device.json")); err != nil {
		t.Error("an unrelated file went")
	}
	if _, err := os.Stat(filepath.Join(dir, "folder-1.db")); err != nil {
		t.Error("the state of a mapped folder went")
	}
}

// After the server rejected the token, the agent asks again only after AuthRetry, not at the usual backoff of a second.
func TestAgentBacksOffWhenTheTokenIsRejected(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer ts.Close()
	api := &API{Base: ts.URL, Token: "revoked", HC: ts.Client()}
	a := &Agent{Dir: t.TempDir(), API: api, NewFS: cfs.OS, OS: runtime.GOOS, ScanInterval: time.Hour, RetryAfter: time.Hour, AuthRetry: 1500 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, "two polls", func() bool { mu.Lock(); defer mu.Unlock(); return len(at) >= 2 })
	mu.Lock()
	gap := at[1].Sub(at[0])
	mu.Unlock()
	if gap < 1400*time.Millisecond {
		t.Fatalf("asked again after %v", gap)
	}
}

// A server that accepts a request and then says nothing must not hold the client for good (the timeouts of
// NewHTTPClient and GetBlocks).
func TestSilentServerTimesOut(t *testing.T) {
	oldResp, oldIdle := responseTimeout, idleTimeout
	responseTimeout, idleTimeout = 200*time.Millisecond, 200*time.Millisecond
	defer func() { responseTimeout, idleTimeout = oldResp, oldIdle }()
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/blocks/get" {
			w.Write([]byte("start"))
			w.(http.Flusher).Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(release) // first: Close waits for the handlers
	hc, err := NewHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Base: ts.URL, Token: "t", HC: hc}
	start := time.Now()
	if _, err := api.Commit(context.Background(), 1, nil); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("commit against a silent server: %v after %v", err, time.Since(start))
	}
	body, err := api.GetBlocks(context.Background(), []proto.Hash{{1}})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	start = time.Now()
	buf := make([]byte, 100)
	n, _ := body.Read(buf)
	if n != 5 {
		t.Fatalf("read %d bytes", n)
	}
	if _, err := body.Read(buf); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("a stalled download: %v after %v", err, time.Since(start))
	}
}

// dirCounter records the directories listed.
type dirCounter struct {
	cfs.FS
	mu   sync.Mutex
	dirs []string
}

func (d *dirCounter) ReadDir(p string) ([]cfs.Entry, error) {
	d.mu.Lock()
	d.dirs = append(d.dirs, p)
	d.mu.Unlock()
	return d.FS.ReadDir(p)
}

func (d *dirCounter) take() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.dirs
	d.dirs = nil
	return out
}

func (g *engine) scanHint(dirs ...string) Report {
	g.t.Helper()
	rep, err := g.f.Scan(context.Background(), dirs...)
	if err != nil {
		g.t.Fatal(err)
	}
	return rep
}

// A change directly in the root (hint ".") does not walk the whole tree: only the root, and directories the local
// state does not know yet; a directory that went away is noticed too (design 5.2).
func TestRootHintDoesNotWalkKnownDirectories(t *testing.T) {
	e, f, _, g := setup(t)
	g.put("a/one.txt", []byte("1"))
	g.put("a/deep/two.txt", []byte("2"))
	g.put("b/three.txt", []byte("3"))
	g.put("log.txt", []byte("v1"))
	g.scan()
	dc := &dirCounter{FS: g.fs}
	g.f.FS = dc
	dc.take()

	g.put("log.txt", []byte("v2"))
	g.put("b/three.txt", []byte("changed, but only a hint for b may find it"))
	if rep := g.scanHint("."); rep.Committed != 1 {
		t.Fatalf("root hint: %+v", rep)
	}
	for _, d := range dc.take() {
		if d != "" {
			t.Fatalf("listed the directory %q", d)
		}
	}
	if got, _ := e.serverFile(f, "log.txt"); string(got) != "v2" {
		t.Fatalf("log.txt = %q", got)
	}
	if got, _ := e.serverFile(f, "b/three.txt"); string(got) != "3" {
		t.Fatal("the root hint scanned a known directory")
	}
	if rep := g.scanHint("b"); rep.Committed != 1 {
		t.Fatalf("hint b: %+v", rep)
	}

	// A new directory is entered, a removed one is deleted on the server, both from the root hint alone.
	g.put("new/x.txt", []byte("x"))
	g.put("new/sub/y.txt", []byte("y"))
	for _, p := range []string{"a/one.txt", "a/deep/two.txt"} {
		g.fs.Remove(p)
	}
	g.fs.Remove("a/deep")
	g.fs.Remove("a")
	if rep := g.scanHint("."); rep.Committed != 4 {
		t.Fatalf("new and removed directories: %+v", rep)
	}
	for p, want := range map[string]bool{"new/x.txt": true, "new/sub/y.txt": true, "a/one.txt": false, "a/deep/two.txt": false, "b/three.txt": true} {
		if _, ok := e.serverFile(f, p); ok != want {
			t.Errorf("%s on the server: %v, want %v", p, ok, want)
		}
	}
	// A "" (events lost) still means everything.
	dc.take()
	g.put("b/z.txt", []byte("z"))
	if rep := g.scanHint("", "."); rep.Committed != 1 || len(dc.take()) < 4 {
		t.Fatalf("full scan: %+v", rep)
	}
}

// Lost events (or a watcher that stopped) make the agent start a new watcher, which keeps working.
func TestAgentRestartsTheWatcherAfterLostEvents(t *testing.T) {
	e := newCEnv(t)
	e.srv.PollTimeout = 300 * time.Millisecond
	dev, api := e.newDevice()
	f := e.newFolder("One")
	root := filepath.Join(t.TempDir(), "one")
	e.mapFolder(dev, f, root, true)
	a := &Agent{Dir: t.TempDir(), API: api, NewFS: cfs.OS, OS: runtime.GOOS, Stable: 0, ScanInterval: time.Hour, RetryAfter: 50 * time.Millisecond, Debounce: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	watcher := func() any {
		a.mu.Lock()
		defer a.mu.Unlock()
		if r := a.runners[f]; r != nil && r.w != nil {
			return r.w
		}
		return nil
	}
	eventually(t, "the first watcher", func() bool { return watcher() != nil })
	first := watcher()
	a.mu.Lock()
	r := a.runners[f]
	a.mu.Unlock()
	r.events <- ""
	eventually(t, "a new watcher", func() bool { w := watcher(); return w != nil && w != first })
	writeFile(t, filepath.Join(root, "after.txt"), "still watched")
	eventually(t, "a file written after the restart to upload", func() bool {
		_, ok := e.serverFile(f, "after.txt")
		return ok
	})
}

// holdCommits blocks the commits of one folder until released: a long upload.
type holdCommits struct {
	base    http.RoundTripper
	folder  int64
	release chan struct{}
}

func (h *holdCommits) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/commit") {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		var req proto.CommitReq
		json.Unmarshal(b, &req)
		if req.Folder == h.folder {
			select {
			case <-h.release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
	}
	return h.base.RoundTrip(r)
}

// A folder stuck in a long upload does not hold up the others: new files, retries and configuration changes of
// the rest go on (design 3).
func TestFoldersAreIndependentDuringALongUpload(t *testing.T) {
	e := newCEnv(t)
	e.srv.PollTimeout = 300 * time.Millisecond
	dev, api := e.newDevice()
	slow, fast := e.newFolder("Slow"), e.newFolder("Fast")
	hold := &holdCommits{base: api.HC.Transport, folder: slow, release: make(chan struct{})}
	api.HC.Transport = hold
	rootSlow, rootFast := filepath.Join(t.TempDir(), "slow"), filepath.Join(t.TempDir(), "fast")
	writeFile(t, filepath.Join(rootSlow, "big.bin"), "pretend this is 40 GB")
	writeFile(t, filepath.Join(rootFast, "one.txt"), "1")
	e.mapFolder(dev, slow, rootSlow, true)
	e.mapFolder(dev, fast, rootFast, true)
	a := &Agent{Dir: t.TempDir(), API: api, NewFS: cfs.OS, OS: runtime.GOOS, Stable: 0, ScanInterval: time.Hour, RetryAfter: 50 * time.Millisecond, Debounce: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	released := false
	release := func() {
		if !released {
			released = true
			close(hold.release)
		}
	}
	defer func() { release(); cancel(); <-done }()

	eventually(t, "the fast folder's first file", func() bool { _, ok := e.serverFile(fast, "one.txt"); return ok })
	writeFile(t, filepath.Join(rootFast, "two.txt"), "2") // the slow folder's commit is still hanging
	eventually(t, "a file added to the fast folder meanwhile", func() bool { _, ok := e.serverFile(fast, "two.txt"); return ok })
	if _, ok := e.serverFile(slow, "big.bin"); ok {
		t.Fatal("the held commit went through")
	}
	// The held folder is the one reported as busy.
	a.mu.Lock()
	busy := a.runners[slow].busy
	a.mu.Unlock()
	if !busy {
		t.Fatal("the slow folder is not busy")
	}
	release()
	eventually(t, "the held upload to finish", func() bool { _, ok := e.serverFile(slow, "big.bin"); return ok })
}
