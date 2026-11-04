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
)

//go:embed web
var webFS embed.FS

var funcs = template.FuncMap{"size": formatSize, "time": timeTag, "icon": icon, "stText": stText, "stClass": stClass, "add1": func(i int) int { return i + 1 }}

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
func timeTag(unix int64) template.HTML {
	if unix <= 0 {
		return "从未"
	}
	t := time.Unix(unix, 0).UTC()
	return template.HTML(`<time datetime="` + t.Format(time.RFC3339) + `">` + t.Format("2006-01-02 15:04:05") + `</time>`)
}

// icon draws an icon of the sprite /static/icons.svg.
func icon(name string, cls ...string) template.HTML {
	c := "i"
	if len(cls) > 0 {
		c += " " + cls[0]
	}
	return template.HTML(`<svg class="` + c + `" aria-hidden="true" viewBox="0 0 24 24"><use href="/static/icons.svg#i-` + name + `"/></svg>`)
}

func stText(state string) string {
	switch state {
	case "idle":
		return "已同步"
	case "syncing":
		return "同步中"
	case "error":
		return "有错误"
	}
	return state
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

func parseTemplates() map[string]*template.Template {
	pages := map[string]*template.Template{}
	for _, n := range []string{"login", "error", "overview", "device", "folder", "settings", "live"} {
		pages[n] = template.Must(template.New("layout").Funcs(funcs).ParseFS(webFS, "web/layout.html", "web/"+n+".html"))
	}
	return pages
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
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// userError is shown on the error page; any other error from a handler is a 500.
type userError struct {
	status    int
	msg, back string
}

func (e userError) Error() string { return e.msg }

// dialogState is a page shown with a dialog open: a form the server refused (the page behind the dialog comes back
// with the input kept and the message under the field, design 4.6) or the result of one (the enrollment command).
type dialogState struct {
	status   int // 0: 200
	dialog   string
	errs     map[string]string // field name -> message
	ruleErrs []ignore.LineError
	extra    map[string]any // more page data
	page     func(http.ResponseWriter, *http.Request, *dialogState) error
}

func (e *dialogState) Error() string { return "form: " + strings.Join(mapValues(e.errs), "; ") }

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

type pageFn = func(http.ResponseWriter, *http.Request, *dialogState) error

// fieldErr refuses a form: page is shown again with the dialog open and msg under the field.
func fieldErr(page pageFn, status int, dialog, field, msg string) error {
	return &dialogState{status: status, dialog: dialog, errs: map[string]string{field: msg}, page: page}
}

type handler func(w http.ResponseWriter, r *http.Request) error

func (s *Server) webRoutes(mux *http.ServeMux) {
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticFiles(static)))
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, http.StatusOK, "login", "登录", map[string]any{})
	})
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		s.endSession(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /events", s.events)
	for pattern, h := range map[string]handler{
		"GET /{$}":                          s.overview,
		"POST /devices/add":                 s.addDevice,
		"POST /folders/new":                 s.newFolder,
		"GET /settings":                     s.settings,
		"POST /settings":                    s.saveSettings,
		"POST /settings/timezone":           s.saveTimeZone,
		"POST /rules":                       s.saveGlobalRules,
		"POST /rules/validate":              s.validateRules,
		"GET /devices/{id}":                 s.devicePageH,
		"POST /devices/{id}/mapping":        s.saveMapping,
		"POST /devices/{id}/revoke":         s.revoke,
		"GET /folders/{id}":                 s.folderPageH,
		"GET /folders/{id}/restore-preview": s.restorePreviewPage,
		"GET /folders/{id}/changed":         s.folderChanged,
		"POST /folders/{id}/restore":        s.restore,
		"POST /folders/{id}/restore-dir":    s.restoreDir,
		"POST /folders/{id}/delete":         s.deleteFolder,
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
			s.render(w, ue.status, "error", "出错了", map[string]any{"Error": ue.msg, "Back": ue.back})
		default:
			slog.Error("web", "path", r.URL.Path, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}
}

func bad(status int, back, msg string) error { return userError{status, msg, back} }

// render shows a page. Every page of a session carries the bell and its popover, so the notifications are computed
// here unless the handler has done it.
func (s *Server) render(w http.ResponseWriter, status int, name, title string, data map[string]any) {
	data["Title"], data["Auth"], data["Page"] = title, name != "login", name
	if name != "login" {
		if _, ok := data["Notif"]; !ok {
			data["Notif"] = s.notifications()
		}
		data["TZ"] = MetaString(s.db, "time_zone")
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
	if err := s.tmpl[name].ExecuteTemplate(&buf, root, data); err != nil {
		slog.Error("render", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
		data["Open"], data["Errs"], data["RuleErrs"] = ds.dialog, ds.errs, ds.ruleErrs
		vals := map[string]string{}
		for k, v := range r.PostForm {
			vals[k] = v[0]
		}
		data["Vals"] = vals
		for k, v := range ds.extra {
			data[k] = v
		}
	}
	data["Flash"] = flashText(r)
	if r.Method != http.MethodGet { // a page that answers a POST is reloaded from the page's own address
		data["LiveURL"] = map[string]string{"overview": "/", "settings": "/settings", "device": "/devices/" + r.PathValue("id"),
			"folder": "/folders/" + r.PathValue("id") + "?dir=" + url.QueryEscape(r.FormValue("dir"))}[name]
	}
	s.render(w, status, name, title, data)
}

// flashText is the one-line result an action leaves in the redirect URL: a fixed word or numbers, never echoed text.
func flashText(r *http.Request) string {
	switch r.FormValue("ok") {
	case "saved":
		return "已保存"
	case "rules":
		if n, err := strconv.Atoi(r.FormValue("deleted")); err == nil && n > 0 {
			return fmt.Sprintf("排除规则已保存, 服务端删除了 %d 个文件", n)
		}
		return "排除规则已保存"
	case "restored":
		return "已恢复"
	}
	n, err1 := strconv.Atoi(r.FormValue("restored"))
	failed, err2 := strconv.Atoi(r.FormValue("failed"))
	if err1 != nil || err2 != nil {
		return ""
	}
	msg := fmt.Sprintf("已恢复 %d 个文件", n)
	if failed > 0 {
		msg += fmt.Sprintf(", %d 个失败 (名称冲突或被同时修改)", failed)
	}
	return msg
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.locked(r) {
		http.Error(w, "too many failures", http.StatusTooManyRequests)
		return
	}
	if !s.checkAdminPassword(r.FormValue("password")) {
		s.authFailed(r, "admin password")
		s.render(w, http.StatusUnauthorized, "login", "登录", map[string]any{"Error": "密码错误"})
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
	folderNames := map[int64]string{}
	for _, f := range fols {
		folderNames[f.ID] = f.Name
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
			d.Version = strconv.Itoa(rep.req.AgentVersion)
			d.ConfigState = map[bool]string{true: "最新", false: "待更新"}[rep.req.ConfigVersion == cfgVersion]
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
	s.show(w, r, ds, "overview", "总览", data)
	return nil
}

var deviceName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func (s *Server) addDevice(w http.ResponseWriter, r *http.Request) error {
	name := strings.TrimSpace(r.FormValue("name"))
	if !deviceName.MatchString(name) {
		return fieldErr(s.overviewPage, http.StatusBadRequest, "dlg-add-device", "name", "设备名称只能用字母, 数字, - 和 _, 至多 32 个字符 (冲突副本的文件名会包含它)")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM devices WHERE name=?`, name).Scan(&n)
	s.mu.Lock()
	for k, c := range s.codes {
		if s.now().After(c.expires) {
			delete(s.codes, k)
		} else if c.name == name {
			n++
		}
	}
	if n > 0 {
		s.mu.Unlock()
		return fieldErr(s.overviewPage, http.StatusConflict, "dlg-add-device", "name", "已有同名设备")
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
		return fieldErr(s.overviewPage, http.StatusBadRequest, "dlg-new-folder", "name", "文件夹名称不能为空, 至多 64 个字节, 不含控制字符")
	}
	var n int
	if s.db.QueryRow(`SELECT count(*) FROM folders WHERE name=?`, name).Scan(&n); n > 0 {
		return fieldErr(s.overviewPage, http.StatusConflict, "dlg-new-folder", "name", "已有同名文件夹")
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
	s.show(w, r, ds, "settings", "设置", map[string]any{"KeepDays": metaInt(s.db, "keep_days"), "Rules": rules, "TimeZone": MetaString(s.db, "time_zone")})
	return nil
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) error {
	days, err := strconv.Atoi(r.FormValue("keep_days"))
	if err != nil || days < 1 || days > 3650 {
		return fieldErr(s.settingsPage, http.StatusBadRequest, "keep", "keep_days", "保留天数必须是 1 到 3650 的整数")
	}
	if err := SetMeta(s.db, "keep_days", days); err != nil {
		return err
	}
	s.requestGC() // clears the versions that the new value has expired at once
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
			return fieldErr(s.settingsPage, http.StatusBadRequest, "tz", "tz", "时区名称无效, 例如 UTC 或 Asia/Shanghai")
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

// hitsToken identifies a list of hits: the confirmation of a save is valid for exactly the files the person saw.
func hitsToken(hits []ruleHit) string {
	h := sha256.New()
	for _, x := range hits {
		fmt.Fprintf(h, "%d\x00%s\n", x.Folder, x.Path)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// hitsView is what the page shows of the hits: the number, the first few paths and the token to confirm them.
type hitsView struct {
	Total int
	Paths []string
	More  int
	Token string
}

const hitsShown = 10

func newHitsView(hits []ruleHit, withFolder bool) *hitsView {
	v := &hitsView{Total: len(hits), Token: hitsToken(hits)}
	for _, h := range hits[:min(len(hits), hitsShown)] {
		if withFolder {
			v.Paths = append(v.Paths, h.Name+": "+h.Path)
		} else {
			v.Paths = append(v.Paths, h.Path)
		}
	}
	v.More = len(hits) - len(v.Paths)
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
// Files on the server that the new rules cover are deleted by the save (design 8.3), so the first POST only lists them
// (the dialog comes back with the files and a confirmation token) and the POST that carries the token saves.
func (s *Server) saveRules(w http.ResponseWriter, r *http.Request, page pageFn, dialog, back, read, write string, impact func(text string) ([]ruleHit, bool, error), args ...any) error {
	text := strings.TrimRight(strings.ReplaceAll(r.FormValue("rules"), "\r\n", "\n"), "\n")
	if len(text) > maxRulesText {
		return fieldErr(page, http.StatusBadRequest, dialog, "rules", "规则太长")
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
		if len(hits) > 0 && r.FormValue("confirm") != hitsToken(hits) {
			return &dialogState{status: http.StatusConflict, dialog: dialog, page: page, extra: map[string]any{"Hits": newHitsView(hits, withFolder)}}
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

// validateRules answers the rule boxes while the user types: the errors of the same parser that saveRules uses, so
// that the browser and the server never disagree.
func (s *Server) validateRules(w http.ResponseWriter, r *http.Request) error {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRulesText+1))
	if err != nil {
		return bad(http.StatusRequestEntityTooLarge, "/", "规则太长")
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
		out.Errors = append(out.Errors, lineErr{e.Line, e.Msg})
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
		s.broadcast()
	}
	return nil
}

func (s *Server) saveGlobalRules(w http.ResponseWriter, r *http.Request) error {
	// The new global rules act on every folder, each with its own rules after them.
	impact := func(text string) (all []ruleHit, _ bool, _ error) {
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
	return s.saveRules(w, r, s.settingsPage, "rules", "/settings?ok=rules", `SELECT value FROM meta WHERE key='global_rules'`, `UPDATE meta SET value=? WHERE key='global_rules'`, impact)
}

func (s *Server) saveFolderRules(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	impact := func(text string) ([]ruleHit, bool, error) {
		hits, err := s.ruleHits(f.ID, f.Name, MetaString(s.db, "global_rules"), text)
		return hits, false, err
	}
	return s.saveRules(w, r, s.folderPage, "dlg-rules", "/folders/"+strconv.FormatInt(f.ID, 10)+"?ok=rules", `SELECT rules FROM folders WHERE id=?`, `UPDATE folders SET rules=? WHERE id=?`, impact, f.ID)
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
		err = bad(http.StatusNotFound, "/", "没有这个设备")
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
	}
	maps, err := queryAll(s.db, func(rows *sql.Rows, m *mapping) error {
		return rows.Scan(&m.FolderID, &m.Folder, &m.Path, &m.Adopt)
	}, `SELECT f.id, f.name, m.path, m.adopt FROM mappings m JOIN folders f ON f.id=m.folder_id WHERE m.device_id=? ORDER BY f.name`, d.ID)
	if err != nil {
		return err
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
		head.Version = strconv.Itoa(rep.req.AgentVersion)
		head.ConfigState = map[bool]string{true: "最新", false: "待更新"}[rep.req.ConfigVersion == cfgVersion]
		head.TooOld = rep.tooOld
		for _, fs := range rep.req.Folders {
			e, w := noticeKindCounts(fs.Errors)
			head.Errors, head.Warnings = head.Errors+e, head.Warnings+w
			for i := range maps {
				if maps[i].FolderID == fs.ID {
					m := &maps[i]
					m.Reported, m.State, m.Synced, m.PendingUp, m.PendingDown, m.Errors, m.Warnings = true, fs.State, fs.LastSyncedAt, fs.PendingUp, fs.PendingDown, e, w
				}
			}
		}
	}
	s.mu.Unlock()
	data := map[string]any{"Device": d, "Head": head, "Mappings": maps, "Free": free}
	if ds != nil && ds.dialog == "dlg-map-edit" {
		for _, m := range maps {
			if strconv.FormatInt(m.FolderID, 10) == r.PostForm.Get("folder") {
				data["EditName"] = m.Folder
			}
		}
	}
	s.show(w, r, ds, "device", "设备 "+d.Name, data)
	return nil
}

// cleanMapping validates a mapping path for a device of the given OS against
// the device's other mapping paths and returns its normalized form (design 3.2).
func cleanMapping(osName, p string, others []string) (string, error) {
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 }) {
		return "", errors.New("路径含控制字符")
	}
	norm, sep, fold := path.Clean, "/", func(q string) string { return q }
	if osName == "windows" {
		if len(p) < 3 || p[0]|0x20 < 'a' || p[0]|0x20 > 'z' || p[1] != ':' || (p[2] != '\\' && p[2] != '/') {
			return "", errors.New(`Windows 路径必须是盘符路径, 例如 D:\Docs`)
		}
		sep, fold = `\`, strings.ToLower
		norm = func(q string) string { // drive, then the rest cleaned like a Linux path ("..", ".", doubled separators)
			rest := path.Clean("/" + strings.ReplaceAll(q[2:], `\`, "/"))
			return q[:2] + strings.ReplaceAll(rest, "/", `\`)
		}
	} else if !strings.HasPrefix(p, "/") {
		return "", errors.New("Linux 路径必须以 / 开头")
	}
	contains := func(a, b string) bool { // a is b or a parent of b
		a, b = fold(strings.TrimSuffix(a, sep)), fold(b)
		return b == a || strings.HasPrefix(b, a+sep)
	}
	clean := norm(p)
	for _, o := range others {
		if contains(clean, o) || contains(o, clean) {
			return "", fmt.Errorf("路径与该设备的另一个映射 %s 重叠", o)
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
		return bad(http.StatusNotFound, back, "没有这个文件夹")
	}
	err = s.configChange(func(tx *sql.Tx) (bool, error) {
		if r.FormValue("action") == "delete" {
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
			return false, fieldErr(s.devicePage, http.StatusBadRequest, dialog, "path", err.Error())
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
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM mappings WHERE device_id=?`, `DELETE FROM devices WHERE id=?`} {
		if _, err := tx.Exec(q, d.ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.reports, d.ID)
	s.bumpLocked()
	s.mu.Unlock()
	slog.Info("device revoked", "device", d.Name)
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}
