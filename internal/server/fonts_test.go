package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Web UI ships its fonts (Source Sans 3 as "Kaze Sans", JetBrains Mono, Source Han Sans per language); tools/fontsubset/subset.py
// builds them, and writes beside them the characters each subset holds (tools/fontsubset/coverage).
var fontFiles = []string{
	"SourceSans3-Latin.woff2", "JetBrainsMono-Regular-Latin.woff2", "JetBrainsMono-Bold-Latin.woff2",
	"SourceHanSansCN-Regular-Subset.woff2", "SourceHanSansCN-Bold-Subset.woff2",
	"SourceHanSansTW-Regular-Subset.woff2", "SourceHanSansTW-Bold-Subset.woff2",
	"SourceHanSansJP-Regular-Subset.woff2", "SourceHanSansJP-Bold-Subset.woff2",
	"SourceHanSansKR-Regular-Subset.woff2", "SourceHanSansKR-Bold-Subset.woff2",
	"SourceHanSansCN-Name.woff2", "SourceHanSansTW-Name.woff2", "SourceHanSansJP-Name.woff2", "SourceHanSansKR-Name.woff2",
}

const maxFaceBytes = 2 << 20 // low traffic is the first goal: one CJK face stays within 2 MB

func TestFontFilesAreEmbeddedAndSmall(t *testing.T) {
	for _, n := range fontFiles {
		b, err := fs.ReadFile(webFS, "web/fonts/"+n)
		if err != nil {
			t.Errorf("%s: %v", n, err)
			continue
		}
		if len(b) < 4 || string(b[:4]) != "wOF2" {
			t.Errorf("%s is not a woff2 file", n)
		}
		if len(b) > maxFaceBytes {
			t.Errorf("%s is %d bytes, over the %d budget", n, len(b), maxFaceBytes)
		}
	}
	for _, n := range []string{"LICENSE-source-sans.md", "LICENSE-source-han-sans.txt", "LICENSE-jetbrains-mono.txt", "SOURCE.md"} {
		if b, err := fs.ReadFile(webFS, "web/fonts/"+n); err != nil || len(b) == 0 {
			t.Errorf("web/fonts/%s is missing: the fonts are shipped with their licenses", n)
		}
	}
}

func TestFontsCSSAndStacks(t *testing.T) {
	fc, err := fs.ReadFile(webFS, "web/fonts.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range fontFiles {
		if !strings.Contains(string(fc), "/static/fonts/"+n) {
			t.Errorf("fonts.css does not load %s", n)
		}
	}
	layout, _ := fs.ReadFile(webFS, "web/layout.html")
	if !strings.Contains(string(layout), `href="/static/fonts.css"`) {
		t.Error("layout.html does not link fonts.css")
	}
	css, _ := fs.ReadFile(webFS, "web/style.css")
	root := regexp.MustCompile(`(?m)^:root[^{]*\{[^}]*\}`).FindAllString(string(css), -1)
	all := strings.Join(root, "\n")
	for _, want := range []string{"Kaze Sans", "JetBrains Mono", "Kaze Han Sans CN", "Kaze Han Sans TW", "Kaze Han Sans JP", "Kaze Han Sans KR", "Noto Sans CJK KR", "Source Han Sans"} {
		if !strings.Contains(all, want) {
			t.Errorf("the :root font stacks of style.css do not name %q", want)
		}
	}
	if strings.Contains(all, "Cascadia") || strings.Contains(all, "Consolas") {
		t.Error("the monospace stack still starts from Windows fonts")
	}
	// A native option list is set in its own language (the language names), so the option carries lang.
	set, _ := fs.ReadFile(webFS, "web/settings.html")
	if !strings.Contains(string(set), `lang="{{.Code}}">{{.Name}}</option>`) {
		t.Error("the language options of settings.html carry no lang attribute")
	}
}

func readCoverage(t *testing.T, name string) map[rune]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tools", "fontsubset", "coverage", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[rune]bool{}
	for _, r := range strings.TrimRight(string(b), "\n") {
		m[r] = true
	}
	return m
}

// Every character of the texts of a language (dictionary, templates, scripts) is in the embedded Latin face or the CJK
// face of that language, or is in no Source font at all (coverage/nosource.txt), so the page never needs a fallback
// font for its own words. The language names of i18n.go are each set in their own language (option lang), so each is
// checked against the face of its language.
func TestSubsetsCoverEveryInterfaceCharacter(t *testing.T) {
	latin := readCoverage(t, "latin")
	nosource := readCoverage(t, "nosource")
	region := map[string]string{"zh-CN": "cn", "zh-TW": "tw", "ja": "jp", "ko": "kr", "en": ""}
	// English pages hold no Han face: the row-menu dots in the text of the write-control dialog come from the machine.
	fromMachine := map[rune]bool{'\u22ef': true}
	shared := ""
	for _, f := range []string{"web/app.js"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		shared += string(b)
	}
	hs, _ := filepath.Glob("web/*.html")
	for _, f := range hs {
		b, _ := os.ReadFile(f)
		shared += string(b)
	}
	check := func(what, text string, faces ...map[rune]bool) {
		seen := map[rune]bool{}
		var missing []string
	next:
		for _, r := range text {
			if r < 0x80 || seen[r] || nosource[r] || latin[r] {
				continue
			}
			seen[r] = true
			for _, f := range faces {
				if f[r] {
					continue next
				}
			}
			missing = append(missing, string(r))
		}
		if len(missing) > 0 {
			t.Errorf("%s: %d characters are in no embedded subset: %s", what, len(missing), strings.Join(missing, ""))
		}
	}
	for lang, reg := range region {
		cjk := map[rune]bool{}
		if reg != "" {
			cjk = readCoverage(t, reg)
		} else {
			cjk = fromMachine
		}
		b, err := os.ReadFile("i18n/" + lang + ".json")
		if err != nil {
			t.Fatal(err)
		}
		check(lang, string(b)+shared, cjk)
	}
	for _, l := range languages {
		if reg, ok := map[string]string{"zh-CN": "cn", "zh-TW": "tw", "ja": "jp", "ko": "kr"}[l.Code]; ok {
			check("language name "+l.Code, l.Name, readCoverage(t, "name-"+reg))
		}
	}
}

// A font file is cached for a week (it changes only with a release); other static files are revalidated.
func TestFontsAreCachedForAWeek(t *testing.T) {
	e := newEnv(t)
	c := e.webClient()
	resp, err := c.Get(e.ts.URL + "/static/fonts/SourceSans3-Latin.woff2")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=604800" {
		t.Errorf("font Cache-Control = %q", got)
	}
	resp, err = c.Get(e.ts.URL + "/static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("style.css Cache-Control = %q", got)
	}
}

// A grid card body lets a wide child (the time zone list set in the monospace font) shrink to the card: at 400 px the
// select overflowed the card by 19 px without minmax(0, 1fr).
func TestStackColumnMayShrink(t *testing.T) {
	css, _ := fs.ReadFile(webFS, "web/style.css")
	if !regexp.MustCompile(`(?m)^\.stack \{[^}]*grid-template-columns: minmax\(0, 1fr\)`).Match(css) {
		t.Error(".stack has no minmax(0, 1fr) column")
	}
}

// Each language's stacks name the Han face of its own region (the stack of ja once named the Traditional Chinese face
// and still passed a substring check), and each language option is set in the name face of its region.
func TestStacksMatchTheirLanguage(t *testing.T) {
	css, _ := fs.ReadFile(webFS, "web/style.css")
	region := map[string]string{"zh-TW": "TW", "ja": "JP", "ko": "KR"}
	all := regexp.MustCompile(`Kaze Han Sans (CN|TW|JP|KR)`)
	for lang, reg := range region {
		m := regexp.MustCompile(`(?m)^:root:lang\(` + lang + `\) \{[^\n]*$`).FindString(string(css))
		if m == "" {
			t.Errorf("style.css has no :root:lang(%s) rule", lang)
			continue
		}
		for _, f := range all.FindAllStringSubmatch(m, -1) {
			if f[1] != reg {
				t.Errorf(":root:lang(%s) names the %s face, want %s", lang, f[1], reg)
			}
		}
		if !strings.Contains(m, "Kaze Han Sans "+reg) {
			t.Errorf(":root:lang(%s) does not name the %s face", lang, reg)
		}
	}
	for lang, reg := range map[string]string{"zh-CN": "CN", "zh-TW": "TW", "ja": "JP", "ko": "KR"} {
		m := regexp.MustCompile(`(?m)^option\[lang=` + lang + `\] \{[^\n]*$`).FindString(string(css))
		if !strings.Contains(m, `"Kaze Name `+reg+`"`) || strings.Contains(m, "Kaze Han Sans") {
			t.Errorf("option[lang=%s] is not set in the name face of %s alone: %q", lang, reg, m)
		}
	}
	root := regexp.MustCompile(`(?m)^:root \{[^}]*\}`).FindString(string(css))
	if !strings.Contains(root, "Kaze Han Sans CN") || strings.Contains(root, "Kaze Han Sans TW") {
		t.Error("the default :root stack is not the Simplified Chinese one")
	}
	if en := regexp.MustCompile(`(?m)^:root:lang\(en\) \{[^\n]*$`).FindString(string(css)); en == "" || strings.Contains(en, "Kaze Han Sans") {
		t.Error("English pages must hold no embedded Han face")
	}
}

// The unicode-range of every face holds every character its subset has (a character in the font but outside the range
// would never be used).
func TestFontsCSSRangesHoldTheCoverage(t *testing.T) {
	fc, _ := fs.ReadFile(webFS, "web/fonts.css")
	for _, c := range []struct{ file, cov string }{
		{"SourceSans3-Latin.woff2", "latin"}, {"SourceHanSansCN-Regular-Subset.woff2", "cn"}, {"SourceHanSansTW-Regular-Subset.woff2", "tw"},
		{"SourceHanSansJP-Regular-Subset.woff2", "jp"}, {"SourceHanSansKR-Regular-Subset.woff2", "kr"},
		{"SourceHanSansCN-Name.woff2", "name-cn"}, {"SourceHanSansTW-Name.woff2", "name-tw"}, {"SourceHanSansJP-Name.woff2", "name-jp"}, {"SourceHanSansKR-Name.woff2", "name-kr"},
	} {
		m := regexp.MustCompile(`url\("/static/fonts/` + regexp.QuoteMeta(c.file) + `"\)[^}]*unicode-range: ([^;]*);`).FindSubmatch(fc)
		if m == nil {
			t.Errorf("%s: no unicode-range in fonts.css", c.file)
			continue
		}
		var spans [][2]int64
		for _, p := range strings.Split(string(m[1]), ",") {
			p = strings.TrimPrefix(strings.TrimSpace(p), "U+")
			lo, hi, _ := strings.Cut(p, "-")
			a, _ := strconv.ParseInt(lo, 16, 32)
			b := a
			if hi != "" {
				b, _ = strconv.ParseInt(hi, 16, 32)
			}
			spans = append(spans, [2]int64{a, b})
		}
		for r := range readCoverage(t, c.cov) {
			ok := false
			for _, s := range spans {
				if int64(r) >= s[0] && int64(r) <= s[1] {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s: U+%04X is in the font but outside its unicode-range", c.file, r)
			}
		}
	}
}
