package server

import (
	"database/sql"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// offlineAfter is how long a device may be silent before it raises a notice (design 4.3).
const offlineAfter = 7 * 24 * time.Hour

// notice is one line of a folder's notifications. Notices are computed from
// the current state on every request; none is stored (design 4.3).
type notice struct {
	Error                 bool   // an error needs action to go away; otherwise it is a reminder
	Kind                  string // a proto kind, or kindConflict
	Device, Path, Message string
	Since                 int64
	Owner                 string // the device that holds the exclusive behind a refusal
	Link                  string
	ReportedAt            int64 // when the device last reported, if it is offline; else 0
}

// kindConflict is the kind of the notice of a conflict copy on the server.
const kindConflict = "conflict"

// adviceOf is what to do about a kind of notice, in the language of the page ("" when there is nothing to say).
func adviceOf(t translator, kind string) string {
	switch kind {
	case proto.KindRejected, proto.KindIO, proto.KindFolder, proto.KindInUse, proto.KindSkipped, kindConflict:
		return t.T("advice." + kind)
	}
	return ""
}

// browseLink points at the directory holding p in the folder's file browser; the page highlights the row.
func browseLink(folder int64, p string) string {
	dir := ""
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		dir = p[:i]
	}
	return "/folders/" + strconv.FormatInt(folder, 10) + "?dir=" + url.QueryEscape(dir) + "#" + url.PathEscape(p)
}

// folderNotices lists the notices of a folder: what the mapped devices report,
// and the conflict copies on the server.
func (s *Server) folderNotices(folder int64) ([]notice, error) {
	type dev struct {
		id   int64
		name string
	}
	devs, err := queryAll(s.db, func(rows *sql.Rows, d *dev) error { return rows.Scan(&d.id, &d.name) },
		`SELECT d.id, d.name FROM mappings m JOIN devices d ON d.id=m.device_id WHERE m.folder_id=? ORDER BY d.name`, folder)
	if err != nil {
		return nil, err
	}
	locks, err := folderLocks(s.db, folder)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []notice
	s.mu.Lock()
	for _, d := range devs {
		rep := s.reports[d.id]
		if rep == nil {
			continue
		}
		var stale int64
		if !rep.online(now) {
			stale = rep.at.Unix()
		}
		for _, fs := range rep.req.Folders {
			if fs.ID != folder {
				continue
			}
			for _, e := range fs.Errors {
				n := notice{Kind: e.Kind, Device: d.name, Path: e.Path, Message: e.Message, Since: e.Since, ReportedAt: stale,
					Error: e.Kind != proto.KindInUse && e.Kind != proto.KindSkipped}
				if e.Kind == proto.KindRejected {
					if e.Message == proto.ReasonLocked { // the refusal of one path
						if l := lockedBy(locks, d.id, pp.Key(e.Path)); l != nil {
							n.Owner = l.Device
						}
					}
				}
				if e.Kind == proto.KindFolder {
					n.Link = "/devices/" + strconv.FormatInt(d.id, 10)
				} else {
					n.Link = browseLink(folder, e.Path)
				}
				out = append(out, n)
			}
		}
	}
	s.mu.Unlock()
	db, err := s.folderDB(folder)
	if err != nil {
		return nil, err
	}
	conflicts, err := queryAll(db, func(rows *sql.Rows, n *notice) error {
		return rows.Scan(&n.Path, &n.Device, &n.Since)
	}, `SELECT path, device, created_at FROM files WHERE conflict AND hash IS NOT NULL ORDER BY path`)
	if err != nil {
		return nil, err
	}
	for _, n := range conflicts {
		n.Error, n.Kind, n.Link = true, kindConflict, browseLink(folder, n.Path) // the page says what a conflict copy is
		out = append(out, n)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Error && !out[j].Error })
	return out, nil
}

// offlineDevices lists the devices that have not connected for offlineAfter.
func (s *Server) offlineDevices() ([]offlineDevice, error) {
	type row struct {
		id            int64
		name          string
		seen, created int64
	}
	rows, err := queryAll(s.db, func(rows *sql.Rows, r *row) error { return rows.Scan(&r.id, &r.name, &r.seen, &r.created) },
		`SELECT id, name, last_seen, created_at FROM devices ORDER BY name`)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []offlineDevice
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		last := time.Unix(max(r.seen, r.created), 0)
		if rep := s.reports[r.id]; rep != nil && rep.at.After(last) {
			last = rep.at
		}
		if now.Sub(last) >= offlineAfter {
			out = append(out, offlineDevice{ID: r.id, Name: r.name, Days: int(now.Sub(last) / (24 * time.Hour)), Last: last.Unix()})
		}
	}
	return out, nil
}

type offlineDevice struct {
	ID   int64
	Name string
	Days int
	Last int64
}

// noticeRow is a line of the notification popover: one notice, or the fold of the many notices of one kind that a
// device reports (the popover would otherwise be filled by one device's 300 symbolic links).
type noticeRow struct {
	notice
	Fold     bool
	FoldKind string   // what the folded notices are (a kind, or linkKind)
	Sub      []notice // what is folded
}

// foldAbove is the number of notices of one device and kind above which the popover folds them.
const foldAbove = 5

// linkKind is the fold key of the notices of symbolic links a device cannot create (design 6.7): one line per device
// however few there are, and the line does not change unless the set of links does.
const linkKind = "link-not-created"

func isLinkNotice(n notice) bool {
	return n.Kind == proto.KindSkipped && strings.HasPrefix(n.Message, proto.LinkNotCreated)
}

// noticeGroup is one folder's part of the popover.
type noticeGroup struct {
	ID               int64
	Name             string
	Errors, Warnings int
	Rows             []noticeRow
}

// notifView is everything the popover and the bell show.
type notifView struct {
	Errors, Warnings int
	Groups           []noticeGroup
	Offline          []offlineDevice
}

func (v notifView) Total() int { return v.Errors + v.Warnings }

// foldNotices turns notices into popover rows, errors first.
func foldNotices(notes []notice) []noticeRow {
	type key struct{ device, kind string }
	kindOf := func(n notice) string {
		if isLinkNotice(n) {
			return linkKind
		}
		return n.Kind
	}
	count := map[key]int{}
	for _, n := range notes {
		count[key{n.Device, kindOf(n)}]++
	}
	var rows []noticeRow
	folded := map[key]int{} // key -> index in rows
	for _, n := range notes {
		k := key{n.Device, kindOf(n)}
		if count[k] <= foldAbove && k.kind != linkKind {
			rows = append(rows, noticeRow{notice: n})
			continue
		}
		i, ok := folded[k]
		if !ok {
			i = len(rows)
			folded[k] = i
			rows = append(rows, noticeRow{notice: notice{Error: n.Error, Kind: n.Kind, Device: n.Device, Since: n.Since, ReportedAt: n.ReportedAt}, Fold: true, FoldKind: k.kind})
		}
		r := &rows[i]
		r.Sub = append(r.Sub, n)
		r.Since = min(r.Since, n.Since)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Error && !rows[j].Error })
	return rows
}

// notifications computes what the popover shows. A folder whose database cannot be read is left out (the overview
// reports it).
func (s *Server) notifications() notifView {
	var v notifView
	fols, err := queryAll(s.db, func(rows *sql.Rows, f *folderRow) error { return rows.Scan(&f.ID, &f.Name) }, `SELECT id, name FROM folders ORDER BY name`)
	if err != nil {
		slog.Error("notifications", "err", err)
	}
	for _, f := range fols {
		notes, err := s.folderNotices(f.ID)
		if err != nil {
			slog.Warn("notifications", "folder", f.ID, "err", err)
			continue
		}
		if len(notes) == 0 {
			continue
		}
		g := noticeGroup{ID: f.ID, Name: f.Name, Rows: foldNotices(notes)}
		for _, n := range notes {
			if n.Error {
				g.Errors++
			} else {
				g.Warnings++
			}
		}
		v.Errors += g.Errors
		v.Warnings += g.Warnings
		v.Groups = append(v.Groups, g)
	}
	if v.Offline, err = s.offlineDevices(); err != nil {
		slog.Error("notifications", "err", err)
	}
	v.Warnings += len(v.Offline)
	return v
}

// deviceStatus is one folder line of a device in the overview.
type deviceStatus struct {
	FolderID               int64
	Folder, State          string
	Synced                 int64
	PendingUp, PendingDown int
}

// noticeKindCounts counts the errors and reminders among a device's reported problems.
func noticeKindCounts(errs []proto.FileError) (errors, warnings int) {
	for _, e := range errs {
		if e.Kind == proto.KindInUse || e.Kind == proto.KindSkipped {
			warnings++
		} else {
			errors++
		}
	}
	return
}
