package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

func (e *env) linkOp(path string, base int64, target string) proto.Op {
	op := e.op(path, base, target)
	op.Link = true
	return op
}

// Design 6.7: a link is an entry whose content is its target; it replaces and is replaced by files with versions,
// has a type of its own, takes no children, and keeps its flag through restore.
func TestLinkEntries(t *testing.T) {
	e := newEnv(t)
	f := e.folder("Docs")
	if r := e.commit(f, e.linkOp("l", 0, "../target"))[0]; r.Status != proto.StatusOK {
		t.Fatalf("link: %+v", r)
	}
	if r := e.commit(f, e.op("l/child", 0, "x"))[0]; r.Status != proto.StatusRejected || r.Reason != proto.ReasonTypeConflict {
		t.Fatalf("a file below a link: %+v", r)
	}
	if r := e.commit(f, e.linkOp("d/sub", 0, "t"), e.op("d", 0, "x"))[1]; r.Status != proto.StatusRejected {
		t.Fatalf("a file where a link's directory is: %+v", r)
	}
	long := strings.Repeat("x", proto.MaxLinkTarget+1)
	if r := e.commit(f, e.linkOp("long", 0, long))[0]; r.Status != proto.StatusRejected || r.Reason != proto.ReasonInvalidLink {
		t.Fatalf("long target: %+v", r)
	}
	if r := e.commit(f, e.linkOp("limit", 0, strings.Repeat("x", proto.MaxLinkTarget)))[0]; r.Status != proto.StatusOK {
		t.Fatalf("target at the limit: %+v", r)
	}
	if r := e.commit(f, e.linkOp("empty", 0, ""))[0]; r.Status != proto.StatusRejected || r.Reason != proto.ReasonInvalidLink {
		t.Fatalf("empty target: %+v", r)
	}

	db, _ := e.s.folderDB(f)
	var seq int64
	db.QueryRow(`SELECT seq FROM files WHERE path='l'`).Scan(&seq)
	// link -> file: the link becomes a version.
	if r := e.commit(f, e.op("l", seq, "plain"))[0]; r.Status != proto.StatusOK {
		t.Fatalf("file over link: %+v", r)
	}
	var vid int64
	if err := db.QueryRow(`SELECT id FROM versions WHERE path='l' AND link=1`).Scan(&vid); err != nil {
		t.Fatalf("the link was not kept as a version: %v", err)
	}
	if _, err := e.s.restoreVersion(f, vid); err != nil {
		t.Fatal(err)
	}
	var link bool
	var size int64
	if err := db.QueryRow(`SELECT link, size FROM files WHERE path='l'`).Scan(&link, &size); err != nil || !link || size != int64(len("../target")) {
		t.Fatalf("restored: link=%v size=%d %v", link, size, err)
	}
	// What the clients read carries the flag.
	var row fileRow
	if err := row.scan(db.QueryRow(`SELECT ` + fileCols + ` FROM files WHERE path='limit'`)); err != nil || !row.entry().Link {
		t.Fatalf("the entry loses the link flag: %v", err)
	}
}

// Export writes links as links, never follows one on the way, and replaces a link left at the file's place.
func TestExportLinks(t *testing.T) {
	e := newEnv(t)
	f := e.folder("Docs")
	e.commit(f, e.linkOp("lnk", 0, "/etc/passwd"), e.op("dir/file", 0, "content"), e.linkOp("dir/rel", 0, "file"))
	data := filepath.Dir(e.s.varDir)
	in := &Installer{Out: new(strings.Builder), Lookup: func(string) (int, int, error) { return 0, 0, os.ErrNotExist }}
	dir := t.TempDir()
	if err := in.Export(ExportOptions{Data: data, Dir: dir, Folder: "Docs"}); err != nil {
		t.Fatal(err)
	}
	root := dir
	if tg, err := os.Readlink(filepath.Join(root, "lnk")); err != nil || tg != "/etc/passwd" {
		t.Fatalf("lnk -> %q %v", tg, err)
	}
	if tg, err := os.Readlink(filepath.Join(root, "dir", "rel")); err != nil || tg != "file" {
		t.Fatalf("dir/rel -> %q %v", tg, err)
	}

	// An export directory that already has links where files go: the files replace the links, nothing outside is written.
	outside := t.TempDir()
	dir2 := t.TempDir()
	os.Symlink(outside, filepath.Join(dir2, "dir"))                          // a link where the directory "dir" goes
	os.Symlink(filepath.Join(outside, "victim"), filepath.Join(dir2, "lnk")) // a link where the link "lnk" goes
	err := in.Export(ExportOptions{Data: data, Dir: dir2, Folder: "Docs"})
	if err == nil {
		t.Error("export below a link reported no problem")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("export wrote outside its directory: %v", entries)
	}
	if tg, _ := os.Readlink(filepath.Join(dir2, "lnk")); tg != "/etc/passwd" {
		t.Fatalf("lnk -> %q", tg)
	}
}

// A device's links it cannot create fold to one line however few there are (design 6.7).
func TestLinkNoticeFoldsToOneLine(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	dev, tok := e.enrollDevice("win", "windows")
	e.mapFolder(dev, f, "/docs")
	msg := proto.LinkNotCreated + " (on Windows turn on Developer Mode)"
	e.report(tok, f, proto.FileError{Path: "a", Kind: proto.KindSkipped, Message: msg, Since: e.clk.Now().Unix()},
		proto.FileError{Path: "b", Kind: proto.KindSkipped, Message: msg, Since: e.clk.Now().Unix()},
		proto.FileError{Path: "c", Kind: proto.KindSkipped, Message: msg, Since: e.clk.Now().Unix()})
	_, page := e.get(c, "/")
	if !strings.Contains(page, "win: 3 个符号链接未建立") || !strings.Contains(page, "开发人员模式") {
		t.Fatalf("no folded line:\n%s", page)
	}
	e.report(tok, f, proto.FileError{Path: "a", Kind: proto.KindSkipped, Message: msg, Since: e.clk.Now().Unix()})
	_, page = e.get(c, "/")
	if !strings.Contains(page, "win: 1 个符号链接未建立") {
		t.Fatalf("a single link is not folded:\n%s", page)
	}
}
