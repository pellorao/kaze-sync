package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
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
		err = bad(http.StatusNotFound, "/", "err.noFolder")
	}
	return f, err
}

// entryTag is a label on a row of the file list; with Href it is a link (a held tag opens the held dialog).
type entryTag struct{ Class, Text, Title, Href string }

// dirTotal is what a directory holds in live files, itself and below.
type dirTotal struct{ Files, Bytes int64 }

type browseEntry struct {
	Name, Path string
	IsDir      bool
	Gone       bool  // a deleted file; for a directory: nothing but deleted files below it
	Time       int64 // modified; for a deleted file: when it was deleted; for a directory: the latest modification below it
	Size       int64 // for a directory: the total size of the files below it
	Unsynced   bool  // reported by a device but not on the server (a symbolic link, a refused name)
	Tags       []entryTag
	Note       *notice // an unsynced entry's report
	WC         wcInfo
	HeldN      int    // changes devices hold below (or at) this entry
	HeldDev    string // the devices that hold them
}

// browse lists the entries directly below dir ("" or "a/b"); directories are
// derived from the paths below them, with the total size and latest modification time of the files below each
// (design 4.5; summed from the rows the scan reads anyway). Tombstones count only when deleted is set, and are
// never part of a total. The third result is the number and total size of the live files in dir and below
// (the page head, design 4.5), summed from the same rows.
func (s *Server) browse(folder int64, dir string, deleted bool) ([]browseEntry, dirTotal, error) {
	var tot dirTotal
	db, err := s.folderDB(folder)
	if err != nil {
		return nil, tot, err
	}
	prefix, q, args := "", `SELECT path, size, mtime_ns, created_at, hash IS NULL FROM files WHERE true`, []any{}
	if dir != "" {
		prefix = dir + "/"
		q += ` AND path >= ? AND path < ?` // "0" is the character after "/"
		args = append(args, prefix, dir+"0")
	}
	rows, err := db.Query(q+` ORDER BY path`, args...)
	if err != nil {
		return nil, tot, err
	}
	defer rows.Close()
	var out []browseEntry
	seen := map[string]int{} // directory name -> index in out
	for rows.Next() {
		var p string
		var size, mtime, created int64
		var gone bool
		if err := rows.Scan(&p, &size, &mtime, &created, &gone); err != nil {
			return nil, tot, err
		}
		if !gone {
			tot.Files++
			tot.Bytes += size
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
				d := &out[i]
				d.Gone, d.Size = false, d.Size+size
				d.Time = max(d.Time, mtime/1e9)
			}
			continue
		}
		e := browseEntry{Name: rest, Path: p, Size: size, Time: mtime / 1e9, Gone: gone}
		if gone {
			e.Time = created
		}
		out = append(out, e)
	}
	return out, tot, rows.Err()
}

// noticeIn tells whether a notice belongs to dir (and below) for the counts of the page head: a notice with a path
// by that path, a held notice when one of its held changes is there; one without a place (a folder-level error,
// the reminder of a new folder) only to the top level.
func noticeIn(n notice, dir string) bool {
	if dir == "" {
		return true
	}
	in := func(p string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }
	if n.Kind == kindHeld {
		return slices.ContainsFunc(n.Items, in)
	}
	return in(n.Path)
}

// kindLabel is the short name of a kind of notice ("in use", "read/write error"...).
func kindLabel(t translator, kind string) string { return t.T("kind." + kind) }

// unsyncedLabel says why an entry is not on the server.
func unsyncedLabel(t translator, n notice) string {
	switch {
	case strings.HasPrefix(n.Message, proto.LinkNotCreated):
		return t.T("unsynced.linkNotCreated")
	case strings.Contains(n.Message, "symbolic link"):
		return t.T("unsynced.symlink")
	case strings.Contains(n.Message, "UTF-8"):
		return t.T("unsynced.utf8")
	}
	return kindLabel(t, n.Kind)
}

// decorate adds what the devices report about the entries to the listing: tags on the paths that have notices,
// and a row for each entry of dir that a device reports as
// skipped or refused and the server does not have. The data is in memory: no database query.
func decorate(t translator, entries []browseEntry, dir string, notes []notice) []browseEntry {
	byPath := map[string]int{}
	for i, e := range entries {
		byPath[e.Path] = i
	}
	for _, n := range notes {
		if n.Path == "" {
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
				entries[j].Tags = append(entries[j].Tags, entryTag{Class: class, Text: kindLabel(t, n.Kind) + " · " + n.Device, Title: n.Message})
			}
			continue
		}
		if parent != dir || (n.Kind != proto.KindSkipped && n.Kind != proto.KindRejected) {
			continue
		}
		note := n
		byPath[n.Path] = len(entries)
		entries = append(entries, browseEntry{Name: path.Base(n.Path), Path: n.Path, Unsynced: true, Note: &note,
			Tags: []entryTag{{Class: "warn", Text: t.T("tag.unsynced", "why", unsyncedLabel(t, n), "device", n.Device), Title: n.Message}}})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
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
		s.render(w, r, http.StatusOK, "live", "", map[string]any{})
		return nil
	}
	dir := strings.Trim(r.FormValue("dir"), "/")
	deleted := r.FormValue("deleted") != ""
	db, err := s.folderDB(f.ID)
	if err != nil {
		return err
	}
	seq := metaInt(db, "next_seq") - 1 // before the listing is read: a change made during the read shows as an update
	entries, tot, err := s.browse(f.ID, dir, deleted)
	if err != nil {
		return err
	}
	notes, err := s.folderNotices(f.ID)
	if err != nil {
		return err
	}
	t := trOf(r)
	entries = decorate(t, entries, dir, notes)
	wcRules, err := folderWC(s.db, f.ID)
	if err != nil {
		return err
	}
	s.decorateWC(t, entries, dir, f.ID, wcRules)
	owners := s.activeOwners(f.ID, wcRules)
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
		if !noticeIn(n, dir) {
			continue
		}
		if n.Error {
			errs++
		} else {
			warns++
		}
	}
	rules, global := f.Rules, MetaString(s.db, "global_rules")
	if ds != nil && ds.dialog == "dlg-rules" {
		rules = r.PostForm.Get("rules")
	}
	data := map[string]any{"Folder": f, "Dir": dir, "Deleted": deleted, "Entries": entries, "Crumbs": crumbs, "Devs": devs, "Mapped": len(devs),
		"State": state, "Pending": pending, "Errors": errs, "Warnings": warns, "Files": tot.Files, "Bytes": tot.Bytes, "Seq": seq,
		"Rules": rules, "SavedRules": f.Rules, "RuleCount": countRules(f.Rules), "Global": global, "GlobalCount": countRules(global), "KeepDays": metaInt(s.db, "keep_days"),
		"Scope": dir, "LockPaths": lockPaths(wcRules, owners), "HeldSpans": s.heldSpans(f.ID), "WCHere": wcFor(wcRules, dir)}
	s.wcLine(t, data, owners, wcRules, dir)
	if wp, ok := r.URL.Query()["wc"]; ok && ds == nil { // a link of the notifications opens the dialog (design 2.8.11)
		data["Open"] = "dlg-wc"
		s.wcOpen(data, f.ID, wcRules, strings.Trim(wp[0], "/"))
	} else if ds != nil && ds.dialog == "dlg-wc" {
		s.wcOpen(data, f.ID, wcRules, strings.Trim(r.PostForm.Get("path"), "/"))
	}
	if hd, err := strconv.ParseInt(r.FormValue("held"), 10, 64); err == nil && ds == nil {
		if data["HeldView"], err = s.heldView(f.ID, hd); err != nil {
			return err
		}
		data["Open"] = "dlg-held"
	}
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
		data["File"] = file
		data["Open"] = "dlg-versions"
	}
	s.show(w, r, ds, "folder", t.T("title.folder", "name", f.Name), data)
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
		return bad(http.StatusNotFound, back, "err.noVersion")
	case errors.As(err, &re):
		return bad(http.StatusConflict, back, "err.restoreFailed", "reason", re.msg)
	case err != nil:
		return err
	}
	if fromList {
		http.Redirect(w, r, back+"?dir="+url.QueryEscape(dir)+"&ok=restored"+keepDeleted(r), http.StatusSeeOther)
	} else {
		http.Redirect(w, r, back+"?dir="+url.QueryEscape(dir)+"&file="+url.QueryEscape(p)+"&rv="+strconv.FormatInt(created, 10)+keepDeleted(r), http.StatusSeeOther)
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
		return fieldErr(s.folderPage, http.StatusBadRequest, "dlg-restore", "time", "err.timeFormat")
	}
	restored, failed, err := s.restoreDirAt(f.ID, p, at)
	if err != nil {
		return err
	}
	slog.Info("restore", "folder", f.Name, "path", p, "at", at, "restored", restored, "failed", failed)
	http.Redirect(w, r, fmt.Sprintf("%s?dir=%s&restored=%d&failed=%d%s", back, url.QueryEscape(dir), restored, failed, keepDeleted(r)), http.StatusSeeOther)
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
		return bad(http.StatusBadRequest, "", "err.timeInvalid")
	}
	n, match, items, note, none, err := s.restorePreview(r.Context(), f.ID, p, at, r.FormValue("filter"))
	if errors.Is(err, context.Canceled) {
		return nil // the browser dropped the request: the line moved on
	}
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "Accept-Language, Cookie")
	return s.page(requestLang(r), "folder").ExecuteTemplate(w, "preview", map[string]any{"N": n, "Match": match, "Items": items, "Filtered": globFilter(r.FormValue("filter")) != nil,
		"First": note.At, "Import": note.Import, "None": none})
}

// restoreTicksPage answers the restore dialog's time axis: its marks and ends (restoreTicks), as JSON.
func (s *Server) restoreTicksPage(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	t, err := s.restoreTicks(f.ID, strings.Trim(r.FormValue("path"), "/"))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(t)
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
		return bad(http.StatusConflict, "/folders/"+strconv.FormatInt(f.ID, 10), "err.folderMapped")
	}
	if _, err := tx.Exec(`DELETE FROM writectl WHERE folder_id=?`, f.ID); err != nil {
		return err
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
	if err := removeBackups(s.varDir, folderBackupName(f.ID)); err != nil {
		slog.Warn("cannot remove the backups of the deleted folder", "folder", f.Name, "err", err)
	}
	slog.Info("folder deleted", "folder", f.Name)
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}

// lockPaths is the exclusive settings and the single-writer scopes with a live owner, as the JSON the restore dialog
// reads to warn about them in its range (design 4.5).
func lockPaths(rules []lockRow, owners map[string]ownerNow) string {
	type l struct {
		P, K, D string
		S       bool `json:",omitempty"` // a single-writer scope whose owner is still inside the time window
	}
	out := []l{}
	for _, x := range rules {
		if x.Mode == proto.ModeExclusive {
			out = append(out, l{x.Path, x.key, x.Device, false})
		}
	}
	for k, o := range owners {
		out = append(out, l{o.Path, k, o.Device, true})
	}
	slices.SortFunc(out, func(a, b l) int { return strings.Compare(a.K+"\x00"+a.D, b.K+"\x00"+b.D) })
	b, _ := json.Marshal(out)
	return string(b)
}

// wcOpen sets the path of the write-control dialog the page opens by itself. A file is never unrestricted, so inside a
// free directory it inherits "single" (the rows get this in decorateWC) and the dialog hides the "free" option.
func (s *Server) wcOpen(data map[string]any, folder int64, rules []lockRow, p string) {
	w := wcFor(rules, p)
	if s.isFile(folder, p) {
		data["WCFile"] = true
		if w.InhMode == proto.ModeFree {
			w.InhMode = proto.ModeSingle
		}
	}
	data["WCOpen"] = w
}

// isFile tells whether p is a live file of the folder (a directory has no row of its own).
func (s *Server) isFile(folder int64, p string) bool {
	db, err := s.folderDB(folder)
	if err != nil || p == "" {
		return false
	}
	var one int
	return db.QueryRow(`SELECT 1 FROM files WHERE path=? AND hash IS NOT NULL`, p).Scan(&one) == nil
}
