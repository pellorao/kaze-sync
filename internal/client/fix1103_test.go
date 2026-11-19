package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	t.Parallel()
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
	t.Parallel()
	a := &Agent{Dir: t.TempDir(), kick: make(chan struct{}, 1), runners: map[int64]*runner{1: {seen: 5, ready: true}, 2: {seen: 5, ready: true}}}
	req := proto.PollReq{Cursors: map[int64]int64{1: 5, 2: 5}}
	if !a.handlePoll(req, proto.PollResp{Cursors: map[int64]int64{1: 3, 2: 5}, Epoch: "A"}) {
		t.Fatal("no news")
	}
	if r := a.runners[1]; !r.s.reset || !r.s.pull {
		t.Fatalf("folder 1: %+v", r)
	}
	if r := a.runners[2]; r.s.reset || r.s.pull != true { // pull: the first epoch seen is announced to every folder
		t.Fatalf("folder 2: %+v", r)
	}
	a.runners[2].s.pull = false
	// Same epoch, a seq that is only behind what another request meanwhile made the runner read: nothing.
	a.runners[2].seen = 9
	if a.handlePoll(req, proto.PollResp{Cursors: map[int64]int64{2: 5}, Epoch: "A"}); a.runners[2].s.reset {
		t.Fatal("a late answer reset the folder")
	}
	// A new epoch wakes every folder.
	a.runners[2].s.pull = false
	if !a.handlePoll(req, proto.PollResp{Cursors: map[int64]int64{2: 9}, Epoch: "B"}) || !a.runners[2].s.pull {
		t.Fatal("a new epoch did not wake the folder")
	}
}

// The state of a folder that is not in the cached configuration goes at start; the rest stays.
func TestAgentDropsStateOfUnmappedFolders(t *testing.T) {
	t.Parallel()
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

// After the server rejected the token, the agent asks again only after AuthRetry, not at the usual backoff of a
// second (poll.go): nothing more arrives for a while longer than that backoff.
func TestAgentBacksOffWhenTheTokenIsRejected(t *testing.T) {
	t.Parallel()
	var polls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer ts.Close()
	api := &API{Base: ts.URL, Token: "revoked", HC: ts.Client()}
	a := &Agent{Dir: t.TempDir(), API: api, NewFS: cfs.OS, OS: runtime.GOOS, ScanInterval: time.Hour, RetryAfter: time.Hour, AuthRetry: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, "the first poll", func() bool { return polls.Load() >= 1 })
	time.Sleep(1150 * time.Millisecond) // the usual backoff is one second
	if n := polls.Load(); n != 1 {
		t.Fatalf("%d polls: asked again before AuthRetry", n)
	}
}

// A server that accepts a request and then says nothing must not hold the client for good (the timeouts of
// NewHTTPClient and GetBlocks).
func TestSilentServerTimesOut(t *testing.T) {
	oldResp, oldIdle := responseTimeout, idleTimeout
	responseTimeout, idleTimeout = 60*time.Millisecond, 60*time.Millisecond
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// A partial scan keeps the errors outside the subtrees it covered (design 3.6): a skipped entry stays reported
// when a change elsewhere triggers a hint; the hint for its own directory clears it once it is gone.
func TestPartialScanKeepsErrorsOutsideItsScope(t *testing.T) {
	t.Parallel()
	_, _, _, g := setup(t)
	g.put("a/bad\xff.txt", []byte("1"))
	g.put("b/x.txt", []byte("1"))
	g.put("top\xfe.txt", []byte("1"))
	skipped := func(rep Report) (n int) {
		for _, e := range rep.Errors {
			if e.Kind == proto.KindSkipped {
				n++
			}
		}
		return
	}
	if rep := g.scan(); skipped(rep) != 2 {
		t.Fatalf("full scan: %+v", rep.Errors)
	}
	g.put("b/x.txt", []byte("2"))
	if rep := g.scanHint("b"); rep.Committed != 1 || skipped(rep) != 2 {
		t.Fatalf("hint b: %+v %+v", rep, rep.Errors)
	}
	g.put("c.txt", []byte("3"))
	if rep := g.scanHint("."); rep.Committed != 1 || skipped(rep) != 2 { // the root hint covers the root's entries
		t.Fatalf("root hint: %+v", rep.Errors)
	}
	g.fs.Remove("top\xfe.txt")
	if rep := g.scanHint("."); skipped(rep) != 1 {
		t.Fatalf("root entry gone: %+v", rep.Errors)
	}
	g.fs.Remove("a/bad\xff.txt")
	if rep := g.scanHint("a"); skipped(rep) != 0 {
		t.Fatalf("hint a after removal: %+v", rep.Errors)
	}
}

type failRootFS struct {
	cfs.FS
	fail bool
}

func (f *failRootFS) ReadDir(d string) ([]cfs.Entry, error) {
	if f.fail && d == "" {
		return nil, errors.New("boom")
	}
	return f.FS.ReadDir(d)
}

// A root hint whose directory listing fails fails the scan: the watched directories must not be taken for gone.
func TestRootHintFailsWhenTheRootCannotBeListed(t *testing.T) {
	t.Parallel()
	e, f, _, g := setup(t)
	g.put("a/one.txt", []byte("1"))
	g.scan()
	fr := &failRootFS{FS: g.fs, fail: true}
	g.f.FS = fr
	if _, err := g.f.Scan(context.Background(), "."); err == nil {
		t.Fatal("the failed listing was not reported")
	}
	if len(g.f.watched) != 1 {
		t.Fatalf("watched directories forgotten: %v", g.f.watched)
	}
	fr.fail = false
	if rep := g.scanHint("."); rep.Committed != 0 {
		t.Fatalf("%+v", rep)
	}
	if _, ok := e.serverFile(f, "a/one.txt"); !ok {
		t.Fatal("a/one.txt lost")
	}
}

// A scan of a hint for a directory that has just been removed must not mark it watched: the directory created
// later under that name is a new one with no watch, and only a root hint can find it (newdir).
func TestRemovedTopDirectoryHintDoesNotHideTheNewOne(t *testing.T) {
	t.Parallel()
	e, f, _, g := setup(t)
	g.put("d/one.txt", []byte("1"))
	g.scan()
	g.fs.Remove("d/one.txt")
	g.fs.Remove("d")
	if rep := g.scanHint(".", "d"); rep.Committed != 1 { // events of the removal: the root's and the directory's
		t.Fatalf("removal: %+v", rep)
	}
	g.put("d/two.txt", []byte("2"))
	if rep := g.scanHint("."); rep.Committed != 1 {
		t.Fatalf("new directory with the removed one's name: %+v", rep)
	}
	if _, ok := e.serverFile(f, "d/two.txt"); !ok {
		t.Fatal("d/two.txt not uploaded")
	}
}

// A top directory the client removed itself while applying the server's deletion is not watched any more either:
// the directory the user creates under that name later is found by a root hint (newdir).
func TestPulledRemovalOfTopDirectoryHidesNothing(t *testing.T) {
	t.Parallel()
	_, _, g1, g2, _ := pair(t)
	g1.put("d/one.txt", []byte("1"))
	g1.scan()
	g2.scan()
	g2.pull()
	g2.scan() // enters d: it counts as watched
	g1.fs.Remove("d/one.txt")
	g1.fs.Remove("d")
	g1.scan()
	g2.pull()
	if _, err := g2.fs.Stat("d"); !errorsIsNotExist(err) {
		t.Fatalf("d kept: %v", err)
	}
	g2.put("d/two.txt", []byte("2"))
	if rep := g2.scanHint("."); rep.Committed != 1 {
		t.Fatalf("new directory with the removed one's name: %+v", rep)
	}
}

// A hint outside the root (an attribute event on the root itself used to give "..") never lists or walks anything
// but the root, and pruneDirs reads it as a change of the root.
func TestHintOutsideRootIsRootHint(t *testing.T) {
	t.Parallel()
	for _, h := range []string{"..", "../sibling", "../.."} {
		if out, rootOnly := pruneDirs([]string{h}); len(out) != 0 || !rootOnly {
			t.Errorf("pruneDirs(%q) = %v, %v; want root only", h, out, rootOnly)
		}
	}
	_, _, _, g := setup(t)
	g.put("a/one.txt", []byte("1"))
	g.scan()
	dc := &dirCounter{FS: g.fs}
	g.f.FS = dc
	dc.take()
	g.scanHint("..")
	for _, d := range dc.take() {
		if d != "" {
			t.Fatalf("listed the directory %q", d)
		}
	}
}
