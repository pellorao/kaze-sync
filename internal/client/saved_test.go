package client

import (
	"errors"
	cfs "kaze-sync/internal/client/fs"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

// lockedPair is two devices over one folder where the first holds "code" exclusively and the second knows it.
func lockedPair(t *testing.T) (e *cenv, f int64, g1, g2 *engine) {
	t.Helper()
	e, f, g1, g2, _ = pair(t)
	api2 := g2.f.API
	g1.put("code/a.txt", []byte("one"))
	g1.put("code/d/b.txt", []byte("bee"))
	g1.sync()
	g2.sync()
	if st, _ := e.post("/folders/"+strconv.FormatInt(f, 10)+"/lock", url.Values{"path": {"code"}, "device": {"1"}}); st != 303 && st != 200 {
		t.Fatalf("lock: %d", st)
	}
	g2.setRanges(api2, f, "code")
	g2.sync()
	g2.wantRO("code", true)
	return
}

func (g *engine) saved() (files []string) {
	var walk func(string)
	walk = func(d string) {
		es, _ := g.fs.ReadDir(d)
		for _, c := range es {
			q := d + "/" + c.Name
			if c.IsDir {
				walk(q)
			} else {
				files = append(files, q)
			}
		}
	}
	walk(".syncsaved")
	return
}

func (g *engine) lockedN(rg string) int {
	for _, e := range g.f.lockedErrors() {
		if e.Path == rg {
			return e.N
		}
	}
	return 0
}

func TestReadonlyLocalChangesAreSavedAndServerVersionComesBack(t *testing.T) {
	e, f, g1, g2 := lockedPair(t)
	g2.f.Unprotect() // the user lifts the read-only flags by hand
	g2.put("code/a.txt", []byte("mine"))
	g2.put("code/new.txt", []byte("n"))
	g2.fs.Remove("code/d/b.txt")
	rep := g2.scan()
	if rep.Committed != 0 {
		t.Fatalf("committed %+v", rep)
	}
	g2.sync()
	for p, want := range map[string]string{"code/a.txt": "one", "code/d/b.txt": "bee"} {
		if got, _ := g2.read(p); got != want {
			t.Errorf("%s = %q, want the server's %q", p, got, want)
		}
	}
	if _, ok := g2.read("code/new.txt"); ok {
		t.Error("the new file stayed")
	}
	if got, _ := e.serverFile(f, "code/a.txt"); string(got) != "one" {
		t.Errorf("server a.txt = %q", got)
	}
	if _, ok := e.serverFile(f, "code/new.txt"); ok {
		t.Error("the new file reached the server")
	}
	saved := g2.saved()
	if len(saved) != 2 {
		t.Fatalf("saved %v", saved)
	}
	for _, s := range saved {
		if !strings.HasSuffix(s, "/code/a.txt") && !strings.HasSuffix(s, "/code/new.txt") {
			t.Errorf("unexpected %s", s)
		}
	}
	g2.wantRO("code", true)
	if n := g2.lockedN("code"); n != 2 {
		t.Errorf("N = %d", n)
	}
	rep = g2.scan()
	var got proto.FileError
	for _, er := range rep.Errors {
		if er.Kind == proto.KindRejected {
			got = er
		}
	}
	if got.Path != "code" || got.N != 2 || rep.State != "error" {
		t.Errorf("report %+v", rep)
	}
	// Nothing reached the other device either.
	g1.sync()
	if got, _ := g1.read("code/a.txt"); got != "one" {
		t.Errorf("g1 a.txt = %q", got)
	}
	// The user deletes what was saved: the warning goes.
	for _, s := range saved {
		g2.fs.Remove(s)
	}
	g2.scan()
	if n := g2.lockedN("code"); n != 0 {
		t.Errorf("N = %d after emptying .syncsaved", n)
	}
}

func TestReadonlyOfflineLeavesLocalChangesAlone(t *testing.T) {
	_, _, _, g2 := lockedPair(t)
	g2.f.Unprotect()
	g2.put("code/a.txt", []byte("mine"))
	api := *g2.f.API
	api.Base = "http://127.0.0.1:1"
	good := g2.f.API
	g2.f.API = &api
	if _, err := g2.f.Scan(t.Context()); err == nil {
		t.Fatal("scan did not fail offline")
	}
	if got, _ := g2.read("code/a.txt"); got != "mine" || len(g2.saved()) != 0 {
		t.Errorf("touched: %q %v", got, g2.saved())
	}
	g2.f.API = good
	g2.sync()
	if got, _ := g2.read("code/a.txt"); got != "one" || len(g2.saved()) != 1 {
		t.Errorf("online: %q %v", got, g2.saved())
	}
}

func TestReadonlyStaleConfigLockedRejection(t *testing.T) {
	e, f, g1, g2 := lockedPair(t)
	g2.setRanges(g2.f.API, f) // the device has not heard of the exclusive yet
	g2.f.Unprotect()
	g2.put("code/d/b.txt", []byte("mine"))
	if rep := g2.scan(); rep.Committed != 0 {
		t.Fatalf("committed %+v", rep)
	}
	g2.sync()
	if got, _ := g2.read("code/d/b.txt"); got != "bee" {
		t.Errorf("b.txt = %q", got)
	}
	if len(g2.saved()) != 1 {
		t.Errorf("saved %v", g2.saved())
	}
	if got, _ := e.serverFile(f, "code/d/b.txt"); string(got) != "bee" {
		t.Errorf("server b.txt = %q", got)
	}
	_ = g1
}

func TestReadonlyPullKeepsUnscannedLocalChange(t *testing.T) {
	_, _, g1, g2 := lockedPair(t)
	g2.f.Unprotect()
	g2.put("code/a.txt", []byte("mine")) // not scanned yet
	g1.put("code/a.txt", []byte("two"))
	g1.f.Readonly = nil
	g1.sync()
	g2.pull()
	if got, _ := g2.read("code/a.txt"); got != "two" {
		t.Errorf("a.txt = %q", got)
	}
	if s := g2.saved(); len(s) != 1 || !strings.HasSuffix(s[0], "/code/a.txt") {
		t.Errorf("saved %v", s)
	}
}

// failFS fails the calls named in it.
type failFS struct {
	cfs.FS
	noWritable string // SetWritable(p, true) of this path fails
	noRename   bool
}

func (x failFS) SetWritable(p string, w bool) error {
	if w && p == x.noWritable {
		return errors.New("injected")
	}
	return x.FS.SetWritable(p, w)
}

func (x failFS) Rename(a, b string) error {
	if x.noRename {
		return errors.New("injected")
	}
	return x.FS.Rename(a, b)
}

// A refused move is reported, leaves the file where it was, and read-only.
func TestSaveAwayReportsFailures(t *testing.T) {
	_, _, _, g2 := lockedPair(t)
	inner := g2.f.guard.FS
	g2.f.guard.FS = failFS{FS: inner, noWritable: "code/a.txt"}
	if err := g2.f.saveAway("code/a.txt"); err == nil {
		t.Fatal("a failed unlock was not reported")
	}
	g2.f.guard.FS = failFS{FS: inner, noRename: true}
	if err := g2.f.saveAway("code/a.txt"); err == nil {
		t.Fatal("a failed move was not reported")
	}
	g2.f.guard.FS = inner
	g2.wantRO("code", true)
	if n := len(g2.saved()); n != 0 {
		t.Fatalf("%d files saved", n)
	}
}
