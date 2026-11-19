package server

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

// More held changes than the dialog draws are folded by directory (design 2.8.11): one row per directory, files at
// the folded level keep their own rows, and the dialog has a select-all box and buttons that set the checked rows'
// action at once.
func TestHeldDialogFoldsByDirectory(t *testing.T) {
	l, it := heldEnv(t)
	var items []proto.HeldItem
	for _, d := range []string{"s0", "s1", "s2", "s3", "s3b", "s4", "s5", "s6", "s7", "s8", "s9"} {
		for j := 0; j < 250; j++ {
			x := it
			x.Path = "d/" + d + "/f" + strconv.Itoa(j)
			items = append(items, x)
		}
	}
	for j := 0; j < 3; j++ {
		x := it
		x.Path = "d/top" + strconv.Itoa(j)
		items = append(items, x)
	}
	l.heldReport(l.tb, l.f, items...)
	v, err := l.s.heldView(l.f, l.b)
	if err != nil || v == nil || len(v.Groups) != 1 {
		t.Fatalf("view: %v %+v", err, v)
	}
	g := v.Groups[0]
	if !g.Folded || g.Total != len(items) || len(g.Rows) != 14 {
		t.Fatalf("folded %v total %d rows %d", g.Folded, g.Total, len(g.Rows))
	}
	dirs := 0
	for _, r := range g.Rows {
		if r.Dir {
			dirs++
			if r.N != 250 || strings.HasSuffix(r.Path, "/") {
				t.Errorf("directory row %+v", r)
			}
		}
	}
	if dirs != 11 {
		t.Fatalf("%d directory rows, want 11", dirs)
	}
	_, page := l.get(l.web, "/folders/"+itoa(l.f)+"?held="+itoa(l.b))
	for want, n := range map[string]int{`name="dr"`: 11, `name="r"`: 3, `data-sel `: 14, `data-selall`: 1, `data-bulk="overwrite"`: 1, `data-bulkmsg`: 1} {
		if c := strings.Count(page, want); c != n {
			t.Errorf("%s appears %d times, want %d", want, c, n)
		}
	}
	if !strings.Contains(page, "已按目录折叠为 14 行") || strings.Contains(page, "只显示前") {
		t.Error("the dialog does not say that the rows are folded")
	}
	// A directory row's action covers what is under that directory and nothing else (s3, not s3b); upload is not
	// one of its actions.
	seen := l.seq(l.f)
	l.post(l.web, "/folders/"+itoa(l.f)+"/resolve", url.Values{"device": {itoa(l.b)}, "scope": {"d:D"}, "seen": {itoa(seen)},
		"dr": {"d/s3", "d/s5"}, "da": {"discard", "upload"}})
	acts, _ := l.pollActions(l.tb, l.f)
	if len(acts) != 250 {
		t.Fatalf("%d actions, want 250", len(acts))
	}
	for _, a := range acts {
		if !strings.HasPrefix(a.Path, "d/s3/") || a.Action != proto.ActDiscard || a.Expect == nil {
			t.Fatalf("action %+v", a)
		}
	}
}

// Fewer changes than the limit are not folded, and a scope whose changes all sit in one directory is cut instead of
// folded into a single row (that row would only repeat the whole-scope buttons).
func TestHeldFoldDepth(t *testing.T) {
	few := []proto.HeldItem{{Path: "a/b/c"}, {Path: "a/d"}}
	if k := heldFoldDepth(few, ""); k != 0 {
		t.Errorf("few paths folded at %d", k)
	}
	var flat []proto.HeldItem
	for i := 0; i < maxHeldRows+1; i++ {
		flat = append(flat, proto.HeldItem{Path: "a/f" + strconv.Itoa(100000+i)})
	}
	if k := heldFoldDepth(flat, ""); k != 0 {
		t.Errorf("one directory folded at %d", k)
	}
	if k := heldFoldDepth(flat, "a"); k != 0 {
		t.Errorf("the scope's own files folded at %d", k)
	}
	if dirAt("a/b/c", 0) != "" || dirAt("a/b/c", 1) != "a" || dirAt("a/b/c", 2) != "a/b" || dirAt("a/b/c", 3) != "" {
		t.Error("dirAt")
	}
}

func TestHeldFoldDepthWithRepeatedReports(t *testing.T) {
	items := make([]proto.HeldItem, maxHeldRows+1)
	for i := range items {
		items[i].Path = "a/file"
	}
	if k := heldFoldDepth(items, ""); k != 0 {
		t.Errorf("repeated reports in one directory folded at %d", k)
	}

	items[len(items)-1].Path = "a/zz/nested"
	if k := heldFoldDepth(items, ""); k != 0 {
		t.Errorf("folded at %d, although each repeated file would still occupy a row", k)
	}
}
