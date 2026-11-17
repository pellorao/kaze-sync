package server

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

// K1: the restore question of a version row sits in a panel under the table. Inside the row it widened the last column
// (554px) and pushed the other columns out of view, at every width.
func TestVersionRestoreQuestionIsOutsideTheTable(t *testing.T) {
	l := newLockEnv(t)
	l.apiCommit(l.ta, l.f, l.op("d/a.txt", 0, "one"))
	l.clk.Add(time.Hour)
	if r := l.apiCommit(l.ta, l.f, l.op("d/a.txt", 1, "two")); r[0].Status != proto.StatusOK {
		t.Fatalf("second write: %+v", r[0])
	}
	_, page := l.get(l.web, "/folders/"+itoa(l.f)+"?dir=d&file=d%2Fa.txt")
	i := strings.Index(page, `<table class="vers">`)
	j := strings.Index(page[i:], "</table>")
	if i < 0 || j < 0 {
		t.Fatal("no version table")
	}
	table := page[i : i+j]
	if strings.Contains(table, "<form") || strings.Contains(table, `class="q"`) {
		t.Error("the restore question is inside the table")
	}
	ids := regexp.MustCompile(`data-ask="(\d+)"`).FindAllStringSubmatch(table, -1)
	if len(ids) != 1 {
		t.Fatalf("%d restore buttons, want 1 (the other version is the current one)", len(ids))
	}
	for _, want := range []string{`<form method="post" action="/folders/` + itoa(l.f) + `/restore" class="ask" id="ask-` + ids[0][1] + `" role="group" aria-labelledby="ask-q-` + ids[0][1] + `" hidden>`, `data-ask-cancel="` + ids[0][1] + `"`, `name="version" value="` + ids[0][1] + `"`} {
		if !strings.Contains(page[i+j:], want) {
			t.Errorf("the page has no %q after the table", want)
		}
	}
}

// K2: the dialog body is one grid column that may shrink below its content, so a wide table scrolls in its own box
// instead of widening the whole dialog (at 400px the "choose one by one" table made the dialog scroll sideways).
func TestDialogBodyColumnMayShrink(t *testing.T) {
	if r := rule(t, styleCSS(t), ".dlg-b"); !strings.Contains(r, "grid-template-columns: minmax(0, 1fr)") {
		t.Errorf(".dlg-b lets its content widen the dialog: %s", r)
	}
}
