package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// apiCommit commits as the device with the token and returns the result of every op.
func (e *env) apiCommit(tok string, folder int64, ops ...proto.Op) []proto.Result {
	e.t.Helper()
	st, out := e.api(tok, "commit", proto.CommitReq{Folder: folder, Ops: ops})
	if st != 200 {
		e.t.Fatalf("commit: %d %s", st, out)
	}
	var r proto.CommitResp
	json.Unmarshal(out, &r)
	return r.Results
}

// lockEnv is a folder F mapped to devices A and B, with a logged-in administrator.
type lockEnv struct {
	*env
	web    *http.Client
	f      int64
	a, b   int64
	ta, tb string
}

func newLockEnv(t *testing.T) *lockEnv {
	e := newEnv(t)
	e.setAdminPassword("pw")
	l := &lockEnv{env: e, web: e.webClient(), f: e.folder("F")}
	e.login(l.web, "pw")
	l.a, l.ta = e.enrollDevice("A", "linux")
	l.b, l.tb = e.enrollDevice("B", "windows")
	e.mapFolder(l.a, l.f, "/a")
	e.mapFolder(l.b, l.f, `D:\b`)
	return l
}

func (l *lockEnv) lock(path string, dev int64) (int, string) {
	form := url.Values{"path": {path}, "mode": {"inherit"}}
	if dev != 0 {
		form.Set("mode", "exclusive")
		form.Set("device", itoa(dev))
	}
	return l.post(l.web, "/folders/"+itoa(l.f)+"/writectl", form)
}

func TestLockRejectsOtherDevicesWrites(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	if st, _ := l.lock("repo/.git", l.a); st != 303 {
		t.Fatalf("lock: %d", st)
	}
	// B: add, modify, delete under the lock and the locked path itself are refused; names compare by key.
	for _, p := range []string{"repo/.git/HEAD", "REPO/.GIT/index", "repo/.git"} {
		if r := l.apiCommit(l.tb, l.f, l.op(p, 0, "x")); r[0].Status != proto.StatusRejected || r[0].Reason != proto.ReasonOwner {
			t.Fatalf("B %s: %+v", p, r[0])
		}
	}
	if l.fcount(l.f, `SELECT count(*) FROM files`) != 0 {
		t.Fatal("a refused op changed the state")
	}
	// A writes; B's modification and deletion of that file are refused too, with no conflict, whatever the base.
	if r := l.apiCommit(l.ta, l.f, l.op("repo/.git/HEAD", 0, "ref")); r[0].Status != proto.StatusOK {
		t.Fatalf("A: %+v", r[0])
	}
	if r := l.apiCommit(l.tb, l.f, l.op("repo/.git/HEAD", 99, "mine"), proto.Op{Path: "repo/.git/HEAD", BaseSeq: 1}); r[0].Reason != proto.ReasonOwner || r[1].Reason != proto.ReasonOwner {
		t.Fatalf("B on A's file: %+v", r)
	}
	// Outside the lock, and a sibling whose name only starts the same, B writes freely.
	if r := l.apiCommit(l.tb, l.f, l.op("repo/main.go", 0, "m"), l.op("repo/.github", 0, "g")); r[0].Status != proto.StatusOK || r[1].Status != proto.StatusOK {
		t.Fatalf("B outside: %+v", r)
	}
	// The administrator (devID 0, as the Web UI restore writes) is not bound.
	l.commit(l.f, l.op("repo/.git/HEAD", 1, "restored"))
}

func TestLockWholeFolder(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.lock("", l.a)
	if r := l.apiCommit(l.tb, l.f, l.op("any/file", 0, "x")); r[0].Reason != proto.ReasonOwner {
		t.Fatalf("whole-folder lock: %+v", r[0])
	}
	if r := l.apiCommit(l.ta, l.f, l.op("any/file", 0, "x")); r[0].Status != proto.StatusOK {
		t.Fatalf("owner: %+v", r[0])
	}
}

func TestLockRules(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	if st, _ := l.lock("a/b", l.a); st != 303 {
		t.Fatalf("lock: %d", st)
	}
	v := metaInt(l.s.db, "config_version")
	if st, _ := l.lock("a/bc", l.b); st != 303 { // a sibling that shares a prefix of the name
		t.Fatalf("sibling: %d", st)
	}
	if st, _ := l.lock("A/B", l.a); st != 303 || metaInt(l.s.db, "config_version") != v+1 {
		t.Fatalf("same claim again moved the config version: %d", metaInt(l.s.db, "config_version"))
	}
	// A device that does not map the folder cannot hold a range.
	c, _ := l.enrollDevice("C", "linux")
	if st, _ := l.lock("z", c); st != 400 {
		t.Fatalf("unmapped: %d", st)
	}
	if st, _ := l.lock("bad//path", l.a); st != 400 {
		t.Fatalf("bad path: %d", st)
	}
	// Move to the other device in one step.
	if st, _ := l.lock("a/b", l.b); st != 303 {
		t.Fatalf("transfer: %d", st)
	}
	if l.count(`SELECT count(*) FROM writectl WHERE key=? AND device_id=?`, pp.Key("a/b"), l.b) != 1 {
		t.Fatal("not transferred")
	}
	// Remove.
	before := metaInt(l.s.db, "config_version")
	if st, _ := l.lock("a/b", 0); st != 303 || l.count(`SELECT count(*) FROM writectl`) != 1 || metaInt(l.s.db, "config_version") != before+1 {
		t.Fatal("not removed")
	}
	if r := l.apiCommit(l.ta, l.f, l.op("a/b/x", 0, "x")); r[0].Status != proto.StatusOK {
		t.Fatalf("after removing: %+v", r[0])
	}
}

func TestLockGoesWithTheDeviceOrItsMapping(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.lock("x", l.a)
	l.lock("y", l.b)
	v := metaInt(l.s.db, "config_version")
	// Deleting A's mapping frees x.
	st, _ := l.post(l.web, "/devices/"+itoa(l.a)+"/mapping", url.Values{"folder": {itoa(l.f)}, "action": {"delete"}})
	if st != 303 || l.count(`SELECT count(*) FROM writectl WHERE device_id=?`, l.a) != 0 || metaInt(l.s.db, "config_version") <= v {
		t.Fatalf("mapping delete: %d", st)
	}
	// Revoking B frees y and tells the others.
	v = metaInt(l.s.db, "config_version")
	if st, _ := l.post(l.web, "/devices/"+itoa(l.b)+"/revoke", url.Values{}); st != 303 || l.count(`SELECT count(*) FROM writectl WHERE mode='exclusive'`) != 0 || metaInt(l.s.db, "config_version") <= v {
		t.Fatalf("revoke: %d", st)
	}
}

// Restoring through the Web UI works inside a locked range, and the result reaches every device.
func TestWebRestoreIgnoresLocks(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.lock("d", l.a)
	l.apiCommit(l.ta, l.f, l.op("d/f", 0, "one"))
	l.apiCommit(l.ta, l.f, l.op("d/f", 1, "two"))
	var vid int64
	l.s.folderDBMust(t, l.f).QueryRow(`SELECT id FROM versions WHERE path='d/f'`).Scan(&vid)
	if st, _ := l.post(l.web, "/folders/"+itoa(l.f)+"/restore", url.Values{"version": {itoa(vid)}}); st != 303 {
		t.Fatalf("restore: %d", st)
	}
	if got := readCurrent(t, l.env, l.f, "d/f"); got != "one" {
		t.Fatalf("d/f = %q", got)
	}
	if l.fcount(l.f, `SELECT count(*) FROM files WHERE path='d/f' AND device='web'`) != 1 {
		t.Fatal("the restore is not written as the Web UI")
	}
}

func TestMigrateToLocksSchema(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if e.count(`SELECT count(*) FROM sqlite_master WHERE name='writectl'`) != 1 || SchemaVersion != 5 {
		t.Fatal("no locks table")
	}
}

func TestLockWebUI(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.lock("repo", l.a)
	l.apiCommit(l.ta, l.f, l.op("repo/x.txt", 0, "x"), l.op("other/y.txt", 0, "y"))
	_, body := l.get(l.web, "/folders/"+itoa(l.f))
	for _, w := range []string{`data-wc-mode="exclusive"`, `data-wc-dev="` + itoa(l.a) + `"`, `独占 · A`, `id="dlg-wc"`} {
		if !strings.Contains(body, w) {
			t.Errorf("folder page lacks %s", w)
		}
	}
	_, body = l.get(l.web, "/folders/"+itoa(l.f)+"?dir=repo")
	if !strings.Contains(body, "A") || !strings.Contains(body, "写入控制") {
		t.Error("no write-control line inside the range")
	}
	// The device page counts the exclusive and the revoke confirmation names it.
	_, body = l.get(l.web, "/devices/"+itoa(l.a))
	if !strings.Contains(body, "独占 1 处") || !strings.Contains(body, "同时解除它的 1 处独占") {
		t.Error("device page lacks the exclusive count")
	}
}

// A device that is not there is named by its id in the refusal.
func TestLockUnknownDeviceNamedById(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	if st, body := l.lock("z", 987); st != 400 || !strings.Contains(body, "987") {
		t.Fatalf("unknown device: %d %s", st, body)
	}
}

// The other side of a concurrent write is taken within the scope of the held changes, and not carried over to
// another device's notice (it never names the device itself).
func TestHeldOtherWriterPerScope(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	c, tc := l.enrollDevice("C", "linux")
	l.mapFolder(c, l.f, "/c")
	SetMeta(l.s.db, "write_window", 0) // writes of different devices in turn are not refused
	l.apiCommit(tc, l.f, l.op("sub/x", 0, "1"))
	l.apiCommit(l.tb, l.f, l.wcOp("other/y", 0, 1, "1")) // B has seen the write of C
	now := l.clk.Now().Unix()
	l.heldReport(l.ta, l.f, proto.HeldItem{Path: "other/z", Scope: "d:OTHER", Reason: "owner", Kind: "modify", At: now}, proto.HeldItem{Path: "sub/z", Scope: "d:SUB", Reason: "owner", Kind: "modify", At: now}, proto.HeldItem{Path: "sub/w", Scope: "d:SUB", Reason: "owner", Kind: "modify", At: now})
	l.heldReport(l.tb, l.f, proto.HeldItem{Path: "sub/z", Scope: "d:SUB", Reason: "owner", Kind: "modify", At: now})
	notes, err := l.s.folderNotices(l.f)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, n := range notes {
		if n.Kind == kindHeld {
			got[n.Device+" "+itoa(int64(n.N))] = n.Other // a scope without a rule shows as the root: told apart by its count
		}
	}
	// A holds in "other" (written by B) and in "sub" (written by C); B holds in "sub" (written by C).
	want := map[string]string{"A 1": "B", "A 2": "C", "B 1": "C"}
	if len(got) != len(want) {
		t.Fatalf("notices %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: other %q, want %q (%v)", k, got[k], v, got)
		}
	}
}
