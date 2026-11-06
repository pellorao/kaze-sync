package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/ntfy"
)

func pushEnv(t *testing.T) (*env, chan string) {
	t.Helper()
	e := newEnv(t)
	ch := make(chan string, 50)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- string(b)
	}))
	t.Cleanup(srv.Close)
	n, err := ntfy.New(ntfy.Config{URL: srv.URL, Topic: "t"}, "Server")
	if err != nil {
		t.Fatal(err)
	}
	e.s.Push = n
	return e, ch
}

func pushGet(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no push")
	}
	return ""
}

func pushNone(t *testing.T, ch chan string) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("unexpected push %q", m)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestPushConflictsOnceThenNothingUntilGone(t *testing.T) {
	e, ch := pushEnv(t)
	f := e.folder("docs")
	e.s.pushCheck()
	pushNone(t, ch)
	e.commit(f, e.op("sub/report.conflict-laptop-20261101-153012.docx", 0, "c"))
	for range 5 {
		e.s.pushCheck()
	}
	m := pushGet(t, ch)
	if !strings.Contains(m, "1 conflict copies in folder docs") || strings.Contains(m, "report") || strings.Contains(m, "sub/") {
		t.Fatalf("message %q", m)
	}
	pushNone(t, ch)
}

func TestPushOfflineDeviceAndDisk(t *testing.T) {
	e, ch := pushEnv(t)
	e.enrollDevice("laptop", "linux")
	e.s.pushCheck()
	pushNone(t, ch)
	e.clk.Add(8 * 24 * time.Hour)
	e.s.pushCheck()
	if m := pushGet(t, ch); !strings.Contains(m, "Device laptop has not connected for 8 days") {
		t.Fatalf("message %q", m)
	}
	old := freeSpace
	defer func() { freeSpace = old }()
	freeSpace = func(string) (uint64, bool) { return 10 << 20, true }
	e.s.pushCheck()
	if m := pushGet(t, ch); !strings.Contains(m, "Free disk space") {
		t.Fatalf("message %q", m)
	}
	freeSpace = func(string) (uint64, bool) { return 10 << 30, true }
	e.s.pushCheck()
	if m := pushGet(t, ch); !strings.Contains(m, "fine again") {
		t.Fatalf("message %q", m)
	}
}

func TestPushOffIsSilentAndSafe(t *testing.T) {
	e := newEnv(t)
	e.folder("docs")
	e.s.pushCheck() // Push is nil: must not panic
}
