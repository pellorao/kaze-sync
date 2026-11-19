package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"kaze-sync/internal/ignore"
)

func baseKeys(t *testing.T) map[string]string {
	t.Helper()
	d := dictOf(languages[0].Code)
	if len(d) == 0 {
		t.Fatal("the base dictionary is empty")
	}
	return d
}

var placeholder = regexp.MustCompile(`\{[A-Za-z0-9]+\}`)

func placeholders(s string) string {
	p := placeholder.FindAllString(s, -1)
	sort.Strings(p)
	return strings.Join(p, " ")
}

// Every language has exactly the keys of the base language (English may add ".1" variants for a count of one), no
// text is empty, and a text carries the placeholders of its base text.
func TestDictionariesMatchTheBase(t *testing.T) {
	t.Parallel()
	base := baseKeys(t)
	for _, l := range languages {
		d := dictOf(l.Code)
		for k, v := range d {
			if strings.TrimSpace(v) == "" {
				t.Errorf("%s: %q is empty", l.Code, k)
			}
			bk := strings.TrimSuffix(k, ".1")
			if k != bk && l.Code != "en" {
				t.Errorf("%s: %q: only English has count variants", l.Code, k)
			}
			if _, ok := base[bk]; !ok {
				t.Errorf("%s: %q is not in the base dictionary", l.Code, k)
				continue
			}
			if got, want := placeholders(v), placeholders(base[bk]); got != want && k == bk {
				t.Errorf("%s: %q has placeholders [%s], the base has [%s]", l.Code, k, got, want)
			}
		}
		for k := range base {
			if _, ok := d[k]; !ok {
				t.Errorf("%s: lacks %q", l.Code, k)
			}
		}
	}
}

// A ".1" variant is a variant of a text that counts, so its base text has a count placeholder.
func TestCountVariantsHaveACount(t *testing.T) {
	t.Parallel()
	for k, v := range dictOf("en") {
		if !strings.HasSuffix(k, ".1") {
			continue
		}
		base := dictOf("en")[strings.TrimSuffix(k, ".1")]
		if p := placeholders(v); p != placeholders(base) {
			t.Errorf("%q has placeholders [%s], %q has [%s]", k, p, strings.TrimSuffix(k, ".1"), placeholders(base))
		}
	}
}

// The keys named in the templates, the Go code and app.js all exist, and every key of the base dictionary is used by
// one of them (or belongs to a family that is built from a state or kind at run time).
func TestDictionaryKeysAreUsed(t *testing.T) {
	t.Parallel()
	base := baseKeys(t)
	groups := map[string]bool{}
	for k := range base {
		groups[k[:strings.Index(k, ".")]] = true
	}
	var names []string
	for g := range groups {
		names = append(names, regexp.QuoteMeta(g))
	}
	keyRe := regexp.MustCompile(`"((?:` + strings.Join(names, "|") + `)\.[A-Za-z0-9_.-]*[A-Za-z0-9_])"`)
	dynamic := []string{"st.", "cfg.", "kind.", "advice.", "fold.", "unsynced.", "js.ago_", "js.menu.", "held.why.", "held.kind.", "held.note.", "wc.tag.", "wc.mode.", "js.wc.mode."}

	var files []string
	for _, pat := range []string{"*.go", "web/*.html", "web/app.js"} {
		m, _ := filepath.Glob(pat)
		files = append(files, m...)
	}
	used := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range keyRe.FindAllStringSubmatch(string(b), -1) {
			key := m[1]
			used[key] = true
			if _, ok := base[key]; !ok && !strings.HasSuffix(f, "i18n.go") {
				t.Errorf("%s uses %q, which the dictionary lacks", f, key)
			}
		}
	}
	for k := range base {
		if used[k] {
			continue
		}
		fam := false
		for _, p := range dynamic {
			fam = fam || strings.HasPrefix(k, p)
		}
		if !fam {
			t.Errorf("%q is not used", k)
		}
	}
	// The families: a text for every state and kind the code can ask for.
	for _, k := range []string{"st.idle", "st.syncing", "st.error", "cfg.latest", "cfg.pending", "kind.rejected", "kind.io", "kind.folder", "kind.in_use",
		"kind.skipped", "advice.rejected", "advice.io", "advice.folder", "advice.in_use", "advice.skipped", "advice.report", "kind.report", "advice.held", "fold.skipped",
		"fold.in_use", "fold.link-not-created", "unsynced.linkNotCreated", "unsynced.symlink", "unsynced.utf8"} {
		if _, ok := base[k]; !ok {
			t.Errorf("the dictionary lacks %q", k)
		}
	}
}

func TestMatchLang(t *testing.T) {
	t.Parallel()
	for header, want := range map[string]string{
		"":                              "en",
		"fr-FR,fr;q=0.9":                "en",
		"zh-CN,zh;q=0.9,en;q=0.8":       "zh-CN",
		"zh":                            "zh-CN",
		"zh-SG":                         "zh-CN",
		"zh-Hans-CN":                    "zh-CN",
		"zh-TW":                         "zh-TW",
		"zh-HK,zh;q=0.9":                "zh-TW",
		"zh-Hant":                       "zh-TW",
		"zh-Hant-TW":                    "zh-TW",
		"ZH-tw":                         "zh-TW",
		"ja,en;q=0.5":                   "ja",
		"ja-JP":                         "ja",
		"ko-KR,ko;q=0.9":                "ko",
		"en-US,en;q=0.9,ja;q=0.8":       "en",
		"de;q=0.9,ja;q=0.8,en;q=0.7":    "ja",
		"en;q=0.5, ja;q=0.9":            "ja",
		"ja;q=0":                        "en",
		"fr, ko ; q=0.4, zh-TW ; q=0.6": "zh-TW",
		"*":                             "en",
		";q=0.5, , ko":                  "ko",
	} {
		if got := matchLang(header); got != want {
			t.Errorf("matchLang(%q) = %s, want %s", header, got, want)
		}
	}
}

func TestRequestLangCookieBeatsTheHeader(t *testing.T) {
	t.Parallel()
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Language", "ja")
	if got := requestLang(r); got != "ja" {
		t.Errorf("header only: %s", got)
	}
	r.AddCookie(&http.Cookie{Name: langCookie, Value: "ko"})
	if got := requestLang(r); got != "ko" {
		t.Errorf("cookie and header: %s", got)
	}
	r, _ = http.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Language", "ja")
	r.AddCookie(&http.Cookie{Name: langCookie, Value: "xx"})
	if got := requestLang(r); got != "ja" {
		t.Errorf("a cookie with an unknown language is ignored, got %s", got)
	}
}

func TestTranslatorTexts(t *testing.T) {
	t.Parallel()
	en, zh := trFor("en"), trFor("zh-CN")
	if got := en.T("fo.nFiles", "n", 1); got != "1 file" {
		t.Errorf("one: %q", got)
	}
	if got := en.T("fo.nFiles", "n", 2); got != "2 files" {
		t.Errorf("two: %q", got)
	}
	if got := en.T("fo.nFiles", "n", int64(1)); got != "1 file" {
		t.Errorf("int64 one: %q", got)
	}
	if got := zh.T("fo.nFiles", "n", 1); got != "1 个文件" {
		t.Errorf("zh one: %q", got)
	}
	if got := zh.T("no.such.key"); got != "no.such.key" {
		t.Errorf("an unknown key shows as itself, got %q", got)
	}
	// A value is never searched for placeholders, and the HTML form escapes values but not its own tags.
	if got := en.T("err.restoreFailed", "reason", "{reason}"); got != "Cannot restore: {reason}" {
		t.Errorf("value rescanned: %q", got)
	}
	if got := string(en.H("pv.count", "n", 3)); got != "Files that existed then and differ now: <b>3</b>" {
		t.Errorf("tags: %q", got)
	}
	if got := string(en.H("notif.offlineDays", "href", "/x?a=1&b=2", "name", "<i>x</i>", "d", 3)); !strings.Contains(got, `<a href="/x?a=1&amp;b=2">`) || !strings.Contains(got, "&lt;i&gt;x&lt;/i&gt;") {
		t.Errorf("escaping: %q", got)
	}
}

func TestI18nScript(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.ts.Client()
	get := func(path, etag string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := get("/i18n/ja", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(body, "window.KS_I18N=") || !strings.Contains(body, "コピーしました") || strings.Contains(body, "ov.folders") {
		t.Fatalf("script: %d %.200s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("content type %q", ct)
	}
	var obj map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(body, "window.KS_I18N="), ";")), &obj); err != nil || obj["js.copied"] == "" || obj["nav.settings"] == "" {
		t.Errorf("the script is not the dictionary: %v", err)
	}
	if r2, _ := get("/i18n/ja", resp.Header.Get("ETag")); r2.StatusCode != 304 {
		t.Errorf("revalidation: %d", r2.StatusCode)
	}
	if r3, _ := get("/i18n/xx", ""); r3.StatusCode != 404 {
		t.Errorf("unknown language: %d", r3.StatusCode)
	}
}

// Every page renders in every language: no key shows through, no placeholder is left, and the page says its language.
func TestPagesRenderInEveryLanguage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.setAdminPassword("pw")
	f := e.folder("Docs")
	dev, _ := e.enrollDevice("laptop", "linux")
	e.mapFolder(dev, f, "/docs")
	e.commit(f, e.op("d/a.txt", 0, "one"), e.op("d/a.conflict-laptop-20261101-120000.txt", 0, "two"))
	leftover := regexp.MustCompile(`\{[a-z]+\}|\b(?:common|nav|title|login|notif|ov|nf|ad|en|dev|ma|me|fo|pv|rs|ru|hits|ver|un|set|err|flash|js)\.[a-zA-Z0-9]+\b`)
	for _, l := range languages {
		c := &http.Client{Transport: acceptLang{e.ts.Client().Transport, l.Code}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		c.Jar = e.webClient().Jar
		e.login(c, "pw")
		for _, path := range []string{"/", "/settings", "/devices/" + itoa(dev), "/folders/" + itoa(f), "/folders/" + itoa(f) + "?dir=d&deleted=1",
			"/folders/" + itoa(f) + "?dir=d&file=d%2Fa.txt", "/devices/add", "/folders/99"} {
			status, page := e.get(c, path)
			if status != 200 && status != 404 && status != 405 {
				t.Errorf("%s %s: %d", l.Code, path, status)
				continue
			}
			if status == 200 && !strings.Contains(page, `<html lang="`+l.Code+`">`) {
				t.Errorf("%s %s: wrong page language", l.Code, path)
			}
			text := regexp.MustCompile(`(?s)<(pre|script)[^>]*>.*?</(pre|script)>`).ReplaceAllString(page, "")
			text = regexp.MustCompile(`href="[^"]*"|src="[^"]*"|(?:id|for|name|class|data-[a-z-]+)="[^"]*"`).ReplaceAllString(text, "")
			if m := leftover.FindString(text); m != "" {
				t.Errorf("%s %s: %q shows through", l.Code, path, m)
			}
		}
		// A form error is in the language too.
		_, page := e.post(c, "/devices/add", url.Values{"name": {"bad name!"}})
		if want := trFor(l.Code).T("err.deviceName"); !strings.Contains(page, want) {
			t.Errorf("%s: the form error is not %q", l.Code, want)
		}
	}
}

func TestSetLanguage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	post := func(lang string) *http.Response {
		resp, err := c.PostForm(e.ts.URL+"/settings/language", url.Values{"lang": {lang}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	cookie := func(resp *http.Response) *http.Cookie {
		for _, ck := range resp.Cookies() {
			if ck.Name == langCookie {
				return ck
			}
		}
		return nil
	}
	resp := post("ko")
	if ck := cookie(resp); resp.StatusCode != 303 || ck == nil || ck.Value != "ko" || !ck.Secure || !ck.HttpOnly || ck.Path != "/" {
		t.Fatalf("choosing: %d %v", resp.StatusCode, ck)
	}
	// The choice wins over the browser's list (this client sends zh-CN), and the settings page offers it.
	if _, page := e.get(c, "/settings"); !strings.Contains(page, `<html lang="ko">`) || !strings.Contains(page, `value="ko" selected`) {
		t.Errorf("settings after choosing ko:\n%s", page)
	}
	// Follow the browser again.
	resp = post("")
	if ck := cookie(resp); resp.StatusCode != 303 || ck == nil || ck.MaxAge >= 0 {
		t.Fatalf("clearing: %d %v", resp.StatusCode, ck)
	}
	if _, page := e.get(c, "/settings"); !strings.Contains(page, `<html lang="zh-CN">`) {
		t.Errorf("settings after clearing:\n%s", page)
	}
	if resp := post("xx"); resp.StatusCode != 400 {
		t.Errorf("unknown language: %d", resp.StatusCode)
	}
	// Not for strangers.
	resp, err := e.ts.Client().PostForm(e.ts.URL+"/settings/language", url.Values{"lang": {"ja"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 303 && resp.Header.Get("Location") != "/login" || len(resp.Cookies()) > 0 {
		t.Errorf("without a session: %d %s %v", resp.StatusCode, resp.Header.Get("Location"), resp.Cookies())
	}
}

// The messages of the rule parser come out in the language of the request; an unknown message stays as it is.
func TestRuleMessagesAreTranslated(t *testing.T) {
	t.Parallel()
	for _, l := range languages {
		tr := trFor(l.Code)
		for _, m := range []string{ignore.ErrTrailingBackslash, ignore.ErrBracket} {
			if got := ruleMsg(tr, m); got == m && l.Code != "en" || got == "" {
				t.Errorf("%s: %q is not translated", l.Code, m)
			}
		}
	}
	if got := ruleMsg(trFor("ja"), "something else"); got != "something else" {
		t.Errorf("unknown message: %q", got)
	}
}
