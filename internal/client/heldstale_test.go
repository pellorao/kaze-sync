package client

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// heldDelete sets up b holding its deletion of x.txt (a changed it first), while the server still has the file. The
// root is free, so the deletion is held for its base (reason conflict), as in the directories the stock cases are in;
// a held change of a single-writer scope (reason owner) is sent again at every restart (Requeue).
func heldDelete(t *testing.T) (a, b *engine) {
	t.Helper()
	_, _, a, b, _ = pair(t)
	a.put("x.txt", []byte("one"))
	a.sync()
	b.sync()
	a.put("x.txt", []byte("two")) // b has not seen this when it deletes
	a.sync()
	b.fs.Remove("x.txt")
	b.sync()
	if rows, _ := b.f.DB.held(""); len(rows) != 1 || rows[0].Kind != "delete" || rows[0].Reason != "conflict" {
		t.Fatalf("setup: held %+v", rows)
	}
	return a, b
}

// settled checks that b has no held change, no frozen scope and no base row for x.txt left.
func settled(t *testing.T, b *engine) {
	t.Helper()
	rows, _ := b.f.DB.held("")
	if len(rows) != 0 || len(b.f.frozen) != 0 {
		t.Fatalf("stale held %+v frozen %v", rows, b.f.frozen)
	}
	if _, have, _ := b.f.DB.get("x.txt"); have {
		t.Fatal("the base row of x.txt is still there")
	}
}

// restartOn rebuilds b's Folder over fsys like an upgrade of the client does: nothing in memory survives.
func restartOn(b *engine, fsys cfs.FS, wc []proto.WCRule) {
	name := b.f.DeviceName
	b.restart(b.f.API, proto.FolderConfig{ID: b.f.ID, Adopt: true, Writectl: wc}, fsys)
	b.f.DeviceName = name
}

// A held deletion whose path the other device then deleted too is settled: both ends lack the file and the server
// has it deleted, so nothing is left to decide (design 2.8.6 "same as the server", 2.8.9 "delete vs deleted").
func TestHeldDeleteClearsWhenServerDeletedToo(t *testing.T) {
	a, b := singlePair(t)
	a.put("x.txt", []byte("one"))
	a.sync()
	b.sync()
	a.put("x.txt", []byte("two")) // b has not seen this when it deletes
	a.sync()
	b.fs.Remove("x.txt")
	b.sync()
	if rows, _ := b.f.DB.held(""); len(rows) != 1 || rows[0].Kind != "delete" {
		t.Fatalf("setup: held %+v", rows)
	}
	a.fs.Remove("x.txt")
	a.sync()
	for i := 0; i < 3; i++ {
		b.sync()
	}
	settled(t, b)
}

// The stock case: a client before the fix skipped the server's deletion in its frozen scope and moved its cursor past
// it, so the tombstone never comes again. After the upgrade the held deletion still clears, by asking the server.
func TestHeldDeleteClearsAfterCursorPassedTombstone(t *testing.T) {
	a, b := heldDelete(t)
	a.fs.Remove("x.txt")
	a.sync()
	page, err := b.f.API.Changes(context.Background(), b.f.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.f.DB.setMeta("cursor", page.Cursor); err != nil { // what the old client left: the cursor past the tombstone
		t.Fatal(err)
	}
	restartOn(b, b.fs, nil)
	for i := 0; i < 3; i++ {
		b.sync()
	}
	settled(t, b)
	if _, ok := b.read("x.txt"); ok {
		t.Fatal("x.txt came back")
	}
}

// The other order: the change held here is a modification, the server's deletion passes by while it is held, and only
// then is the file deleted here. The held deletion is born after the tombstone went past, and clears all the same.
func TestHeldDeleteClearsWhenDeletedAfterServerDeletion(t *testing.T) {
	_, _, a, b, _ := pair(t)
	a.put("x.txt", []byte("one"))
	a.sync()
	b.sync()
	a.put("x.txt", []byte("two"))
	a.sync()
	b.put("x.txt", []byte("mine"))
	b.sync()
	if rows, _ := b.f.DB.held(""); len(rows) != 1 || rows[0].Kind != "modify" {
		t.Fatalf("setup: held %+v", rows)
	}
	a.fs.Remove("x.txt")
	a.sync()
	b.sync() // the tombstone goes past the frozen scope
	b.fs.Remove("x.txt")
	for i := 0; i < 3; i++ {
		b.sync()
	}
	settled(t, b)
}

// A file replaced here by a directory of the same name: the held deletion of the file clears once the server deleted
// it too, and the directory and its contents stay.
func TestHeldDeleteClearsWhenFileBecameDirectory(t *testing.T) {
	a, b := heldDelete(t)
	b.put("x.txt/inner.txt", []byte("in"))
	b.sync()
	a.fs.Remove("x.txt")
	a.sync()
	for i := 0; i < 3; i++ {
		b.sync()
	}
	rows, _ := b.f.DB.held("")
	for _, h := range rows {
		if h.Path == "x.txt" {
			t.Fatalf("held deletion of the file stays: %+v", rows)
		}
	}
	if _, have, _ := b.f.DB.get("x.txt"); have {
		t.Fatal("the base row of the file x.txt is still there")
	}
	if s, ok := b.read("x.txt/inner.txt"); !ok || s != "in" {
		t.Fatalf("the directory's file: %q %v", s, ok)
	}
}

// restoreFS puts x.txt back, with the content, size and mtime of its base, the first time a Stat after arming finds
// it missing: a user restoring the file between the client's check that it is gone and what the client does next.
type restoreFS struct {
	*cfs.Mem
	t        *testing.T
	armed    bool
	restored bool
	mtime    int64
}

func (r *restoreFS) Stat(p string) (cfs.Entry, error) {
	e, err := r.Mem.Stat(p)
	if r.armed && p == "x.txt" && errors.Is(err, fs.ErrNotExist) {
		r.armed, r.restored = false, true
		putFile(r.t, r.Mem, "x.txt", []byte("one"))
		if err := r.Mem.SetMeta("x.txt", r.mtime, false); err != nil {
			r.t.Fatal(err)
		}
	}
	return e, err
}

// Settling a held deletion only forgets the record and the base: a file the user puts back meanwhile, even one with
// the base's size and mtime, is not deleted (design 2.8.6).
func TestHeldDeleteSettleKeepsRestoredFile(t *testing.T) {
	a, b := heldDelete(t)
	row, have, _ := b.f.DB.get("x.txt")
	if !have {
		t.Fatal("setup: no base row")
	}
	rfs := &restoreFS{Mem: b.fs, t: t, mtime: row.MtimeNs}
	restartOn(b, rfs, nil)
	b.scan()
	a.fs.Remove("x.txt")
	a.sync()
	rfs.armed = true
	b.pull()
	rfs.armed = false
	if rfs.restored {
		if s, ok := b.read("x.txt"); !ok || s != "one" {
			t.Fatalf("the restored file was deleted: %q %v", s, ok)
		}
	}
	rows, _ := b.f.DB.held("")
	for _, h := range rows {
		if h.Path == "x.txt" && h.Kind == "delete" {
			t.Fatalf("held deletion stays: %+v", rows)
		}
	}
	// Put back after the record went: a new file here, which stays and is sent.
	b.put("x.txt", []byte("one"))
	for i := 0; i < 3; i++ {
		b.sync()
	}
	if s, ok := b.read("x.txt"); !ok || s != "one" {
		t.Fatalf("the restored file: %q %v", s, ok)
	}
	a.sync()
	if s, ok := a.read("x.txt"); !ok || s != "one" {
		t.Fatalf("the restored file on the other device: %q %v", s, ok)
	}
}

// stmtCounter counts the statements run on a database.
type stmtCounter struct {
	drv  driver.Driver
	dsn  string
	stmt atomic.Int64
}

func (c *stmtCounter) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return countConn{conn, c}, nil
}

func (c *stmtCounter) Driver() driver.Driver { return c.drv }

// countConn hides the driver's direct query and exec paths, so that every statement goes through Prepare.
type countConn struct {
	driver.Conn
	c *stmtCounter
}

func (x countConn) Prepare(q string) (driver.Stmt, error) {
	x.c.stmt.Add(1)
	return x.Conn.Prepare(q)
}

// countStatements reopens g's state database behind a stmtCounter.
func countStatements(t *testing.T, g *engine) *stmtCounter {
	t.Helper()
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	c := &stmtCounter{drv: g.f.DB.db.Driver(), dsn: "file:" + g.dbPath + "?" + q.Encode()}
	db := sql.OpenDB(c)
	t.Cleanup(func() { db.Close() })
	g.f.DB.db = db
	return c
}

// commitCounter counts the commit requests of a device.
type commitCounter struct {
	base http.RoundTripper
	n    atomic.Int64
}

func (c *commitCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/commit") {
		c.n.Add(1)
	}
	return c.base.RoundTrip(r)
}

// The cost of reconciling held deletions (first goal: low CPU, low traffic): the server's deletions in a frozen scope
// cost no state-database query each, and the server is asked about the held deletions in one request, once per run of
// the client, not on every Pull.
func TestHeldDeleteReconcileCost(t *testing.T) {
	a, b := singlePair(t)
	a.put("x.txt", []byte("one"))
	a.sync()
	b.sync()
	a.put("x.txt", []byte("two"))
	a.sync()
	b.fs.Remove("x.txt")
	b.sync()
	if rows, _ := b.f.DB.held(""); len(rows) != 1 {
		t.Fatalf("setup: held %+v", rows)
	}
	const n = 200
	for i := 0; i < n; i++ {
		a.put("d/f"+strconv.Itoa(i), []byte{byte(i)})
	}
	a.sync()
	for i := 0; i < n; i++ {
		a.fs.Remove("d/f" + strconv.Itoa(i))
	}
	a.sync()
	c := countStatements(t, b)
	b.pull()
	if got := c.stmt.Load(); got > n/4 {
		t.Fatalf("%d statements for a pull of %d deletions in a frozen scope", got, n)
	}
}

// Three held deletions of files the server still has: one probe request after a restart, none on the pulls after it.
func TestHeldDeleteProbeOncePerRun(t *testing.T) {
	_, _, a, b, _ := pair(t)
	names := []string{"x.txt", "y.txt", "z.txt"}
	for _, p := range names {
		a.put(p, []byte("one"))
	}
	a.sync()
	b.sync()
	for _, p := range names {
		a.put(p, []byte("two"))
	}
	a.sync()
	for _, p := range names {
		b.fs.Remove(p)
	}
	b.sync()
	if rows, _ := b.f.DB.held(""); len(rows) != 3 {
		t.Fatalf("setup: held %+v", rows)
	}
	restartOn(b, b.fs, nil)
	b.scan()
	cc := &commitCounter{base: b.f.API.HC.Transport}
	b.f.API.HC.Transport = cc
	b.pull()
	if got := cc.n.Load(); got != 1 {
		t.Fatalf("%d commit requests to check three held deletions, want 1", got)
	}
	b.sync()
	b.sync()
	if got := cc.n.Load(); got != 1 {
		t.Fatalf("%d commit requests after two more syncs, want still 1", got)
	}
	if rows, _ := b.f.DB.held(""); len(rows) != 3 {
		t.Fatalf("held deletions of files the server has went away: %+v", rows)
	}
	for _, p := range names {
		if s, ok := a.read(p); !ok || s != "two" {
			t.Fatalf("the probe changed %s on the server: %q %v", p, s, ok)
		}
	}
}
