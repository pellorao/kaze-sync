package server

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Every table header cell names what it heads (design 4.8: tables use <th scope>), so a screen reader can announce
// the column of each cell.
func TestTableHeadersHaveScope(t *testing.T) {
	names, _ := fs.Glob(webFS, "web/*.html")
	th := regexp.MustCompile(`<th(\s[^>]*)?>`)
	for _, n := range names {
		b, _ := fs.ReadFile(webFS, n)
		for _, m := range th.FindAllString(string(b), -1) {
			if !strings.Contains(m, `scope="col"`) && !strings.Contains(m, `scope="row"`) {
				t.Errorf("%s: %s has no scope", n, m)
			}
		}
	}
}

// --faint is for disabled states and decorative lines only (design 4.7). Text the user reads reaches 4.5:1 and an
// icon that carries meaning (the row menu button, the offline dot) 3:1 on the surface it sits on (WCAG 1.4.3, 1.4.11).
func TestReadableColoursReachContrast(t *testing.T) {
	b, _ := fs.ReadFile(webFS, "web/style.css")
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(b), "")
	tok := func(scope, name string) string { // the value a rule whose selector list holds scope gives the token
		val := regexp.MustCompile(`--` + name + `: *(#[0-9a-fA-F]{6})`)
		for _, r := range regexp.MustCompile(`(?m)^([^{}\n]+)\{([^}]*)\}`).FindAllStringSubmatch(css, -1) {
			for _, sel := range strings.Split(r[1], ",") {
				if m := val.FindStringSubmatch(r[2]); strings.TrimSpace(sel) == scope && m != nil {
					return m[1]
				}
			}
		}
		return ""
	}
	colour := func(sel string) string {
		m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(sel) + ` *\{[^}]*[^-]color: *var\(--(\w+)\)`).FindStringSubmatch(css)
		if m == nil {
			t.Fatalf("no colour for %s", sel)
		}
		return m[1]
	}
	ratio := func(a, b string) float64 {
		x, y := lum(hexRGB(t, a)), lum(hexRGB(t, b))
		if x < y {
			x, y = y, x
		}
		return (x + 0.05) / (y + 0.05)
	}
	for _, c := range []struct {
		sel, raised string // raised: the container whose own token values apply ("" for the page)
		surfaces    []string
		min         float64
	}{
		{".tag.dev.off", "", []string{"bg", "surface"}, 4.5}, // offline device tag in tables and the device header
		{".menu .hint", ".pop", []string{"raised"}, 4.5},     // why a menu item is disabled
		{".more", "", []string{"bg", "surface"}, 3},          // the row menu button at rest
		{".dot", "", []string{"bg", "surface"}, 3},           // the offline dot before a device name
	} {
		var name string
		if c.sel != ".dot" {
			name = colour(c.sel)
		} else {
			m := regexp.MustCompile(`(?m)^\.dot *\{[^}]*background: *var\(--(\w+)\)`).FindStringSubmatch(css)
			if m == nil {
				t.Fatal("no background for .dot")
			}
			name = m[1]
		}
		if name == "faint" {
			t.Errorf("%s uses --faint", c.sel)
		}
		v := ""
		if c.raised != "" {
			v = tok(c.raised, name)
		}
		if v == "" {
			v = tok(":root", name)
		}
		if v == "" {
			t.Fatalf("%s: no value for --%s", c.sel, name)
		}
		for _, s := range c.surfaces {
			if r := ratio(v, tok(":root", s)); r < c.min {
				t.Errorf("%s: --%s %s on --%s is %.2f:1, below %.1f:1", c.sel, name, v, s, r, c.min)
			}
		}
	}
}
