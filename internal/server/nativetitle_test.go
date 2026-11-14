package server

import (
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// The script names its hints in the page's own tip, never in a native title (ui.md; nativetitle): a timestamp's zone, the
// error of a rule line and the time zone sample.
func TestScriptSetsNoNativeTitle(t *testing.T) {
	js, err := fs.ReadFile(webFS, "web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\.title\s*\+?=[^=]|setAttribute\(\s*["']title["']|\btitle\s*[:,}]|["']title["']\s*[:\]]|createElement(NS)?\([^)]*["']title["']|\bel\(\s*["']title["']`)
	if m := re.FindAllIndex(js, -1); m != nil {
		for _, x := range m {
			t.Errorf("app.js sets a native title: %q", js[x[0]:x[1]])
		}
	}
}

func TestTimeScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "time.test.js")
	cmd.Dir = "jstest"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// The zone is written once, in the header of each time column, as plain text with a UTC fallback for a page without the
// script (nativetitle2); a <time> is not a Tab stop, since a page holds dozens to hundreds of them.
func TestTimeColumnsNameTheZoneInTheHeader(t *testing.T) {
	for _, c := range []struct{ file, key string }{{"web/folder.html", "fo.modified"}, {"web/folder.html", "rs.time"}, {"web/overview.html", "ov.lastConn"}} {
		b, err := fs.ReadFile(webFS, c.file)
		if err != nil {
			t.Fatal(err)
		}
		re := regexp.MustCompile(`<th[^>]*>\{\{t "` + regexp.QuoteMeta(c.key) + `"\}\} <span class="tzn" data-tzname>\(UTC\)</span></th>`)
		if !re.Match(b) {
			t.Errorf("%s: the header %s does not carry the zone", c.file, c.key)
		}
	}
	for _, f := range []string{"web/folder.html", "web/overview.html", "web/layout.html", "web/settings.html", "web/device.html"} {
		b, _ := fs.ReadFile(webFS, f)
		if regexp.MustCompile(`<time[^>]*tabindex`).Match(b) {
			t.Errorf("%s: a <time> is a Tab stop", f)
		}
	}
	web, _ := os.ReadFile("web.go")
	if regexp.MustCompile(`<time[^>]*tabindex`).Match(web) {
		t.Error("web.go: timeTag writes a tabindex")
	}
}

// A long zone name wraps inside the header: a nowrap header would widen the version dialog past a 400 px screen.
func TestZoneNameInHeaderWraps(t *testing.T) {
	css, _ := fs.ReadFile(webFS, "web/style.css")
	if !regexp.MustCompile(`(?m)^\.tzn \{[^}]*white-space: normal;[^}]*overflow-wrap: anywhere`).Match(css) {
		t.Error("style.css: .tzn must allow wrapping (white-space: normal, overflow-wrap: anywhere)")
	}
}
