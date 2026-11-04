package server

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// The Web UI speaks five languages (design 4.9). One dictionary per language, a flat JSON object of key -> text, is
// embedded and read the first time a request needs that language. Texts carry {name} placeholders; a few carry the
// inline tags <code>, <b> and <a> (an <a> takes its address from {href}). Keys that start with "js." are also sent to
// the browser (/i18n/<lang>) for the texts app.js writes.

//go:embed i18n/*.json
var i18nFS embed.FS

type language struct{ Code, Name string }

// languages lists the supported languages, each named in itself. The first is the base: the dictionary the others
// are checked against, and the original text.
var languages = []language{{"zh-CN", "简体中文"}, {"zh-TW", "繁體中文"}, {"en", "English"}, {"ja", "日本語"}, {"ko", "한국어"}}

const (
	defaultLang = "en" // when nothing in the request matches
	langCookie  = "__Host-kaze-lang"
)

func isLang(code string) bool {
	for _, l := range languages {
		if l.Code == code {
			return true
		}
	}
	return false
}

// requestLang is the language of a request: the one chosen in the settings (a cookie of this browser), otherwise the
// best match of the browser's own list (Accept-Language).
func requestLang(r *http.Request) string {
	if c, err := r.Cookie(langCookie); err == nil && isLang(c.Value) {
		return c.Value
	}
	return matchLang(r.Header.Get("Accept-Language"))
}

// matchLang picks the first language of an Accept-Language header (by quality, then order) that the UI has. Chinese
// in traditional script (zh-TW, zh-HK, zh-MO, zh-Hant) is zh-TW, any other Chinese is zh-CN.
func matchLang(header string) string {
	type tag struct {
		name string
		q    float64
	}
	var tags []tag
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if _, v, ok := strings.Cut(strings.ReplaceAll(params, " ", ""), "q="); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				q = f
			}
		}
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" && q > 0 {
			tags = append(tags, tag{name, q})
		}
	}
	sort.SliceStable(tags, func(i, j int) bool { return tags[i].q > tags[j].q })
	for _, t := range tags {
		base, _, _ := strings.Cut(t.name, "-")
		switch base {
		case "zh":
			for _, trad := range []string{"-tw", "-hk", "-mo", "-hant"} {
				if strings.Contains(t.name, trad) {
					return "zh-TW"
				}
			}
			return "zh-CN"
		case "en", "ja", "ko":
			return base
		}
	}
	return defaultLang
}

var (
	dictMu sync.Mutex
	dicts  = map[string]map[string]string{}
)

// dictOf returns the dictionary of a language, reading it on first use. An unreadable one is empty: texts then fall
// back to the other languages (the tests make sure the files are whole).
func dictOf(code string) map[string]string {
	dictMu.Lock()
	defer dictMu.Unlock()
	if d, ok := dicts[code]; ok {
		return d
	}
	d := map[string]string{}
	b, err := i18nFS.ReadFile("i18n/" + code + ".json")
	if err == nil {
		err = json.Unmarshal(b, &d)
	}
	if err != nil {
		slog.Error("dictionary", "lang", code, "err", err)
	}
	dicts[code] = d
	return d
}

// translator writes the texts of one language.
type translator struct {
	lang string
	d    map[string]string
}

func trFor(code string) translator {
	if !isLang(code) {
		code = defaultLang
	}
	return translator{code, dictOf(code)}
}

func trOf(r *http.Request) translator { return trFor(requestLang(r)) }

// lookup finds the text of a key: in this language, else English, else the base language; an unknown key shows as
// itself. n is the count the text is about: when it is 1 and the key has a ".1" variant (English "1 file" against
// "2 files"), that is the text.
func (t translator) lookup(key string, n int) string {
	for _, d := range []map[string]string{t.d, dictOf("en"), dictOf(languages[0].Code)} {
		if n == 1 {
			if s, ok := d[key+".1"]; ok {
				return s
			}
		}
		if s, ok := d[key]; ok {
			return s
		}
	}
	return key
}

// firstCount is the first whole-number argument of a text: the count that its ".1" variant is chosen by.
func firstCount(kv []any) int {
	for i := 1; i < len(kv); i += 2 {
		switch n := kv[i].(type) {
		case int:
			return n
		case int64:
			return int(n)
		}
	}
	return -1
}

func fill(s string, kv []any, esc bool) string {
	if len(kv) == 0 {
		return s
	}
	pairs := make([]string, 0, len(kv))
	for i := 0; i+1 < len(kv); i += 2 {
		v := fmt.Sprint(kv[i+1])
		if _, raw := kv[i+1].(template.HTML); esc && !raw {
			v = html.EscapeString(v)
		}
		pairs = append(pairs, "{"+fmt.Sprint(kv[i])+"}", v)
	}
	return strings.NewReplacer(pairs...).Replace(s) // one pass: a value is never searched for placeholders
}

// T is the plain text of a key, with name/value pairs for its placeholders.
func (t translator) T(key string, kv ...any) string {
	return fill(t.lookup(key, firstCount(kv)), kv, false)
}

var inlineTags = strings.NewReplacer("&lt;code&gt;", "<code>", "&lt;/code&gt;", "</code>", "&lt;b&gt;", "<b>", "&lt;/b&gt;", "</b>",
	"&lt;a&gt;", `<a href="{href}">`, "&lt;/a&gt;", "</a>")

// H is the text of a key as HTML: the text and the values are escaped, except the inline tags of the text and values
// that already are template.HTML.
func (t translator) H(key string, kv ...any) template.HTML {
	return template.HTML(fill(inlineTags.Replace(html.EscapeString(t.lookup(key, firstCount(kv)))), kv, true))
}

// jsDict is the script that hands the browser the "js." texts of a language, and its ETag.
var jsDicts sync.Map // language -> [2]string{script, etag}

func jsDict(code string) (script, etag string) {
	if v, ok := jsDicts.Load(code); ok {
		p := v.([2]string)
		return p[0], p[1]
	}
	out := map[string]string{}
	for _, d := range []map[string]string{dictOf(languages[0].Code), dictOf("en"), dictOf(code)} { // the better language overrides
		for k, v := range d {
			if strings.HasPrefix(k, "js.") || strings.HasPrefix(k, "common.") || strings.HasPrefix(k, "nav.") {
				out[k] = v
			}
		}
	}
	b, _ := json.Marshal(out)
	script = "window.KS_I18N=" + string(b) + ";"
	sum := sha256.Sum256([]byte(script))
	etag = `"` + hex.EncodeToString(sum[:8]) + `"`
	jsDicts.Store(code, [2]string{script, etag})
	return script, etag
}

// i18nScript serves GET /i18n/{lang}: the texts of app.js in one language.
func i18nScript(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("lang")
	if !isLang(code) {
		http.NotFound(w, r)
		return
	}
	script, etag := jsDict(code)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Write([]byte(script))
}

// msg is a message of the page before it is put into a language: a dictionary key with the values of its
// placeholders. Handlers and validators return these (as errors where an error is the natural carrier); the page
// that shows one turns it into the language of the request.
type msg struct {
	key string
	kv  []any
}

func M(key string, kv ...any) msg { return msg{key, kv} }

func (m msg) Error() string { return m.key }

func (t translator) Msg(m msg) string { return t.T(m.key, m.kv...) }

// setLanguage is POST /settings/language: the choice is kept in a cookie of this browser; "" (follow the browser)
// removes it.
func (s *Server) setLanguage(w http.ResponseWriter, r *http.Request) error {
	code := r.FormValue("lang")
	if code != "" && !isLang(code) {
		return bad(http.StatusBadRequest, "/settings", "err.language")
	}
	c := &http.Cookie{Name: langCookie, Value: code, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 365 * 24 * 3600}
	if code == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, "/settings?ok=saved", http.StatusSeeOther)
	return nil
}
