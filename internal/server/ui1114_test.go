package server

import (
	"io/fs"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// openDialog puts the focus on the chosen radio of the write-control dialog (ui1114 D2); the behaviour is tested with
// node's built-in runner, like the tip box.
func TestDialogScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "dialog.test.js")
	cmd.Dir = "jstest"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// A folder name in the overview wraps (a long one is cut anywhere) but never shrinks below about eight characters: a short
// name is not set one letter to a line on a narrow screen (ui1114 D1; `overflow-wrap: anywhere` alone lets the column
// shrink to one letter).
func TestOverviewFolderNameKeepsAMinimumWidth(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	e.folder("Docs")
	_, body := e.get(c, "/")
	if !strings.Contains(body, `<span class="fname fo">`) {
		t.Fatal("the overview folder name is not marked class fname fo")
	}
	css, _ := fs.ReadFile(webFS, "web/style.css")
	m := regexp.MustCompile(`(?m)^\.fname\.fo a[^{]*\{[^}]*min-width: *(\d+)px`).FindSubmatch(css)
	if n, _ := strconv.Atoi(string(m[1])); m == nil || n < 64 {
		t.Fatalf("style.css has no min-width of at least 64px for .fname.fo a: %s", m)
	}
	if !regexp.MustCompile(`\.fname a, \.fname \.t \{[^}]*overflow-wrap: anywhere`).Match(css) {
		t.Error("a long name no longer wraps anywhere")
	}
}

// When no folder is left to map, the add-mapping button says why in a tip a keyboard or a touch user reaches (ui1114
// D3): it stays focusable (aria-disabled, not disabled), opens nothing, and has no native title (ui.md).
func TestAddMappingButtonExplainsItself(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("F")
	dev, _ := e.enrollDevice("A", "linux")
	e.mapFolder(dev, f, "/a")
	_, body := e.get(c, "/devices/"+itoa(dev))
	btn := regexp.MustCompile(`<button[^>]*>`).FindAllString(body[strings.Index(body, `<div class="card-h">`):], 1)[0]
	if strings.Contains(btn, " disabled") || strings.Contains(btn, "data-dialog") || strings.Contains(btn, "title=") {
		t.Errorf("the button is disabled, opens a dialog or has a title: %s", btn)
	}
	for _, w := range []string{`aria-disabled="true"`, `data-tip="ma-none"`, `aria-describedby="ma-none"`} {
		if !strings.Contains(btn, w) {
			t.Errorf("the button lacks %s: %s", w, btn)
		}
	}
	if !regexp.MustCompile(`id="ma-none" hidden><span class="tp">[^<]+</span>`).MatchString(body) {
		t.Error("the tip content is missing")
	}
	// With a folder left to map the button is the plain one.
	e.folder("G")
	_, body = e.get(c, "/devices/"+itoa(dev))
	btn = regexp.MustCompile(`<button[^>]*>`).FindAllString(body[strings.Index(body, `<div class="card-h">`):], 1)[0]
	if !strings.Contains(btn, `data-dialog="dlg-map-add"`) || strings.Contains(btn, "aria-disabled") {
		t.Errorf("the button is not the plain one: %s", btn)
	}
}

// No template gives an element a native title (a hint is a tip, ui.md).
func TestTemplatesHaveNoNativeTitle(t *testing.T) {
	files, _ := fs.Glob(webFS, "web/*.html")
	for _, p := range files {
		b, _ := fs.ReadFile(webFS, p)
		if regexp.MustCompile(`<[a-z][^>]*\stitle=`).Match(b) {
			t.Errorf("%s has a title attribute", p)
		}
	}
}
