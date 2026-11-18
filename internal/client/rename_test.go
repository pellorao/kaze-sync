package client

import (
	"context"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
)

// A device renamed on the server keeps syncing with its token, takes the new name from the configuration for its
// folders, and writes it into device.json (design 2.5).
func TestAgentTakesTheNewDeviceName(t *testing.T) {
	e := newCEnv(t)
	dir := t.TempDir()
	if err := Enroll(context.Background(), dir, e.ts.URL, e.code(), e.fp); err != nil {
		t.Fatal(err)
	}
	dev, err := LoadDevice(dir)
	if err != nil {
		t.Fatal(err)
	}
	hc, err := NewHTTPClient(e.fp)
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Base: dev.Server, Token: dev.Token, HC: hc}
	f := e.newFolder("Docs")
	root := filepath.Join(t.TempDir(), "docs")
	e.mapFolder(dev, f, root, true)
	a := &Agent{Dir: dir, API: api, NewFS: cfs.OS, OS: runtime.GOOS, DeviceName: dev.Name, Stable: 0, Debounce: 50 * time.Millisecond,
		ScanInterval: time.Hour, RetryAfter: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	stopped := false
	stop := func() {
		if !stopped {
			cancel()
			<-done
			stopped = true
		}
	}
	defer stop()
	writeFile(t, filepath.Join(root, "a.txt"), "one")
	eventually(t, "a.txt to be uploaded", func() bool {
		return e.count(f, `SELECT count(*) FROM files WHERE path='a.txt' AND device=?`, dev.Name) == 1
	})

	if st, body := e.post("/devices/"+strconv.FormatInt(dev.DeviceID, 10)+"/rename", url.Values{"name": {"renamed"}}); st != 303 {
		t.Fatalf("rename: %d %s", st, body)
	}
	eventually(t, "device.json to hold the new name", func() bool {
		d, err := LoadDevice(dir)
		return err == nil && d.Name == "renamed" && d.Token == dev.Token
	})
	eventually(t, "the folder to use the new name", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		r := a.runners[f]
		return r != nil && !r.busy && r.f.DeviceName == "renamed"
	})
	writeFile(t, filepath.Join(root, "b.txt"), "two")
	eventually(t, "b.txt to be uploaded under the new name", func() bool {
		return e.count(f, `SELECT count(*) FROM files WHERE path='b.txt' AND device='renamed'`) == 1
	})

	// Another enrollment in the same state directory is not overwritten by this agent's name.
	stop()
	other := dev
	other.Token, other.Name = "another", "other"
	if err := SaveDevice(dir, other); err != nil {
		t.Fatal(err)
	}
	a.DeviceName = "x"
	a.saveName()
	if d, _ := LoadDevice(dir); d.Name != "other" {
		t.Fatalf("device.json of another enrollment rewritten: %q", d.Name)
	}
}
