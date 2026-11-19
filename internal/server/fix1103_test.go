package server

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

// A keep_days that cannot be read stops the GC instead of expiring everything older than a day.
func TestGCRefusesWithoutKeepDays(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := e.folder("Docs")
	e.commit(f, e.op("a", 0, "first"))
	e.clk.Add(time.Hour)
	e.commit(f, e.op("a", 1, "second"))
	e.clk.Add(5 * 24 * time.Hour)
	for _, bad := range []string{`DELETE FROM meta WHERE key='keep_days'`, `UPDATE meta SET value=0 WHERE key='keep_days'`} {
		e.s.db.Exec(`INSERT OR REPLACE INTO meta VALUES('keep_days', 30)`)
		e.s.db.Exec(bad)
		if _, err := e.s.gc(); err == nil {
			t.Fatalf("%s: gc went ahead", bad)
		}
		if e.fcount(f, `SELECT count(*) FROM versions`) != 1 {
			t.Fatalf("%s: a version was deleted", bad)
		}
	}
}

// One unreadable folder database does not take the overview down (design 2.3), and a leftover database file
// of the same id does not stop a new folder from being created.
func TestOverviewSurvivesBrokenFolderAndLeftovers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	good, broken := e.folder("good"), e.folder("broken")
	e.commit(good, e.op("a", 0, "x"))
	e.s.fmu.Lock()
	e.s.fdbs[broken].Close()
	delete(e.s.fdbs, broken)
	e.s.fmu.Unlock()
	if err := os.WriteFile(FolderDBPath(e.s.varDir, broken), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, page := e.get(c, "/")
	if st != 200 || !strings.Contains(page, "good") || !strings.Contains(page, "无法读取") {
		t.Fatalf("overview: %d\n%s", st, page)
	}

	// A crash between creating the file and committing leaves it behind; the id is then handed out for real.
	next := broken + 1
	stale := FolderDBPath(e.s.varDir, next)
	if err := os.WriteFile(stale, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := e.s.createFolder("later")
	if err != nil || id != next {
		t.Fatalf("createFolder: %d %v", id, err)
	}
	if st, _ := e.get(c, "/folders/"+itoa(id)); st != 200 {
		t.Fatalf("new folder page: %d", st)
	}
}

// The history queries work without HTTP: a version comes back as the current state, and a directory comes back as
// it was at a time (history.go).
func TestHistoryWithoutHTTP(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := e.folder("Docs")
	e.commit(f, e.op("d/a", 0, "first"), e.op("d/b", 0, "bee"))
	between := e.clk.Now().Add(30 * time.Minute)
	e.clk.Add(time.Hour)
	e.commit(f, e.op("d/a", 1, "second"))
	e.commit(f, proto.Op{Path: "d/b", BaseSeq: 2}) // deleted
	e.clk.Add(time.Hour)

	if _, err := e.s.restoreVersion(f, 999); !errors.Is(err, errNoVersion) {
		t.Fatalf("unknown version: %v", err)
	}
	var vid int64
	e.s.folderDBMust(t, f).QueryRow(`SELECT id FROM versions WHERE path='d/a'`).Scan(&vid)
	if p, err := e.s.restoreVersion(f, vid); err != nil || p != "d/a" {
		t.Fatalf("restoreVersion: %q %v", p, err)
	}
	if got := readCurrent(t, e, f, "d/a"); got != "first" {
		t.Fatalf("d/a = %q", got)
	}
	e.commit(f, e.op("d/a", 5, "third"))
	restored, failed, err := e.s.restoreDirAt(f, "d", between)
	if err != nil || restored != 2 || failed != 0 {
		t.Fatalf("restoreDirAt: %d %d %v", restored, failed, err)
	}
	if readCurrent(t, e, f, "d/a") != "first" || readCurrent(t, e, f, "d/b") != "bee" {
		t.Fatal("the directory is not as it was")
	}
}

func (s *Server) folderDBMust(t *testing.T, id int64) *sql.DB {
	t.Helper()
	db, err := s.folderDB(id)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func readCurrent(t *testing.T, e *env, f int64, p string) string {
	t.Helper()
	var blocks []byte
	if err := e.s.folderDBMust(t, f).QueryRow(`SELECT blocks FROM files WHERE path=? AND hash IS NOT NULL`, p).Scan(&blocks); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	var out []byte
	for _, h := range proto.SplitHashes(blocks) {
		r, err := e.s.blobs.Open(h)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r)
		r.Close()
		out = append(out, b...)
	}
	return string(out)
}
