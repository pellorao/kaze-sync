package server

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Web UI ships its fonts (Source Sans 3 as "Kaze Sans", Source Serif 4 as "Kaze Serif" for the headings, JetBrains Mono, Source Han
// Sans and Serif per language); tools/fontsubset/subset.py
// builds them, and writes beside them the characters each subset holds (tools/fontsubset/coverage).
var fontFiles = []string{
	"SourceSans3-Latin.woff2", "JetBrainsMono-Regular-Latin.woff2", "JetBrainsMono-Bold-Latin.woff2",
	"SourceHanSansCN-Regular-Subset.woff2", "SourceHanSansCN-Bold-Subset.woff2",
	"SourceHanSansTW-Regular-Subset.woff2", "SourceHanSansTW-Bold-Subset.woff2",
	"SourceHanSansJP-Regular-Subset.woff2", "SourceHanSansJP-Bold-Subset.woff2",
	"SourceHanSansKR-Regular-Subset.woff2", "SourceHanSansKR-Bold-Subset.woff2",
	"SourceHanSansCN-Name.woff2", "SourceHanSansTW-Name.woff2", "SourceHanSansJP-Name.woff2", "SourceHanSansKR-Name.woff2",
	"SourceSerif4-SemiBold-Latin.woff2",
	"SourceHanSerifCN-SemiBold-Subset.woff2", "SourceHanSerifTW-SemiBold-Subset.woff2", "SourceHanSerifJP-SemiBold-Subset.woff2", "SourceHanSerifKR-SemiBold-Subset.woff2",
}

const maxFaceBytes = 2 << 20 // low traffic is the first goal: one CJK face stays within 2 MB

func TestFontFilesAreEmbeddedAndSmall(t *testing.T) {
	t.Parallel()
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
	for _, n := range []string{"LICENSE-source-sans.md", "LICENSE-source-han-sans.txt", "LICENSE-source-serif.md", "LICENSE-source-han-serif.txt", "LICENSE-jetbrains-mono.txt", "SOURCE.md"} {
		if b, err := fs.ReadFile(webFS, "web/fonts/"+n); err != nil || len(b) == 0 {
			t.Errorf("web/fonts/%s is missing: the fonts are shipped with their licenses", n)
		}
	}
}

func TestFontsCSSAndStacks(t *testing.T) {
	t.Parallel()
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
	for _, want := range []string{"Kaze Sans", "JetBrains Mono", "Kaze Han Sans CN", "Kaze Han Sans TW", "Kaze Han Sans JP", "Kaze Han Sans KR", "Noto Sans CJK KR", "Source Han Sans", "Kaze Serif", "Kaze Han Serif CN", "Kaze Han Serif TW", "Kaze Han Serif JP", "Kaze Han Serif KR", "Noto Serif CJK KR", "Source Han Serif"} {
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
	t.Parallel()
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
	check := func(what, text string, latin, nosource map[rune]bool, faces ...map[rune]bool) {
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
		check(lang, string(b)+shared, latin, nosource, cjk)
		// The headings use the serif faces for the same texts (a title holds folder and device names, so the range is the same).
		scjk := fromMachine
		if reg != "" {
			scjk = readCoverage(t, "serif-"+reg)
		}
		check(lang+" (serif)", string(b)+shared, readCoverage(t, "serif-latin"), readCoverage(t, "nosource-serif"), scjk)
	}
	for _, l := range languages {
		if reg, ok := map[string]string{"zh-CN": "cn", "zh-TW": "tw", "ja": "jp", "ko": "kr"}[l.Code]; ok {
			check("language name "+l.Code, l.Name, latin, nosource, readCoverage(t, "name-"+reg))
		}
	}
}

// A font file is cached for a week (it changes only with a release); other static files are revalidated.
func TestFontsAreCachedForAWeek(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	css, _ := fs.ReadFile(webFS, "web/style.css")
	if !regexp.MustCompile(`(?m)^\.stack \{[^}]*grid-template-columns: minmax\(0, 1fr\)`).Match(css) {
		t.Error(".stack has no minmax(0, 1fr) column")
	}
}

// Each language's stacks name the Han face of its own region (the stack of ja once named the Traditional Chinese face
// and still passed a substring check), and each language option is set in the name face of its region.
func TestStacksMatchTheirLanguage(t *testing.T) {
	t.Parallel()
	css, _ := fs.ReadFile(webFS, "web/style.css")
	region := map[string]string{"zh-TW": "TW", "ja": "JP", "ko": "KR"}
	all := regexp.MustCompile(`Kaze Han Sans (CN|TW|JP|KR)`)
	allSerif := regexp.MustCompile(`Kaze Han Serif (CN|TW|JP|KR)`)
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
		for _, f := range allSerif.FindAllStringSubmatch(m, -1) {
			if f[1] != reg {
				t.Errorf(":root:lang(%s) names the %s serif face, want %s", lang, f[1], reg)
			}
		}
		if !strings.Contains(m, "--serif:") || !strings.Contains(m, "Kaze Han Serif "+reg) {
			t.Errorf(":root:lang(%s) does not set --serif to the %s serif face", lang, reg)
		}
	}
	for lang, reg := range map[string]string{"zh-CN": "CN", "zh-TW": "TW", "ja": "JP", "ko": "KR"} {
		m := regexp.MustCompile(`(?m)^option\[lang=` + lang + `\] \{[^\n]*$`).FindString(string(css))
		if !strings.Contains(m, `"Kaze Name `+reg+`"`) || strings.Contains(m, "Kaze Han Sans") {
			t.Errorf("option[lang=%s] is not set in the name face of %s alone: %q", lang, reg, m)
		}
	}
	root := regexp.MustCompile(`(?m)^:root \{[^}]*\}`).FindString(string(css))
	if !strings.Contains(root, "Kaze Han Sans CN") || strings.Contains(root, "Kaze Han Sans TW") || !strings.Contains(root, "Kaze Han Serif CN") || strings.Contains(root, "Kaze Han Serif TW") {
		t.Error("the default :root stack is not the Simplified Chinese one")
	}
	if en := regexp.MustCompile(`(?m)^:root:lang\(en\) \{[^\n]*$`).FindString(string(css)); en == "" || strings.Contains(en, "Kaze Han Sans") || strings.Contains(en, "Kaze Han Serif") {
		t.Error("English pages must hold no embedded Han face")
	}
}

// The unicode-range of every face holds every character its subset has (a character in the font but outside the range
// would never be used).
func TestFontsCSSRangesHoldTheCoverage(t *testing.T) {
	t.Parallel()
	fc, _ := fs.ReadFile(webFS, "web/fonts.css")
	for _, c := range []struct{ file, cov string }{
		{"SourceSans3-Latin.woff2", "latin"}, {"SourceHanSansCN-Regular-Subset.woff2", "cn"}, {"SourceHanSansTW-Regular-Subset.woff2", "tw"},
		{"SourceHanSansJP-Regular-Subset.woff2", "jp"}, {"SourceHanSansKR-Regular-Subset.woff2", "kr"},
		{"SourceSerif4-SemiBold-Latin.woff2", "serif-latin"}, {"SourceHanSerifCN-SemiBold-Subset.woff2", "serif-cn"}, {"SourceHanSerifTW-SemiBold-Subset.woff2", "serif-tw"},
		{"SourceHanSerifJP-SemiBold-Subset.woff2", "serif-jp"}, {"SourceHanSerifKR-SemiBold-Subset.woff2", "serif-kr"},
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

// The headings (h1, h2, h3 and the title line of a folder, device or settings page) are set in the serif stack, like the status
// page; the body and everything else stay sans (user 2026-11-15).
func TestHeadingsAreSerifBodyIsSans(t *testing.T) {
	t.Parallel()
	css, _ := fs.ReadFile(webFS, "web/style.css")
	rule := regexp.MustCompile(`(?m)^h1, h2, h3, \.crumbs \{([^}]*)\}`).FindStringSubmatch(string(css))
	if rule == nil || !strings.Contains(rule[1], "font-family: var(--serif)") || !strings.Contains(rule[1], "font-weight: 600") {
		t.Fatal("style.css does not set h1, h2, h3 and .crumbs in var(--serif) at weight 600")
	}
	if b := regexp.MustCompile(`(?m)^body \{[^}]*\}`).FindString(string(css)); !strings.Contains(b, "var(--sans)") || strings.Contains(b, "serif") {
		t.Errorf("the body is not set in the sans stack: %s", b)
	}
	// The face has the one weight 600 (weights 500 and 700 take it as it is, with no synthetic bold in between).
	fc, _ := fs.ReadFile(webFS, "web/fonts.css")
	for _, fam := range []string{"Kaze Serif", "Kaze Han Serif CN", "Kaze Han Serif TW", "Kaze Han Serif JP", "Kaze Han Serif KR"} {
		if !regexp.MustCompile(`font-family: "` + fam + `"; font-style: normal; font-weight: 600;`).Match(fc) {
			t.Errorf("fonts.css does not declare %q at weight 600 only", fam)
		}
	}
	// Source Serif 4 and Source Han Serif carry the Reserved Font Name "Source": no CSS family is named after it.
	for _, p := range []string{"web/fonts.css", "web/style.css"} {
		b, _ := fs.ReadFile(webFS, p)
		if strings.Contains(string(b), `font-family: "Source`) {
			t.Errorf("%s declares a face under a Source name", p)
		}
	}
}

// The font files are Modified Versions of faces that carry the Reserved Font Name "Source" (OFL 1.1 clause 3; Source Han Sans
// and Source Han Serif carry it too, as their license files say): no name record of any shipped face except the copyright,
// license and license URL says "Source", and the families are the Kaze names (needs python3 with fontTools and brotli).
func TestEveryFaceIsRenamed(t *testing.T) {
	t.Parallel()
	for _, n := range []string{"LICENSE-source-sans.md", "LICENSE-source-serif.md", "LICENSE-source-han-sans.txt", "LICENSE-source-han-serif.txt"} {
		b, _ := fs.ReadFile(webFS, "web/fonts/"+n)
		if !strings.Contains(string(b), "Reserved Font") {
			t.Errorf("%s: no Reserved Font Name statement (the renaming rule would be moot)", n)
		}
	}
	tool, _ := os.ReadFile("../../tools/fontsubset/subset.py")
	for _, want := range []string{"def rename_han", `SERIF_FAMILY = "Kaze Serif"`} {
		if !strings.Contains(string(tool), want) {
			t.Errorf("tools/fontsubset/subset.py lacks %q", want)
		}
	}
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
bad = [n.toUnicode() for n in f["name"].names if n.nameID not in (0, 13, 14) and "ource" in n.toUnicode()]
fam = sorted({n.toUnicode() for n in f["name"].names if n.nameID == 1})
lic = sorted({n.nameID for n in f["name"].names if n.nameID in (0, 13, 14)})
print(bad, fam, lic)`
	for _, c := range []struct{ file, family string }{
		{"SourceSans3-Latin.woff2", "Kaze Sans"}, {"SourceSerif4-SemiBold-Latin.woff2", "Kaze Serif"},
		{"SourceHanSansCN-Regular-Subset.woff2", "Kaze Han Sans CN"}, {"SourceHanSansCN-Bold-Subset.woff2", "Kaze Han Sans CN"},
		{"SourceHanSansTW-Regular-Subset.woff2", "Kaze Han Sans TW"}, {"SourceHanSansJP-Regular-Subset.woff2", "Kaze Han Sans JP"},
		{"SourceHanSansKR-Regular-Subset.woff2", "Kaze Han Sans KR"}, {"SourceHanSansKR-Name.woff2", "Kaze Han Sans KR"},
		{"SourceHanSerifCN-SemiBold-Subset.woff2", "Kaze Han Serif CN SemiBold"}, {"SourceHanSerifTW-SemiBold-Subset.woff2", "Kaze Han Serif TW SemiBold"},
		{"SourceHanSerifJP-SemiBold-Subset.woff2", "Kaze Han Serif JP SemiBold"}, {"SourceHanSerifKR-SemiBold-Subset.woff2", "Kaze Han Serif KR SemiBold"},
	} {
		out, err := exec.Command(py, "-I", "-c", code, "web/fonts/"+c.file).CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got, want := strings.TrimSpace(string(out)), "[] ['"+c.family+"'] [0, 13, 14]"; got != want {
			t.Errorf("%s name table: %s, want %s", c.file, got, want)
		}
	}
}
