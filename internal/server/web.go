package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"kaze-sync/internal/ignore"
	"kaze-sync/internal/proto"
	"kaze-sync/internal/version"
)

//go:embed web
var webFS embed.FS

// funcs are the template functions of one language (the texts they write are in it).
func funcs(t translator) template.FuncMap {
	return template.FuncMap{
		"t": t.T, "tm": t.H, "size": formatSize, "icon": icon, "stClass": stClass, "add1": func(i int) int { return i + 1 },
		"time":   func(unix int64) template.HTML { return timeTag(t, unix) },
		"stText": func(state string) string { return stText(t, state) },
		"advice": func(kind string) string { return adviceOf(t, kind) },
		"noteAdvice": func(v any) string {
			switch n := v.(type) {
			case notice:
				return adviceOf(t, n.Kind)
			case noticeRow:
				return adviceOf(t, n.Kind)
			}
			return ""
		},
		"heldMsg": func(v any) string {
			switch n := v.(type) {
			case notice:
				return heldMsg(t, n)
			case noticeRow:
				return heldMsg(t, n.notice)
			}
			return ""
		},
		"heldWhy": func(reason, other string) string { return heldWhy(t, reason, other) },
		"wcIcon": func(w wcInfo, isDir bool, held int, id string) template.HTML {
			return wcIcon(t, w, isDir, held, id, false)
		},
		"wcIconF": func(w wcInfo, id string) template.HTML { return wcIcon(t, w, true, 0, id, true) },
		"fold":    func(kind string, n int) string { return t.T("fold."+kind, "n", n) },
		"cfg":     func(state string) string { return cfgText(t, state) },
		"lineErr": func(line int, m string) string { return t.T("js.ruleLine", "n", line, "msg", ruleMsg(t, m)) },
		"names": func(devs []devTag) string {
			var out []string
			for _, d := range devs {
				out = append(out, d.Name)
			}
			return strings.Join(out, ", ")
		},
		"urlq": url.QueryEscape, "delq": func(deleted bool) string { return map[bool]string{true: "&deleted=1"}[deleted] },
		"raw": func(s string) template.HTML { return template.HTML(s) }, // only literals of the templates
		"countdown": func(unix int64) template.HTML {
			return template.HTML(`<span data-countdown="` + strconv.FormatInt(unix, 10) + `"></span>`)
		},
	}
}

func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f, suffix := float64(n), "KMGTP"
	i := -1
	for f >= unit && i < len(suffix)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %ciB", f, suffix[i])
}

// timeTag renders a time as the page shows it: app.js rewrites the text in the zone the settings ask for (design 4.7);
// without it the page reads in UTC.
func timeTag(t translator, unix int64) template.HTML {
	if unix <= 0 {
		return template.HTML(html.EscapeString(t.T("common.never")))
	}
	u := time.Unix(unix, 0).UTC()
	return template.HTML(`<time datetime="` + u.Format(time.RFC3339) + `">` + u.Format("2006-01-02 15:04:05") + `</time>`)
}

// icon draws an icon of the sprite /static/icons.svg.
func icon(name string, cls ...string) template.HTML {
	c := "i"
	if len(cls) > 0 {
		c += " " + cls[0]
	}
	return template.HTML(`<svg class="` + c + `" aria-hidden="true" viewBox="0 0 24 24"><use href="/static/icons.svg#i-` + name + `"/></svg>`)
}

func stText(t translator, state string) string {
	switch state {
	case "idle", "syncing", "error":
		return t.T("st." + state)
	}
	return state
}

// cfgText says whether a device has the latest configuration: "latest", "pending", or "-" when unknown.
func cfgText(t translator, state string) string {
	if state == "-" {
		return "-"
	}
	return t.T("cfg." + state)
}

// ruleMsg puts the message of the rule parser (English, see package ignore) into the language of the page.
func ruleMsg(t translator, m string) string {
	switch m {
	case ignore.ErrTrailingBackslash:
		return t.T("rule.trailingBackslash")
	case ignore.ErrBracket:
		return t.T("rule.bracket")
	}
	return m
}

func stClass(state string) string {
	switch state {
	case "idle":
		return "st-ok"
	case "syncing":
		return "st-sync"
	case "error":
		return "st-err"
	}
	return "st-none"
}

func parseTemplates(t translator) map[string]*template.Template {
	pages := map[string]*template.Template{}
	for _, n := range []string{"login", "error", "overview", "device", "folder", "settings", "live"} {
		pages[n] = template.Must(template.New("layout").Funcs(funcs(t)).ParseFS(webFS, "web/layout.html", "web/"+n+".html"))
	}
	return pages
}

// page is a template set in a language, parsed the first time the language is asked for.
func (s *Server) page(lang, name string) *template.Template {
	s.tmplMu.Lock()
	defer s.tmplMu.Unlock()
	set := s.tmpl[lang]
	if set == nil {
		set = parseTemplates(trFor(lang))
		s.tmpl[lang] = set
	}
	return set[name]
}

// staticFiles serves the embedded files; the ETag of each is the hash of its content, so a browser asks again on
// every visit but downloads only what changed (design: low traffic).
func staticFiles(static fs.FS) http.Handler {
	etags := map[string]string{}
	fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(static, p)
			sum := sha256.Sum256(b)
			etags[p] = `"` + hex.EncodeToString(sum[:8]) + `"`
		}
		return nil
	})
	files := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag := etags[strings.TrimPrefix(r.URL.Path, "/")]; tag != "" {
			w.Header().Set("ETag", tag)
			if strings.HasPrefix(strings.TrimPrefix(r.URL.Path, "/"), "fonts/") && strings.HasSuffix(r.URL.Path, ".woff2") {
				// Fonts change only with a release and are the heaviest files: a browser reuses them for a week without asking.
				w.Header().Set("Cache-Control", "public, max-age=604800")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
		}
		files.ServeHTTP(w, r)
	})
}

// userError is shown on the error page; any other error from a handler is a 500.
type userError struct {
	status int
	m      msg
	back   string
}

func (e userError) Error() string { return e.m.key }

// dialogState is a page shown with a dialog open: a form the server refused (the page behind the dialog comes back
// with the input kept and the message under the field, design 4.6) or the result of one (the enrollment command).
type dialogState struct {
	status   int // 0: 200
	dialog   string
	errs     map[string]msg // field name -> message
	ruleErrs []ignore.LineError
	extra    map[string]any // more page data
	page     func(http.ResponseWriter, *http.Request, *dialogState) error
}

func (e *dialogState) Error() string { return "form: " + strings.Join(mapValues(e.errs), "; ") }

func mapValues(m map[string]msg) []string {
	var out []string
	for _, v := range m {
		out = append(out, v.key)
	}
	sort.Strings(out)
	return out
}

type pageFn = func(http.ResponseWriter, *http.Request, *dialogState) error

// fieldErr refuses a form: page is shown again with the dialog open and the message under the field.
func fieldErr(page pageFn, status int, dialog, field, key string, kv ...any) error {
	return &dialogState{status: status, dialog: dialog, errs: map[string]msg{field: M(key, kv...)}, page: page}
}

type handler func(w http.ResponseWriter, r *http.Request) error

func (s *Server) webRoutes(mux *http.ServeMux) {
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticFiles(static)))
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, r, http.StatusOK, "login", trOf(r).T("title.login"), map[string]any{})
	})
	mux.HandleFunc("GET /i18n/{lang}", i18nScript)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		s.endSession(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /events", s.events)
	for pattern, h := range map[string]handler{
		"GET /resources":                    s.resourceJSON,
		"GET /{$}":                          s.overview,
		"POST /devices/add":                 s.addDevice,
		"POST /folders/new":                 s.newFolder,
		"GET /settings":                     s.settings,
		"POST /settings":                    s.saveSettings,
		"POST /settings/timezone":           s.saveTimeZone,
		"POST /settings/language":           s.setLanguage,
		"POST /rules":                       s.saveGlobalRules,
		"POST /rules/validate":              s.validateRules,
		"POST /rules/preview":               s.previewGlobalRules,
		"POST /folders/{id}/rules/preview":  s.previewFolderRules,
		"GET /devices/{id}":                 s.devicePageH,
		"POST /devices/{id}/mapping":        s.saveMapping,
		"POST /devices/{id}/revoke":         s.revoke,
		"POST /devices/{id}/rename":         s.renameDevice,
		"GET /folders/{id}":                 s.folderPageH,
		"GET /folders/{id}/restore-preview": s.restorePreviewPage,
		"GET /folders/{id}/restore-ticks":   s.restoreTicksPage,
		"GET /folders/{id}/changed":         s.folderChanged,
		"POST /folders/{id}/restore":        s.restore,
		"POST /folders/{id}/restore-dir":    s.restoreDir,
		"POST /folders/{id}/delete":         s.deleteFolder,
		"POST /folders/{id}/writectl":       s.writectlHandler,
		"POST /folders/{id}/wc-ack":         s.wcAckHandler,
		"POST /folders/{id}/resolve":        s.resolveHandler,
		"POST /settings/window":             s.saveWindow,
		"POST /folders/{id}/rules":          s.saveFolderRules,
	} {
		mux.HandleFunc(pattern, s.web(h))
	}
}

// web requires an admin session and turns handler errors into responses.
func (s *Server) web(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hasSession(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		var ds *dialogState
		var ue userError
		switch err := h(w, r); {
		case err == nil:
		case errors.As(err, &ds):
			if err := ds.page(w, r, ds); err != nil {
				slog.Error("web", "path", r.URL.Path, "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		case errors.As(err, &ue):
			t := trOf(r)
			s.render(w, r, ue.status, "error", t.T("title.error"), map[string]any{"Error": t.Msg(ue.m), "Back": ue.back})
		default:
			slog.Error("web", "path", r.URL.Path, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}
}

func bad(status int, back, key string, kv ...any) error {
	return userError{status, M(key, kv...), back}
}

// render shows a page. Every page of a session carries the bell and its popover, so the notifications are computed
// here unless the handler has done it.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name, title string, data map[string]any) {
	lang := requestLang(r)
	data["Title"], data["Auth"], data["Page"], data["Lang"] = title, name != "login", name, lang
	if name != "login" {
		if _, ok := data["Notif"]; !ok {
			data["Notif"] = s.notifications()
		}
		data["TZ"] = MetaString(s.db, "time_zone")
		data["Version"] = version.Version
	}
	for _, k := range []string{"Errs", "Vals"} {
		if _, ok := data[k]; !ok {
			data[k] = map[string]string{}
		}
	}
	if _, ok := data["Open"]; !ok {
		data["Open"] = ""
	}
	root := "layout"
	if name == "live" {
		root = "live"
	}
	var buf bytes.Buffer
	if err := s.page(lang, name).ExecuteTemplate(&buf, root, data); err != nil {
		slog.Error("render", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "Accept-Language, Cookie") // the language is chosen by these
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// show renders a page that may carry a dialog state, and the one-line result of the previous action.
func (s *Server) show(w http.ResponseWriter, r *http.Request, ds *dialogState, name, title string, data map[string]any) {
	status := http.StatusOK
	if ds != nil {
		if ds.status != 0 {
			status = ds.status
		}
		errs := map[string]string{}
		for k, m := range ds.errs {
			errs[k] = trOf(r).Msg(m)
		}
		data["Open"], data["Errs"], data["RuleErrs"] = ds.dialog, errs, ds.ruleErrs
		vals := map[string]string{}
		for k, v := range r.PostForm {
			vals[k] = v[0]
		}
		data["Vals"] = vals
		for k, v := range ds.extra {
			data[k] = v
		}
	}
	data["Flash"] = flashText(trOf(r), r)
	if r.Method != http.MethodGet { // a page that answers a POST is reloaded from the page's own address
		data["LiveURL"] = map[string]string{"overview": "/", "settings": "/settings", "device": "/devices/" + r.PathValue("id"),
			"folder": "/folders/" + r.PathValue("id") + "?dir=" + url.QueryEscape(r.FormValue("dir"))}[name]
	}
	s.render(w, r, status, name, title, data)
}

// flashText is the one-line result an action leaves in the redirect URL: a fixed word or numbers, never echoed text.
func flashText(t translator, r *http.Request) string {
	switch r.FormValue("ok") {
	case "saved":
		return t.T("flash.saved")
	case "rules":
		if n, err := strconv.Atoi(r.FormValue("deleted")); err == nil && n > 0 {
			return t.T("flash.rulesDeleted", "n", n)
		}
		return t.T("flash.rules")
	case "restored":
		return t.T("flash.restored")
	}
	n, err1 := strconv.Atoi(r.FormValue("restored"))
	failed, err2 := strconv.Atoi(r.FormValue("failed"))
	if err1 != nil || err2 != nil {
		return ""
	}
	if failed > 0 {
		return t.T("flash.restoredFailed", "n", n, "f", failed)
	}
	return t.T("flash.restoredN", "n", n)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.locked(r) {
		http.Error(w, "too many failures", http.StatusTooManyRequests)
		return
	}
	if !s.checkAdminPassword(r.FormValue("password")) {
		s.authFailed(r, "admin password")
		t := trOf(r)
		s.render(w, r, http.StatusUnauthorized, "login", t.T("title.login"), map[string]any{"Error": t.T("login.bad")})
		return
	}
	s.authOK(r)
	s.startSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// queryAll runs a query and scans every row with scan.
func queryAll[T any](db interface {
	Query(string, ...any) (*sql.Rows, error)
}, scan func(*sql.Rows, *T) error, q string, args ...any) ([]T, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var v T
		if err := scan(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// devTag is a device named in a folder's row.
type devTag struct {
	ID     int64
	Name   string
	Online bool
}

// mappedDevices lists, per folder, the devices that map it.
func (s *Server) mappedDevices() (map[int64][]devTag, error) {
	type row struct {
		folder int64
		devTag
	}
	rows, err := queryAll(s.db, func(rows *sql.Rows, r *row) error { return rows.Scan(&r.folder, &r.ID, &r.Name) },
		`SELECT m.folder_id, d.id, d.name FROM mappings m JOIN devices d ON d.id=m.device_id ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := map[int64][]devTag{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		r.Online = s.reports[r.ID] != nil && s.reports[r.ID].online(now)
		out[r.folder] = append(out[r.folder], r.devTag)
	}
	return out, nil
}

// folderStatus sums up what the devices that map a folder report: "unmapped", "error", "syncing" (with the number of
// pending files), "idle", or "offline" when none of them is online.
func (s *Server) folderStatus(folder int64, devs []devTag) (state string, pending int) {
	if len(devs) == 0 {
		return "unmapped", 0
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	online, syncing, failed := false, false, false
	for _, d := range devs {
		rep := s.reports[d.ID]
		if rep == nil {
			continue
		}
		on := rep.online(now)
		online = online || on
		for _, fs := range rep.req.Folders {
			if fs.ID != folder {
				continue
			}
			if errs, _ := noticeKindCounts(fs.Errors); errs > 0 || fs.State == "error" {
				failed = true
			}
			if on && (fs.State == "syncing" || fs.PendingUp+fs.PendingDown > 0) {
				syncing = true
				pending += fs.PendingUp + fs.PendingDown
			}
		}
	}
	switch {
	case failed:
		return "error", 0
	case !online:
		return "offline", 0
	case syncing:
		return "syncing", pending
	}
	return "idle", 0
}

type ovFolder struct {
	ID, Files, Bytes int64
	Name, Err        string // Err: the folder's database cannot be read; the rest of the page still works (design 2.3)
	State            string
	Pending          int
	Devs             []devTag
	Errors, Warnings int
	WC               wcInfo // the write control of the folder's root (design 2.8.11)
}

type ovDevice struct {
	ID                             int64
	Name, OS, Version, ConfigState string
	LastSeen                       int64
	Online                         bool
	TooOld                         int
	Status                         []deviceStatus
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) error {
	return s.overviewPage(w, r, nil)
}

func (s *Server) overviewPage(w http.ResponseWriter, r *http.Request, ds *dialogState) error {
	fols, err := queryAll(s.db, func(rows *sql.Rows, f *ovFolder) error { return rows.Scan(&f.ID, &f.Name) }, `SELECT id, name FROM folders ORDER BY name`)
	if err != nil {
		return err
	}
	roots, err := rootWC(s.db)
	if err != nil {
		return err
	}
	folderNames := map[int64]string{}
	for i, f := range fols {
		folderNames[f.ID] = f.Name
		fols[i].WC = roots[f.ID]
		if fols[i].WC.Mode == "" {
			fols[i].WC.Mode = proto.ModeSingle // no row: the default (design 2.8.2)
		}
	}
	mapped, err := s.mappedDevices()
	if err != nil {
		return err
	}
	nv := s.notifications()
	counts := map[int64]noticeGroup{}
	for _, g := range nv.Groups {
		counts[g.ID] = g
	}
	for i := range fols {
		f := &fols[i]
		f.Devs = mapped[f.ID]
		f.State, f.Pending = s.folderStatus(f.ID, f.Devs)
		f.Errors, f.Warnings = counts[f.ID].Errors, counts[f.ID].Warnings
		if f.Files, f.Bytes, err = s.folderStats(f.ID); err != nil {
			slog.Error("overview", "folder", f.ID, "err", err)
			f.Err = err.Error()
		}
	}
	cfgVersion := metaInt(s.db, "config_version")
	devs, err := queryAll(s.db, func(rows *sql.Rows, d *ovDevice) error {
		var seen int64
		if err := rows.Scan(&d.ID, &d.Name, &d.OS, &seen); err != nil {
			return err
		}
		d.Version, d.ConfigState = "-", "-"
		s.mu.Lock()
		if rep := s.reports[d.ID]; rep != nil {
			seen = max(seen, rep.at.Unix())
			if rep.req.ClientVersion != "" {
				d.Version = rep.req.ClientVersion
			}
			d.ConfigState = map[bool]string{true: "latest", false: "pending"}[rep.req.ConfigVersion == cfgVersion]
			d.Online = rep.online(s.now())
			if rep.tooOld > 0 {
				d.TooOld, d.ConfigState = rep.tooOld, "-"
			}
			for _, fs := range rep.req.Folders {
				d.Status = append(d.Status, deviceStatus{FolderID: fs.ID, Folder: folderNames[fs.ID], State: fs.State, Synced: fs.LastSyncedAt, PendingUp: fs.PendingUp, PendingDown: fs.PendingDown})
			}
			sort.Slice(d.Status, func(i, j int) bool { return d.Status[i].Folder < d.Status[j].Folder })
		}
		s.mu.Unlock()
		d.LastSeen = seen
		return nil
	}, `SELECT id, name, os, last_seen FROM devices ORDER BY name`)
	if err != nil {
		return err
	}
	data := map[string]any{"Devices": devs, "Folders": fols, "Notif": nv, "Server": "https://" + r.Host, "Fingerprint": s.Fingerprint}
	s.show(w, r, ds, "overview", trOf(r).T("nav.overview"), data)
	return nil
}

var deviceName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// nameErrLocked checks a device name for enrollment and renaming alike (design 2.5): the pattern, the reserved name of
// restores, and no other device (self is the device being renamed, 0 for a new one) or pending enrollment code of that
// name. It returns the HTTP status and message key of the problem, or 0. The caller holds s.mu, which guards the codes.
func (s *Server) nameErrLocked(name string, self int64) (int, string) {
	if !deviceName.MatchString(name) {
		return http.StatusBadRequest, "err.deviceName"
	}
	if strings.EqualFold(name, "web") { // the device name of restores: a device of that name would be bound by the write window
		return http.StatusBadRequest, "err.deviceReserved"
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM devices WHERE name=? AND id<>?`, name, self).Scan(&n)
	for k, c := range s.codes {
		if s.now().After(c.expires) {
			delete(s.codes, k)
		} else if c.name == name {
			n++
		}
	}
	if n > 0 {
		return http.StatusConflict, "err.deviceDup"
	}
	return 0, ""
}

func (s *Server) addDevice(w http.ResponseWriter, r *http.Request) error {
	name := strings.TrimSpace(r.FormValue("name"))
	s.mu.Lock()
	if status, key := s.nameErrLocked(name, 0); status != 0 {
		s.mu.Unlock()
		return fieldErr(s.overviewPage, status, "dlg-add-device", "name", key)
	}
	code := group(randomBase32(12), 4)
	expires := s.now().Add(codeLife)
	s.codes[sha(normalizeSecret(code))] = pendingCode{name, expires}
	s.mu.Unlock()
	return s.overviewPage(w, r, &dialogState{dialog: "dlg-enroll", extra: map[string]any{"Code": code, "CodeName": name, "Expires": expires.Unix()}})
}

func (s *Server) newFolder(w http.ResponseWriter, r *http.Request) error {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 64 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 }) {
		return fieldErr(s.overviewPage, http.StatusBadRequest, "dlg-new-folder", "name", "err.folderName")
	}
	var n int
	if s.db.QueryRow(`SELECT count(*) FROM folders WHERE name=?`, name).Scan(&n); n > 0 {
		return fieldErr(s.overviewPage, http.StatusConflict, "dlg-new-folder", "name", "err.folderDup")
	}
	id, err := s.createFolder(name)
	if err != nil {
		return err
	}
	http.Redirect(w, r, "/folders/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
	return nil
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) error {
	return s.settingsPage(w, r, nil)
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, ds *dialogState) error {
	rules := MetaString(s.db, "global_rules")
	if ds != nil && ds.dialog == "rules" {
		rules = r.PostForm.Get("rules")
	}
	langSetting := ""
	if c, err := r.Cookie(langCookie); err == nil && isLang(c.Value) {
		langSetting = c.Value
	}
	s.show(w, r, ds, "settings", trOf(r).T("nav.settings"), map[string]any{"KeepDays": metaInt(s.db, "keep_days"), "Rules": rules, "SavedRules": MetaString(s.db, "global_rules"), "TimeZone": MetaString(s.db, "time_zone"), "WindowMin": metaInt(s.db, "write_window") / 60,
		"Langs": languages, "LangSetting": langSetting})
	return nil
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) error {
	days, err := strconv.Atoi(r.FormValue("keep_days"))
	if err != nil || days < 1 || days > 3650 {
		return fieldErr(s.settingsPage, http.StatusBadRequest, "keep", "keep_days", "err.keepDays")
	}
	if err := SetMeta(s.db, "keep_days", days); err != nil {
		return err
	}
	s.requestGC() // clears the versions that the new value has expired at once
	http.Redirect(w, r, "/settings?ok=saved", http.StatusSeeOther)
	return nil
}

// saveWindow stores the single-writer time window (design 2.8.11); it only matters to the server and changes no
// configuration, like keep_days.
func (s *Server) saveWindow(w http.ResponseWriter, r *http.Request) error {
	min, err := strconv.Atoi(r.FormValue("window"))
	if err != nil || min < 0 || min > 60 {
		return fieldErr(s.settingsPage, http.StatusBadRequest, "window", "window", "err.window")
	}
	if err := SetMeta(s.db, "write_window", min*60); err != nil {
		return err
	}
	http.Redirect(w, r, "/settings?ok=saved", http.StatusSeeOther)
	return nil
}

// timeZoneName matches the names of the IANA zones ("UTC", "Asia/Shanghai", "America/Argentina/Buenos_Aires",
// "Etc/GMT+8"). The server holds no zone database (OpenWrt has none); the browser converts, so it is the browser
// that knows whether a name exists, and it falls back to its own zone for an unknown one.
var timeZoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+){0,2}$`)

func (s *Server) saveTimeZone(w http.ResponseWriter, r *http.Request) error {
	tz := ""
	if r.FormValue("tz_mode") == "fixed" {
		if tz = strings.TrimSpace(r.FormValue("tz")); len(tz) > 64 || !timeZoneName.MatchString(tz) {
			return fieldErr(s.settingsPage, http.StatusBadRequest, "tz", "tz", "err.timeZone")
		}
	}
	if err := SetMeta(s.db, "time_zone", tz); err != nil {
		return err
	}
	s.uiBump()
	http.Redirect(w, r, "/settings?ok=saved", http.StatusSeeOther)
	return nil
}

// maxRulesText bounds a rule set: every device receives it with its configuration.
const maxRulesText = 64 << 10

// ruleLines splits the text of a rule box the way saveRules and the validation take it.
func ruleLines(text string) []string {
	return strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
}

// ruleHit is a live file on the server that exclusion rules cover.
type ruleHit struct {
	Folder     int64
	Name, Path string // the folder's name, the file's path
	Seq        int64
}

// ruleHits lists the live files of a folder that its rules (the global ones, then its own) exclude. The match is
// case-sensitive: Windows devices fold case, but the server deletes only what the rules name exactly.
func (s *Server) ruleHits(folder int64, name, global, own string) ([]ruleHit, error) {
	m, _ := ignore.Compile(append(splitRules(global), splitRules(own)...), false)
	if m == nil {
		return nil, nil
	}
	db, err := s.folderDB(folder)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT path, seq FROM files WHERE hash IS NOT NULL ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ruleHit
	for rows.Next() {
		h := ruleHit{Folder: folder, Name: name}
		if err := rows.Scan(&h.Path, &h.Seq); err != nil {
			return nil, err
		}
		if m.ExcludedPath(h.Path, false) {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

// hitsToken identifies a preview: the rule text and the list of hits. A save is valid for exactly the rules and the
// files the person saw (no table: the server recomputes it at the save).
func hitsToken(text string, hits []ruleHit) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%s\n", len(text), text)
	for _, x := range hits {
		fmt.Fprintf(h, "%d\x00%s\n", x.Folder, x.Path)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// hitsView is what the page shows of the hits: the number, the first few paths and the token to confirm them.
// Changed says that the files differ from the ones the person previewed. Total counts every hit and Token covers
// them all; Filter (a glob, see globFilter) only narrows the list: Match is how many pass it, Paths the first
// previewMax of those.
type hitsView struct {
	Total, Match int
	Paths        []string
	Filter       string
	Filtered     bool
	Token        string
	Changed      bool
}

func newHitsView(text string, hits []ruleHit, withFolder bool, filter string) *hitsView {
	v := &hitsView{Total: len(hits), Token: hitsToken(text, hits)}
	keep := globFilter(filter)
	if keep != nil {
		v.Filtered, v.Filter = true, strings.TrimSpace(filter)
	}
	for _, h := range hits {
		if keep != nil && !keep(h.Path) {
			continue
		}
		v.Match++
		if len(v.Paths) >= previewMax {
			continue
		}
		if withFolder {
			v.Paths = append(v.Paths, h.Name+": "+h.Path)
		} else {
			v.Paths = append(v.Paths, h.Path)
		}
	}
	return v
}

// deleteHits deletes the files the rules cover from the server as ordinary versioned deletions, marked as made by
// the exclusion so that the clients keep their local copies (design 8.3). A file that changed since the hit was
// listed is left alone. It returns the number deleted.
func (s *Server) deleteHits(hits []ruleHit) int {
	deleted := 0
	for i := 0; i < len(hits); {
		j := i
		var ops []proto.Op
		for j < len(hits) && hits[j].Folder == hits[i].Folder && len(ops) < proto.MaxCommitOps {
			ops = append(ops, proto.Op{Path: hits[j].Path, BaseSeq: hits[j].Seq})
			j++
		}
		res, err := s.commit(hits[i].Folder, excludedDevice, ops)
		if err != nil {
			slog.Error("exclusion delete", "folder", hits[i].Name, "err", err)
		}
		for _, r := range res {
			if r.Status == proto.StatusOK {
				deleted++
			}
		}
		i = j
	}
	if len(hits) > 0 {
		slog.Info("exclusion delete", "listed", len(hits), "deleted", deleted)
	}
	return deleted
}

// saveRules validates rule text (the trust boundary, design 3.2), stores it with write (whose last
// argument is the text and whose other arguments are args) and, when it changed, pushes the new configuration.
// A refused text comes back as the page behind the dialog (page), with the errors listed under the text.
// Files on the server that the new rules cover are deleted by the save (design 8.3), so the person previews first
// (rulesPreview, which hands out a token for the text and the files) and the POST that carries the token saves. A POST
// with no token (no script in the browser) is answered with that preview and saves with the next one. A token that does
// not match what the server finds now (files came in or went since) saves nothing: the refreshed preview comes back.
func (s *Server) saveRules(w http.ResponseWriter, r *http.Request, page pageFn, dialog, back, read, write string, impact func(text string) ([]ruleHit, bool, error), args ...any) error {
	text := ruleText(r)
	if len(text) > maxRulesText {
		return fieldErr(page, http.StatusBadRequest, dialog, "rules", "err.rulesLong")
	}
	if _, errs := ignore.Compile(strings.Split(text, "\n"), false); len(errs) > 0 {
		return &dialogState{status: http.StatusBadRequest, dialog: dialog, ruleErrs: errs, page: page}
	}
	var old string
	if err := s.db.QueryRow(read, args...).Scan(&old); err != nil {
		return err
	}
	var hits []ruleHit
	if old != text {
		var withFolder bool
		var err error
		if hits, withFolder, err = impact(text); err != nil {
			return err
		}
		if confirm := r.FormValue("confirm"); confirm != hitsToken(text, hits) && (confirm != "" || len(hits) > 0) {
			v := newHitsView(text, hits, withFolder, "")
			v.Changed = confirm != ""
			return &dialogState{status: http.StatusConflict, dialog: dialog, page: page, extra: map[string]any{"Hits": v}}
		}
	}
	err := s.configChange(func(tx *sql.Tx) (bool, error) {
		var old string
		if err := tx.QueryRow(read, args...).Scan(&old); err != nil || old == text {
			return false, err
		}
		_, err := tx.Exec(write, append([]any{text}, args...)...)
		return true, err
	})
	if err != nil {
		return err
	}
	if n := s.deleteHits(hits); n > 0 {
		back += "&deleted=" + strconv.Itoa(n)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
	return nil
}

// ruleText is the text of a rule box in a request, as saveRules and the preview take it.
func ruleText(r *http.Request) string {
	return strings.TrimRight(strings.ReplaceAll(r.FormValue("rules"), "\r\n", "\n"), "\n")
}

// rulesPreview answers the rule boxes when the person asks to preview: the files on the server that the text in the
// box would delete, as the fragment of the "hits" template (also for none), with the token that a save must carry.
// Nothing is stored.
func (s *Server) rulesPreview(impact func(text string) ([]ruleHit, bool, error)) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		r.Body = http.MaxBytesReader(w, r.Body, 2*maxRulesText)
		text := ruleText(r)
		if len(text) > maxRulesText {
			return bad(http.StatusBadRequest, "/", "err.rulesLong")
		}
		if _, errs := ignore.Compile(strings.Split(text, "\n"), false); len(errs) > 0 {
			return bad(http.StatusBadRequest, "/", "err.rulesInvalid")
		}
		hits, withFolder, err := impact(text)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Vary", "Accept-Language, Cookie")
		return s.page(requestLang(r), "settings").ExecuteTemplate(w, "hits", newHitsView(text, hits, withFolder, r.FormValue("filter")))
	}
}

// validateRules answers the rule boxes while the user types: the errors of the same parser that saveRules uses, so
// that the browser and the server never disagree.
func (s *Server) validateRules(w http.ResponseWriter, r *http.Request) error {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRulesText+1))
	if err != nil {
		return bad(http.StatusRequestEntityTooLarge, "/", "err.rulesLong")
	}
	type lineErr struct {
		Line int    `json:"line"`
		Msg  string `json:"msg"`
	}
	out := struct {
		Errors []lineErr `json:"errors"`
	}{[]lineErr{}}
	_, errs := ignore.Compile(ruleLines(string(b)), false)
	for _, e := range errs {
		out.Errors = append(out.Errors, lineErr{e.Line, ruleMsg(trOf(r), e.Msg)})
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(out)
}

// configChange runs change in a transaction. When change reports a change, the configuration version goes up in the
// same transaction and the hanging polls are woken, so that every edit of a device's configuration is announced
// (design 3.3).
func (s *Server) configChange(change func(tx *sql.Tx) (bool, error)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed, err := change(tx)
	if err != nil {
		return err
	}
	if changed {
		if _, err := tx.Exec(`UPDATE meta SET value=value+1 WHERE key='config_version'`); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed {
		s.mu.Lock()
		s.noticeMappings = nil
		s.broadcastLocked()
		s.mu.Unlock()
	}
	return nil
}

// globalImpact lists the files that new global rules would delete: they act on every folder, each with its own rules after them.
func (s *Server) globalImpact() func(text string) ([]ruleHit, bool, error) {
	return func(text string) (all []ruleHit, _ bool, _ error) {
		fols, err := queryAll(s.db, func(rows *sql.Rows, f *folderRow) error { return rows.Scan(&f.ID, &f.Name, &f.Rules) }, `SELECT id, name, rules FROM folders ORDER BY name`)
		for _, f := range fols {
			hits, herr := s.ruleHits(f.ID, f.Name, text, f.Rules)
			if herr != nil {
				return nil, false, herr
			}
			all = append(all, hits...)
		}
		return all, true, err
	}
}

func (s *Server) saveGlobalRules(w http.ResponseWriter, r *http.Request) error {
	return s.saveRules(w, r, s.settingsPage, "rules", "/settings?ok=rules", `SELECT value FROM meta WHERE key='global_rules'`, `UPDATE meta SET value=? WHERE key='global_rules'`, s.globalImpact())
}

// folderImpact lists the files that new rules of a folder would delete.
func (s *Server) folderImpact(f folderRow) func(text string) ([]ruleHit, bool, error) {
	return func(text string) ([]ruleHit, bool, error) {
		hits, err := s.ruleHits(f.ID, f.Name, MetaString(s.db, "global_rules"), text)
		return hits, false, err
	}
}

func (s *Server) saveFolderRules(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	return s.saveRules(w, r, s.folderPage, "dlg-rules", "/folders/"+strconv.FormatInt(f.ID, 10)+"?ok=rules", `SELECT rules FROM folders WHERE id=?`, `UPDATE folders SET rules=? WHERE id=?`, s.folderImpact(f), f.ID)
}

func (s *Server) previewFolderRules(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	return s.rulesPreview(s.folderImpact(f))(w, r)
}

func (s *Server) previewGlobalRules(w http.ResponseWriter, r *http.Request) error {
	return s.rulesPreview(s.globalImpact())(w, r)
}

type deviceRow struct {
	ID       int64
	Name, OS string
	LastSeen int64
}

func (s *Server) loadDevice(r *http.Request) (d deviceRow, err error) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	err = s.db.QueryRow(`SELECT id, name, os, last_seen FROM devices WHERE id=?`, id).Scan(&d.ID, &d.Name, &d.OS, &d.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		err = bad(http.StatusNotFound, "/", "err.noDevice")
	}
	return d, err
}

func (s *Server) devicePageH(w http.ResponseWriter, r *http.Request) error {
	return s.devicePage(w, r, nil)
}

func (s *Server) devicePage(w http.ResponseWriter, r *http.Request, ds *dialogState) error {
	d, err := s.loadDevice(r)
	if err != nil {
		return err
	}
	type mapping struct {
		FolderID         int64
		Folder, Path     string
		Adopt, Reported  bool
		State            string
		Synced           int64
		PendingUp        int
		PendingDown      int
		Errors, Warnings int
		Locks            int // exclusive ranges the device holds in this folder
		Held             int // changes the device holds for the user in this folder
	}
	maps, err := queryAll(s.db, func(rows *sql.Rows, m *mapping) error {
		return rows.Scan(&m.FolderID, &m.Folder, &m.Path, &m.Adopt)
	}, `SELECT f.id, f.name, m.path, m.adopt FROM mappings m JOIN folders f ON f.id=m.folder_id WHERE m.device_id=? ORDER BY f.name`, d.ID)
	if err != nil {
		return err
	}
	nLocks := 0
	type lc struct{ folder, n int64 }
	cnt, lerr := queryAll(s.db, func(rows *sql.Rows, c *lc) error { return rows.Scan(&c.folder, &c.n) },
		`SELECT folder_id, count(*) FROM writectl WHERE mode='exclusive' AND device_id=? GROUP BY folder_id`, d.ID)
	if lerr != nil {
		return lerr
	}
	for _, c := range cnt {
		nLocks += int(c.n)
		for i := range maps {
			if maps[i].FolderID == c.folder {
				maps[i].Locks = int(c.n)
			}
		}
	}
	free, err := queryAll(s.db, func(rows *sql.Rows, f *folderRow) error {
		return rows.Scan(&f.ID, &f.Name)
	}, `SELECT id, name FROM folders WHERE id NOT IN (SELECT folder_id FROM mappings WHERE device_id=?) ORDER BY name`, d.ID)
	if err != nil {
		return err
	}
	head := struct {
		Online, Known        bool
		Version, ConfigState string
		TooOld               int
		LastSeen, ReportedAt int64
		Errors, Warnings     int
	}{LastSeen: d.LastSeen, Version: "-", ConfigState: "-"}
	cfgVersion := metaInt(s.db, "config_version")
	s.mu.Lock()
	if rep := s.reports[d.ID]; rep != nil {
		head.Known, head.LastSeen = true, max(d.LastSeen, rep.at.Unix())
		head.Online = rep.online(s.now())
		if !head.Online {
			head.ReportedAt = rep.at.Unix()
		}
		if rep.req.ClientVersion != "" {
			head.Version = rep.req.ClientVersion
		}
		head.ConfigState = map[bool]string{true: "latest", false: "pending"}[rep.req.ConfigVersion == cfgVersion]
		head.TooOld = rep.tooOld
		for _, fs := range rep.req.Folders {
			e, w := noticeKindCounts(fs.Errors)
			head.Errors, head.Warnings = head.Errors+e, head.Warnings+w
			for i := range maps {
				if maps[i].FolderID == fs.ID {
					m := &maps[i]
					m.Reported, m.State, m.Synced, m.PendingUp, m.PendingDown, m.Errors, m.Warnings, m.Held = true, fs.State, fs.LastSyncedAt, fs.PendingUp, fs.PendingDown, e, w, fs.Held
				}
			}
		}
	}
	s.mu.Unlock()
	data := map[string]any{"Device": d, "Head": head, "Mappings": maps, "Free": free, "LockCount": nLocks}
	if ds != nil && ds.dialog == "dlg-map-edit" {
		for _, m := range maps {
			if strconv.FormatInt(m.FolderID, 10) == r.PostForm.Get("folder") {
				data["EditName"] = m.Folder
			}
		}
	}
	s.show(w, r, ds, "device", trOf(r).T("title.device", "name", d.Name), data)
	return nil
}

// cleanMapping validates a mapping path for a device of the given OS against
// the device's other mapping paths and returns its normalized form (design 3.2).
func cleanMapping(osName, p string, others []string) (string, error) {
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 }) {
		return "", M("err.pathControl")
	}
	norm, sep, fold := path.Clean, "/", func(q string) string { return q }
	if osName == "windows" {
		if len(p) < 3 || p[0]|0x20 < 'a' || p[0]|0x20 > 'z' || p[1] != ':' || (p[2] != '\\' && p[2] != '/') {
			return "", M("err.pathWindows")
		}
		sep, fold = `\`, strings.ToLower
		norm = func(q string) string { // drive, then the rest cleaned like a Linux path ("..", ".", doubled separators)
			rest := path.Clean("/" + strings.ReplaceAll(q[2:], `\`, "/"))
			return q[:2] + strings.ReplaceAll(rest, "/", `\`)
		}
	} else if !strings.HasPrefix(p, "/") {
		return "", M("err.pathLinux")
	}
	contains := func(a, b string) bool { // a is b or a parent of b
		a, b = fold(strings.TrimSuffix(a, sep)), fold(b)
		return b == a || strings.HasPrefix(b, a+sep)
	}
	clean := norm(p)
	for _, o := range others {
		if contains(clean, o) || contains(o, clean) {
			return "", M("err.pathOverlap", "path", o)
		}
	}
	return clean, nil
}

func (s *Server) saveMapping(w http.ResponseWriter, r *http.Request) error {
	d, err := s.loadDevice(r)
	if err != nil {
		return err
	}
	back := "/devices/" + strconv.FormatInt(d.ID, 10)
	folder, _ := strconv.ParseInt(r.FormValue("folder"), 10, 64)
	var n int
	if s.db.QueryRow(`SELECT count(*) FROM folders WHERE id=?`, folder).Scan(&n); n == 0 {
		return bad(http.StatusNotFound, back, "err.noFolder")
	}
	err = s.configChange(func(tx *sql.Tx) (bool, error) {
		if r.FormValue("action") == "delete" {
			if _, err := releaseExclusive(tx, d.ID, folder); err != nil { // an exclusive setting of a device that no longer maps the folder would freeze it
				return false, err
			}
			res, err := tx.Exec(`DELETE FROM mappings WHERE device_id=? AND folder_id=?`, d.ID, folder)
			if err != nil {
				return false, err
			}
			n, _ := res.RowsAffected()
			return n > 0, nil
		}
		type mapping struct {
			folder int64
			path   string
			adopt  bool
		}
		all, err := queryAll(tx, func(rows *sql.Rows, m *mapping) error {
			return rows.Scan(&m.folder, &m.path, &m.adopt)
		}, `SELECT folder_id, path, adopt FROM mappings WHERE device_id=?`, d.ID)
		if err != nil {
			return false, err
		}
		var others []string
		var old *mapping
		for i, m := range all {
			if m.folder == folder {
				old = &all[i]
			} else {
				others = append(others, m.path)
			}
		}
		clean, err := cleanMapping(d.OS, strings.TrimSpace(r.FormValue("path")), others)
		if err != nil {
			dialog := "dlg-map-add"
			if old != nil {
				dialog = "dlg-map-edit"
			}
			m, _ := err.(msg)
			return false, fieldErr(s.devicePage, http.StatusBadRequest, dialog, "path", m.key, m.kv...)
		}
		adopt := r.FormValue("adopt") != ""
		if old != nil && old.path == clean && old.adopt == adopt {
			return false, nil
		}
		_, err = tx.Exec(`INSERT OR REPLACE INTO mappings VALUES(?,?,?,?)`, d.ID, folder, clean, adopt)
		return true, err
	})
	if err != nil {
		return err
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
	return nil
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) error {
	d, err := s.loadDevice(r)
	if err != nil {
		return err
	}
	err = s.configChange(func(tx *sql.Tx) (bool, error) {
		freed, err := releaseExclusive(tx, d.ID, 0) // a revoked device would leave its range frozen; the other devices' configuration changes
		if err != nil {
			return false, err
		}
		for _, q := range []string{`DELETE FROM mappings WHERE device_id=?`, `DELETE FROM devices WHERE id=?`} {
			if _, err := tx.Exec(q, d.ID); err != nil {
				return false, err
			}
		}
		return freed > 0, nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.reports, d.ID)
	s.noticeMappings = nil
	s.bumpLocked()
	s.mu.Unlock()
	slog.Info("device revoked", "device", d.Name)
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}

// renameDevice gives a device another name (design 2.5). It serializes name reads with the best-effort updates
// to current rows across folders. Historical versions keep their writer's name.
func (s *Server) renameDevice(w http.ResponseWriter, r *http.Request) error {
	s.renameMu.Lock()
	defer s.renameMu.Unlock()
	d, err := s.loadDevice(r)
	if err != nil {
		return err
	}
	back := "/devices/" + strconv.FormatInt(d.ID, 10)
	name := strings.TrimSpace(r.FormValue("name"))
	if name == d.Name {
		http.Redirect(w, r, back+"?ok=saved", http.StatusSeeOther)
		return nil
	}
	s.mu.Lock()
	status, key := s.nameErrLocked(name, d.ID)
	s.mu.Unlock()
	if status != 0 {
		return fieldErr(s.devicePage, status, "dlg-rename", "name", key)
	}
	err = s.configChange(func(tx *sql.Tx) (bool, error) {
		_, err := tx.Exec(`UPDATE devices SET name=? WHERE id=?`, name, d.ID)
		return true, err
	})
	if err != nil && strings.Contains(err.Error(), "UNIQUE") { // an enrollment took the name since the check
		return fieldErr(s.devicePage, http.StatusConflict, "dlg-rename", "name", "err.deviceDup")
	}
	if err != nil {
		return err
	}
	slog.Info("device renamed", "device_id", d.ID, "old", d.Name, "new", name)
	s.renameWriter(d.Name, name)
	http.Redirect(w, r, back+"?ok=saved", http.StatusSeeOther)
	return nil
}

// renameWriter puts a renamed device's new name on the current rows it wrote, in every folder (design 2.5): the
// write-control judgement (2.8.3) and the own-commit check (6.1) compare writers by name, and would take the device's
// own recent writes for another device's. Old versions keep the name they were written under.
func (s *Server) renameWriter(old, name string) {
	ids, err := queryAll(s.db, func(rows *sql.Rows, id *int64) error { return rows.Scan(id) }, `SELECT id FROM folders`)
	if err != nil {
		slog.Warn("device renamed: cannot list the folders", "err", err)
	}
	for _, id := range ids {
		db, err := s.folderDB(id)
		if err == nil {
			_, err = db.Exec(`UPDATE files SET device=? WHERE device=?`, name, old)
		}
		if err != nil {
			slog.Warn("device renamed: the folder's current rows keep the old name", "folder", id, "err", err)
		}
	}
	s.uiBump()
}

// wcIcon is the icon of a directory or file that shows its write control (design 2.8.11): the shape and color follow
// the effective mode, a filled body marks a setting on the path itself (an outline: inherited), a triangle marks held
// changes below it. The texts that were labels sit in a tip (id), which app.js shows on hover, tap, and keyboard focus of
// the row's name (it links the tip with aria-describedby) or, with focus set, of the icon itself. The tip names the state
// and its effect only; where the setting comes from is told by filled or outline and by the write-control dialog.
func wcIcon(t translator, w wcInfo, isDir bool, held int, id string, focus bool) template.HTML {
	kind := "file"
	if isDir {
		kind = "folder"
	}
	cls := "wci wc-" + w.Mode
	if w.Explicit {
		cls += " set"
	}
	label := t.T("wc.tag." + w.Mode)
	if w.Mode == proto.ModeExclusive {
		label = t.T("wc.tag.exclusive", "device", w.Dev)
	}
	var b strings.Builder
	b.WriteString(`<span class="` + cls + `" data-tip="` + id + `"`)
	if focus {
		b.WriteString(` tabindex="0" role="img" aria-label="` + html.EscapeString(label) + `" aria-describedby="` + id + `"`)
	}
	b.WriteString(`><svg class="i wc" aria-hidden="true" viewBox="0 0 24 24"><use href="/static/icons.svg#i-` + kind + `-` + w.Mode + `"/></svg>`)
	if held > 0 {
		b.WriteString(string(icon("alert", "ov")))
	}
	b.WriteString(`<span class="tip-c" id="` + id + `" hidden><span class="tp">` + html.EscapeString(label) + `</span><span class="ts">` +
		html.EscapeString(t.T("dlg.wc."+w.Mode+".help")) + `</span>`)
	if !isDir && w.Mode == proto.ModeFree { // the file is a scope of its own although the directory is free (design 2.8.2)
		b.WriteString(`<span class="ts">` + html.EscapeString(t.T("wc.tip.file")) + `</span>`)
	}
	b.WriteString(`</span></span>`)
	return template.HTML(b.String())
}
