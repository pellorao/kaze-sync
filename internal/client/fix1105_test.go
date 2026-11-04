package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// conflictName gives up with an error instead of probing names without end.
func TestConflictNameGivesUp(t *testing.T) {
	_, _, _, g := setup(t)
	g.f.DeviceName = "pc"
	at := time.Unix(1_700_000_000, 0)
	g.f.Now = func() time.Time { return at }
	g.put("a.txt", []byte("x"))
	q, err := g.f.conflictName("a.txt")
	if err != nil || q != "a.conflict-pc-"+at.Format("20060102-150405")+".txt" {
		t.Fatalf("first name %q %v", q, err)
	}
	for i := 0; i < maxConflictNames; i++ {
		g.put("a.conflict-pc-"+at.Add(time.Duration(i)*time.Second).Format("20060102-150405")+".txt", []byte("x"))
	}
	if q, err := g.f.conflictName("a.txt"); err == nil {
		t.Fatalf("a free name %q although %d are taken", q, maxConflictNames)
	}
}

// A block taken from a batch response must have the length the entry says.
func TestPrefetchedBlockWithTheWrongLengthIsRefused(t *testing.T) {
	e := &proto.Entry{Path: "f", Size: 4, Blocks: []proto.Hash{{1}}}
	r := &blockReader{ctx: context.Background(), e: e, prefetch: map[proto.Hash][]byte{{1}: []byte("abc")}}
	err := r.load(0)
	var re remoteError
	if !errors.As(err, &re) {
		t.Fatalf("load: %v (%s)", err, fmt.Sprint(r.cur))
	}
}

// pullCounter counts the requests that read the change list (a pull).
type pullCounter struct {
	base http.RoundTripper
	n    atomic.Int64
}

func (c *pullCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "/changes") {
		c.n.Add(1)
	}
	return c.base.RoundTrip(r)
}

// The agent starts before the server is reachable (the cached configuration lets the folder run): the pull
// that fails must be retried with growing delays, not in a loop (11-04: 190000 lines in 33 seconds).
func TestAgentDoesNotSpinWhileTheServerIsUnreachable(t *testing.T) {
	e := newCEnv(t)
	dev, api := e.newDevice()
	f := e.newFolder("One")
	root := filepath.Join(t.TempDir(), "one")
	e.mapFolder(dev, f, root, true)
	dir := t.TempDir()
	resp, err := api.Poll(context.Background(), proto0())
	if err != nil || resp.Config == nil {
		t.Fatalf("poll: %v %+v", err, resp)
	}
	if err := writeJSON(filepath.Join(dir, "config.json"), resp.Config); err != nil {
		t.Fatal(err)
	}
	pc := &pullCounter{base: api.HC.Transport}
	hc := *api.HC
	hc.Transport = pc
	dead := &API{Base: "https://127.0.0.1:1", Token: api.Token, HC: &hc}
	a := &Agent{Dir: dir, API: dead, NewFS: cfs.OS, OS: runtime.GOOS, Stable: 0, ScanInterval: time.Hour, RetryAfter: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	time.Sleep(1500 * time.Millisecond)
	// attempts at about 0, 0.2, 0.6 and 1.4 s
	if n := pc.n.Load(); n > 8 {
		t.Fatalf("%d pulls in 1.5 s against a dead server", n)
	}
}
