package server

import (
	"io/fs"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

// rowOf cuts the table row of a path out of a folder page.
func rowOf(t *testing.T, page, path string) string {
	t.Helper()
	i := strings.Index(page, `data-path="`+path+`" data-name=`)
	if i < 0 {
		t.Fatalf("no row %s", path)
	}
	j := strings.Index(page[i:], "</tr>")
	return page[i : i+j]
}

// The file list shows write control by icon only (design 2.8.11): shape and color by the effective mode for directories
// and files alike, a filled body where the setting is on the path itself, and the former labels in the icon's tip.
func TestFileListShowsWriteControlByIcon(t *testing.T) {
	l := newLockEnv(t)
	l.apiCommit(l.ta, l.f, l.op("README.md", 0, "r"), l.op("key.pem", 0, "k"), l.op("repo/x.go", 0, "x"),
		l.op("scratch/a.txt", 0, "a"), l.op("scratch/b.txt", 0, "b"), l.op("scratch/sub/c.txt", 0, "c"))
	set := func(p, mode string, dev int64) {
		form := url.Values{"path": {p}, "mode": {mode}}
		if dev != 0 {
			form.Set("device", itoa(dev))
		}
		if st, body := l.post(l.web, "/folders/"+itoa(l.f)+"/writectl", form); st != 303 {
			t.Fatalf("%s %s: %d %.300s", p, mode, st, body)
		}
	}
	set("key.pem", proto.ModeExclusive, l.a)
	set("scratch", proto.ModeFree, 0)
	set("scratch/b.txt", proto.ModeSingle, 0)
	_, page := l.get(l.web, "/folders/"+itoa(l.f))
	cases := []struct{ path, icon, cls string }{
		{"repo", "i-folder-single", `wci wc-single"`},
		{"scratch", "i-folder-free", `wci wc-free set"`},
		{"README.md", "i-file-single", `wci wc-single"`},
		{"key.pem", "i-file-exclusive", `wci wc-exclusive set"`},
	}
	for _, c := range cases {
		row := rowOf(t, page, c.path)
		if !strings.Contains(row, "#"+c.icon+`"`) || !strings.Contains(row, `class="`+c.cls) {
			t.Errorf("%s: want %s %s in\n%s", c.path, c.icon, c.cls, row)
		}
		if strings.Contains(row, `class="tag wc-`) || strings.Contains(row, " title=") {
			t.Errorf("%s: a write-control label or a browser tip is left:\n%s", c.path, row)
		}
	}
	for _, w := range []string{"继承, 来自 /</span>", "独占 · A</span><span class=\"ts\">设置在此</span>", "其他设备的任何改动都会冻结它在该范围的同步."} {
		if !strings.Contains(page, w) {
			t.Errorf("the tips lack %q", w)
		}
	}
	// The name link is described by the icon's tip, so that keyboard focus and screen readers reach it.
	if row := rowOf(t, page, "repo"); !regexp.MustCompile(`data-tip="(wt\d+)".*aria-describedby="(wt\d+)"`).MatchString(row) {
		t.Errorf("the row's link is not described by the tip:\n%s", row)
	}
	// Files follow their directory: plain in a free one, and a setting of their own fills the icon.
	_, page = l.get(l.web, "/folders/"+itoa(l.f)+"?dir=scratch")
	if row := rowOf(t, page, "scratch/a.txt"); !strings.Contains(row, "#i-file-free\"") || strings.Contains(row, " set\"") || !strings.Contains(row, "继承, 来自 /scratch") {
		t.Errorf("a file of a free directory:\n%s", row)
	}
	if row := rowOf(t, page, "scratch/b.txt"); !strings.Contains(row, "#i-file-single\"") || !strings.Contains(row, "wc-single set\"") {
		t.Errorf("a file set to single writer:\n%s", row)
	}
	// The current directory's icon sits in the title, focusable since it has no link.
	if !regexp.MustCompile(`<span class="here"><span class="wci wc-free set" data-tip="wt-here" tabindex="0" role="img" aria-label="不限"`).MatchString(page) {
		t.Error("the title has no focusable icon of the current directory")
	}
}

// The overview shows each folder's root setting without opening it; a root without a row is single writer by default.
func TestOverviewShowsTheRootWriteControl(t *testing.T) {
	l := newLockEnv(t)
	g := l.folder("G")
	if st, _ := l.post(l.web, "/folders/"+itoa(g)+"/writectl", url.Values{"path": {""}, "mode": {"free"}}); st != 303 {
		t.Fatal("root free")
	}
	_, page := l.get(l.web, "/")
	f := regexp.MustCompile(`<span class="wci wc-single" data-tip="wf` + itoa(l.f) + `"><svg class="i wc"[^>]*><use href="/static/icons.svg#i-folder-single"/></svg><span class="tip-c" id="wf\d+" hidden><span class="tp">单端</span><span class="ts">默认 \(根目录还没有保存设置\)</span>`)
	if !f.MatchString(page) {
		t.Errorf("folder F (default root):\n%s", page)
	}
	if !strings.Contains(page, `<span class="wci wc-free set" data-tip="wf`+itoa(g)+`">`) || !strings.Contains(page, `aria-describedby="wf`+itoa(g)+`">G</a>`) {
		t.Error("folder G (root set to any writer)")
	}
}

// Every icon the templates, the Go code and app.js name is in the sprite, and no icon is a font glyph (the user asked
// for vector icons, 2026-11-12); a glyph may stay only as screen-reader text.
func TestIconsAreVectorsOfTheSprite(t *testing.T) {
	sprite, _ := fs.ReadFile(webFS, "web/icons.svg")
	has := map[string]bool{}
	for _, m := range regexp.MustCompile(`<symbol id="i-([a-z-]+)"`).FindAllStringSubmatch(string(sprite), -1) {
		has[m[1]] = true
	}
	for _, kind := range []string{"folder", "file"} {
		for _, mode := range []string{proto.ModeFree, proto.ModeSingle, proto.ModeExclusive} {
			if !has[kind+"-"+mode] {
				t.Errorf("no icon %s-%s", kind, mode)
			}
		}
	}
	named := regexp.MustCompile(`\{\{icon "([a-z-]+)"|\bicon\("([a-z-]+)"`)
	sr := regexp.MustCompile(`<span class="sr">[^<]*</span>`)
	glyph := regexp.MustCompile(`[\x{2190}-\x{21FF}\x{2300}-\x{23FF}\x{25A0}-\x{27BF}\x{2B00}-\x{2BFF}\x{1F300}-\x{1FAFF}]`)
	files := []string{"web/app.js"}
	matches, _ := fs.Glob(webFS, "web/*.html")
	files = append(files, matches...)
	for _, p := range files {
		b, _ := fs.ReadFile(webFS, p)
		for _, m := range named.FindAllStringSubmatch(string(b), -1) {
			if n := m[1] + m[2]; !has[n] {
				t.Errorf("%s names icon %q, which the sprite lacks", p, n)
			}
		}
		if g := glyph.FindString(sr.ReplaceAllString(string(b), "")); g != "" {
			t.Errorf("%s draws an icon with the character %q", p, g)
		}
	}
	for _, n := range []string{"alert", "more", "warn", "err", "link"} { // used by the Go code
		if !has[n] {
			t.Errorf("no icon %s", n)
		}
	}
}
