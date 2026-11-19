package fs

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var errInjected = errors.New("injected")

func write(t *testing.T, f FS, p, data string) {
	t.Helper()
	w, _, err := f.OpenAppend(p)
	if err != nil {
		t.Fatalf("OpenAppend(%q): %v", p, err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, f FS, p string) string {
	t.Helper()
	r, err := f.Open(p)
	if err != nil {
		t.Fatalf("Open(%q): %v", p, err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func names(t *testing.T, f FS, dir string) string {
	t.Helper()
	es, err := f.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return strings.Join(out, ",")
}

// conformance runs the same checks on every FS implementation. Whether names
// are case-insensitive is probed, so the OS case adapts to the host file system.
func conformance(t *testing.T, f FS) {
	t.Run("basics", func(t *testing.T) {
		if err := f.MkdirAll("d/e"); err != nil {
			t.Fatal(err)
		}
		write(t, f, "d/e/b.txt", "hello")
		write(t, f, "d/e/a.txt", "x")
		if got := names(t, f, "d/e"); got != "a.txt,b.txt" {
			t.Errorf("ReadDir = %q", got)
		}
		// OpenAppend appends and reports the existing length.
		w, n, err := f.OpenAppend("d/e/b.txt")
		if err != nil || n != 5 {
			t.Fatalf("OpenAppend = %v, %v", n, err)
		}
		io.WriteString(w, "!!")
		w.Close()
		if got := read(t, f, "d/e/b.txt"); got != "hello!!" {
			t.Errorf("content = %q", got)
		}
		if err := f.SetMeta("d/e/b.txt", 1_700_000_000_123_456_700, true); err != nil {
			t.Fatal(err)
		}
		e, err := f.Stat("d/e/b.txt")
		if err != nil || e.Size != 7 || e.IsDir || e.MtimeNs != 1_700_000_000_123_456_700 || e.Name != "b.txt" {
			t.Errorf("Stat = %+v, %v", e, err)
		}
		_, isOS := f.(osFS)
		if wantExec := !(isOS && runtime.GOOS == "windows"); e.Exec != wantExec {
			t.Errorf("Exec = %v", e.Exec)
		}
		if e, _ := f.Stat("d"); !e.IsDir {
			t.Error("d is not a directory")
		}
		if _, err := f.Stat("nope"); !isNotExist(err) {
			t.Errorf("Stat(missing) = %v", err)
		}
		if err := f.Remove("d"); err == nil {
			t.Error("Remove of non-empty directory succeeded")
		}
		if err := f.Sync("d/e/b.txt"); err != nil {
			t.Error(err)
		}
		if err := f.Remove("d/e/b.txt"); err != nil {
			t.Fatal(err)
		}
		if err := f.Remove("d/e/b.txt"); !isNotExist(err) {
			t.Errorf("second Remove = %v", err)
		}
	})

	t.Run("rename", func(t *testing.T) {
		write(t, f, "src", "new")
		write(t, f, "dst", "old")
		if err := f.Rename("src", "dst"); err != nil {
			t.Fatalf("Rename over existing: %v", err)
		}
		if got := read(t, f, "dst"); got != "new" {
			t.Errorf("dst = %q", got)
		}
		if _, err := f.Stat("src"); !isNotExist(err) {
			t.Errorf("src still exists: %v", err)
		}
		if err := f.Rename("missing", "x"); !isNotExist(err) {
			t.Errorf("Rename(missing) = %v", err)
		}
	})

	t.Run("case", func(t *testing.T) {
		f.MkdirAll("Case")
		write(t, f, "Case/File.TXT", "1")
		_, err := f.Stat("case/file.txt")
		insensitive := err == nil
		if insensitive {
			if e, _ := f.Stat("case"); !e.IsDir {
				t.Error("Stat(case) should find Case")
			}
			if got := names(t, f, "case"); got != "File.TXT" {
				t.Errorf("ReadDir via other case = %q", got)
			}
			if got := read(t, f, "CASE/file.txt"); got != "1" {
				t.Errorf("content via other case = %q", got)
			}
		} else if !isNotExist(err) {
			t.Fatalf("Stat = %v", err)
		}
		// A case-only rename shows the new name in ReadDir on both kinds of FS.
		if err := f.Rename("Case/File.TXT", "Case/file.txt"); err != nil {
			t.Fatal(err)
		}
		if got := names(t, f, "Case"); got != "file.txt" {
			t.Errorf("after case-only rename ReadDir = %q", got)
		}
		if got := read(t, f, "Case/file.txt"); got != "1" {
			t.Errorf("content after rename = %q", got)
		}
		if mem, ok := f.(*Mem); ok && mem.ci != insensitive {
			t.Errorf("probe says insensitive=%v, Mem.ci=%v", insensitive, mem.ci)
		}
	})
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

func TestOS(t *testing.T) { conformance(t, OS(t.TempDir())) }

func TestMem(t *testing.T) {
	for _, ci := range []bool{false, true} {
		t.Run(map[bool]string{false: "sensitive", true: "insensitive"}[ci], func(t *testing.T) {
			conformance(t, NewMem(MemOptions{CaseInsensitive: ci}))
		})
	}
}

func TestMemLockAndFail(t *testing.T) {
	m := NewMem(MemOptions{})
	write(t, m, "f", "data")
	write(t, m, "g", "other")
	m.Lock("f")
	if _, err := m.Open("f"); !errors.Is(err, ErrLocked) {
		t.Errorf("Open locked = %v", err)
	}
	if _, _, err := m.OpenAppend("f"); !errors.Is(err, ErrLocked) {
		t.Errorf("OpenAppend locked = %v", err)
	}
	if err := m.Rename("g", "f"); !errors.Is(err, ErrLocked) {
		t.Errorf("Rename onto locked = %v", err)
	}
	if err := m.Rename("f", "h"); !errors.Is(err, ErrLocked) {
		t.Errorf("Rename of locked = %v", err)
	}
	if err := m.Remove("f"); !errors.Is(err, ErrLocked) {
		t.Errorf("Remove locked = %v", err)
	}
	if got := names(t, m, ""); got != "f,g" {
		t.Errorf("names = %q", got)
	}
	m.Unlock("f")
	if err := m.Rename("g", "f"); err != nil {
		t.Errorf("Rename after Unlock = %v", err)
	}
}

func TestFaultTransport(t *testing.T) {
	var got []int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, len(b))
		mu.Unlock()
		if err != nil {
			return
		}
	}))
	defer srv.Close()
	ft := NewFaultTransport(nil)
	client := &http.Client{Transport: ft}
	post := func(n int) error {
		resp, err := client.Post(srv.URL, "application/octet-stream", strings.NewReader(strings.Repeat("x", n)))
		if err == nil {
			resp.Body.Close()
		}
		return err
	}

	ft.FailAfter(1, errInjected)
	if err := post(10); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if err := post(10); !errors.Is(err, errInjected) {
		t.Fatalf("second request: %v", err)
	}
	if err := post(10); err != nil {
		t.Fatalf("third request: %v", err)
	}
	mu.Lock()
	n := len(got)
	got = nil
	mu.Unlock()
	if n != 2 {
		t.Errorf("server saw %d requests, want 2 (failed one is not sent)", n)
	}

	ft.CutBodyAfter(0, 4000, errInjected)
	if err := post(100000); !errors.Is(err, errInjected) {
		t.Fatalf("cut request: %v", err)
	}
	if err := post(10); err != nil {
		t.Fatalf("request after cut: %v", err)
	}
	// The handler of the cut request records its body when it notices the cut, which may be after the next request.
	sawBodies := func(sizes ...int) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, want := range sizes {
			if !slices.Contains(got, want) {
				return false
			}
		}
		return true
	}
	for end := time.Now().Add(5 * time.Second); !sawBodies(4000, 10) && time.Now().Before(end); {
		time.Sleep(time.Millisecond)
	}
	if !sawBodies(4000, 10) {
		t.Errorf("server saw bodies of %v bytes", got)
	}
}

// No operation of the real file system goes through a symbolic link that is a directory of the path (design 6.7).
func TestOSRefusesToWorkBelowALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating links needs privileges there")
	}
	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	os.MkdirAll(root, 0o755)
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "victim"), []byte("v"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "lnk")); err != nil {
		t.Fatal(err)
	}
	f := OS(root)
	w, _, err := f.OpenAppend("lnk/new")
	if w != nil {
		w.Close()
	}
	checks := map[string]error{
		"OpenAppend": err,
		"MkdirAll":   f.MkdirAll("lnk/sub/dir"),
		"Remove":     f.Remove("lnk/victim"),
		"Rename":     f.Rename("lnk/victim", "moved"),
		"Symlink":    f.Symlink("x", "lnk/l"),
		"SetMeta":    f.SetMeta("lnk/victim", 1, true),
	}
	_, checks["Open"] = f.Open("lnk/victim")
	_, checks["Readlink"] = f.Readlink("lnk/victim")
	_, checks["ReadDir"] = f.ReadDir("lnk")
	for name, err := range checks {
		if !errors.Is(err, ErrLinkOnPath) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Fatalf("outside was changed: %v", entries)
	}
	// The link itself is an ordinary entry: it can be read, replaced and removed.
	if e, err := f.Stat("lnk"); err != nil || !e.IsLink || e.IsDir {
		t.Fatalf("Stat(lnk) = %+v %v", e, err)
	}
	if tg, err := f.Readlink("lnk"); err != nil || tg != outside {
		t.Fatalf("Readlink(lnk) = %q %v", tg, err)
	}
	if err := f.Remove("lnk"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "victim")); err != nil {
		t.Fatal("removing the link removed its target's content")
	}
}
