package server

import (
	"io/fs"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"kaze-sync/internal/version"
)

// The top bar of a signed-in page reads "Kaze Sync" and the version in a muted grey; the login page names the product but
// does not tell the version to someone who is not signed in (semver task).
func TestTopBarShowsNameAndVersion(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	_, login := e.get(c, "/login")
	if !strings.Contains(login, "Kaze Sync") || strings.Contains(login, "kaze-sync</span>") {
		t.Error("the login page does not say Kaze Sync")
	}
	if strings.Contains(login, version.Version) || strings.Contains(login, `class="ver"`) {
		t.Error("the login page shows the version")
	}
	e.login(c, "pw")
	_, body := e.get(c, "/")
	want := `Kaze Sync<span class="ver">` + version.Version + `</span></a>`
	if !strings.Contains(body, want) || !strings.Contains(body, "<title>") || !strings.Contains(body, "- Kaze Sync</title>") {
		t.Errorf("the top bar lacks %q", want)
	}
}

func hexRGB(t *testing.T, s string) [3]float64 {
	t.Helper()
	var c [3]float64
	for i := range c {
		v, err := strconv.ParseUint(s[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		f := float64(v) / 255
		if f <= 0.03928 {
			c[i] = f / 12.92
		} else {
			c[i] = math.Pow((f+0.055)/1.055, 2.4)
		}
	}
	return c
}

func lum(c [3]float64) float64 { return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2] }

// The version text is greyer than the name but still reads at 4.5:1 on the bar (ui.md); the page has one (dark) theme.
func TestVersionContrast(t *testing.T) {
	css, _ := fs.ReadFile(webFS, "web/style.css")
	root := string(css)
	tok := func(name string) string {
		m := regexp.MustCompile(`--` + name + `: *(#[0-9a-fA-F]{6})`).FindStringSubmatch(root)
		if m == nil {
			t.Fatalf("no token --%s", name)
		}
		return m[1]
	}
	if strings.Contains(root, "prefers-color-scheme: light") {
		t.Fatal("a light theme exists: check the version colour on it too")
	}
	m := regexp.MustCompile(`(?m)^\.logo \.ver *\{[^}]*color: *var\(--(\w+)\)`).FindStringSubmatch(root)
	if m == nil {
		t.Fatal("style.css has no colour for .logo .ver")
	}
	if m[1] == "fg" {
		t.Error("the version is not muted")
	}
	a, b := lum(hexRGB(t, tok(m[1]))), lum(hexRGB(t, tok("bar")))
	if a < b {
		a, b = b, a
	}
	if r := (a + 0.05) / (b + 0.05); r < 4.5 {
		t.Errorf("version contrast %.2f:1 is below 4.5:1", r)
	} else {
		t.Logf("version contrast %.2f:1", r)
	}
}
