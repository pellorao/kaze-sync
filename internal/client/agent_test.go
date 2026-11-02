package client

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
)

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 250; i++ {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSyncsMappingsIndependently(t *testing.T) {
	e := newCEnv(t)
	e.srv.PollTimeout = 300 * time.Millisecond
	dev, api := e.newDevice()
	f1, f2, f3 := e.newFolder("One"), e.newFolder("Two"), e.newFolder("Three")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "one", "a.txt"), "alpha")
	writeFile(t, filepath.Join(root, "two", "sub", "b.txt"), "beta")
	writeFile(t, filepath.Join(root, "three", "c.txt"), "gamma")
	e.mapFolder(dev, f1, filepath.Join(root, "one"), true)
	e.mapFolder(dev, f2, filepath.Join(root, "two"), true)
	e.mapFolder(dev, f3, filepath.Join(root, "three"), false) // not empty, not adopted: must stay untouched

	dir := t.TempDir()
	a := &Agent{Dir: dir, API: api, NewFS: cfs.OS, OS: runtime.GOOS, Stable: 0, ScanInterval: time.Hour, RetryAfter: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	}()

	eventually(t, "both adopted folders to upload", func() bool {
		_, ok1 := e.serverFile(f1, "a.txt")
		_, ok2 := e.serverFile(f2, "sub/b.txt")
		return ok1 && ok2
	})
	if _, ok := e.serverFile(f3, "c.txt"); ok {
		t.Fatal("the folder that was not adopted uploaded")
	}
	if _, err := os.Stat(filepath.Join(root, "three", markerName)); err == nil {
		t.Fatal("marker written into the non-adopted directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal("configuration not cached")
	}

	// Removing a mapping stops syncing it, deletes its marker and state, and keeps the local files.
	if st, _ := e.post("/devices/"+itoa(dev.DeviceID)+"/mapping", url.Values{"folder": {itoa(f2)}, "action": {"delete"}}); st != 303 {
		t.Fatalf("delete mapping: %d", st)
	}
	eventually(t, "the mapping to be forgotten", func() bool {
		_, err := os.Stat(filepath.Join(root, "two", markerName))
		return err != nil
	})
	if _, err := os.Stat(filepath.Join(root, "two", "sub", "b.txt")); err != nil {
		t.Fatal("local file removed with the mapping")
	}
	if _, err := os.Stat(filepath.Join(dir, "folder-"+itoa(f2)+".db")); err == nil {
		t.Fatal("state database kept")
	}
}

func TestAgentUsesCachedConfigurationOffline(t *testing.T) {
	e := newCEnv(t)
	dev, api := e.newDevice()
	f := e.newFolder("One")
	root := filepath.Join(t.TempDir(), "one")
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	e.mapFolder(dev, f, root, true)
	// Fetch the configuration once, then run the agent against a dead server address.
	dir := t.TempDir()
	resp, err := api.Poll(context.Background(), proto0())
	if err != nil || resp.Config == nil {
		t.Fatalf("poll: %v %+v", err, resp)
	}
	if err := writeJSON(filepath.Join(dir, "config.json"), resp.Config); err != nil {
		t.Fatal(err)
	}
	dead := &API{Base: "https://127.0.0.1:1", Token: api.Token, HC: api.HC}
	a := &Agent{Dir: dir, API: dead, NewFS: cfs.OS, OS: runtime.GOOS, Stable: 0, ScanInterval: time.Hour, RetryAfter: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, "the marker from the cached configuration", func() bool {
		_, err := os.Stat(filepath.Join(root, markerName))
		return err == nil
	})
}

func TestAgentsPropagateChangesBothWays(t *testing.T) {
	e := newCEnv(t)
	f := e.newFolder("One")
	var roots [2]string
	for i := range roots {
		dev, api := e.newDevice()
		roots[i] = filepath.Join(t.TempDir(), "one")
		e.mapFolder(dev, f, roots[i], true)
		a := &Agent{Dir: t.TempDir(), API: api, NewFS: cfs.OS, OS: runtime.GOOS, DeviceName: dev.Name, Stable: 0,
			ScanInterval: time.Hour, RetryAfter: 50 * time.Millisecond, Debounce: 50 * time.Millisecond}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	read := func(root, p string) (string, bool) {
		b, err := os.ReadFile(filepath.Join(root, p))
		return string(b), err == nil
	}
	eventually(t, "both roots to be set up", func() bool {
		_, e1 := os.Stat(filepath.Join(roots[0], markerName))
		_, e2 := os.Stat(filepath.Join(roots[1], markerName))
		return e1 == nil && e2 == nil
	})
	writeFile(t, filepath.Join(roots[0], "sub", "a.txt"), "alpha")
	eventually(t, "a new file to reach the other device", func() bool { s, ok := read(roots[1], "sub/a.txt"); return ok && s == "alpha" })
	writeFile(t, filepath.Join(roots[1], "sub", "a.txt"), "beta")
	eventually(t, "the edit to come back", func() bool { s, _ := read(roots[0], "sub/a.txt"); return s == "beta" })
	os.Remove(filepath.Join(roots[0], "sub", "a.txt"))
	eventually(t, "the deletion to arrive", func() bool { _, ok := read(roots[1], "sub/a.txt"); return !ok })
}

// pollCounter counts the long polls an agent sends.
type pollCounter struct {
	base http.RoundTripper
	n    atomic.Int64
}

func (c *pollCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/poll") {
		c.n.Add(1)
	}
	return c.base.RoundTrip(r)
}

// A folder that cannot be synced (non-empty directory, not adopted) must not make the long poll return at once.
func TestUnusableFolderDoesNotSpinThePoll(t *testing.T) {
	e := newCEnv(t)
	e.srv.PollTimeout = 30 * time.Second
	dev, api := e.newDevice()
	f1, f2 := e.newFolder("One"), e.newFolder("Two")
	root1, root2 := filepath.Join(t.TempDir(), "one"), filepath.Join(t.TempDir(), "two")
	writeFile(t, filepath.Join(root1, "mine.txt"), "not yours")
	e.mapFolder(dev, f1, root1, false)
	e.mapFolder(dev, f2, root2, true)
	pc := &pollCounter{base: api.HC.Transport}
	api.HC.Transport = pc
	a := &Agent{Dir: t.TempDir(), API: api, NewFS: cfs.OS, OS: runtime.GOOS, DeviceName: dev.Name, ScanInterval: time.Hour, RetryAfter: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, "the usable folder to be set up", func() bool {
		_, err := os.Stat(filepath.Join(root2, markerName))
		return err == nil
	})
	time.Sleep(time.Second)
	if n := pc.n.Load(); n > 6 {
		t.Fatalf("%d polls in about a second: the poll spins", n)
	}
}

// A status report postponed by statusEvery must not leave a stale wake time behind: nextWake would stay zero and the loop would spin.
func TestStaleStatusRetryDoesNotSpinTheLoop(t *testing.T) {
	a := &Agent{runners: map[int64]*runner{1: {}}}
	canceled := false
	a.cancel, a.sig, a.pollAt = func() { canceled = true }, "outdated", time.Now()
	a.statusChanged()
	if a.statusAt.IsZero() || canceled {
		t.Fatal("the report should be postponed")
	}
	a.pollAt = time.Now().Add(-statusEvery)
	a.statusChanged() // the poll is no longer running (nobody restarts it here), so the due retry is consumed
	if !canceled || !a.statusAt.IsZero() {
		t.Fatalf("canceled=%v statusAt=%v", canceled, a.statusAt)
	}
	if d := a.nextWake(); d < time.Minute {
		t.Fatalf("nextWake %v: the loop would spin", d)
	}
}
