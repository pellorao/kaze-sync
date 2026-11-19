package server

import (
	"net/url"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

// wcOp is an op with a since (design 2.8.3).
func (l *lockEnv) wcOp(path string, base, since int64, content string) proto.Op {
	o := l.op(path, base, content)
	o.Since = since
	return o
}

func (l *lockEnv) mode(path, mode string, dev int64) {
	l.t.Helper()
	form := url.Values{"path": {path}, "mode": {mode}}
	if dev != 0 {
		form.Set("device", itoa(dev))
	}
	if st, body := l.post(l.web, "/folders/"+itoa(l.f)+"/writectl", form); st != 303 {
		l.t.Fatalf("writectl %s %s: %d %s", path, mode, st, body)
	}
}

func owner(r proto.Result) string {
	if r.Reason != proto.ReasonOwner || r.Owner == nil {
		return "-"
	}
	return r.Owner.Cond + ":" + r.Owner.Device
}

// Single scope, the time window: another device's write is concurrent for N, then the writer changes hands.
func TestSingleWindow(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	if r := l.apiCommit(l.ta, l.f, l.wcOp("repo/a", 0, 0, "1")); r[0].Status != proto.StatusOK {
		t.Fatalf("A: %+v", r[0])
	}
	l.clk.Add(30 * time.Second)
	// B has read A's write (since 1): within the window it still is concurrent.
	if r := l.apiCommit(l.tb, l.f, l.wcOp("other", 0, 1, "x")); owner(r[0]) != "window:A" || r[0].Owner.Ref == 0 {
		t.Fatalf("B inside the window: %+v", r[0])
	}
	if l.fcount(l.f, `SELECT count(*) FROM files WHERE path='other'`) != 0 {
		t.Fatal("a refused op changed the state")
	}
	l.clk.Add(31 * time.Second)
	if r := l.apiCommit(l.tb, l.f, l.wcOp("other", 0, 1, "x")); r[0].Status != proto.StatusOK {
		t.Fatalf("B after the window: %+v", r[0])
	}
	// B is the owner now; A, which has not read B's write, is concurrent through the version too.
	l.clk.Add(2 * time.Minute)
	if r := l.apiCommit(l.ta, l.f, l.wcOp("repo/a", 1, 1, "2")); owner(r[0]) != "stale:B" || r[0].Owner.Ref != 2 {
		t.Fatalf("A on old content: %+v", r[0])
	}
	// Caught up: since 2 and past the window.
	if r := l.apiCommit(l.ta, l.f, l.wcOp("repo/a", 1, 2, "2")); r[0].Status != proto.StatusOK {
		t.Fatalf("A caught up: %+v", r[0])
	}
}

// The version condition catches a device that went offline: its since is from before.
func TestSingleStaleAfterOffline(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.apiCommit(l.ta, l.f, l.wcOp("x", 0, 0, "1")) // seq 1
	l.clk.Add(time.Hour)
	l.apiCommit(l.tb, l.f, l.wcOp("y", 0, 1, "1")) // seq 2 by B
	l.clk.Add(10 * time.Hour)
	// A edited since 1 while offline; the only newer write in the scope is B's.
	if r := l.apiCommit(l.ta, l.f, l.wcOp("x", 1, 1, "2")); owner(r[0]) != "stale:B" {
		t.Fatalf("offline A: %+v", r[0])
	}
	// A's own writes never count against it, and the batch takes the smallest since.
	if r := l.apiCommit(l.tb, l.f, l.wcOp("y", 2, 2, "2"), l.wcOp("z", 0, 1, "2")); r[0].Status != proto.StatusOK || r[1].Status != proto.StatusOK {
		t.Fatalf("B: %+v", r)
	}
}

// The window can be turned off; the rules deletions (web:rules) and the Web UI (devID 0) are not bound.
func TestSingleWindowZeroAndClockBack(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.apiCommit(l.ta, l.f, l.wcOp("a", 0, 0, "1"))
	SetMeta(l.s.db, "write_window", 0)
	if r := l.apiCommit(l.tb, l.f, l.wcOp("b", 0, 1, "1")); r[0].Status != proto.StatusOK {
		t.Fatalf("window 0: %+v", r[0])
	}
	SetMeta(l.s.db, "write_window", 60)
	l.clk.Add(-time.Hour) // the clock went back: the window counts as passed
	if r := l.apiCommit(l.ta, l.f, l.wcOp("a", 1, 2, "2")); r[0].Status != proto.StatusOK {
		t.Fatalf("clock back: %+v", r[0])
	}
}

// Exclusive refuses everything of the others, also a write of a file inside it; the owner is bound by the base check only.
func TestExclusiveAndFree(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.mode("repo", proto.ModeExclusive, l.a)
	if r := l.apiCommit(l.tb, l.f, l.wcOp("repo/f", 0, 0, "x")); owner(r[0]) != "exclusive:A" {
		t.Fatalf("B in the exclusive: %+v", r[0])
	}
	if r := l.apiCommit(l.ta, l.f, l.wcOp("repo/f", 0, 0, "x")); r[0].Status != proto.StatusOK {
		t.Fatalf("A: %+v", r[0])
	}
	// A free directory: no version condition, files of different names never clash, the same file within N does.
	l.mode("scratch", proto.ModeFree, 0)
	l.apiCommit(l.ta, l.f, l.wcOp("scratch/a", 0, 0, "1"))
	if r := l.apiCommit(l.tb, l.f, l.wcOp("scratch/b", 0, 0, "1")); r[0].Status != proto.StatusOK {
		t.Fatalf("another file of a free directory: %+v", r[0])
	}
	if r := l.apiCommit(l.tb, l.f, l.wcOp("scratch/a", 2, 0, "2")); owner(r[0]) != "window:A" {
		t.Fatalf("the same file within N: %+v", r[0])
	}
	l.clk.Add(2 * time.Minute)
	if r := l.apiCommit(l.tb, l.f, l.wcOp("scratch/a", 2, 0, "2")); r[0].Status != proto.StatusOK {
		t.Fatalf("the same file after N: %+v", r[0])
	}
}

// A decision (resolve) skips the check, and writes in a deeper scope do not count in the outer one.
func TestResolveAndNestedScopes(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.apiCommit(l.ta, l.f, l.wcOp("a", 0, 0, "1"))
	l.clk.Add(time.Hour)
	op := l.wcOp("a", 1, 0, "2")
	if r := l.apiCommit(l.tb, l.f, op); owner(r[0]) != "stale:A" {
		t.Fatalf("B: %+v", r[0])
	}
	op.Resolve = true
	if r := l.apiCommit(l.tb, l.f, op); r[0].Status != proto.StatusOK {
		t.Fatalf("resolve: %+v", r[0])
	}
	l.mode("sub", proto.ModeSingle, 0) // its own scope: A's root write does not make B's write there concurrent
	if r := l.apiCommit(l.ta, l.f, l.wcOp("sub/x", 0, 99, "1")); r[0].Status != proto.StatusOK {
		t.Fatalf("A in sub: %+v", r[0])
	}
	l.clk.Add(time.Hour)
	if r := l.apiCommit(l.tb, l.f, l.wcOp("sub/y", 0, 3, "1")); r[0].Status != proto.StatusOK {
		t.Fatalf("B in sub, caught up: %+v", r[0])
	}
}

// Saving the default for the root changes nothing the devices see; a transfer does; the config lists the root first.
func TestWritectlConfig(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	v := metaInt(l.s.db, "config_version")
	l.mode("", proto.ModeSingle, 0)
	if metaInt(l.s.db, "config_version") != v {
		t.Fatal("confirming the default changed the config")
	}
	l.mode("repo", proto.ModeExclusive, l.a)
	l.mode("repo", proto.ModeExclusive, l.b)
	if metaInt(l.s.db, "config_version") != v+2 {
		t.Fatal("setting and transferring must each change the config")
	}
	cfg, _ := l.s.effectiveConfig(l.a)
	if w := cfg.Folders[0].Writectl; len(w) != 2 || w[0].Path != "" || w[0].Mode != proto.ModeSingle || w[1] != (proto.WCRule{Path: "repo", Mode: proto.ModeExclusive}) {
		t.Fatalf("writectl = %+v", w)
	}
	if st, _ := l.post(l.web, "/folders/"+itoa(l.f)+"/writectl", url.Values{"path": {""}, "mode": {"inherit"}}); st != 400 {
		t.Fatalf("the root takes no inherit: %d", st)
	}
}

// Version 3 locks become exclusive settings, the window gets its default.
func TestMigrateLocksToWritectl(t *testing.T) {
	t.Parallel()
	db, err := OpenDB(t.TempDir()+"/meta.db", true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrate(db, migrations[:3]); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO locks VALUES(1, 'Repo', 'repo', 7, 5)`)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var mode string
	var dev int64
	if err := db.QueryRow(`SELECT mode, device_id FROM writectl WHERE key='repo'`).Scan(&mode, &dev); err != nil || mode != "exclusive" || dev != 7 {
		t.Fatalf("row: %q %d %v", mode, dev, err)
	}
	if metaInt(db, "write_window") != 60 {
		t.Fatal("no default window")
	}
}
