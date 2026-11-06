package ntfy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type msg struct{ path, title, body, prio string }

func fake(t *testing.T) (*Notifier, chan msg, *time.Time) {
	t.Helper()
	ch := make(chan msg, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- msg{r.URL.Path, r.Header.Get("Title"), string(b), r.Header.Get("Priority")}
	}))
	t.Cleanup(srv.Close)
	n, err := New(Config{URL: srv.URL + "/", Topic: "t-opic"}, "pc")
	if err != nil || n == nil {
		t.Fatal(n, err)
	}
	now := time.Unix(1e9, 0)
	n.now = func() time.Time { return now }
	return n, ch, &now
}

func get(t *testing.T, ch chan msg) msg {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no message")
	}
	return msg{}
}

func none(t *testing.T, ch chan msg) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("unexpected message %+v", m)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestSendsOnceAfterThresholdThenRecovered(t *testing.T) {
	n, ch, _ := fake(t)
	n.Fail("poll", "cannot reach the server", 3)
	n.Fail("poll", "cannot reach the server", 3)
	none(t, ch)
	n.Fail("poll", "cannot reach the server", 3)
	m := get(t, ch)
	if m.path != "/t-opic" || m.body != "cannot reach the server" || m.prio != "high" || !strings.HasPrefix(m.title, "[pc] Problem") {
		t.Fatalf("%+v", m)
	}
	for range 50 { // a retry loop cannot spam
		n.Fail("poll", "cannot reach the server", 3)
	}
	none(t, ch)
	n.Ok("poll", "the server is reachable again")
	if m := get(t, ch); !strings.Contains(m.title, "Recovered") || m.body != "the server is reachable again" {
		t.Fatalf("%+v", m)
	}
	n.Ok("poll", "again") // already cleared: nothing
	none(t, ch)
}

func TestNoRecoveredWhenNothingWasSent(t *testing.T) {
	n, ch, _ := fake(t)
	n.Fail("a", "x", 3)
	n.Ok("a", "back")
	none(t, ch)
}

func TestFlappingIsHeldBackForAnHour(t *testing.T) {
	n, ch, now := fake(t)
	n.Fail("a", "bad", 1)
	get(t, ch)
	n.Ok("a", "good")
	get(t, ch)
	n.Fail("a", "bad", 1) // within the hour: held
	none(t, ch)
	*now = now.Add(Interval)
	n.Fail("a", "bad", 1) // still failing an hour later: sent
	if m := get(t, ch); m.body != "bad" {
		t.Fatal(m)
	}
}

func TestDistinctProblemsAreIndependentAndCapped(t *testing.T) {
	n, ch, _ := fake(t)
	n.Fail("a", "A", 1)
	n.Fail("b", "B", 1)
	get(t, ch)
	get(t, ch)
	for i := range 40 {
		n.Fail(string(rune('A'+i)), "x", 1)
	}
	got := 0
	for {
		select {
		case <-ch:
			got++
			continue
		case <-time.After(300 * time.Millisecond):
		}
		break
	}
	if got != globalCap-2 {
		t.Fatalf("got %d messages after the first two, want %d", got, globalCap-2)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if n, err := Load(dir, "x"); n != nil || err != nil {
		t.Fatal("missing file must disable", n, err)
	}
	f := filepath.Join(dir, File)
	os.WriteFile(f, []byte(`{"url":"","topic":""}`), 0o600)
	if n, err := Load(dir, "x"); n != nil || err != nil {
		t.Fatal("empty default must disable", n, err)
	}
	os.WriteFile(f, []byte(`{"url":"ftp://h","topic":"t"}`), 0o600)
	if _, err := Load(dir, "x"); err == nil {
		t.Fatal("bad scheme accepted")
	}
	os.WriteFile(f, []byte(`{"url":"https://ntfy.example.org","topic":"t"}`), 0o600)
	if n, err := Load(dir, "x"); n == nil || err != nil || n.endpoint != "https://ntfy.example.org/t" {
		t.Fatal(n, err)
	}
	var nilN *Notifier
	nilN.Fail("a", "b", 1) // must not panic
	nilN.Ok("a", "b")
}

func TestUnreachableServerDoesNotBlockOrPanic(t *testing.T) {
	n, _ := New(Config{URL: "http://127.0.0.1:1", Topic: "t"}, "pc")
	start := time.Now()
	n.Fail("a", "x", 1)
	if time.Since(start) > time.Second {
		t.Fatal("Fail blocked")
	}
}
