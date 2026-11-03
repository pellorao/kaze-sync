package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"kaze-sync/internal/ignore"
	"kaze-sync/internal/proto"
)

func TestFolderDatabasesAreIndependent(t *testing.T) {
	e := newEnv(t)
	a, b := e.folder("a"), e.folder("b")
	for _, f := range []int64{a, b} {
		if _, err := os.Stat(FolderDBPath(e.s.varDir, f)); err != nil {
			t.Fatalf("folder %d has no database: %v", f, err)
		}
	}
	// seq counts per folder.
	if r := e.commit(a, e.op("x", 0, "1"), e.op("y", 0, "1")); r[1].Seq != 2 {
		t.Fatalf("a: %+v", r)
	}
	if r := e.commit(b, e.op("x", 0, "1")); r[0].Seq != 1 {
		t.Fatalf("b starts at its own seq 1: %+v", r)
	}
	// Cursors of the mapped folders, per folder.
	dev, tok := e.enrollDevice("d", "linux")
	e.mapFolder(dev, a, "/a")
	e.mapFolder(dev, b, "/b")
	_, out := e.api(tok, "poll", proto.PollReq{ConfigVersion: 1})
	var pr proto.PollResp
	json.Unmarshal(out, &pr)
	if len(pr.Cursors) != 2 || pr.Cursors[a] != 2 || pr.Cursors[b] != 1 {
		t.Fatalf("cursors %v", pr.Cursors)
	}
	// A broken database of one folder leaves the other folder working.
	e.s.fmu.Lock()
	e.s.fdbs[a].Close()
	delete(e.s.fdbs, a)
	e.s.fmu.Unlock()
	if err := os.WriteFile(FolderDBPath(e.s.varDir, a), []byte("not a database, long enough to be read as a header....................................."), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(FolderDBPath(e.s.varDir, a) + "-wal")
	os.Remove(FolderDBPath(e.s.varDir, a) + "-shm")
	if _, err := e.s.commit(a, "dev", []proto.Op{e.op("z", 0, "1")}); err == nil {
		t.Fatal("commit into a broken database succeeded")
	}
	if r := e.commit(b, e.op("y", 0, "2")); status(r) != "ok" {
		t.Fatalf("b after a broke: %+v", r)
	}
}

func TestCreateFolderIsAtomic(t *testing.T) {
	e := newEnv(t)
	// A leftover database file at the id the folder would get is replaced (the id is new: design 2.3).
	path := FolderDBPath(e.s.varDir, 1)
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("stale"), 0o600)
	if _, err := e.s.createFolder("docs"); err != nil {
		t.Fatalf("createFolder over a leftover: %v", err)
	}
	// What cannot be removed makes the creation fail without a folder row.
	blocked := FolderDBPath(e.s.varDir, 2)
	os.MkdirAll(filepath.Join(blocked, "x"), 0o700)
	if _, err := e.s.createFolder("more"); err == nil {
		t.Fatal("created over an unremovable path")
	}
	if e.count(`SELECT count(*) FROM folders`) != 1 {
		t.Fatal("folder row left behind")
	}
	os.RemoveAll(blocked)
	// A missing database is never recreated implicitly.
	os.Remove(path)
	if _, err := e.s.folderDB(7); err == nil {
		t.Fatal("opened a missing folder database")
	}
	if _, err := os.Stat(FolderDBPath(e.s.varDir, 7)); err == nil {
		t.Fatal("missing folder database created")
	}
}

func TestMigrateFoldersRunsPerDatabase(t *testing.T) {
	e := newEnv(t)
	f := e.folder("docs")
	if err := migrateFolders(e.s.db, e.s.varDir); err != nil {
		t.Fatal(err)
	}
	db, _ := e.s.folderDB(f)
	if v, _ := schemaVersion(db); v != FolderSchemaVersion {
		t.Fatalf("folder schema %d", v)
	}
	e.s.db.Exec(`INSERT INTO folders(id, name, rules) VALUES(9, 'ghost', '')`)
	if err := migrateFolders(e.s.db, e.s.varDir); err == nil {
		t.Fatal("a folder without a database passed migration")
	}
}

// The factory rules are checked against git on sample paths of the new entries.
func TestDefaultRulesAgainstGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	rules := strings.Split(defaultRules, "\n")
	m, errs := ignore.Compile(rules, false)
	if len(errs) != 0 {
		t.Fatalf("default rules do not compile: %v", errs)
	}
	type sample struct {
		path  string
		isDir bool
	}
	var samples []sample
	// Directory-only rules (trailing slash) are sampled as directories, the rest as files.
	for _, p := range []string{"$RECYCLE.BIN", "System Volume Information", ".Trash-1000", ".Trashes", ".Spotlight-V100",
		".TemporaryItems", ".DocumentRevisions-V100", ".fseventd", ".stfolder", ".stversions"} {
		samples = append(samples, sample{p, true}, sample{"sub/" + p, true})
	}
	for _, p := range []string{"$RECYCLE.BIN", ".Trash-1000", ".stfolder", ".Trash", "System Volume"} {
		samples = append(samples, sample{"f/" + p, false})
	}
	for _, p := range []string{"a.txt~", ".~lock.a.odt#", ".a.txt.swp", "x.kate-swp", "big.iso.part", "f.filepart", "v.crdownload",
		"~$a.docx", "d.tmp", "x.part.txt", "sw.swp.bak", ".apdisk", ".directory", "n.textClipping", "photothumb.db", ".fuse_hidden0001",
		".nfs0001", "._a.txt", ".stignore", "s.unison", ".sync.ffs_db", ".sync_x.db-wal", "keep.txt", "notes.md", "a.swp", ".Trash"} {
		samples = append(samples, sample{p, false}, sample{"sub/" + p, false})
	}
	root := t.TempDir()
	git := func(stdin string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if ee, ok := err.(*exec.ExitError); err != nil && !(ok && ee.ExitCode() == 1) {
			t.Fatalf("git %v: %v", args, err)
		}
		return string(out)
	}
	git("", "init", "-q")
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte(defaultRules+"\n"), 0o644)
	// check-ignore --no-index judges a path as a directory only when it exists as one.
	var stdin strings.Builder
	for _, s := range samples {
		full := filepath.Join(root, s.path)
		if s.isDir {
			os.MkdirAll(full, 0o755)
		} else {
			os.MkdirAll(filepath.Dir(full), 0o755)
			os.WriteFile(full, nil, 0o644)
		}
	}
	for _, s := range samples {
		stdin.WriteString(s.path + "\x00")
	}
	ignored := map[string]bool{}
	for _, p := range strings.Split(git(stdin.String(), "check-ignore", "--no-index", "-z", "--stdin"), "\x00") {
		ignored[p] = true
	}
	for _, s := range samples {
		if got, want := m.ExcludedPath(s.path, s.isDir), ignored[s.path]; got != want {
			t.Errorf("%q dir=%v: matcher %v, git %v", s.path, s.isDir, got, want)
		}
	}
}
