package client

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// lostRow commits two versions of a.txt, then puts the local row of the first back: the server has the second, the
// device does not know it (a commit whose local row was not stored). The file then changes to a third version.
func lostRow(t *testing.T, name string) (*cenv, int64, *engine) {
	t.Helper()
	e := newCEnv(t)
	f := e.newFolder("Docs")
	dev, api := e.newDevice()
	e.mapFolder(dev, f, "/data/docs", true)
	g := e.engine(api, f, nil, true)
	if name == "@own" {
		name = dev.Name
	}
	g.f.DeviceName = name
	g.put("a.txt", []byte("one"))
	g.scan()
	row, ok, err := g.f.DB.get("a.txt")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	g.put("a.txt", []byte("two!"))
	g.scan()
	if err := g.f.DB.apply([]localRow{row}, nil); err != nil {
		t.Fatal(err)
	}
	g.put("a.txt", []byte("three"))
	return e, f, g
}

func conflictCopies(g *engine) int {
	n := 0
	entries, _ := g.fs.ReadDir("")
	for _, en := range entries {
		if strings.Contains(en.Name, ".conflict-") {
			n++
		}
	}
	return n
}

// The server's current entry is this device's own commit: commit on top of it, no conflict copy.
func TestOwnCommitIsNotAConflict(t *testing.T) {
	t.Parallel()
	e, f, g := lostRow(t, "@own")
	g.scan()
	if n := conflictCopies(g); n != 0 {
		t.Fatalf("%d conflict copies", n)
	}
	if got, _ := e.serverFile(f, "a.txt"); string(got) != "three" {
		t.Fatalf("server has %q", got)
	}
}

// Another device name, or none: a real conflict, which is held.
func TestForeignCommitStaysAConflict(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"somebody-else", ""} {
		_, _, g := lostRow(t, name)
		g.scan()
		if n := g.f.DB.heldCount(); n != 1 || conflictCopies(g) != 0 {
			t.Fatalf("name %q: %d held, %d conflict copies", name, n, conflictCopies(g))
		}
	}
}

// A database written by a newer build is refused, and the refusal is not a failure to open a damaged file.
func TestNewerLocalDatabaseIsRefused(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "folder-1.db")
	l, err := openLocal(p)
	if err != nil {
		t.Fatal(err)
	}
	if v := l.metaInt("schema_version"); v != localSchemaVersion {
		t.Fatalf("version %d", v)
	}
	l.setMeta("schema_version", localSchemaVersion+1)
	l.Close()
	if _, err := openLocal(p); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open: %v", err)
	}
}

// A database of the first version (no link column) is migrated and marked.
func TestOldLocalDatabaseIsMigrated(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "folder-1.db")
	db, _ := sql.Open("sqlite", p)
	db.Exec(`CREATE TABLE local(path TEXT PRIMARY KEY, key TEXT NOT NULL, seq INTEGER NOT NULL, hash BLOB NOT NULL,
  blocks BLOB NOT NULL, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, exec INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE meta(key TEXT PRIMARY KEY, value) WITHOUT ROWID; INSERT INTO meta VALUES('schema_version', 1)`)
	db.Close()
	l, err := openLocal(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if v := l.metaInt("schema_version"); v != localSchemaVersion {
		t.Fatalf("version %d", v)
	}
	if err := l.apply([]localRow{{Path: "x", Key: "x", Hash: []byte{1}, Blocks: []byte{}, Link: true}}, nil); err != nil {
		t.Fatal(err)
	}
}

// A failure to store the scan's state rows must not stop the commit.
func TestScanGoesOnWhenStateRowsCannotBeStored(t *testing.T) {
	t.Parallel()
	e := newCEnv(t)
	f := e.newFolder("Docs")
	dev, api := e.newDevice()
	e.mapFolder(dev, f, "/data/docs", true)
	g := e.engine(api, f, nil, true)
	g.put("a.txt", []byte("one"))
	g.scan()
	g.put("a.txt", []byte("one")) // same content, new mtime: a stat-only row
	g.put("b.txt", []byte("bee"))
	g.f.DB.db.Exec(`CREATE TRIGGER nope BEFORE INSERT ON local WHEN new.path='a.txt' BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	rep, err := g.f.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := e.serverFile(f, "b.txt"); !ok || string(got) != "bee" {
		t.Fatalf("b.txt not committed (committed %d)", rep.Committed)
	}
}
