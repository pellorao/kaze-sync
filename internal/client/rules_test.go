package client

import (
	"path/filepath"
	"testing"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// Design 3.2 example: a folder rule re-includes one file below an excluded directory; releasing the
// exclusion later brings the rest down, which needs the cursor to start again from 0 (design 8.3).
func TestFolderRulesOverrideGlobalAndRulesChangeReconciles(t *testing.T) {
	e, f, g1, g2, _ := pair(t)
	for _, p := range []string{"build/a.txt", "build/keep.txt", "build/sub/x.txt", "top.txt"} {
		g1.put(p, []byte("content of "+p))
	}
	g1.sync()

	api := g2.f.API
	rules := []string{"build/", "!/build/", "/build/*", "!/build/keep.txt"}
	g2.restart(api, proto.FolderConfig{ID: f, Adopt: true, Rules: rules}, g2.fs)
	g2.sync()
	for p, want := range map[string]bool{"top.txt": true, "build/keep.txt": true, "build/a.txt": false, "build/sub/x.txt": false} {
		if _, ok := g2.read(p); ok != want {
			t.Fatalf("%s present=%v, want %v", p, ok, want)
		}
	}

	// Excluding more keeps the local file and leaves the server alone.
	g2.restart(api, proto.FolderConfig{ID: f, Adopt: true, Rules: []string{"build/"}}, g2.fs)
	g2.sync()
	if _, ok := g2.read("build/keep.txt"); !ok {
		t.Fatal("newly excluded local file removed")
	}
	if _, ok := e.serverFile(f, "build/keep.txt"); !ok {
		t.Fatal("newly excluded file removed from the server")
	}
	if rows, _ := g2.f.DB.all(); rows["build/keep.txt"].Path != "" {
		t.Fatal("newly excluded file still tracked")
	}

	// Releasing the exclusion: the identical file is adopted and the missing ones arrive.
	g2.restart(api, proto.FolderConfig{ID: f, Adopt: true}, g2.fs)
	g2.sync()
	for _, p := range []string{"build/a.txt", "build/sub/x.txt", "build/keep.txt"} {
		if got, ok := g2.read(p); !ok || got != "content of "+p {
			t.Fatalf("%s = %q %v after the release", p, got, ok)
		}
	}
	if n := e.count(f, `SELECT count(*) FROM files WHERE hash IS NOT NULL`); n != 4 {
		t.Fatalf("server holds %d files", n)
	}
}

func TestBuiltinExclusionsCannotBeOverridden(t *testing.T) {
	e, f, api, g := setup(t)
	g.restart(api, proto.FolderConfig{ID: f, Adopt: true, Rules: []string{"!.syncroot", "!.syncpart-*", "!*"}}, g.fs)
	g.put(".syncpart-123", []byte("partial"))
	g.put("dir/.syncpart-9", []byte("partial"))
	g.put("ok.txt", []byte("ok"))
	g.scan()
	if n := e.count(f, `SELECT count(*) FROM files WHERE path LIKE '%syncpart%' OR path LIKE '%syncroot%'`); n != 0 {
		t.Fatalf("%d built-in files uploaded", n)
	}
	if _, ok := e.serverFile(f, "ok.txt"); !ok {
		t.Fatal("ok.txt not uploaded")
	}
}

// The matcher mode follows the device: Windows ignores case, Linux does not.
func TestRulesCaseModeFollowsSystem(t *testing.T) {
	db, err := openLocal(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fc := proto.FolderConfig{ID: 1, Rules: []string{"*.TMP", "/Build/"}}
	lin, win := NewFolder(fc, cfs.NewMem(cfs.MemOptions{}), db, nil, false), NewFolder(fc, cfs.NewMem(cfs.MemOptions{}), db, nil, true)
	for p, want := range map[string][2]bool{"a.tmp": {false, true}, "a.TMP": {true, true}, "build": {false, true}} {
		if lin.Matcher.Excluded(p, true) != want[0] || win.Matcher.Excluded(p, true) != want[1] {
			t.Errorf("%s: linux %v windows %v, want %v", p, lin.Matcher.Excluded(p, true), win.Matcher.Excluded(p, true), want)
		}
	}
}
