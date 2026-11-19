package server

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Web UI's type scale follows the status page (fontsize task, 2026-11-15): the same Primer steps (12, 14, 16, 20 px as
// rem variables), body text 16px, buttons, table headers and tags 14px, mono beside sans at 14px. Every font size in
// style.css comes from the variables; the one exception is the text inside the time axis SVG, which stays in px like the
// status page's chart text.

func styleCSS(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(webFS, "web/style.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// rule returns the declarations of the first rule whose selector is exactly sel.
func rule(t *testing.T, css, sel string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(sel) + ` \{([^}]*)\}`).FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("style.css has no rule %q", sel)
	}
	return m[1]
}

func TestTypeScaleVariablesMatchTheStatusPage(t *testing.T) {
	t.Parallel()
	css := styleCSS(t)
	root := rule(t, css, ":root")
	for k, v := range map[string]string{"--fs-xs": "0.75rem", "--fs-sm": "0.875rem", "--fs-md": "1rem", "--fs-lg": "1.25rem"} {
		if !regexp.MustCompile(k + `: *` + regexp.QuoteMeta(v) + `;`).MatchString(root) {
			t.Errorf(":root does not define %s: %s", k, v)
		}
	}
	if !regexp.MustCompile(`font: *var\(--fs-md\)/1\.6 var\(--sans\)`).MatchString(rule(t, css, "body")) {
		t.Error("body is not var(--fs-md)/1.6 (16px text, line height 1.6)")
	}
	if !strings.Contains(rule(t, css, ".btn"), "var(--fs-sm)") {
		t.Error("buttons are not 14px (--fs-sm)")
	}
	if !strings.Contains(rule(t, css, "th"), "var(--fs-sm)") {
		t.Error("table headers are not 14px (--fs-sm)")
	}
	if !strings.Contains(rule(t, css, "input[type=text], input[type=password], input[type=number], select, textarea"), "var(--fs-md)") {
		t.Error("inputs are not 16px (--fs-md)")
	}
}

func TestEveryFontSizeIsFromTheScale(t *testing.T) {
	t.Parallel()
	css := styleCSS(t)
	// the size of `font-size: X` and of the shorthand `font: [weight] X[/line-height] family`
	tok := regexp.MustCompile(`(var\(--fs-(?:xs|sm|md|lg)\)|inherit|\d+(?:\.\d+)?(?:px|rem|em))`)
	for _, line := range strings.Split(css, "\n") {
		for _, decl := range regexp.MustCompile(`font(?:-size)?:[^;}]*`).FindAllString(line, -1) {
			if strings.HasPrefix(decl, "font-family") || strings.HasPrefix(decl, "font-weight") || strings.HasPrefix(decl, "font-style") || strings.HasPrefix(decl, "font-variant") {
				continue
			}
			v := strings.TrimSpace(decl[strings.Index(decl, ":")+1:])
			if v == "inherit" {
				continue
			}
			m := tok.FindString(v)
			if strings.HasPrefix(decl, "font:") {
				// the shorthand: the size is the token before an optional "/line-height"
				m = regexp.MustCompile(`(?:^|\s)(var\(--fs-(?:xs|sm|md|lg)\)|\d+(?:\.\d+)?px)(?:/|\s)`).FindStringSubmatch(" " + v + " ")[1]
			}
			if strings.HasPrefix(m, "var(--fs-") {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), ".axis .lb") && m == "12px" { // text inside an SVG stays in px
				continue
			}
			t.Errorf("font size %q is not a scale variable: %s", m, strings.TrimSpace(line))
		}
	}
}

func TestRowsAndControlsGrowWithTheText(t *testing.T) {
	t.Parallel()
	css := styleCSS(t)
	px := func(sel, prop string) int {
		m := regexp.MustCompile(`(?:^|[;\s])` + prop + `: *(\d+)px`).FindStringSubmatch(rule(t, css, sel))
		if m == nil {
			t.Fatalf("%s has no %s in px", sel, prop)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if h := px(".btn", "height"); h < 32 {
		t.Errorf(".btn is %dpx high, want at least 32", h)
	}
	if h := px("td", "height"); h < 44 {
		t.Errorf("td is %dpx high, want at least 44", h)
	}
	if h := px("table.files td", "height"); h < 40 {
		t.Errorf("file rows are %dpx high, want at least 40", h)
	}
	if h := px(".tag", "height"); h < 24 {
		t.Errorf(".tag is %dpx high, want at least 24", h)
	}
	if w := px("dialog.dlg", "width"); w < 640 {
		t.Errorf("dialogs are %dpx wide, want at least 640", w)
	}
	if w := px(".notif", "width"); w < 480 {
		t.Errorf("the notice list is %dpx wide, want at least 480", w)
	}
}
