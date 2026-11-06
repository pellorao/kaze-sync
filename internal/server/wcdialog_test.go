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
