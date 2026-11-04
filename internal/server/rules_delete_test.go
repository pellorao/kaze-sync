package server

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

var confirmToken = regexp.MustCompile(`name="confirm" value="([0-9a-f]+)"`)

// Saving rules that cover files on the server first lists them; only a save that carries the token of that list
// deletes them, as ordinary versioned deletions that the clients are told came from the rules (design 8.3).
func TestSavingRulesListsThenDeletesTheCoveredFiles(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("F")
	e.commit(f, e.op("build/a.o", 0, "a"), e.op("build/b.o", 0, "b"), e.op("keep.txt", 0, "k"))
	form := url.Values{"rules": {"build/"}}

	st, body := e.post(c, "/folders/"+itoa(f)+"/rules", form)
	m := confirmToken.FindStringSubmatch(body)
	if st != 409 || m == nil || !strings.Contains(body, `data-hits="2"`) || !strings.Contains(body, "各设备上的本地文件保留") {
		t.Fatalf("first save: %d\n%s", st, body)
	}
	if got := hitsList(body); got != "build/a.o,build/b.o" {
		t.Fatalf("listed %q", got)
	}
	if e.count(`SELECT count(*) FROM folders WHERE rules='build/'`) != 0 || e.fcount(f, `SELECT count(*) FROM files WHERE hash IS NOT NULL`) != 3 {
		t.Fatal("the first POST changed something")
	}

	form.Set("confirm", "0000000000000000") // a list the person did not see
	if st, _ = e.post(c, "/folders/"+itoa(f)+"/rules", form); st != 409 {
		t.Fatalf("a wrong token: %d", st)
	}
	form.Set("confirm", m[1])
	if st, _ = e.post(c, "/folders/"+itoa(f)+"/rules", form); st != 303 {
		t.Fatalf("confirmed save: %d", st)
	}
	if e.count(`SELECT count(*) FROM folders WHERE rules='build/'`) != 1 {
		t.Fatal("rules not saved")
	}
	if e.fcount(f, `SELECT count(*) FROM files WHERE hash IS NOT NULL`) != 1 || e.fcount(f, `SELECT count(*) FROM versions WHERE path LIKE 'build/%'`) != 2 {
		t.Fatal("the files were not deleted with their versions kept")
	}
	// The deletions are marked for the clients: the changes carry Excluded for these, not for an ordinary deletion.
	tokDev, tok := e.enrollDevice("d1", "linux")
	e.mapFolder(tokDev, f, "/x")
	e.commit(f, e.op("plain.txt", 0, "p"))
	e.commit(f, proto.Op{Path: "plain.txt", BaseSeq: e.seq(f)})
	excl := map[string]bool{}
	for _, en := range e.changes(tok, f, 0).Entries {
		if en.Hash == nil {
			excl[en.Path] = en.Excluded
		}
	}
	if !excl["build/a.o"] || !excl["build/b.o"] || excl["plain.txt"] || len(excl) != 3 {
		t.Fatalf("deletions marked %v", excl)
	}

	// Rules that cover nothing save at once, and so does removing a rule.
	if st, _ = e.post(c, "/folders/"+itoa(f)+"/rules", url.Values{"rules": {"build/\n*.tmp"}}); st != 303 {
		t.Fatalf("no hits: %d", st)
	}
	if st, _ = e.post(c, "/folders/"+itoa(f)+"/rules", url.Values{"rules": {""}}); st != 303 {
		t.Fatalf("removing: %d", st)
	}
}

// New global rules reach into every folder; the list names the folders.
func TestSavingGlobalRulesDeletesInEveryFolder(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f1, f2 := e.folder("One"), e.folder("Two")
	e.commit(f1, e.op("x.log", 0, "1"), e.op("y.txt", 0, "1"))
	e.commit(f2, e.op("z.log", 0, "2"))
	form := url.Values{"rules": {"*.log"}}
	st, body := e.post(c, "/rules", form)
	m := confirmToken.FindStringSubmatch(body)
	if st != 409 || m == nil || !strings.Contains(body, `data-hits="2"`) || !strings.Contains(body, "One: x.log") || !strings.Contains(body, "Two: z.log") {
		t.Fatalf("list: %d\n%s", st, body)
	}
	form.Set("confirm", m[1])
	if st, _ = e.post(c, "/rules", form); st != 303 {
		t.Fatalf("confirmed: %d", st)
	}
	if e.fcount(f1, `SELECT count(*) FROM files WHERE hash IS NOT NULL`) != 1 || e.fcount(f2, `SELECT count(*) FROM files WHERE hash IS NOT NULL`) != 0 {
		t.Fatal("not deleted in every folder")
	}
}

// hitsList reads the paths of the confirmation list out of the page, in order.
func hitsList(body string) string {
	i := strings.Index(body, `data-hits=`)
	if i < 0 {
		return ""
	}
	var out []string
	for _, m := range regexp.MustCompile(`<li><span class="p">([^<]*)</span></li>`).FindAllStringSubmatch(body[i:], -1) {
		out = append(out, m[1])
	}
	return strings.Join(out, ",")
}
