package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/ntfy"
	"kaze-sync/internal/proto"
)

func fakeNtfy(t *testing.T) (*ntfy.Notifier, chan string) {
	t.Helper()
	ch := make(chan string, 50)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- r.Header.Get("Title") + " | " + string(b)
	}))
	t.Cleanup(srv.Close)
	n, err := ntfy.New(ntfy.Config{URL: srv.URL, Topic: "t"}, "pc")
	if err != nil {
		t.Fatal(err)
	}
	return n, ch
}

func expect(t *testing.T, ch chan string, want string) {
	t.Helper()
	select {
	case m := <-ch:
		if !strings.Contains(m, want) {
			t.Fatalf("message %q, want it to contain %q", m, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no message containing %q", want)
	}
}

func expectNone(t *testing.T, ch chan string) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("unexpected message %q", m)
	case <-time.After(150 * time.Millisecond):
	}
}

// A folder that keeps failing is pushed once after the retries, not on every cycle, and its recovery once.
func TestNotifyFolderPushesAfterRetriesOnceAndRecovery(t *testing.T) {
	n, ch := fakeNtfy(t)
	a := &Agent{Notify: n}
	r := &runner{}
	r.cfg.ID, r.cfg.Name = 7, "Docs"
	boom := errors.New("secret path /home/x failed")
	for range failuresBeforePush - 1 {
		a.notifyFolder(r, Report{}, boom, true)
	}
	expectNone(t, ch)
	for range 20 {
		a.notifyFolder(r, Report{}, boom, true)
	}
	m := <-ch
	if !strings.Contains(m, "Docs") || strings.Contains(m, "secret") || strings.Contains(m, "/home") {
		t.Fatalf("message %q", m)
	}
	expectNone(t, ch)
	a.notifyFolder(r, Report{}, nil, true)
	expect(t, ch, "works again")
}

// While polls fail to connect, folder failures are the same outage and are not pushed.
func TestNotifyFolderSilentDuringOutage(t *testing.T) {
	n, ch := fakeNtfy(t)
	a := &Agent{Notify: n}
	r := &runner{}
	r.cfg.ID, r.cfg.Name = 7, "Docs"
	boom := errors.New("connection refused")
	a.pollDown.Store(true)
	for range 3 * failuresBeforePush {
		a.notifyFolder(r, Report{}, boom, true)
	}
	a.pollDown.Store(false)
	a.notifyFolder(r, Report{}, nil, true)
	expectNone(t, ch)
	for range failuresBeforePush { // a failure with the server reachable still counts
		a.notifyFolder(r, Report{}, boom, true)
	}
	<-ch
}

// Disk and permission errors count; files in use do not.
func TestNotifyFolderFileErrors(t *testing.T) {
	n, ch := fakeNtfy(t)
	a := &Agent{Notify: n}
	r := &runner{}
	r.cfg.ID, r.cfg.Name = 1, "Docs"
	inUse := Report{Errors: []proto.FileError{{Path: "a", Kind: proto.KindInUse}}}
	for range 5 {
		a.notifyFolder(r, inUse, nil, true)
	}
	expectNone(t, ch)
	bad := Report{Errors: []proto.FileError{{Path: "private/a.txt", Kind: proto.KindIO, Message: "disk full"}}}
	for range failuresBeforePush {
		a.notifyFolder(r, bad, nil, true)
	}
	m := <-ch
	if !strings.Contains(m, "1 files of folder Docs") || strings.Contains(m, "private") || strings.Contains(m, "disk full") {
		t.Fatalf("message %q", m)
	}
	a.notifyFolder(r, Report{}, nil, true)
	expect(t, ch, "no more file errors")
}

// A rejected device token stops sync: the user is told once, whatever the retries do.
func TestAgentPushesWhenTheTokenIsRejected(t *testing.T) {
	n, ch := fakeNtfy(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer ts.Close()
	api := &API{Base: ts.URL, Token: "revoked", HC: ts.Client()}
	a := &Agent{Dir: t.TempDir(), API: api, NewFS: cfs.OS, OS: runtime.GOOS, ScanInterval: time.Hour, RetryAfter: time.Hour, AuthRetry: 100 * time.Millisecond, Notify: n}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	expect(t, ch, "does not accept this device")
	expectNone(t, ch) // polls keep failing; no second message
}
