package server

import (
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

// attr reads a JSON attribute of the page (the dialogs read their data from it).
func attr(t *testing.T, page, name string, into any) {
	t.Helper()
	m := regexp.MustCompile(name + `="([^"]*)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the page has no %s", name)
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), into); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// The write-control dialog carries, per device and scope, how many changes are held and the directory that contains
// them all, so that saving a setting over them asks first (design 2.8.11).
func TestWriteControlDialogCarriesHeldSpans(t *testing.T) {
	t.Parallel()
	l, it := heldEnv(t)
	at := func(p string) proto.HeldItem { x := it; x.Path = p; return x }
	l.heldReport(l.tb, l.f, at("d/a/x.txt"), at("d/a/y.txt"), at("d/b/z.txt"), at("d/top.txt"))
	var spans []heldSpan
	_, page := l.get(l.web, "/folders/"+itoa(l.f))
	attr(t, page, "data-held", &spans)
	if len(spans) != 1 || spans[0].P != "d" || spans[0].N != 4 || spans[0].D != "B" {
		t.Fatalf("spans %+v", spans)
	}
	l.heldReport(l.tb, l.f, at("d/a/x.txt"), at("d/a/y.txt"))
	_, page = l.get(l.web, "/folders/"+itoa(l.f))
	attr(t, page, "data-held", &spans)
	if len(spans) != 1 || spans[0].P != "d/a" || spans[0].N != 2 {
		t.Fatalf("spans %+v", spans)
	}
	l.heldReport(l.tb, l.f)
	_, page = l.get(l.web, "/folders/"+itoa(l.f))
	attr(t, page, "data-held", &spans)
	if len(spans) != 0 {
		t.Fatalf("spans %+v after the list went empty", spans)
	}
}

// The restore dialog learns of a single writer that wrote a moment ago, and stops hearing of it after the window.
func TestRestoreNoteNamesLiveSingleWriter(t *testing.T) {
	t.Parallel()
	l, _ := heldEnv(t)
	type lk struct {
		P, K, D string
		S       bool
	}
	var locks []lk
	_, page := l.get(l.web, "/folders/"+itoa(l.f))
	attr(t, page, "data-locks", &locks)
	if len(locks) != 1 || locks[0].D != "A" || !locks[0].S || locks[0].P != "" {
		t.Fatalf("locks %+v", locks)
	}
	l.clk.Add(time.Duration(metaInt(l.s.db, "write_window")+1) * time.Second)
	_, page = l.get(l.web, "/folders/"+itoa(l.f))
	attr(t, page, "data-locks", &locks)
	if len(locks) != 0 {
		t.Fatalf("locks %+v after the window", locks)
	}
	l.lock("d", l.b)
	_, page = l.get(l.web, "/folders/"+itoa(l.f))
	attr(t, page, "data-locks", &locks)
	if len(locks) != 1 || locks[0].D != "B" || locks[0].S || locks[0].P != "d" {
		t.Fatalf("locks %+v with an exclusive", locks)
	}
}

// Every group of the decision dialog has the warning for mixed choices (hidden until the script shows it).
func TestHeldDialogHasMixedChoiceWarning(t *testing.T) {
	t.Parallel()
	l, it := heldEnv(t)
	l.heldReport(l.tb, l.f, it)
	_, page := l.get(l.web, "/folders/"+itoa(l.f)+"?held="+itoa(l.b))
	if !strings.Contains(page, "data-mixwarn hidden") {
		t.Error("the decision dialog lacks the warning for mixed choices")
	}
}

// A restore sent back with an error reopens the dialog by itself; the page still carries the locks the dialog names
// in its note (design 4.5), the same as the page the menu opens it on.
func TestRestoreErrorPageCarriesLocks(t *testing.T) {
	t.Parallel()
	l, _ := heldEnv(t)
	st, page := l.post(l.web, "/folders/"+itoa(l.f)+"/restore-dir", url.Values{"path": {""}, "dir": {""}, "time": {"garbage"}})
	if st != 400 || !regexp.MustCompile(`id="dlg-restore"[^>]* data-autoopen`).MatchString(page) {
		t.Fatalf("restore error: %d, dialog not reopened", st)
	}
	var locks []struct{ D string }
	attr(t, page, "data-locks", &locks)
	if len(locks) != 1 || locks[0].D != "A" {
		t.Fatalf("locks %+v", locks)
	}
}

// A file cannot be unrestricted (design 2.8.2): inside a free directory it is a single-writer scope of its own,
// so its dialog inherits "single", not "free"; the directory itself keeps inheriting "free".
func TestFileInFreeDirInheritsSingle(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.mode("free", proto.ModeFree, 0)
	l.apiCommit(l.ta, l.f, l.op("free/x.txt", 0, "x"), l.op("free/sub/y.txt", 0, "y"))
	_, page := l.get(l.web, "/folders/"+itoa(l.f)+"?dir=free")
	for _, row := range regexp.MustCompile(`(?s)<tr [^>]*>`).FindAllString(page, -1) {
		switch {
		case strings.Contains(row, `data-path="free/x.txt"`):
			if !strings.Contains(row, `data-inh="single"`) {
				t.Errorf("file row inherits %s", row)
			}
		case strings.Contains(row, `data-path="free/sub"`):
			if !strings.Contains(row, `data-inh="free"`) {
				t.Errorf("directory row inherits %s", row)
			}
		}
	}
}

// The dialog that reopens after a failed save (and the one a notification link opens) knows that the path is a file:
// inside a free directory it inherits "single" there too.
func TestWCReopenedDialogOfFileInheritsSingle(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.mode("free", proto.ModeFree, 0)
	l.apiCommit(l.ta, l.f, l.op("free/x.txt", 0, "x"))
	dlg := regexp.MustCompile(`<dialog id="dlg-wc"[^>]*>`)
	st, page := l.post(l.web, "/folders/"+itoa(l.f)+"/writectl", url.Values{"path": {"free/x.txt"}, "mode": {"exclusive"}, "device": {"0"}})
	if d := dlg.FindString(page); st != 400 || !strings.Contains(d, ` data-inh="single"`) {
		t.Errorf("failed save of a file: %d %s", st, d)
	}
	_, page = l.get(l.web, "/folders/"+itoa(l.f)+"?wc=free/x.txt")
	if d := dlg.FindString(page); !strings.Contains(d, ` data-inh="single"`) || !strings.Contains(d, " data-file") {
		t.Errorf("link to a file: %s", d)
	}
	_, page = l.get(l.web, "/folders/"+itoa(l.f)+"?wc=free")
	if d := dlg.FindString(page); strings.Contains(d, " data-file") || !strings.Contains(d, ` data-inh="single"`) {
		t.Errorf("link to a directory: %s", d)
	}
}

// A file cannot be unrestricted (design 2.8.2): the server refuses "free" for a file and saves nothing (the
// dialog only hides the option); a directory, and the other modes of a file, still save.
func TestWriteControlRefusesFreeForFile(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.apiCommit(l.ta, l.f, l.op("d/x.txt", 0, "x"))
	wc := "/folders/" + itoa(l.f) + "/writectl"
	v := metaInt(l.s.db, "config_version")
	st, page := l.post(l.web, wc, urlValues("d/x.txt", "free"))
	if st != 400 || !regexp.MustCompile(`id="dlg-wc"[^>]* data-autoopen`).MatchString(page) {
		t.Errorf("free for a file: %d, dialog not reopened", st)
	}
	if n := l.count(`SELECT count(*) FROM writectl`); n != 0 || metaInt(l.s.db, "config_version") != v {
		t.Errorf("free for a file saved: %d rows, config_version %d -> %d", n, v, metaInt(l.s.db, "config_version"))
	}
	if n := l.count(`SELECT count(*) FROM folders WHERE wc_ack=1`); n != 0 {
		t.Errorf("refused save confirmed the folder")
	}
	if st, _ := l.post(l.web, wc, urlValues("d", "free")); st != 303 {
		t.Errorf("free for a directory: %d", st)
	}
	if st, _ := l.post(l.web, wc, urlValues("d/x.txt", "single")); st != 303 {
		t.Errorf("single for a file: %d", st)
	}
}

func urlValues(path, mode string) url.Values { return url.Values{"path": {path}, "mode": {mode}} }

// Saving a write control returns to the listing the user was looking at: "show deleted" stays on, so the dialog form
// carries it and the redirect repeats it.
func TestWCSaveKeepsShowDeleted(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	_, page := l.get(l.web, "/folders/"+itoa(l.f)+"?deleted=1")
	form := regexp.MustCompile(`(?s)<dialog id="dlg-wc".*?</dialog>`).FindString(page)
	if !strings.Contains(form, `name="deleted" value="1"`) {
		t.Errorf("dialog form does not carry the switch: %s", form)
	}
	_, page = l.get(l.web, "/folders/"+itoa(l.f))
	if form = regexp.MustCompile(`(?s)<dialog id="dlg-wc".*?</dialog>`).FindString(page); strings.Contains(form, `name="deleted"`) {
		t.Error("switch carried although off")
	}
	for _, c := range []struct {
		deleted string
		want    string
	}{{"1", "?dir=sub&deleted=1"}, {"", "?dir=sub"}} {
		resp, err := l.web.PostForm(l.ts.URL+"/folders/"+itoa(l.f)+"/writectl", url.Values{"path": {"sub"}, "mode": {"single"}, "dir": {"sub"}, "deleted": {c.deleted}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Location"); got != "/folders/"+itoa(l.f)+c.want {
			t.Errorf("deleted=%q: returned to %q", c.deleted, got)
		}
	}
}
