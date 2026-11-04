package server

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"kaze-sync/internal/proto"
)

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

// entryTag is a label on a row of the file list.
type entryTag struct{ Class, Text, Title string }

type browseEntry struct {
	Name, Path string
	IsDir      bool
	Gone       bool  // a deleted file; for a directory: nothing but deleted files below it
	Time       int64 // modified; for a deleted file: when it was deleted
	Size       int64
	Conflict   bool   // a conflict copy
	Orig       string // the path of the file it is a copy of
	Unsynced   bool   // reported by a device but not on the server (a symbolic link, a refused name)
	Tags       []entryTag
	Note       *notice // an unsynced entry's report
}

// conflictName splits "dir/report.conflict-laptop-20261101-153012.docx" into its original and extension parts.
var conflictName = regexp.MustCompile(`^(.*)\.conflict-[^/]+-\d{8}-\d{6}(\.[^/]*)?$`)

// conflictOriginal returns the path of the file a conflict copy was made from.
func conflictOriginal(p string) string {
	if m := conflictName.FindStringSubmatch(p); m != nil {
		return m[1] + m[2]
	}
	return ""
}

// browse lists the entries directly below dir ("" or "a/b"); directories are
// derived from the paths below them. Tombstones count only when deleted is set.
func (s *Server) browse(folder int64, dir string, deleted bool) ([]browseEntry, error) {
	db, err := s.folderDB(folder)
	if err != nil {
		return nil, err
	}
	prefix, q, args := "", `SELECT path, size, mtime_ns, created_at, conflict, hash IS NULL FROM files WHERE true`, []any{}
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
	seen := map[string]int{} // directory name -> index in out
	for rows.Next() {
		var p string
		var size, mtime, created int64
		var conflict, gone bool
		if err := rows.Scan(&p, &size, &mtime, &created, &conflict, &gone); err != nil {
			return nil, err
		}
		if gone && !deleted {
			continue
		}
		rest := p[len(prefix):]
		if name, _, isDir := strings.Cut(rest, "/"); isDir {
			i, ok := seen[name]
			if !ok {
				i = len(out)
				seen[name] = i
				out = append(out, browseEntry{Name: name, Path: prefix + name, IsDir: true, Gone: true})
			}
			if !gone {
				out[i].Gone = false
			}
			continue
		}
		e := browseEntry{Name: rest, Path: p, Size: size, Time: mtime / 1e9, Gone: gone, Conflict: conflict && !gone}
		if gone {
			e.Time = created
		}
		if e.Conflict {
			e.Orig = conflictOriginal(p)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

var kindLabel = map[string]string{
	proto.KindInUse: "占用中", proto.KindIO: "读写错误", proto.KindRejected: "被拒绝", proto.KindSkipped: "已跳过", proto.KindFolder: "文件夹错误",
}

// unsyncedLabel says why an entry is not on the server.
func unsyncedLabel(n notice) string {
	switch {
	case strings.Contains(n.Message, "symbolic link"):
		return "符号链接"
	case strings.Contains(n.Message, "UTF-8"):
		return "非 UTF-8 文件名"
	}
	return kindLabel[n.Kind]
}

// decorate adds what the devices report about the entries to the listing: tags on the paths that have notices,
// "has a conflict" on the originals of conflict copies, and a row for each entry of dir that a device reports as
// skipped or refused and the server does not have. The data is in memory: no database query.
func decorate(entries []browseEntry, dir string, notes []notice) []browseEntry {
	byPath := map[string]int{}
	for i, e := range entries {
		byPath[e.Path] = i
	}
	for i, e := range entries {
		if e.Conflict {
			entries[i].Tags = append(entries[i].Tags, entryTag{"err", "冲突副本", ""})
			if j, ok := byPath[e.Orig]; ok && !hasTag(entries[j].Tags, "有冲突") {
				entries[j].Tags = append(entries[j].Tags, entryTag{"err", "有冲突", ""})
			}
		}
	}
	for _, n := range notes {
		if n.Path == "" || n.Kind == kindConflict {
			continue
		}
		parent := ""
		if i := strings.LastIndexByte(n.Path, '/'); i >= 0 {
			parent = n.Path[:i]
		}
		if j, ok := byPath[n.Path]; ok {
			if !entries[j].IsDir {
				class := "warn"
				if n.Error {
					class = "err"
				}
				entries[j].Tags = append(entries[j].Tags, entryTag{class, kindLabel[n.Kind] + " · " + n.Device, n.Message})
			}
			continue
		}
		if parent != dir || (n.Kind != proto.KindSkipped && n.Kind != proto.KindRejected) {
			continue
		}
		note := n
		byPath[n.Path] = len(entries)
		entries = append(entries, browseEntry{Name: path.Base(n.Path), Path: n.Path, Unsynced: true, Note: &note,
			Tags: []entryTag{{"warn", "未同步: " + unsyncedLabel(n) + " · " + n.Device, n.Message}}})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func hasTag(tags []entryTag, text string) bool {
	for _, t := range tags {
		if t.Text == text {
			return true
		}
	}
	return false
}

func (s *Server) folderPageH(w http.ResponseWriter, r *http.Request) error {
	return s.folderPage(w, r, nil)
}

func (s *Server) folderPage(w http.ResponseWriter, r *http.Request, ds *dialogState) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	if r.Header.Get("X-Live") != "" { // a live update wants the bell, not the listing
		s.render(w, http.StatusOK, "live", "", map[string]any{})
		return nil
	}
	dir := strings.Trim(r.FormValue("dir"), "/")
	deleted := r.FormValue("deleted") != ""
	db, err := s.folderDB(f.ID)
	if err != nil {
		return err
	}
	seq := metaInt(db, "next_seq") - 1 // before the listing is read: a change made during the read shows as an update
	entries, err := s.browse(f.ID, dir, deleted)
	if err != nil {
		return err
	}
	notes, err := s.folderNotices(f.ID)
	if err != nil {
		return err
	}
	entries = decorate(entries, dir, notes)
	type crumb struct{ Name, Path string }
	var crumbs []crumb
	for i, p := range strings.Split(dir, "/") {
		if p != "" {
			crumbs = append(crumbs, crumb{p, strings.Join(strings.Split(dir, "/")[:i+1], "/")})
		}
	}
	mapped, err := s.mappedDevices()
	if err != nil {
		return err
	}
	devs := mapped[f.ID]
	state, pending := s.folderStatus(f.ID, devs)
	var errs, warns int
	for _, n := range notes {
		if n.Error {
			errs++
		} else {
			warns++
		}
	}
	files, bytes, err := s.folderStats(f.ID)
	if err != nil {
		return err
	}
	rules, global := f.Rules, MetaString(s.db, "global_rules")
	if ds != nil && ds.dialog == "dlg-rules" {
		rules = r.PostForm.Get("rules")
	}
	data := map[string]any{"Folder": f, "Dir": dir, "Deleted": deleted, "Entries": entries, "Crumbs": crumbs, "Devs": devs, "Mapped": len(devs),
		"State": state, "Pending": pending, "Errors": errs, "Warnings": warns, "Files": files, "Bytes": bytes, "Seq": seq,
		"Rules": rules, "SavedRules": f.Rules, "RuleCount": countRules(f.Rules), "Global": global, "GlobalCount": countRules(global), "KeepDays": metaInt(s.db, "keep_days"),
		"Scope": dir}
	if ds != nil && ds.dialog == "dlg-restore" {
		data["Scope"] = r.PostForm.Get("path")
	}
	if rv, err := strconv.ParseInt(r.FormValue("rv"), 10, 64); err == nil {
		data["Restored"] = rv // when the version that has just been restored was written: only the number is echoed
	}
	if file := r.FormValue("file"); file != "" {
		if data["Versions"], err = s.versionList(f.ID, file); err != nil {
			return err
		}
		data["File"], data["FileOrig"] = file, conflictOriginal(file)
		data["Open"] = "dlg-versions"
	}
	s.show(w, r, ds, "folder", "文件夹 "+f.Name, data)
	return nil
}

// countRules counts the lines of a rule text that are rules.
func countRules(text string) int {
	n := 0
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" && l[0] != '#' {
			n++
		}
	}
	return n
}

// restore commits an old version as the new current state (restoreVersion). With a path instead of a version it
// takes the last content of that path (the deleted file's menu entry).
func (s *Server) restore(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	back := "/folders/" + strconv.FormatInt(f.ID, 10)
	dir := strings.Trim(r.FormValue("dir"), "/")
	db, err := s.folderDB(f.ID)
	if err != nil {
		return err
	}
	vid, _ := strconv.ParseInt(r.FormValue("version"), 10, 64)
	fromList := vid == 0
	if fromList {
		err = db.QueryRow(`SELECT id FROM versions WHERE path=? ORDER BY replaced_at DESC, id DESC LIMIT 1`, strings.Trim(r.FormValue("path"), "/")).Scan(&vid)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	var created int64
	db.QueryRow(`SELECT created_at FROM versions WHERE id=?`, vid).Scan(&created)
	p, err := s.restoreVersion(f.ID, vid)
	var re restoreError
	switch {
	case errors.Is(err, errNoVersion):
		return bad(http.StatusNotFound, back, "没有这个版本")
	case errors.As(err, &re):
		return bad(http.StatusConflict, back, "无法恢复: "+re.msg)
	case err != nil:
		return err
	}
	if fromList {
		http.Redirect(w, r, back+"?dir="+url.QueryEscape(dir)+"&ok=restored", http.StatusSeeOther)
	} else {
		http.Redirect(w, r, back+"?dir="+url.QueryEscape(dir)+"&file="+url.QueryEscape(p)+"&rv="+strconv.FormatInt(created, 10), http.StatusSeeOther)
	}
	return nil
}

// restoreDir brings back what a directory or file held at a point in time (restoreDirAt).
func (s *Server) restoreDir(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	p, dir := strings.Trim(r.FormValue("path"), "/"), strings.Trim(r.FormValue("dir"), "/")
	back := "/folders/" + strconv.FormatInt(f.ID, 10)
	at, err := ParseTime(r.FormValue("time"))
	if err != nil {
		return fieldErr(s.folderPage, http.StatusBadRequest, "dlg-restore", "time", "时间格式: 2026-11-03 22:48:27")
	}
	restored, failed, err := s.restoreDirAt(f.ID, p, at)
	if err != nil {
		return err
	}
	slog.Info("restore", "folder", f.Name, "path", p, "at", at, "restored", restored, "failed", failed)
	http.Redirect(w, r, fmt.Sprintf("%s?dir=%s&restored=%d&failed=%d", back, url.QueryEscape(dir), restored, failed), http.StatusSeeOther)
	return nil
}

// restorePreviewPage answers the restore dialog: what restoreDir would restore, as an HTML fragment.
func (s *Server) restorePreviewPage(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	p := strings.Trim(r.FormValue("path"), "/")
	at, err := ParseTime(r.FormValue("time"))
	if err != nil {
		return bad(http.StatusBadRequest, "", "时间格式无效")
	}
	n, items, note, none, err := s.restorePreview(f.ID, p, at)
	if err != nil {
		return err
	}
	for i := range items {
		if p != "" && strings.HasPrefix(items[i].Path, p+"/") {
			items[i].Path = items[i].Path[len(p)+1:]
		} else if p != "" {
			items[i].Path = path.Base(items[i].Path)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return s.tmpl["folder"].ExecuteTemplate(w, "preview", map[string]any{"N": n, "Items": items, "More": n - len(items), "First": note.At, "Import": note.Import, "None": none})
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
	s.mu.Lock()
	delete(s.stats, f.ID)
	s.bumpLocked()
	s.mu.Unlock()
	p := FolderDBPath(s.varDir, f.ID)
	for _, suffix := range []string{"", "-wal", "-shm"} { // the row is gone, so the folder is: a file that cannot be removed is only logged, the others still go
		if err := os.Remove(p + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("cannot remove a file of the deleted folder", "folder", f.Name, "file", p+suffix, "err", err)
		}
	}
	slog.Info("folder deleted", "folder", f.Name)
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}
