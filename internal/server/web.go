package server

import (
	"bytes"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
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

var funcs = template.FuncMap{"size": formatSize}

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

func formatTime(unix int64) string {
	if unix == 0 {
		return "从未"
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}

func parseTemplates() map[string]*template.Template {
	pages := map[string]*template.Template{}
	for _, n := range []string{"login", "error", "overview", "device", "folder"} {
		pages[n] = template.Must(template.New("layout").Funcs(funcs).ParseFS(webFS, "web/layout.html", "web/"+n+".html"))
	}
	return pages
}

// userError is shown on the error page; any other error from a handler is a 500.
type userError struct {
	status    int
	msg, back string
}

func (e userError) Error() string { return e.msg }

type handler func(w http.ResponseWriter, r *http.Request) error

func (s *Server) webRoutes(mux *http.ServeMux) {
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, http.StatusOK, "login", "登录", map[string]any{})
	})
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		s.endSession(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	for pattern, h := range map[string]handler{
		"GET /{$}":                       s.overview,
		"POST /devices/add":              s.addDevice,
		"POST /folders/new":              s.newFolder,
		"POST /settings":                 s.saveSettings,
		"POST /rules":                    s.saveGlobalRules,
		"GET /devices/{id}":              s.devicePage,
		"POST /devices/{id}/mapping":     s.saveMapping,
		"POST /devices/{id}/revoke":      s.revoke,
		"GET /folders/{id}":              s.folderPage,
		"POST /folders/{id}/restore":     s.restore,
		"POST /folders/{id}/restore-dir": s.restoreDir,
		"POST /folders/{id}/delete":      s.deleteFolder,
		"POST /folders/{id}/rules":       s.saveFolderRules,
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
		switch err := h(w, r); {
		case err == nil:
		case errors.As(err, new(userError)):
			var ue userError
			errors.As(err, &ue)
			s.render(w, ue.status, "error", "出错了", map[string]any{"Error": ue.msg, "Back": ue.back})
		default:
			slog.Error("web", "path", r.URL.Path, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}
}

func bad(status int, back, msg string) error { return userError{status, msg, back} }

func (s *Server) render(w http.ResponseWriter, status int, name, title string, data map[string]any) {
	data["Title"], data["Auth"] = title, name != "login"
	var buf bytes.Buffer
	if err := s.tmpl[name].ExecuteTemplate(&buf, "layout", data); err != nil {
		slog.Error("render", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
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

func (s *Server) overview(w http.ResponseWriter, r *http.Request) error {
	return s.overviewPage(w, r, "", "")
}

func (s *Server) overviewPage(w http.ResponseWriter, r *http.Request, code, codeName string) error {
	type dev struct {
		ID                                       int64
		Name, OS, LastSeen, Version, ConfigState string
		Online                                   bool
		Status                                   []deviceStatus
	}
	folderNames := map[int64]string{}
	if rows, err := s.db.Query(`SELECT id, name FROM folders`); err == nil {
		for rows.Next() {
			var id int64
			var n string
			rows.Scan(&id, &n)
			folderNames[id] = n
		}
		rows.Close()
	}
	cfgVersion := metaInt(s.db, "config_version")
	devs, err := queryAll(s.db, func(rows *sql.Rows, d *dev) error {
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
				d.Version, d.ConfigState = fmt.Sprintf("%d (过旧, 请升级客户端)", rep.tooOld), "-"
			}
			for _, fs := range rep.req.Folders {
				d.Status = append(d.Status, deviceStatus{Folder: folderNames[fs.ID], State: fs.State, Synced: formatTime(fs.LastSyncedAt), PendingUp: fs.PendingUp, PendingDown: fs.PendingDown})
			}
			sort.Slice(d.Status, func(i, j int) bool { return d.Status[i].Folder < d.Status[j].Folder })
		}
		s.mu.Unlock()
		d.LastSeen = formatTime(seen)
		return nil
	}, `SELECT id, name, os, last_seen FROM devices ORDER BY name`)
	if err != nil {
		return err
	}
	type fol struct {
		ID, Files, Bytes int64
		Name             string
	}
	fols, err := queryAll(s.db, func(rows *sql.Rows, f *fol) error {
		return rows.Scan(&f.ID, &f.Name)
	}, `SELECT id, name FROM folders ORDER BY name`)
	if err != nil {
		return err
	}
	for i := range fols {
		db, err := s.folderDB(fols[i].ID)
		if err != nil {
			return err
		}
		if err := db.QueryRow(`SELECT count(*), coalesce(sum(size), 0) FROM files WHERE hash IS NOT NULL`).Scan(&fols[i].Files, &fols[i].Bytes); err != nil {
			return err
		}
	}
	var summary []folderSummary
	for _, f := range fols {
		notes, err := s.folderNotices(f.ID)
		if err != nil {
			return err
		}
		fs := folderSummary{ID: f.ID, Name: f.Name}
		for _, n := range notes {
			if n.Error {
				fs.Errors++
			} else {
				fs.Warnings++
			}
		}
		if len(notes) > 0 {
			summary = append(summary, fs)
		}
	}
	offline, err := s.offlineDevices()
	if err != nil {
		return err
	}
	s.render(w, http.StatusOK, "overview", "总览", map[string]any{"Devices": devs, "Folders": fols, "Summary": summary, "Offline": offline, "KeepDays": metaInt(s.db, "keep_days"), "Rules": MetaString(s.db, "global_rules"),
		"Code": code, "CodeName": codeName, "Server": "https://" + r.Host, "Fingerprint": s.Fingerprint})
	return nil
}

var deviceName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func (s *Server) addDevice(w http.ResponseWriter, r *http.Request) error {
	name := strings.TrimSpace(r.FormValue("name"))
	if !deviceName.MatchString(name) {
		return bad(http.StatusBadRequest, "/", "设备名称只能用字母, 数字, - 和 _, 至多 32 个字符 (冲突副本的文件名会包含它)")
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
		return bad(http.StatusConflict, "/", "已有同名设备")
	}
	code := group(randomBase32(12), 4)
	s.codes[sha(normalizeSecret(code))] = pendingCode{name, s.now().Add(codeLife)}
	s.mu.Unlock()
	return s.overviewPage(w, r, code, name)
}

func (s *Server) newFolder(w http.ResponseWriter, r *http.Request) error {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 64 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 }) {
		return bad(http.StatusBadRequest, "/", "文件夹名称不能为空, 至多 64 个字节, 不含控制字符")
	}
	var n int
	if s.db.QueryRow(`SELECT count(*) FROM folders WHERE name=?`, name).Scan(&n); n > 0 {
		return bad(http.StatusConflict, "/", "已有同名文件夹")
	}
	if _, err := s.createFolder(name); err != nil {
		return err
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) error {
	days, err := strconv.Atoi(r.FormValue("keep_days"))
	if err != nil || days < 1 || days > 3650 {
		return bad(http.StatusBadRequest, "/", "保留天数必须是 1 到 3650 的整数")
	}
	if err := SetMeta(s.db, "keep_days", days); err != nil {
		return err
	}
	s.requestGC() // clears the versions that the new value has expired at once
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}

// maxRulesText bounds a rule set: every device receives it with its configuration.
const maxRulesText = 64 << 10

// saveRules validates rule text (the trust boundary, design 3.2), stores it with write (whose last
// argument is the text and whose other arguments are args) and, when it changed, pushes the new configuration.
func (s *Server) saveRules(w http.ResponseWriter, r *http.Request, back, read, write string, args ...any) error {
	text := strings.TrimRight(strings.ReplaceAll(r.FormValue("rules"), "\r\n", "\n"), "\n")
	if len(text) > maxRulesText {
		return bad(http.StatusBadRequest, back, "规则太长")
	}
	if _, errs := ignore.Compile(strings.Split(text, "\n"), false); len(errs) > 0 {
		var msgs []string
		for _, e := range errs {
			msgs = append(msgs, fmt.Sprintf("第 %d 行: %s", e.Line, e.Msg))
		}
		return bad(http.StatusBadRequest, back, strings.Join(msgs, "; "))
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	if err := tx.QueryRow(read, args...).Scan(&old); err != nil {
		return err
	}
	changed := old != text
	if changed {
		if _, err := tx.Exec(write, append([]any{text}, args...)...); err != nil {
			return err
		}
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
	http.Redirect(w, r, back, http.StatusSeeOther)
	return nil
}

func (s *Server) saveGlobalRules(w http.ResponseWriter, r *http.Request) error {
	return s.saveRules(w, r, "/", `SELECT value FROM meta WHERE key='global_rules'`, `UPDATE meta SET value=? WHERE key='global_rules'`)
}

func (s *Server) saveFolderRules(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	return s.saveRules(w, r, "/folders/"+strconv.FormatInt(f.ID, 10), `SELECT rules FROM folders WHERE id=?`, `UPDATE folders SET rules=? WHERE id=?`, f.ID)
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

func (s *Server) devicePage(w http.ResponseWriter, r *http.Request) error {
	d, err := s.loadDevice(r)
	if err != nil {
		return err
	}
	type mapping struct {
		FolderID     int64
		Folder, Path string
		Adopt        bool
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
	s.render(w, http.StatusOK, "device", "设备 "+d.Name, map[string]any{"Device": d, "Mappings": maps, "Free": free, "LastSeen": formatTime(d.LastSeen)})
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
		norm = func(q string) string {
			q = strings.TrimRight(strings.ReplaceAll(q, "/", `\`), `\`)
			if len(q) == 2 {
				q += `\`
			}
			return q
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
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed := false
	if r.FormValue("action") == "delete" {
		res, err := tx.Exec(`DELETE FROM mappings WHERE device_id=? AND folder_id=?`, d.ID, folder)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		changed = n > 0
	} else {
		type mapping struct {
			folder int64
			path   string
			adopt  bool
		}
		all, err := queryAll(tx, func(rows *sql.Rows, m *mapping) error {
			return rows.Scan(&m.folder, &m.path, &m.adopt)
		}, `SELECT folder_id, path, adopt FROM mappings WHERE device_id=?`, d.ID)
		if err != nil {
			return err
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
			return bad(http.StatusBadRequest, back, err.Error())
		}
		adopt := r.FormValue("adopt") != ""
		if changed = old == nil || old.path != clean || old.adopt != adopt; changed {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO mappings VALUES(?,?,?,?)`, d.ID, folder, clean, adopt); err != nil {
				return err
			}
		}
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
	s.mu.Unlock()
	slog.Info("device revoked", "device", d.Name)
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}

type folderRow struct {
	ID          int64
	Name, Rules string
}

func (s *Server) loadFolder(r *http.Request) (f folderRow, err error) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	err = s.db.QueryRow(`SELECT id, name, rules FROM folders WHERE id=?`, id).Scan(&f.ID, &f.Name, &f.Rules)
	if errors.Is(err, sql.ErrNoRows) {
		err = bad(http.StatusNotFound, "/", "没有这个文件夹")
	}
	return f, err
}

type browseEntry struct {
	Name, Path, Mtime, Device string
	IsDir, Gone               bool
	Size                      int64
}

// browse lists the entries directly below dir ("" or "a/b"); directories are
// derived from the paths below them. Tombstones count only when deleted is set.
func (s *Server) browse(folder int64, dir string, deleted bool) ([]browseEntry, error) {
	db, err := s.folderDB(folder)
	if err != nil {
		return nil, err
	}
	prefix, q, args := "", `SELECT path, size, mtime_ns, device, hash IS NULL FROM files WHERE true`, []any{}
	if dir != "" {
		prefix = dir + "/"
		q += ` AND path >= ? AND path < ?` // "0" is the character after "/"
		args = append(args, prefix, dir+"0")
	}
	rows, err := db.Query(q+` ORDER BY path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []browseEntry
	seen := map[string]bool{}
	for rows.Next() {
		var p, dev string
		var size, mtime int64
		var gone bool
		if err := rows.Scan(&p, &size, &mtime, &dev, &gone); err != nil {
			return nil, err
		}
		if gone && !deleted {
			continue
		}
		rest := p[len(prefix):]
		if name, _, isDir := strings.Cut(rest, "/"); isDir {
			if !seen[name] {
				seen[name] = true
				out = append(out, browseEntry{Name: name, Path: prefix + name, IsDir: true})
			}
			continue
		}
		out = append(out, browseEntry{Name: rest, Path: p, Size: size, Mtime: formatTime(mtime / 1e9), Device: dev, Gone: gone})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].IsDir && !out[j].IsDir })
	return out, rows.Err()
}

func (s *Server) folderPage(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	dir := strings.Trim(r.FormValue("dir"), "/")
	deleted := r.FormValue("deleted") != ""
	entries, err := s.browse(f.ID, dir, deleted)
	if err != nil {
		return err
	}
	type crumb struct{ Name, Path string }
	var crumbs []crumb
	for i, p := range strings.Split(dir, "/") {
		if p != "" {
			crumbs = append(crumbs, crumb{p, strings.Join(strings.Split(dir, "/")[:i+1], "/")})
		}
	}
	notes, err := s.folderNotices(f.ID)
	if err != nil {
		return err
	}
	var maps int
	if err := s.db.QueryRow(`SELECT count(*) FROM mappings WHERE folder_id=?`, f.ID).Scan(&maps); err != nil {
		return err
	}
	data := map[string]any{"Notices": notes, "Folder": f, "Dir": dir, "Deleted": deleted, "Entries": entries, "Crumbs": crumbs, "CanDelete": maps == 0}
	_, err1 := strconv.Atoi(r.FormValue("restored"))
	if _, err2 := strconv.Atoi(r.FormValue("failed")); err1 == nil && err2 == nil {
		data["Restored"], data["Failed"] = r.FormValue("restored"), r.FormValue("failed") // numbers only: nothing else is echoed
	}
	if file := r.FormValue("file"); file != "" {
		if data["Versions"], err = s.versionList(f.ID, file); err != nil {
			return err
		}
		data["File"] = file
	}
	s.render(w, http.StatusOK, "folder", "文件夹 "+f.Name, data)
	return nil
}

type versionItem struct {
	ID               int64
	Time, Device     string
	Size             int64
	Current, Deleted bool
}

// versionList returns the current state of a path followed by its older versions, newest first.
func (s *Server) versionList(folder int64, file string) ([]versionItem, error) {
	db, err := s.folderDB(folder)
	if err != nil {
		return nil, err
	}
	var out []versionItem
	var cur fileRow
	if err := cur.scan(db.QueryRow(`SELECT `+fileCols+` FROM files WHERE path=?`, file)); err == nil {
		out = append(out, versionItem{Time: formatTime(cur.createdAt), Device: cur.device, Size: cur.size, Current: true, Deleted: cur.hash == nil})
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	old, err := queryAll(db, func(rows *sql.Rows, v *versionItem) error {
		var t int64
		err := rows.Scan(&v.ID, &t, &v.Device, &v.Size)
		v.Time = formatTime(t)
		return err
	}, `SELECT id, created_at, device, size FROM versions WHERE path=? ORDER BY replaced_at DESC, id DESC`, file)
	return append(out, old...), err
}

// restore commits an old version as the new current state, as device "web".
func (s *Server) restore(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	back := "/folders/" + strconv.FormatInt(f.ID, 10)
	vid, _ := strconv.ParseInt(r.FormValue("version"), 10, 64)
	db, err := s.folderDB(f.ID)
	if err != nil {
		return err
	}
	var v fileRow
	err = v.scan(db.QueryRow(`SELECT `+fileCols+` FROM versions WHERE id=?`, vid))
	if errors.Is(err, sql.ErrNoRows) {
		return bad(http.StatusNotFound, back, "没有这个版本")
	} else if err != nil {
		return err
	}
	var base int64 // the current seq of the path, or 0 when it has never existed
	db.QueryRow(`SELECT seq FROM files WHERE path=?`, v.path).Scan(&base)
	e := v.entry()
	res, err := s.commit(f.ID, "web", []proto.Op{{Path: v.path, BaseSeq: base, Hash: e.Hash, Blocks: e.Blocks, Size: v.size, MtimeNs: v.mtimeNs, Exec: v.exec}})
	if err != nil {
		return err
	}
	if res[0].Status != proto.StatusOK {
		return bad(http.StatusConflict, back, "无法恢复: "+res[0].Status+" "+res[0].Reason)
	}
	http.Redirect(w, r, back+"?dir="+url.QueryEscape(r.FormValue("dir"))+"&file="+url.QueryEscape(v.path), http.StatusSeeOther)
	return nil
}

// ParseTime reads a time given in UTC, or with an explicit zone, as the Web UI and export take it.
func ParseTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, strings.TrimSpace(s), time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("time %q: use YYYY-MM-DD HH:MM[:SS] in UTC", s)
}

// stateAt calls visit for every file the folder held at unix time at (0: now) below dir (design 7): the
// current rows created by then and the versions whose life spans it. Deleted paths have no row.
func stateAt(db *sql.DB, dir string, at int64, visit func(*fileRow) error) error {
	if at == 0 {
		at = math.MaxInt64
	}
	cond, args := "true", []any{}
	if dir != "" {
		cond, args = "path >= ? AND path < ?", []any{dir + "/", dir + "0"} // "0" is the character after "/"
	}
	rows, err := db.Query(`SELECT `+fileCols+` FROM files WHERE hash IS NOT NULL AND created_at <= ? AND `+cond+
		` UNION ALL SELECT `+fileCols+` FROM versions WHERE created_at <= ? AND replaced_at > ? AND `+cond,
		append(append([]any{at}, args...), append([]any{at, at}, args...)...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f fileRow
		if err := f.scan(rows); err != nil {
			return err
		}
		if err := visit(&f); err != nil {
			return err
		}
	}
	return rows.Err()
}

// restoreDir brings back what a directory held at a point in time, as device "web": paths that now hold
// other content, or were deleted, get the old content; paths created since stay (nothing is deleted).
func (s *Server) restoreDir(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	dir := strings.Trim(r.FormValue("dir"), "/")
	back := "/folders/" + strconv.FormatInt(f.ID, 10)
	at, err := ParseTime(r.FormValue("time"))
	if err != nil {
		return bad(http.StatusBadRequest, back, err.Error())
	}
	db, err := s.folderDB(f.ID)
	if err != nil {
		return err
	}
	var ops []proto.Op
	restored, failed := 0, 0
	flush := func() error {
		res, err := s.commit(f.ID, "web", ops)
		for _, x := range res {
			if x.Status == proto.StatusOK {
				restored++
			} else {
				failed++
			}
		}
		ops = ops[:0]
		return err
	}
	err = stateAt(db, dir, at.Unix(), func(v *fileRow) error {
		var cur fileRow
		err := cur.scan(db.QueryRow(`SELECT `+fileCols+` FROM files WHERE path=?`, v.path))
		if err == nil && bytes.Equal(cur.hash, v.hash) {
			return nil // already as it was
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		e := v.entry()
		ops = append(ops, proto.Op{Path: v.path, BaseSeq: cur.seq, Hash: e.Hash, Blocks: e.Blocks, Size: v.size, MtimeNs: v.mtimeNs, Exec: v.exec})
		if len(ops) == proto.MaxCommitOps {
			return flush()
		}
		return nil
	})
	if err == nil && len(ops) > 0 {
		err = flush()
	}
	if err != nil {
		return err
	}
	slog.Info("restore dir", "folder", f.Name, "dir", dir, "at", at, "restored", restored, "failed", failed)
	http.Redirect(w, r, fmt.Sprintf("%s?dir=%s&restored=%d&failed=%d", back, url.QueryEscape(dir), restored, failed), http.StatusSeeOther)
	return nil
}

// deleteFolder removes a folder that no device maps, with its database. Its blocks go at the next GC.
func (s *Server) deleteFolder(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM mappings WHERE folder_id=?`, f.ID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return bad(http.StatusConflict, "/folders/"+strconv.FormatInt(f.ID, 10), "文件夹仍映射到设备, 先在设备页删除映射")
	}
	if _, err := tx.Exec(`DELETE FROM folders WHERE id=?`, f.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.fmu.Lock()
	if db := s.fdbs[f.ID]; db != nil {
		db.Close()
		delete(s.fdbs, f.ID)
	}
	s.fmu.Unlock()
	path := FolderDBPath(s.varDir, f.ID)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	slog.Info("folder deleted", "folder", f.Name)
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}
