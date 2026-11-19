package client

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io/fs"
	"net/http"
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
func heldDelete(t *testing.T) (e *cenv, a, b *engine) {
	t.Helper()
	e, _, a, b, _ = pair(t)
	if h := holdDelete(t, a, b); h.Reason != "conflict" {
		t.Fatalf("setup: held %+v", h)
	}
	return e, a, b
}

// holdDelete makes b hold its deletion of x.txt, which a changed first.
func holdDelete(t *testing.T, a, b *engine) heldRow {
	t.Helper()
	a.put("x.txt", []byte("one"))
	a.sync()
	b.sync()
	a.put("x.txt", []byte("two")) // b has not seen this when it deletes
	a.sync()
	b.fs.Remove("x.txt")
	b.sync()
	rows, _ := b.f.DB.held()
	if len(rows) != 1 || rows[0].Kind != "delete" {
		t.Fatalf("setup: held %+v", rows)
	}
	return rows[0]
}

// settled checks that b has no held change, no frozen scope and no base row for x.txt left.
func settled(t *testing.T, b *engine) {
	t.Helper()
	rows, _ := b.f.DB.held()
	if len(rows) != 0 || len(b.f.frozen) != 0 {
		t.Fatalf("stale held %+v frozen %v", rows, b.f.frozen)
	}
	if _, have, _ := b.f.DB.get("x.txt"); have {
		t.Fatal("the base row of x.txt is still there")
	}
}

// restartOn rebuilds b's Folder over fsys like an upgrade of the client does: nothing in memory survives.
func restartOn(b *engine, fsys cfs.FS) {
	name := b.f.DeviceName
	b.restart(b.f.API, proto.FolderConfig{ID: b.f.ID, Adopt: true}, fsys)
	b.f.DeviceName = name
}

// A held deletion whose path the other device then deleted too is settled: both ends lack the file and the server
// has it deleted, so nothing is left to decide (design 2.8.6 "same as the server", 2.8.9 "delete vs deleted").
func TestHeldDeleteClearsWhenServerDeletedToo(t *testing.T) {
	a, b := singlePair(t)
	holdDelete(t, a, b)
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
	_, a, b := heldDelete(t)
	a.fs.Remove("x.txt")
	a.sync()
	page, err := b.f.API.Changes(context.Background(), b.f.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.f.DB.setMeta("cursor", page.Cursor); err != nil { // what the old client left: the cursor past the tombstone
		t.Fatal(err)
	}
	restartOn(b, b.fs)
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
	if rows, _ := b.f.DB.held(); len(rows) != 1 || rows[0].Kind != "modify" {
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
	_, a, b := heldDelete(t)
	b.put("x.txt/inner.txt", []byte("in"))
	b.sync()
	a.fs.Remove("x.txt")
	a.sync()
	for i := 0; i < 3; i++ {
		b.sync()
	}
	rows, _ := b.f.DB.held()
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
	_, a, b := heldDelete(t)
	row, have, _ := b.f.DB.get("x.txt")
	if !have {
		t.Fatal("setup: no base row")
	}
	rfs := &restoreFS{Mem: b.fs, t: t, mtime: row.MtimeNs}
	restartOn(b, rfs)
	b.scan()
	a.fs.Remove("x.txt")
	a.sync()
	rfs.armed = true
	b.pull()
	rfs.armed = false
	if !rfs.restored { // nothing looked at the disk for x.txt: the pull reads no file, so a restore after it is the same
		rfs.armed = true
		rfs.Stat("x.txt")
	}
	if s, ok := b.read("x.txt"); !ok || s != "one" {
		t.Fatalf("the restored file was deleted: %q %v", s, ok)
	}
	rows, _ := b.f.DB.held()
	for _, h := range rows {
		if h.Path == "x.txt" && h.Kind == "delete" {
			t.Fatalf("held deletion stays: %+v", rows)
		}
	}
	// With the record gone the file is a new one here, which stays and is sent.
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

// stmtCounter counts the statements run on a database; fail, when set, can make one fail before it runs.
type stmtCounter struct {
	drv  driver.Driver
	dsn  string
	stmt atomic.Int64
	fail func(q string) error
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
	if x.c.fail != nil {
		if err := x.c.fail(q); err != nil {
			return nil, err
		}
	}
	return x.Conn.Prepare(q)
}

// countStatements reopens g's state database behind a stmtCounter.
func countStatements(t *testing.T, g *engine) *stmtCounter {
	t.Helper()
	c := &stmtCounter{drv: g.f.DB.db.Driver(), dsn: localDSN(g.dbPath)}
	db := sql.OpenDB(c)
	cleanupOnce(t, func() { db.Close() })
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
	holdDelete(t, a, b)
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
	if rows, _ := b.f.DB.held(); len(rows) != 3 {
		t.Fatalf("setup: held %+v", rows)
	}
	restartOn(b, b.fs)
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
	if rows, _ := b.f.DB.held(); len(rows) != 3 {
		t.Fatalf("held deletions of files the server has went away: %+v", rows)
	}
	for _, p := range names {
		if s, ok := a.read(p); !ok || s != "two" {
			t.Fatalf("the probe changed %s on the server: %q %v", p, s, ok)
		}
	}
}

// A crash in the middle of settling cannot leave the scope unfrozen with the cursor still past the entries it skipped,
// nor frozen with nothing held: the held row, the freeze and the cursor change in one transaction. Here the write of
// the cursor fails after the held deletion was dropped; all of it stays as it was, and a later pull settles it.
func TestHeldDeleteThawIsAtomic(t *testing.T) {
	_, a, b := heldDelete(t)
	a.fs.Remove("x.txt")
	a.sync()
	sc, _ := b.f.scopeOf("x.txt")
	v, ok := b.f.frozen[sc]
	if !ok {
		t.Fatalf("setup: frozen %v", b.f.frozen)
	}
	c := countStatements(t, b)
	var dropped, failed atomic.Bool
	c.fail = func(q string) error {
		if strings.Contains(q, "DELETE FROM held") {
			dropped.Store(true)
		}
		if dropped.Load() && (strings.Contains(q, "INTO meta") || strings.Contains(q, "UPDATE meta")) {
			failed.Store(true)
			return errors.New("disk gone")
		}
		return nil
	}
	_, err := b.f.Pull(context.Background())
	c.fail = nil
	if err == nil || !failed.Load() {
		t.Fatalf("setup: the pull did not reach the failing write: %v", err)
	}
	rows, _ := b.f.DB.held()
	_, frozen := b.f.DB.metaPrefix("frozen:")[sc]
	if !frozen && b.f.DB.metaInt("cursor") > v {
		t.Fatalf("thawed with the cursor at %d, past the freeze at %d", b.f.DB.metaInt("cursor"), v)
	}
	if frozen != (len(rows) > 0) { // settled and still frozen: a scope with nothing held that waits for a scan
		t.Fatalf("frozen %v with held %+v", frozen, rows)
	}
	for i := 0; i < 3; i++ {
		b.sync()
	}
	settled(t, b)
}

// A server restored from a backup that has never had the path: what was known of it (the server still had the file)
// is of the old history and goes with the reset, so the held deletion is asked about again and clears.
func TestHeldDeleteResetForgetsServerPresence(t *testing.T) {
	e, _, b := heldDelete(t)
	b.f.Epoch = "A"
	b.pull()
	if !b.f.heldLive["x.txt"] {
		t.Fatalf("setup: presence not known: %v", b.f.heldLive)
	}
	if _, err := e.folderDB(b.f.ID).Exec(`DELETE FROM files WHERE path='x.txt'`); err != nil { // the restored history
		t.Fatal(err)
	}
	b.f.Epoch = "B"
	for i := 0; i < 3; i++ {
		b.sync()
	}
	settled(t, b)
}

// A reset reads the new history from the start, the frozen scopes too: their freeze cursors go to 0, so the replay
// at their thaw gives the files skipped meanwhile their new seqs.
func TestHeldDeleteResetRereadsFrozenScope(t *testing.T) {
	a, b := singlePair(t)
	a.put("y.txt", []byte("y"))
	holdDelete(t, a, b)
	b.f.Epoch = "A"
	b.pull()
	a.fs.Remove("x.txt")
	a.sync()
	b.f.Epoch = "B" // the reset sets every base seq to 0
	for i := 0; i < 3; i++ {
		b.sync()
	}
	settled(t, b)
	if seq := rowSeq(t, b, "y.txt"); seq == 0 {
		t.Fatal("y.txt in the scope that was frozen kept no seq after the reset")
	}
}

// A held deletion the exclusion rules made on the server keeps its base, like any such deletion: a file put back
// here with the base's size and mtime is not sent while this device has not got the new rules.
func TestHeldDeleteExcludedKeepsBase(t *testing.T) {
	for _, crash := range []bool{false, true} {
		e, _, b := heldDelete(t)
		row, _, _ := b.f.DB.get("x.txt")
		saveRulesConfirmed(t, e, b.f.ID, "x.txt")
		b.scan()
		if crash { // the first pull dies before it settles: the next one must still know the rules did the deletion
			c := countStatements(t, b)
			c.fail = func(q string) error {
				if strings.Contains(q, "DELETE FROM held") {
					return errors.New("crash")
				}
				return nil
			}
			if _, err := b.f.Pull(context.Background()); err == nil {
				t.Fatal("setup: the pull did not fail")
			}
			c.fail = nil
			restartOn(b, b.fs)
			b.scan()
		}
		putFile(t, b.fs, "x.txt", []byte("one")) // after the scan, before the pull
		if err := b.fs.SetMeta("x.txt", row.MtimeNs, false); err != nil {
			t.Fatal(err)
		}
		b.pull()
		for i := 0; i < 3; i++ {
			b.sync()
		}
		if rows, _ := b.f.DB.held(); len(rows) != 0 {
			t.Fatalf("crash %v: held %+v", crash, rows)
		}
		if _, ok := e.serverFile(b.f.ID, "x.txt"); ok {
			t.Fatalf("crash %v: the file the rules excluded was sent again", crash)
		}
		if s, ok := b.read("x.txt"); !ok || s != "one" {
			t.Fatalf("crash %v: the local file: %q %v", crash, s, ok)
		}
	}
}

// A bare probe (proto.Op.Probe) answers with the current entry without its block list, the tombstone included; the
// probe of a discard keeps the blocks it fetches with.
func TestHeldDeleteBareProbe(t *testing.T) {
	e, a, b := heldDelete(t)
	ctx := context.Background()
	full, err := b.f.probe(ctx, false, "x.txt")
	if err != nil || full[0].Current == nil || len(full[0].Current.Blocks) == 0 {
		t.Fatalf("full probe: %+v %v", full, err)
	}
	bare, err := b.f.probe(ctx, true, "x.txt", "none.txt")
	if err != nil || bare[0].Status != proto.StatusConflict || bare[0].Current == nil || bare[0].Current.Hash == nil || len(bare[0].Current.Blocks) != 0 {
		t.Fatalf("bare probe of a live file: %+v %v", bare, err)
	}
	if bare[1].Status != proto.StatusOK || bare[1].Current != nil {
		t.Fatalf("bare probe of nothing: %+v", bare[1])
	}
	saveRulesConfirmed(t, e, b.f.ID, "x.txt")
	bare, err = b.f.probe(ctx, true, "x.txt")
	if err != nil || bare[0].Status != proto.StatusOK || bare[0].Current == nil || bare[0].Current.Hash != nil || !bare[0].Current.Excluded {
		t.Fatalf("bare probe of an excluded deletion: %+v %v", bare, err)
	}
	if s, ok := a.read("x.txt"); !ok || s != "two" {
		t.Fatalf("a probe changed a's file: %q %v", s, ok)
	}
}

// Probes are cut by the block lists the conflicts may answer with, as the bases tell them, and by the request size.
func TestHeldDeleteProbeBatch(t *testing.T) {
	long := strings.Repeat("&", proto.MaxJSONBody/12) // escapes to half of MaxJSONBody
	dels := map[string]heldDel{"a": {size: 3 * proto.BlockSize}, "b": {size: 1}, "c": {}, long: {}, long + "2": {}}
	for _, c := range []struct {
		ask  []string
		maxB int
		want int
	}{
		{[]string{"b", "c"}, 1, 2},
		{[]string{"a", "b"}, 3, 1},
		{[]string{"a", "b"}, 4, 2},
		{[]string{"a"}, 1, 1}, // the first goes alone whatever its size
		{[]string{long, long + "2"}, 100, 1},
	} {
		old := maxCommitBlocks
		maxCommitBlocks = c.maxB
		got := probeBatch(c.ask, dels)
		maxCommitBlocks = old
		if got != c.want {
			t.Errorf("%.10q with %d blocks: %d, want %d", c.ask, c.maxB, got, c.want)
		}
	}
	many := make([]string, proto.MaxCommitOps+5)
	for i := range many {
		many[i] = "c"
	}
	if got := probeBatch(many, dels); got != proto.MaxCommitOps {
		t.Errorf("%d paths: %d, want %d", len(many), got, proto.MaxCommitOps)
	}
}

// The Pull that settles the last held deletion of a scope thaws it, but read it past the freeze: the folder is not in
// step (last_synced_at, design 3.6) until the next Pull has read the scope again.
func TestHeldDeleteThawPendsUntilReplay(t *testing.T) {
	_, a, b := heldDelete(t)
	a.fs.Remove("x.txt")
	a.sync()
	if rep := b.pull(); rep.Synced() || len(b.f.frozen) != 0 {
		t.Fatalf("the settling pull: synced %v frozen %v", rep.Synced(), b.f.frozen)
	}
	if rep := b.pull(); !rep.Synced() {
		t.Fatalf("the replay: %+v", rep)
	}
}
