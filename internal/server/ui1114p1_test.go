package server

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// ui1114-p1fix: the delete-mapping confirmation names the folder and the local path (as the revoke confirmation names
// the device); the mapping table of a narrow screen carries the path under the folder name; Source Sans 3 is shipped
// under a name without the reserved font name; small click targets and the orphan "·" of a wrapped summary line.

var uiLangs = []string{"en", "zh-CN", "zh-TW", "ja", "ko"}

func i18nOf(t *testing.T, lang string) map[string]string {
	t.Helper()
	b, err := fs.ReadFile(webFS, "i18n/"+lang+".json")
	if err != nil {
		b, err = os.ReadFile("i18n/" + lang + ".json")
		if err != nil {
			t.Fatal(err)
		}
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDeleteMappingConfirmNamesFolderAndPath(t *testing.T) {
	for _, l := range uiLangs {
		m := i18nOf(t, l)
		s := m["js.delMapConfirm"]
		if !strings.Contains(s, "{name}") || !strings.Contains(s, "{path}") {
			t.Errorf("%s: js.delMapConfirm is %q, it must carry {name} and {path}", l, s)
		}
		if _, old := m["dev.delMapConfirm"]; old {
			t.Errorf("%s: dev.delMapConfirm is still there (the text is built in the script now)", l)
		}
	}
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	dev, _ := e.enrollDevice("A", "linux")
	e.mapFolder(dev, f, "/home/a/docs")
	_, body := e.get(c, "/devices/"+itoa(dev))
	form := regexp.MustCompile(`<form id="f-del-map"[^>]*>`).FindString(body)
	if form == "" || strings.Contains(form, "data-base") || strings.Contains(form, "data-confirm=") {
		t.Errorf("the delete form must get its confirmation text from the script: %q", form)
	}
}

func TestDeleteMappingScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "delmap.test.js")
	cmd.Dir = "jstest"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// At 640px and below the path moves under the folder name, so the table has no wide column and the row menu stays on
// the screen; above it the path keeps its own column.
func TestMappingTablePutsThePathUnderTheNameOnNarrowScreens(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	dev, _ := e.enrollDevice("A", "linux")
	e.mapFolder(dev, f, "/home/a/docs")
	_, body := e.get(c, "/devices/"+itoa(dev))
	row := regexp.MustCompile(`(?s)<tr data-folder=.*?</tr>`).FindString(body)
	if !regexp.MustCompile(`(?s)<td><a href="/folders/\d+">.*?</a><span class="sm-meta mono">/home/a/docs</span></td>`).MatchString(row) {
		t.Errorf("the folder cell does not carry the path as a second line:\n%s", row)
	}
	if !regexp.MustCompile(`<td class="hide-sm mono small">/home/a/docs</td>`).MatchString(row) {
		t.Errorf("the path column is not hidden on a narrow screen:\n%s", row)
	}
	if !regexp.MustCompile(`<th scope="col" class="hide-sm">[^<]*</th><th scope="col" class="hide-sm">`).MatchString(body) {
		t.Error("the path header is not hidden on a narrow screen")
	}
	css, _ := fs.ReadFile(webFS, "web/style.css")
	if !regexp.MustCompile(`table\.maps \.sm-meta[^{]*\{[^}]*display: *block`).Match(css) {
		t.Error("style.css does not show the second line of the mapping table on a narrow screen")
	}
	if !regexp.MustCompile(`table\.maps \.sm-meta[^{]*\{[^}]*overflow-wrap: *anywhere`).Match(css) || !regexp.MustCompile(`table\.maps td\.mono[^{]*\{[^}]*overflow-wrap: *anywhere`).Match(css) {
		t.Error("a long path in the mapping table does not wrap (second line and path column)")
	}
}

// Source Sans 3 carries the Reserved Font Name "Source" (OFL 1.1 clause 3): the subset we make is a Modified Version, so
// it is shipped as "Kaze Sans" (family name in the font's name table and in the CSS), with the copyright and license
// records kept.
func TestSansFaceDoesNotUseTheReservedName(t *testing.T) {
	for _, p := range []string{"web/fonts.css", "web/style.css"} {
		b, _ := fs.ReadFile(webFS, p)
		if strings.Contains(string(b), "Source Sans") {
			t.Errorf("%s still names the family Source Sans", p)
		}
	}
	fc, _ := fs.ReadFile(webFS, "web/fonts.css")
	if !strings.Contains(string(fc), `font-family: "Kaze Sans"`) {
		t.Error(`fonts.css does not declare "Kaze Sans"`)
	}
	css, _ := fs.ReadFile(webFS, "web/style.css")
	if !strings.Contains(string(css), `"Kaze Sans"`) {
		t.Error(`the font stacks do not start from "Kaze Sans"`)
	}
	tool, _ := os.ReadFile("../../tools/fontsubset/subset.py")
	if !strings.Contains(string(tool), "def rename_family") || !strings.Contains(string(tool), "Kaze Sans") {
		t.Error("tools/fontsubset/subset.py does not rename the family")
	}
	lic, _ := fs.ReadFile(webFS, "web/fonts/SOURCE.md")
	if !strings.Contains(string(lic), "Kaze Sans") {
		t.Error("SOURCE.md does not say that the shipped face is renamed")
	}
	// The font file itself: its name table (needs python3 with fontTools; the font tool's venv is not required to run
	// the other tests).
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	if exec.Command(py, "-I", "-c", "import fontTools, brotli").Run() != nil {
		t.Skip("python3 has no fontTools and brotli (pip install fonttools brotli)")
	}
	code := `
import sys
from fontTools.ttLib import TTFont
f = TTFont(sys.argv[1])
bad = [n.toUnicode() for n in f["name"].names if n.nameID in (1, 3, 4, 6, 16, 17, 21, 25) or n.nameID > 255]
bad = [s for s in bad if "Source" in s]
fam = [n.toUnicode() for n in f["name"].names if n.nameID == 1]
lic = [n.nameID for n in f["name"].names if n.nameID in (0, 13, 14)]
print(bad, fam, sorted(lic))`
	out, err := exec.Command(py, "-I", "-c", code, "web/fonts/SourceSans3-Latin.woff2").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "[] ['Kaze Sans'] [0, 13, 14]" {
		t.Errorf("name table of the Latin face: %s", got)
	}
}

// cssRule returns the bodies of all the rules whose whole selector is sel, joined.
func cssRule(css, sel string) string {
	var out []string
	for _, m := range regexp.MustCompile(`(?m)^`+regexp.QuoteMeta(sel)+`\s*\{([^}]*)\}`).FindAllStringSubmatch(css, -1) {
		out = append(out, m[1])
	}
	return strings.Join(out, " ")
}

// Every pointer target of a link in a table cell, a crumb, a tag or a notice is at least 24px tall and wide (WCAG 2.5.8;
// ui.md); an inline link in running text is exempt, so only these classes are listed.
func TestLinkTargetsReach24px(t *testing.T) {
	b, _ := fs.ReadFile(webFS, "web/style.css")
	css := string(b)
	for _, sel := range []string{".crumbs a", "td > a.dn", ".nitem a", ".grp-h a", ".fname a"} {
		r := cssRule(css, sel)
		if !regexp.MustCompile(`min-height: *24px`).MatchString(r) || !regexp.MustCompile(`min-width: *24px`).MatchString(r) && sel != ".nitem a" && sel != ".grp-h a" {
			t.Errorf("%s has no 24px minimum size: %q", sel, r)
		}
	}
	for _, sel := range []string{".nitem a", ".grp-h a"} {
		r := cssRule(css, sel)
		if !strings.Contains(r, "inline-flex") && !strings.Contains(r, "inline-block") {
			t.Errorf("%s is not a box that min-height can grow", sel)
		}
	}
	if r := cssRule(css, ".devn"); !regexp.MustCompile(`min-height: *24px`).MatchString(r) {
		t.Errorf(".devn (a focusable tip trigger) has no 24px minimum height: %q", r)
	}
	ov, _ := fs.ReadFile(webFS, "web/overview.html")
	if !strings.Contains(string(ov), `<a class="dn" href="/devices/{{.ID}}">`) {
		t.Error("the device name link of the overview is not class dn")
	}
}

// A wrapped summary line must not start with a lone "·": the separators hang in a margin that the line start clips.
func TestSubLineSeparatorsAreClippedAtLineStart(t *testing.T) {
	b, _ := fs.ReadFile(webFS, "web/style.css")
	css := string(b)
	r := cssRule(css, ".sub")
	if !strings.Contains(r, "clip-path") {
		t.Errorf(".sub does not clip the separator of a line start: %q", r)
	}
	if m := regexp.MustCompile(`\.sub \.dot-sep::before\s*\{([^}]*)\}`).FindStringSubmatch(css); m == nil || !strings.Contains(m[1], "position: absolute") {
		t.Error("the separators of .sub are not positioned in the margin of their item")
	}
	if regexp.MustCompile(`\.sub \.dot-sep::before[^{]*\{[^}]*margin-right`).MatchString(css) {
		t.Error("a separator still takes room on its line with margin-right")
	}
}
