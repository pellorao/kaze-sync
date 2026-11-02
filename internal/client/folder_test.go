package client

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

const miB = proto.BlockSize

func setup(t *testing.T) (*cenv, int64, *API, *engine) {
	t.Helper()
	e := newCEnv(t)
	f := e.newFolder("Docs")
	dev, api := e.newDevice()
	e.mapFolder(dev, f, "/data/docs", true)
	return e, f, api, e.engine(api, f, []string{"*.tmp"}, true)
}

func TestScanUploadsAndVersions(t *testing.T) {
	e, f, _, g := setup(t)
	g.put("a.txt", []byte("one"))
	g.put("dir/b.txt", []byte("bee"))
	g.put("skip.tmp", []byte("excluded"))
	g.put("empty", nil)
	rep := g.scan()
	if rep.Committed != 3 || rep.State != "idle" || rep.PendingUp != 0 {
		t.Fatalf("report %+v", rep)
	}
	for p, want := range map[string]string{"a.txt": "one", "dir/b.txt": "bee", "empty": ""} {
		if got, ok := e.serverFile(f, p); !ok || string(got) != want {
			t.Errorf("%s: %q %v", p, got, ok)
		}
	}
	for _, p := range []string{"skip.tmp", ".syncroot"} {
		if _, ok := e.serverFile(f, p); ok {
			t.Errorf("%s was uploaded", p)
		}
	}
	if rep := g.scan(); rep.Committed != 0 {
		t.Fatalf("nothing changed, yet %+v", rep)
	}

	g.put("a.txt", []byte("two"))
	g.fs.Remove("dir/b.txt")
	if rep := g.scan(); rep.Committed != 2 {
		t.Fatalf("modify and delete: %+v", rep)
	}
	if got, _ := e.serverFile(f, "a.txt"); string(got) != "two" {
		t.Fatalf("a.txt = %q", got)
	}
	if _, ok := e.serverFile(f, "dir/b.txt"); ok {
		t.Fatal("deleted file still current on the server")
	}
	// Both the overwrite and the deletion left a version.
	if n := e.count(f, `SELECT count(*) FROM versions WHERE path IN ('a.txt','dir/b.txt')`); n != 2 {
		t.Fatalf("versions: %d", n)
	}
	if rep := g.scan(); rep.Committed != 0 {
		t.Fatalf("second quiet scan: %+v", rep)
	}
}

func TestUnstableFilesWait(t *testing.T) {
	_, _, _, g := setup(t)
	g.f.Stable = 1 << 62 // everything counts as just written (Mem mtimes are tiny but positive)
	g.f.Now = func() time.Time { return time.Unix(0, 1_000_000_001_000) }
	g.put("a.txt", []byte("x"))
	if rep := g.scan(); rep.Committed != 0 || !rep.Retry || rep.PendingUp != 0 {
		t.Fatalf("report %+v", rep)
	}
	g.f.Stable = 0
	if rep := g.scan(); rep.Committed != 1 || rep.Retry {
		t.Fatalf("after the stable period: %+v", rep)
	}
}

func TestChunkedUploadSendsOnlyChangedBlocks(t *testing.T) {
	e, f, api, g := setup(t)
	ct := &countTransport{base: api.HC.Transport}
	api.HC.Transport = ct
	data := pseudoRandom(5*miB+123, 1) // six blocks, the last one short
	g.put("big.db", data)
	g.scan()
	if got, _ := e.serverFile(f, "big.db"); !bytes.Equal(got, data) {
		t.Fatal("first upload content differs")
	}
	if want := int64(5*(36+miB) + 36 + 123); ct.bytes.Load() != want {
		t.Fatalf("first upload sent %d bytes, want %d", ct.bytes.Load(), want)
	}
	ct.bytes.Store(0)
	mod := bytes.Clone(data)
	copy(mod[3*miB+100:], pseudoRandom(1000, 9)) // touches block 3 only
	g.put("big.db", mod)
	g.scan()
	if got, _ := e.serverFile(f, "big.db"); !bytes.Equal(got, mod) {
		t.Fatal("second upload content differs")
	}
	if want := int64(36 + miB); ct.bytes.Load() != want {
		t.Fatalf("modified one block, sent %d bytes, want %d", ct.bytes.Load(), want)
	}
	// Renaming sends no blocks.
	ct.packets.Store(0)
	g.fs.Rename("big.db", "moved.db")
	g.scan()
	if ct.packets.Load() != 0 {
		t.Fatal("rename uploaded blocks")
	}
	if _, ok := e.serverFile(f, "moved.db"); !ok {
		t.Fatal("moved file missing on the server")
	}
}

func TestKilledUploadResumesWithRemainingBlocks(t *testing.T) {
	e, f, api, g := setup(t)
	ft := cfs.NewFaultTransport(api.HC.Transport)
	api.HC.Transport = ft
	data := pseudoRandom(6*miB, 2)
	g.put("big.bin", data)
	// Requests: commit (answers need), then the block upload, which dies inside its third block.
	ft.CutBodyAfter(1, 2*(36+miB)+500, errors.New("killed"))
	if _, err := g.f.Scan(t.Context()); err == nil {
		t.Fatal("scan survived the cut upload")
	}
	if _, ok := e.serverFile(f, "big.bin"); ok {
		t.Fatal("file committed without all blocks")
	}
	if n := e.count(f, `SELECT count(*) FROM files`); n != 0 {
		t.Fatal("files committed")
	}
	// A new process continues: only the four missing blocks are sent.
	ct := &countTransport{base: api.HC.Transport}
	api.HC.Transport = ct
	g.restart(api, proto.FolderConfig{ID: f, Rules: []string{"*.tmp"}, Adopt: true}, g.fs)
	g.scan()
	if want := int64(4 * (36 + miB)); ct.bytes.Load() != want {
		t.Fatalf("resumed upload sent %d bytes, want %d", ct.bytes.Load(), want)
	}
	if got, _ := e.serverFile(f, "big.bin"); !bytes.Equal(got, data) {
		t.Fatal("content differs after resume")
	}
}

func TestSQLiteInUse(t *testing.T) {
	e, f, _, g := setup(t)
	db := append([]byte("SQLite format 3\x00"), pseudoRandom(4000, 3)...)
	g.put("app.db", db)
	g.put("app.db-wal", []byte("pending"))
	g.put("app.db-shm", []byte("shm"))
	g.put("notes.txt", []byte("not a database"))
	g.put("notes.txt-wal", []byte("x")) // looks like a side file, but notes.txt is no database
	rep := g.scan()
	var inUse bool
	for _, er := range rep.Errors {
		inUse = inUse || er.Kind == proto.KindInUse && er.Path == "app.db"
	}
	if !inUse || rep.State != "idle" {
		t.Fatalf("report %+v", rep)
	}
	for _, p := range []string{"app.db", "app.db-wal", "app.db-shm", "notes.txt-wal"} {
		if _, ok := e.serverFile(f, p); ok {
			t.Errorf("%s was uploaded", p)
		}
	}
	if _, ok := e.serverFile(f, "notes.txt"); !ok {
		t.Error("a regular file with a -wal sibling must still sync")
	}
	// The application closes the database: the wal is gone and the file syncs.
	g.fs.Remove("app.db-wal")
	g.fs.Remove("app.db-shm")
	rep = g.scan()
	if got, ok := e.serverFile(f, "app.db"); !ok || !bytes.Equal(got, db) || len(rep.Errors) != 0 {
		t.Fatalf("after closing: ok=%v report %+v", ok, rep)
	}
}

func TestMappingSafety(t *testing.T) {
	e := newCEnv(t)
	f := e.newFolder("Docs")
	dev, api := e.newDevice()
	e.mapFolder(dev, f, "/data/docs", false)

	// A non-empty directory is not adopted without permission: nothing uploaded, no marker.
	g := e.engine(api, f, nil, false)
	g.put("precious.txt", []byte("mine"))
	rep := g.scan()
	if rep.State != "error" || len(rep.Errors) != 1 || rep.Errors[0].Kind != proto.KindFolder || rep.Committed != 0 {
		t.Fatalf("report %+v", rep)
	}
	if _, err := g.fs.Stat(markerName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("marker written into a foreign directory")
	}
	if e.count(f, `SELECT count(*) FROM files`) != 0 {
		t.Fatal("something was uploaded")
	}
	// The error keeps its start time while it lasts.
	since := rep.Errors[0].Since
	if rep2 := g.scan(); rep2.Errors[0].Since != since {
		t.Fatal("error start time changed")
	}
	// Ticking "adopt" (a new Folder from the new configuration) starts syncing.
	g.restart(api, proto.FolderConfig{ID: f, Adopt: true}, g.fs)
	if rep := g.scan(); rep.Committed != 1 || rep.State != "idle" {
		t.Fatalf("after adopting: %+v", rep)
	}
	// The sync root vanishes (disk not mounted): no deletions, a folder error.
	g.fs.Remove("precious.txt")
	g.fs.Remove(markerName)
	rep = g.scan()
	if rep.State != "error" || rep.Errors[0].Kind != proto.KindFolder || rep.Committed != 0 {
		t.Fatalf("report %+v", rep)
	}
	if _, ok := e.serverFile(f, "precious.txt"); !ok {
		t.Fatal("deleted on the server although the root was gone")
	}
	// The marker returns: now the missing file is a real deletion.
	putFile(t, g.fs, markerName, nil)
	if rep := g.scan(); rep.Committed != 1 {
		t.Fatalf("after the marker came back: %+v", rep)
	}
}

func TestEmptyAndMissingDirectoriesGetMarker(t *testing.T) {
	_, _, _, g := setup(t)
	g.f.Adopt = false
	if rep := g.scan(); rep.State != "idle" {
		t.Fatalf("%+v", rep)
	}
	if _, err := g.fs.Stat(markerName); err != nil {
		t.Fatal("no marker in an empty directory")
	}
}

// failDirFS fails reading one directory.
type failDirFS struct {
	cfs.FS
	dir string
}

func (f failDirFS) ReadDir(d string) ([]cfs.Entry, error) {
	if d == f.dir {
		return nil, errors.New("permission denied")
	}
	return f.FS.ReadDir(d)
}

func TestDeletionSafety(t *testing.T) {
	e, f, api, g := setup(t)
	g.put("keep/a.txt", []byte("a"))
	g.put("secret/s.txt", []byte("s"))
	g.put("top.txt", []byte("t"))
	g.scan()

	// An unreadable directory is not "everything deleted".
	g.restart(api, proto.FolderConfig{ID: f, Adopt: true}, failDirFS{g.fs, "keep"})
	rep := g.scan()
	if rep.Committed != 0 || len(rep.Errors) != 1 || rep.Errors[0].Kind != proto.KindIO {
		t.Fatalf("report %+v", rep)
	}
	if _, ok := e.serverFile(f, "keep/a.txt"); !ok {
		t.Fatal("file below an unreadable directory deleted")
	}

	// A newly excluded path is forgotten locally and untouched on the server.
	g.restart(api, proto.FolderConfig{ID: f, Adopt: true, Rules: []string{"secret/"}}, g.fs)
	if rep := g.scan(); rep.Committed != 0 {
		t.Fatalf("exclusion committed something: %+v", rep)
	}
	if _, ok := e.serverFile(f, "secret/s.txt"); !ok {
		t.Fatal("excluded file removed from the server")
	}
	rows, _ := g.f.DB.all()
	if _, ok := rows["secret/s.txt"]; ok {
		t.Fatal("excluded file still tracked")
	}
}

func TestServerRefusals(t *testing.T) {
	e, f, _, g := setup(t)
	g.put("A.txt", []byte("1"))
	g.put("a.txt", []byte("2"))
	g.put("bad:name.txt", []byte("3"))
	rep := g.scan()
	reasons := map[string]string{}
	for _, er := range rep.Errors {
		if er.Kind == proto.KindRejected {
			reasons[er.Path] = er.Message
		}
	}
	if reasons["a.txt"] != "case_collision" || reasons["bad:name.txt"] != "invalid_name" || len(reasons) != 2 || rep.State != "error" {
		t.Fatalf("report %+v", rep)
	}
	if rep.PendingUp != 2 || e.count(f, `SELECT count(*) FROM files`) != 1 {
		t.Fatalf("pending %d", rep.PendingUp)
	}
}

// second returns another device mapped to the same folder.
func second(t *testing.T, e *cenv, f int64) *engine {
	t.Helper()
	dev, api := e.newDevice()
	e.mapFolder(dev, f, "/data/other", true)
	return e.engine(api, f, nil, true)
}

func TestConflictsThatNeedNoPull(t *testing.T) {
	e, f, _, g1 := setup(t)
	g2 := second(t, e, f)

	// Same new content on both: adopted without any error.
	g1.put("same.txt", []byte("equal"))
	g2.put("same.txt", []byte("equal"))
	g1.scan()
	if rep := g2.scan(); len(rep.Errors) != 0 || rep.PendingUp != 0 {
		t.Fatalf("same content: %+v", rep)
	}
	rows, _ := g2.f.DB.all()
	if rows["same.txt"].Seq == 0 {
		t.Fatal("not adopted into the local state")
	}
	if rep := g2.scan(); rep.Committed != 0 {
		t.Fatalf("adopted file committed again: %+v", rep)
	}

	// A lost local row (crash after the commit) is repaired the same way.
	g1.f.DB.apply(nil, []string{"same.txt"})
	if rep := g1.scan(); len(rep.Errors) != 0 || rep.PendingUp != 0 {
		t.Fatalf("lost row: %+v", rep)
	}

	// Modification beats deletion.
	g1.put("doc.txt", []byte("v1"))
	g1.scan()
	g2.put("doc.txt", []byte("v1")) // g2 adopts the same content
	g2.scan()
	g2.fs.Remove("doc.txt")
	g2.scan()
	g1.put("doc.txt", []byte("v2 by dev1"))
	if rep := g1.scan(); rep.PendingUp != 0 || len(rep.Errors) != 0 {
		t.Fatalf("modify vs delete: %+v", rep)
	}
	if got, _ := e.serverFile(f, "doc.txt"); string(got) != "v2 by dev1" {
		t.Fatalf("doc.txt = %q", got)
	}

	// Different content on both sides: the server version stays, the local one becomes a conflict copy (design 6.1).
	g2.put("doc.txt", []byte("v3 by dev2"))
	rep := g2.scan()
	if got, _ := e.serverFile(f, "doc.txt"); string(got) != "v2 by dev1" {
		t.Fatalf("a stale writer overwrote the server: %q", got)
	}
	if len(rep.Errors) != 0 || !rep.Retry {
		t.Fatalf("conflict scan: %+v", rep)
	}
	if rep := g2.scan(); rep.Committed != 1 || rep.PendingUp != 0 { // the copy is uploaded by the retry
		t.Fatalf("conflict copy scan: %+v", rep)
	}
	if _, err := g2.fs.Stat("doc.txt"); err != nil {
		t.Fatalf("server version not downloaded: %v", err)
	}
}
