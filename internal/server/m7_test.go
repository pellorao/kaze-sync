package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kaze-sync/internal/blobstore"
	"kaze-sync/internal/proto"
)

// ageBlock makes a stored block look old (age) so that the grace period no longer protects it.
func (e *env) ageBlock(h proto.Hash, age time.Duration) {
	e.t.Helper()
	x := h.String()
	old := time.Now().Add(-age)
	if err := os.Chtimes(filepath.Join(e.s.varDir, "blobs", x[:2], x[2:4], x), old, old); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) hasBlock(h proto.Hash) bool {
	ok, _ := e.s.blobs.Has(h)
	return ok
}

// A history with dated versions: only versions past keep_days go, and only blocks nothing references and
// that are past the grace period; blocks shared with a live file or another version stay.
func TestGCExpiresVersionsAndSweepsOnlyUnreferencedOldBlocks(t *testing.T) {
	e := newEnv(t)
	f := e.folder("Docs")
	day := 24 * time.Hour
	a1, shared := e.op("a", 0, "a one"), e.op("b", 0, "shared content")
	c := e.op("c", 0, "shared content") // live file sharing b's block
	e.commit(f, a1, shared, c)
	e.clk.Add(10 * day)
	a2 := e.op("a", 1, "a two")
	e.commit(f, a2) // version a1, replaced on day 10
	e.clk.Add(10 * day)
	e.commit(f, proto.Op{Path: "b", BaseSeq: 2}) // version of b, replaced on day 20
	e.clk.Add(10 * day)                          // day 30
	orphan := e.block([]byte("uploaded long ago, never committed"))
	fresh := e.block([]byte("uploaded just now, not committed yet"))
	for _, h := range []proto.Hash{a1.Blocks[0], a2.Blocks[0], shared.Blocks[0], orphan} {
		e.ageBlock(h, 48*time.Hour)
	}
	old, err := os.Create(filepath.Join(e.s.varDir, "tmp", "put-stale"))
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	os.Chtimes(old.Name(), time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	young := filepath.Join(e.s.varDir, "tmp", "put-young")
	os.WriteFile(young, nil, 0o600)

	SetMeta(e.s.db, "keep_days", 15)
	st, err := e.s.gc()
	if err != nil || st.versions != 1 || st.blobs != 2 || st.tmp != 1 {
		t.Fatalf("gc: %+v, %v", st, err)
	}
	if e.fcount(f, `SELECT count(*) FROM versions WHERE path='a'`) != 0 || e.fcount(f, `SELECT count(*) FROM versions WHERE path='b'`) != 1 {
		t.Fatal("wrong versions expired")
	}
	for name, want := range map[string]bool{"a1": false, "orphan": false, "a2": true, "shared": true, "fresh": true} {
		h := map[string]proto.Hash{"a1": a1.Blocks[0], "orphan": orphan, "a2": a2.Blocks[0], "shared": shared.Blocks[0], "fresh": fresh}[name]
		if e.hasBlock(h) != want {
			t.Errorf("block %s present=%v, want %v", name, !want, want)
		}
	}
	if _, err := os.Stat(old.Name()); err == nil {
		t.Error("stale tmp file kept")
	}
	if _, err := os.Stat(young); err != nil {
		t.Error("young tmp file removed")
	}
	// The shared block goes once the last version referencing it expires.
	e.clk.Add(10 * day)
	e.ageBlock(shared.Blocks[0], 48*time.Hour)
	e.commit(f, proto.Op{Path: "c", BaseSeq: 3}) // c's content becomes a version too: still referenced
	if st, err = e.s.gc(); err != nil || e.hasBlock(shared.Blocks[0]) != true {
		t.Fatalf("shared block removed while a version references it: %+v %v", st, err)
	}
	e.clk.Add(20 * day)
	e.ageBlock(shared.Blocks[0], 48*time.Hour)
	if st, err = e.s.gc(); err != nil || e.hasBlock(shared.Blocks[0]) {
		t.Fatalf("shared block kept after every reference expired: %+v %v", st, err)
	}
}

// Saving keep_days in the Web UI clears the expired versions and then their blocks without a restart.
func TestSavingKeepDaysRunsGC(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	v1 := e.op("a", 0, "first")
	e.commit(f, v1)
	e.clk.Add(time.Hour)
	e.commit(f, e.op("a", 1, "second"))
	e.clk.Add(48 * time.Hour)
	e.ageBlock(v1.Blocks[0], 2*time.Hour)
	if e.fcount(f, `SELECT count(*) FROM versions`) != 1 {
		t.Fatal("no version to expire")
	}
	if st, _ := e.post(c, "/settings", url.Values{"keep_days": {"1"}}); st != 303 {
		t.Fatalf("settings: %d", st)
	}
	e.s.gcWG.Wait()
	if e.fcount(f, `SELECT count(*) FROM versions`) != 0 || e.hasBlock(v1.Blocks[0]) {
		t.Fatal("the version or its block survived the save")
	}
}

type countGC struct{ n *atomic.Int32 }

func (countGC) Enabled(context.Context, slog.Level) bool { return true }
func (h countGC) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "gc" {
		h.n.Add(1)
	}
	return nil
}
func (h countGC) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countGC) WithGroup(string) slog.Handler      { return h }

// Requests during a run do not start a second GC beside it; they make it run once more afterwards.
func TestGCRunsOneAtATime(t *testing.T) {
	var n atomic.Int32
	old := slog.Default()
	slog.SetDefault(slog.New(countGC{&n}))
	defer slog.SetDefault(old)
	e := newEnv(t)
	f := e.folder("Docs")
	db, _ := e.s.folderDB(f)
	tx, err := db.Begin() // holds the folder's write lock: the GC waits for it in its first step
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE meta SET value=value WHERE key='next_seq'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		e.s.requestGC()
	}
	time.Sleep(100 * time.Millisecond)
	if n.Load() != 0 {
		t.Fatal("a GC finished while the lock was held")
	}
	tx.Rollback()
	e.s.gcWG.Wait()
	if n.Load() != 2 {
		t.Fatalf("%d GC runs for 5 requests, want 2 (one, and one more for the requests made meanwhile)", n.Load())
	}
	if e.s.gcRunning || e.s.gcAgain {
		t.Fatal("GC state not reset")
	}
}

func TestGCWait(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		last time.Time
		want time.Duration
	}{
		{time.Unix(0, 0), time.Minute},          // never ran: soon after the start
		{now.Add(-25 * time.Hour), time.Minute}, // overdue
		{now.Add(-23 * time.Hour), time.Hour},   // due in an hour
		{now.Add(-time.Minute), 24*time.Hour - time.Minute},
	} {
		if got := gcWait(tc.last, now); got != tc.want {
			t.Errorf("gcWait(%v) = %v, want %v", now.Sub(tc.last), got, tc.want)
		}
	}
}

func sum(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// liveContent returns the content hash of a live path ("" if deleted or absent).
func (e *env) liveHash(f int64, path string) []byte {
	db, _ := e.s.folderDB(f)
	var h []byte
	db.QueryRow(`SELECT hash FROM files WHERE path=?`, path).Scan(&h)
	return h
}

func TestRestoreDirToPointInTime(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	base := "/folders/" + itoa(f)
	e.commit(f, e.op("proj/a", 0, "a1"), e.op("proj/b", 0, "b1"), e.op("proj/sub/c", 0, "c1"), e.op("other", 0, "o1"))
	e.clk.Add(30 * time.Minute)
	t1 := e.clk.Now().Format("2006-01-02 15:04:05")
	e.clk.Add(30 * time.Minute)
	e.commit(f, e.op("proj/a", 1, "a2"), proto.Op{Path: "proj/b", BaseSeq: 2}, e.op("proj/d", 0, "d1"), e.op("other", 4, "o2"))
	post := func(dir, at string) (int, string) {
		resp, err := c.PostForm(e.ts.URL+base+"/restore-dir", url.Values{"path": {dir}, "time": {at}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Location")
	}
	// Directory proj: a and b come back (b was deleted), proj/sub/c is unchanged, d is newer, other is outside.
	if st, loc := post("proj", t1); st != 303 || !strings.Contains(loc, "restored=2&failed=0") {
		t.Fatalf("restore: %d %s", st, loc)
	}
	for path, want := range map[string]string{"proj/a": "a1", "proj/b": "b1", "proj/sub/c": "c1", "proj/d": "d1", "other": "o2"} {
		if !bytes.Equal(e.liveHash(f, path), sum(want)) {
			t.Errorf("%s is not %q after the restore", path, want)
		}
	}
	if n := e.fcount(f, `SELECT count(*) FROM versions WHERE path='proj/a'`); n != 2 {
		t.Errorf("proj/a has %d versions, want 2 (a1, and a2 that the restore replaced)", n)
	}
	// Again: nothing differs now.
	if _, loc := post("proj", t1); !strings.Contains(loc, "restored=0") {
		t.Errorf("second restore: %s", loc)
	}
	// Before the first record: the first record counts (design 7), which brings back o1.
	if _, loc := post("", "2020-01-01 00:00"); !strings.Contains(loc, "restored=1") || !bytes.Equal(e.liveHash(f, "other"), sum("o1")) {
		t.Errorf("restore before the beginning: %s", loc)
	}
	if _, loc := post("", t1); !strings.Contains(loc, "restored=0") {
		t.Errorf("whole-folder restore: %s", loc)
	}
	if st, _ := e.post(c, base+"/restore-dir", url.Values{"path": {""}, "time": {"yesterday"}}); st != 400 {
		t.Errorf("bad time: %d", st)
	}
	_, page := e.get(c, base+"?restored=3&failed=0")
	if !strings.Contains(page, "已恢复 3 个文件") {
		t.Errorf("no result line on the page")
	}
}

// A thousand files deleted together come back, across several commits of at most 1000 operations.
func TestRestoreDirManyFiles(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	const n = 2500
	var add, del []proto.Op
	for i := 0; i < n; i++ {
		add = append(add, e.op(fmt.Sprintf("proj/d%d/f%04d", i%10, i), 0, fmt.Sprintf("content %d", i%50)))
	}
	for i, r := range e.commit(f, add...) {
		del = append(del, proto.Op{Path: add[i].Path, BaseSeq: r.Seq})
	}
	e.clk.Add(time.Minute)
	t1 := e.clk.Now().Format("2006-01-02 15:04:05")
	e.clk.Add(time.Minute)
	e.commit(f, del...)
	if e.fcount(f, `SELECT count(*) FROM files WHERE hash IS NOT NULL`) != 0 {
		t.Fatal("not deleted")
	}
	e.commit(f, e.op("proj/new", 0, "created after the deletion"))
	resp, err := c.PostForm(e.ts.URL+"/folders/"+itoa(f)+"/restore-dir", url.Values{"path": {"proj"}, "time": {t1}})
	if err != nil || !strings.Contains(resp.Header.Get("Location"), fmt.Sprintf("restored=%d&failed=0", n)) {
		t.Fatalf("restore: %v %v", err, resp)
	}
	if got := e.fcount(f, `SELECT count(*) FROM files WHERE hash IS NOT NULL`); got != n+1 {
		t.Fatalf("%d live files, want %d", got, n+1)
	}
}

func TestDeleteFolder(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	keep, gone := e.folder("keep"), e.folder("gone")
	e.commit(gone, e.op("x", 0, "x"))
	dev, _ := e.enrollDevice("d", "linux")
	e.mapFolder(dev, gone, "/g")
	if _, page := e.get(c, "/folders/"+itoa(gone)); !strings.Contains(page, `data-menu="folder-head" data-maps="1"`) {
		t.Error("the page does not tell the menu that the folder is mapped (the deletion is disabled then)")
	}
	if st, _ := e.post(c, "/folders/"+itoa(gone)+"/delete", nil); st != 409 {
		t.Fatalf("deleting a mapped folder: %d", st)
	}
	e.s.db.Exec(`DELETE FROM mappings`)
	if _, page := e.get(c, "/folders/"+itoa(gone)); !strings.Contains(page, `data-menu="folder-head" data-maps="0"`) || !strings.Contains(page, `id="f-del-folder"`) {
		t.Error("the page lacks the deletion of an unmapped folder")
	}
	path := FolderDBPath(e.s.varDir, gone)
	if st, _ := e.post(c, "/folders/"+itoa(gone)+"/delete", nil); st != 303 {
		t.Fatalf("delete: %d", st)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Errorf("%s%s remains", filepath.Base(path), suffix)
		}
	}
	if e.count(`SELECT count(*) FROM folders`) != 1 || e.fcount(keep, `SELECT count(*) FROM files`) != 0 {
		t.Fatal("wrong folders left")
	}
	if st, _ := e.get(c, "/folders/"+itoa(gone)); st != 404 {
		t.Errorf("page of the deleted folder: %d", st)
	}
	// Its id is never used again (clients may still hold state under it) and the new folder starts empty.
	if id := e.folder("again"); id <= gone || e.fcount(id, `SELECT count(*) FROM files`) != 0 {
		t.Fatalf("new folder id %d, want one above %d, empty", id, gone)
	}
}

// opFull is an op for a file of several blocks with a mode and an mtime.
func (e *env) opFull(path string, base int64, data []byte, exec bool, mtimeNs int64) proto.Op {
	h := proto.Hash(sha256.Sum256(data))
	var blocks []proto.Hash
	for off := 0; off < len(data); off += proto.BlockSize {
		blocks = append(blocks, e.block(data[off:min(off+proto.BlockSize, len(data))]))
	}
	return proto.Op{Path: path, BaseSeq: base, Hash: &h, Blocks: blocks, Size: int64(len(data)), MtimeNs: mtimeNs, Exec: exec}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

func TestExport(t *testing.T) {
	e := newEnv(t)
	f := e.folder("Docs")
	other := e.folder("a/b")                                    // a name that cannot be a directory name
	big := bytes.Repeat([]byte("0123456789abcdef"), 300<<10/16) // 2 blocks
	mt := time.Date(2025, 5, 6, 7, 8, 9, 0, time.UTC).UnixNano()
	e.commit(f, e.op("a.txt", 0, "old a"), e.opFull("sub/big.bin", 0, big, true, mt), e.op("gone.txt", 0, "deleted later"), e.op("empty", 0, ""))
	e.commit(other, e.op("o.txt", 0, "other"))
	e.clk.Add(time.Hour)
	t1 := e.clk.Now()
	e.clk.Add(time.Hour)
	e.commit(f, e.op("a.txt", 1, "new a"), proto.Op{Path: "gone.txt", BaseSeq: 3}, e.op("later.txt", 0, "later"))

	var out bytes.Buffer
	in := &Installer{Out: &out, Lookup: func(string) (int, int, error) { return 0, 0, os.ErrNotExist }}
	data := filepath.Dir(e.s.varDir)
	export := func(o ExportOptions) map[string]string {
		o.Data, o.Dir = data, t.TempDir()
		if err := in.Export(o); err != nil {
			t.Fatal(err)
		}
		return readTree(t, o.Dir)
	}
	now := export(ExportOptions{Folder: "Docs"})
	if len(now) != 4 || now["a.txt"] != "new a" || now["later.txt"] != "later" || now["sub/big.bin"] != string(big) || now["empty"] != "" {
		t.Fatalf("export now: %d files %v", len(now), keys(now))
	}
	then := export(ExportOptions{Folder: fmt.Sprint(f), At: t1})
	if len(then) != 4 || then["a.txt"] != "old a" || then["gone.txt"] != "deleted later" || then["sub/big.bin"] != string(big) {
		t.Fatalf("export at t1: %v", keys(then))
	}
	all := export(ExportOptions{})
	if all["Docs/a.txt"] != "new a" || all["folder-"+itoa(other)+"/o.txt"] != "other" || len(all) != 5 {
		t.Fatalf("export of all folders: %v", keys(all))
	}
	dir := t.TempDir()
	if err := in.Export(ExportOptions{Data: data, Dir: dir, Folder: "Docs"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "sub/big.bin"))
	if err != nil || fi.Mode().Perm() != 0o755 || fi.ModTime().UnixNano() != mt {
		t.Fatalf("mode and mtime: %v %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "a.txt")); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", fi.Mode())
	}
	if err := in.Export(ExportOptions{Data: data, Dir: dir, Folder: "nope"}); err == nil {
		t.Error("an unknown folder was exported")
	}

	// A missing block fails that file only and leaves no partial file.
	x := blobstore.Hash(sha256.Sum256([]byte("later")))
	os.Remove(filepath.Join(e.s.varDir, "blobs", x.String()[:2], x.String()[2:4], x.String()))
	bad := t.TempDir()
	if err := in.Export(ExportOptions{Data: data, Dir: bad, Folder: "Docs"}); err == nil || !strings.Contains(err.Error(), "1 files") {
		t.Fatalf("missing block: %v", err)
	}
	if got := readTree(t, bad); len(got) != 3 || got["a.txt"] != "new a" {
		t.Fatalf("after the failure: %v", keys(got))
	}
}

func keys(m map[string]string) []string {
	var k []string
	for p := range m {
		k = append(k, p)
	}
	return k
}
