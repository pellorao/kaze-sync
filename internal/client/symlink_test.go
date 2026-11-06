package client

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

func (g *engine) link(target, p string) {
	g.t.Helper()
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		if err := g.fs.MkdirAll(p[:i]); err != nil {
			g.t.Fatal(err)
		}
	}
	g.fs.Remove(p)
	if err := g.fs.Symlink(target, p); err != nil {
		g.t.Fatal(err)
	}
}

func (g *engine) readlink(p string) (string, bool) {
	t, err := g.fs.Readlink(p)
	return t, err == nil
}

// Design 6.7: a symbolic link syncs as a link, with its target verbatim (also an absolute or dangling one), is
// retargeted, replaced by a file and back, and deleted.
func TestSymbolicLinksSync(t *testing.T) {
	e, f, g1, g2, _ := pair(t)
	g1.link("../outside/dangling", "d/l1")
	g1.link("/abs/path", "l2")
	g1.put("plain", []byte("../outside/dangling")) // the same bytes as a link target, but a file
	g1.sync()
	g2.sync()
	for p, want := range map[string]string{"d/l1": "../outside/dangling", "l2": "/abs/path"} {
		if got, ok := g2.readlink(p); !ok || got != want {
			t.Fatalf("%s -> %q %v", p, got, ok)
		}
	}
	if got, ok := g2.read("plain"); !ok || got != "../outside/dangling" {
		t.Fatalf("plain file = %q %v", got, ok)
	}
	if n := e.count(f, `SELECT count(*) FROM files WHERE link=1 AND hash IS NOT NULL`); n != 2 {
		t.Fatalf("%d links on the server", n)
	}
	if rep := g2.scan(); rep.Committed != 0 {
		t.Fatalf("downloaded links committed back: %+v", rep)
	}

	g1.link("new-target", "l2") // retarget
	g1.sync()
	g2.sync()
	if got, _ := g2.readlink("l2"); got != "new-target" {
		t.Fatalf("retargeted link -> %q", got)
	}

	g1.fs.Remove("l2") // link -> file of the same name
	g1.put("l2", []byte("now a file"))
	g1.sync()
	g2.sync()
	if _, ok := g2.readlink("l2"); ok {
		t.Fatal("l2 is still a link")
	}
	if got, _ := g2.read("l2"); got != "now a file" {
		t.Fatalf("l2 = %q", got)
	}

	g1.link("back", "l2") // file -> link
	g1.sync()
	g2.sync()
	if got, _ := g2.readlink("l2"); got != "back" {
		t.Fatalf("l2 -> %q", got)
	}

	g1.fs.Remove("d/l1")
	g1.sync()
	g2.sync()
	if _, ok := g2.readlink("d/l1"); ok {
		t.Fatal("deleted link still there")
	}
}

// A machine that cannot create links leaves them on the server, says so in one stable notice, and creates them
// by itself once it can (design 6.7).
func TestSymbolicLinksCannotBeCreated(t *testing.T) {
	_, _, g1, g2, gets := pair(t)
	g2.fs.SetNoSymlinks(true)
	g1.link("target-a", "a")
	g1.link("target-b", "dir/b")
	g1.put("file", []byte("data"))
	g1.sync()
	g2.sync()
	if got, _ := g2.read("file"); got != "data" {
		t.Fatalf("file = %q", got)
	}
	notice := func() map[string]proto.FileError {
		out := map[string]proto.FileError{}
		for _, e := range g2.pull().Errors {
			out[e.Path] = e
		}
		return out
	}
	n := notice()
	if len(n) != 2 || n["a"].Kind != proto.KindSkipped || !strings.HasPrefix(n["a"].Message, proto.LinkNotCreated) || n["dir/b"].Kind != proto.KindSkipped {
		t.Fatalf("notice: %+v", n)
	}
	if _, ok := g2.readlink("a"); ok {
		t.Fatal("a link appeared")
	}
	before, requests := n["a"].Since, gets.requests.Load()
	n2 := notice()
	if len(n2) != 2 || n2["a"].Since != before || gets.requests.Load() != requests {
		t.Fatalf("the notice changed or links were fetched again: %+v (%d -> %d requests)", n2, requests, gets.requests.Load())
	}
	if rep := g2.scan(); rep.Committed != 0 || len(rep.Errors) != 2 {
		t.Fatalf("scan: %+v", rep)
	}

	g1.fs.Remove("dir/b") // gone on the server: gone from the notice
	g1.sync()
	if n := notice(); len(n) != 1 {
		t.Fatalf("notice after delete: %+v", n)
	}

	g2.fs.SetNoSymlinks(false) // Developer Mode on
	g2.pull()
	if got, _ := g2.readlink("a"); got != "target-a" {
		t.Fatalf("a -> %q", got)
	}
	if n := notice(); len(n) != 0 {
		t.Fatalf("notice remains: %+v", n)
	}
	if rep := g2.scan(); rep.Committed != 0 {
		t.Fatalf("created link committed back: %+v", rep)
	}
}

// A target that is empty or longer than MaxLinkTarget is not synced and says so.
func TestSymbolicLinkTargetTooLong(t *testing.T) {
	_, _, g1, g2, _ := pair(t)
	g1.link(strings.Repeat("x", proto.MaxLinkTarget+1), "long")
	g1.link(strings.Repeat("y", proto.MaxLinkTarget), "limit")
	rep := g1.scan()
	if len(rep.Errors) != 1 || rep.Errors[0].Path != "long" || rep.Errors[0].Kind != proto.KindSkipped {
		t.Fatalf("report: %+v", rep.Errors)
	}
	g2.sync()
	if _, ok := g2.readlink("long"); ok {
		t.Fatal("too long link synced")
	}
	if got, ok := g2.readlink("limit"); !ok || len(got) != proto.MaxLinkTarget {
		t.Fatalf("limit link: %d %v", len(got), ok)
	}
}

// A link that points outside the folder is synced as a link and never followed: not on scan, not on download.
func TestSymbolicLinkOutsideTheFolderIsNeverFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating links needs privileges there")
	}
	_, f, g1, g2, _ := pair(t)
	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	os.MkdirAll(root, 0o755)
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("do not sync"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside/secret", filepath.Join(root, "file-link")); err != nil {
		t.Fatal(err)
	}
	// g2 works on the real directory; the server holds a file below the name of the local link.
	g2.fs = nil
	g2.restart(g2.f.API, proto.FolderConfig{ID: f, Adopt: true}, cfs.OS(root))
	g1.put("out/x", []byte("from the server"))
	g1.put("out2/y", []byte("another"))
	g1.sync()

	g2.scan() // the link `out` is refused (a file where a directory is), `file-link` is committed as a link
	g2.pull()
	g2.sync()
	if entries, _ := os.ReadDir(outside); len(entries) != 1 || entries[0].Name() != "secret" {
		t.Fatalf("something was written outside the folder: %v", entries)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret")); string(b) != "do not sync" {
		t.Fatalf("outside file changed: %q", b)
	}
	// The link where the server has a directory is held (design 6.6); discarding it makes room for the directory.
	if rows, _ := g2.f.DB.held(""); len(rows) != 1 || rows[0].Path != "out" || rows[0].Reason != "type" {
		t.Fatalf("held: %+v", rows)
	}
	if err := g2.f.Resolve(context.Background(), proto.Action{Path: "out", Action: ActDiscard, Save: false}); err != nil {
		t.Fatal(err)
	}
	g2.sync()
	if entries, _ := os.ReadDir(outside); len(entries) != 1 || entries[0].Name() != "secret" {
		t.Fatalf("something was written outside the folder: %v", entries)
	}
	if b, err := os.ReadFile(filepath.Join(root, "out", "x")); err != nil || string(b) != "from the server" {
		t.Fatalf("out/x = %q %v", b, err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "out")); err != nil || !fi.IsDir() {
		t.Fatalf("out is not a real directory now: %v", err)
	}
	// The link target of file-link reached the other device as text, and the secret's content never did.
	g1.sync()
	if got, ok := g1.readlink("file-link"); !ok || got != "../outside/secret" {
		t.Fatalf("file-link -> %q %v", got, ok)
	}
	if _, ok := g1.read("out/secret"); ok {
		t.Fatal("content below a link was synced")
	}
	if _, ok := g1.read("file-link"); ok {
		t.Fatal("link target content was read")
	}
}
