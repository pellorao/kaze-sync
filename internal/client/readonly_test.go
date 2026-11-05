package client

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// roPair is a pair whose second device is rebuilt with the read-only ranges over its own file system.
func roPair(t *testing.T, windows bool, ranges ...string) (g1, g2 *engine, api2 *API, f int64) {
	t.Helper()
	e, f, g1, g2, _ := pair(t)
	api2 = g2.f.API
	g2.fs = cfs.NewMem(cfs.MemOptions{Windows: windows, Clock: 2_000_000_000_000})
	g2.restart(api2, proto.FolderConfig{ID: f, Adopt: true}, g2.fs)
	_ = e
	if len(ranges) > 0 {
		g2.setRanges(api2, f, ranges...)
	}
	return
}

func (g *engine) setRanges(api *API, f int64, ranges ...string) {
	g.restart(api, proto.FolderConfig{ID: f, Adopt: true, Readonly: ranges}, g.fs)
}

// wantRO checks the read-only state of every entry below p (p included).
func (g *engine) wantRO(p string, ro bool) {
	g.t.Helper()
	var walk func(string)
	walk = func(q string) {
		st, err := g.fs.Stat(q)
		if err != nil {
			g.t.Fatalf("stat %q: %v", q, err)
		}
		if st.IsDir && q != "" && strings.Contains(g.t.Name(), "Windows") {
			// directories have no flag on Windows
		} else if st.ReadOnly != ro {
			g.t.Errorf("%q: read-only = %v, want %v", q, st.ReadOnly, ro)
		}
		if st.IsDir {
			es, _ := g.fs.ReadDir(q)
			for _, c := range es {
				walk(filepath.ToSlash(filepath.Join(q, c.Name)))
			}
		}
	}
	walk(p)
}

func TestReadonlyRangeIsProtectedAndPullStillWrites(t *testing.T) {
	g1, g2, _, _ := roPair(t, false, "code")
	g1.put("code/a.txt", []byte("one"))
	g1.put("code/d/b.txt", []byte("bee"))
	g1.put("free/c.txt", []byte("c"))
	g1.sync()
	g2.sync()
	g2.wantRO("code", true)
	g2.wantRO("free", false)
	if st, _ := g2.fs.Stat(""); st.ReadOnly {
		t.Error("root is read-only")
	}
	// The server's changes keep coming in: modify, add, delete (which empties and removes d).
	g1.put("code/a.txt", []byte("two"))
	g1.put("code/new/n.txt", []byte("n"))
	g1.fs.Remove("code/d/b.txt")
	g1.fs.Remove("code/d")
	g1.sync()
	g2.sync()
	if got, _ := g2.read("code/a.txt"); got != "two" {
		t.Errorf("a.txt = %q", got)
	}
	if _, ok := g2.read("code/new/n.txt"); !ok {
		t.Error("new file missing")
	}
	if _, err := g2.fs.Stat("code/d"); err == nil {
		t.Error("empty directory was not removed")
	}
	g2.wantRO("code", true)
	if rep := g2.scan(); rep.Committed != 0 {
		t.Errorf("protection looked like a change: %+v", rep)
	}
}

func TestReadonlyRangeWindowsOnlyFiles(t *testing.T) {
	g1, g2, _, _ := roPair(t, true, "code")
	g1.put("code/d/b.txt", []byte("bee"))
	g1.sync()
	g2.sync()
	g2.wantRO("code/d/b.txt", true)
	st, _ := g2.fs.Stat("code/d")
	if st.ReadOnly {
		t.Error("a Windows directory got the flag")
	}
	g1.put("code/d/b.txt", []byte("two"))
	g1.sync()
	g2.sync()
	if got, _ := g2.read("code/d/b.txt"); got != "two" {
		t.Errorf("b.txt = %q", got)
	}
	g2.wantRO("code/d/b.txt", true)
}

func TestReadonlyWholeFolder(t *testing.T) {
	g1, g2, _, _ := roPair(t, false, "")
	g1.put("a.txt", []byte("one"))
	g1.sync()
	g2.sync()
	g2.wantRO("a.txt", true)
	if st, _ := g2.fs.Stat(""); !st.ReadOnly {
		t.Error("root is not read-only")
	}
	g1.put("b/c.txt", []byte("c"))
	g1.sync()
	g2.sync()
	if got, _ := g2.read("b/c.txt"); got != "c" {
		t.Errorf("c.txt = %q", got)
	}
}

func TestReadonlyReleaseTransferAndUnprotect(t *testing.T) {
	g1, g2, api, f := roPair(t, false, "code", "docs")
	g1.put("code/a.txt", []byte("one"))
	g1.put("docs/x.txt", []byte("x"))
	g1.sync()
	g2.sync()
	g2.wantRO("code", true)
	g2.wantRO("docs", true)
	// docs is released, code stays.
	g2.setRanges(api, f, "code")
	g2.sync()
	g2.wantRO("code", true)
	g2.wantRO("docs", false)
	// Unprotect gives everything back and forgets the ranges; a partial scan does not protect again.
	g2.f.Unprotect()
	g2.wantRO("code", false)
	if v := g2.f.DB.metaString(readonlyMeta); v != "" {
		t.Errorf("meta = %q", v)
	}
	// The same configuration protects again at the next whole scan, unless the device is suspended.
	g2.f.suspend.Store(true)
	g2.scan()
	g2.wantRO("code", false)
	g2.f.suspend.Store(false)
	g2.scan()
	g2.wantRO("code", true)
}

func TestReadonlyRevokedAndForgottenMapping(t *testing.T) {
	g1, g2, _, _ := roPair(t, false, "code")
	g1.put("code/a.txt", []byte("one"))
	g1.sync()
	g2.sync()
	a := &Agent{runners: map[int64]*runner{}, revoked: true}
	a.runners[1] = &runner{f: g2.f, done: make(chan struct{})}
	a.unprotectRevoked()
	g2.wantRO("code", false)
	g2.scan() // protects again: only the poll flips suspend in the agent
	g2.wantRO("code", true)
	a.revoked = false
	r := &runner{f: g2.f, done: make(chan struct{}), dbPath: filepath.Join(t.TempDir(), "x.db")}
	a.stop(r, true)
	g2.wantRO("code", false)
}

func TestReadonlyRepairsAfterCrashes(t *testing.T) {
	for n := 0; n < 12; n++ {
		g1, g2, api, f := roPair(t, false, "code")
		g1.put("code/a.txt", []byte("one"))
		g1.put("code/d/b.txt", []byte("bee"))
		g1.sync()
		g2.sync()
		g1.put("code/a.txt", []byte("two"))
		g1.put("code/e/n.txt", []byte("n"))
		g1.fs.Remove("code/d/b.txt")
		g1.fs.Remove("code/d")
		g1.sync()
		g2.fs.FailAfter(n, errors.New("injected"))
		g2.f.Pull(t.Context()) // may fail anywhere, leaving things writable
		g2.setRanges(api, f, "code")
		g2.sync()
		g2.wantRO("code", true)
		if got, _ := g2.read("code/a.txt"); got != "two" {
			t.Errorf("n=%d: a.txt = %q", n, got)
		}
	}
}
