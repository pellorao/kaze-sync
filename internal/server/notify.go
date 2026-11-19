package server

import (
	"cmp"
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
const offlineAfter = 3 * 24 * time.Hour

// notice is one line of a folder's notifications. Notices are computed from
// the current state on every request; none is stored (design 4.3).
type notice struct {
	Error                 bool   // an error needs action to go away; otherwise it is a reminder
	Kind                  string // a proto kind, kindHeld or kindNewFolder
	Device, Path, Message string
	Since                 int64
	Link                  string
	ReportedAt            int64 // when the device last reported, if it is offline; else 0
	Folder                int64
	// Of a kindHeld notice: the changes a device holds in one scope, the scope's path ("" is the root), the reason
	// of the first of them and the device behind it (design 2.8.11).
	N                    int
	Scope, Reason, Other string
	Items                []string // of a kindHeld notice: the paths of the held changes (they share memory with the report); for the counts of a directory page
}

// kindHeld is the kind of the notice of the changes a device holds for the user's decision (design 2.8.11);
// kindNewFolder that of a folder whose write rules the user has not yet looked at.
const (
	kindHeld      = "held"
	kindNewFolder = "wc-unset"
	kindHeldCut   = "held-cut" // N held changes of a device are not kept or shown: its report was over the limit
)

// adviceOf is what to do about a kind of notice, in the language of the page ("" when there is nothing to say).
func adviceOf(t translator, kind string) string {
	switch kind {
	case proto.KindRejected, proto.KindIO, proto.KindFolder, proto.KindInUse, proto.KindSkipped, proto.KindReport, kindHeld:
		return t.T("advice." + kind)
	case kindNewFolder:
		return t.T("advice.wc.unset")
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

type noticeMapping struct{ device, folder int64 }

const noticeMappingFrom = ` FROM mappings m JOIN devices d ON d.id=m.device_id JOIN folders f ON f.id=m.folder_id`

// noticeMappingSnapshot gives the push checks the current mappings without querying on each report. The query runs
// outside s.mu (design 4.3): a configuration change drops the cache and the next check rebuilds it. The returned
// generation lets the caller see, under s.mu, that no change came in since.
func (s *Server) noticeMappingSnapshot() (map[noticeMapping]bool, int64) {
	s.mu.Lock()
	m, gen := s.noticeMappings, s.noticeGen
	s.mu.Unlock()
	if m != nil {
		return m, gen
	}
	mappings, err := queryAll(s.db, func(rows *sql.Rows, m *noticeMapping) error { return rows.Scan(&m.device, &m.folder) },
		`SELECT d.id, f.id`+noticeMappingFrom)
	if err != nil {
		slog.Warn("push: cannot read notification mappings", "err", err)
		return nil, gen
	}
	m = make(map[noticeMapping]bool, len(mappings))
	for _, e := range mappings {
		m[e] = true
	}
	s.mu.Lock()
	if s.noticeGen == gen { // no configuration change since the query started
		s.noticeMappings = m
	}
	s.mu.Unlock()
	return m, gen
}

// dropNoticeMappingsLocked invalidates the cache after a change of mappings or devices.
func (s *Server) dropNoticeMappingsLocked() {
	s.noticeMappings = nil
	s.noticeGen++
}

// folderNotices lists the notices of a folder: what the mapped devices report, the changes they hold for the user,
// and the reminder of a new folder.
func (s *Server) folderNotices(folder int64) ([]notice, error) {
	type dev struct {
		id   int64
		name string
	}
	devs, err := queryAll(s.db, func(rows *sql.Rows, d *dev) error { return rows.Scan(&d.id, &d.name) },
		`SELECT d.id, d.name`+noticeMappingFrom+` WHERE f.id=? ORDER BY d.name`, folder)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []notice
	type heldGroup struct {
		n         notice
		firstPath string
	}
	var groups []heldGroup
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
				n := notice{Kind: e.Kind, Device: d.name, Path: e.Path, Message: e.Message, Since: e.Since, ReportedAt: stale, Folder: folder,
					Error: e.Kind != proto.KindInUse && e.Kind != proto.KindSkipped && e.Kind != proto.KindReport}
				if e.Kind == proto.KindFolder {
					n.Link = "/devices/" + strconv.FormatInt(d.id, 10)
				} else {
					n.Link = browseLink(folder, e.Path)
				}
				out = append(out, n)
			}
		}
		if n := rep.heldCut[folder]; n > 0 {
			out = append(out, notice{Error: true, Kind: kindHeldCut, Device: d.name, N: n, ReportedAt: stale, Folder: folder})
		}
		byScope := map[string]*heldGroup{}
		var order []string
		for _, h := range rep.held[folder] {
			g := byScope[h.Scope]
			if g == nil {
				g = &heldGroup{n: notice{Error: true, Kind: kindHeld, Device: d.name, Since: h.At, ReportedAt: stale, Folder: folder, Scope: h.Scope, Reason: h.Reason,
					Link: "/folders/" + strconv.FormatInt(folder, 10) + "?held=" + strconv.FormatInt(d.id, 10)}, firstPath: h.Path}
				byScope[h.Scope] = g
				order = append(order, h.Scope)
			}
			g.n.N++
			g.n.Items = append(g.n.Items, h.Path)
			g.n.Since = min(g.n.Since, h.At)
		}
		for _, sc := range order {
			groups = append(groups, *byScope[sc])
		}
	}
	s.mu.Unlock()
	if len(groups) > 0 {
		rules, err := folderWC(s.db, folder)
		if err != nil {
			return nil, err
		}
		db, err := s.folderDB(folder)
		if err != nil {
			return nil, err
		}
		type otherKey struct{ device, scope string }
		others := map[otherKey]string{} // the other side of a concurrent write, per device and scope
		for _, g := range groups {
			n := g.n
			n.Scope, n.Path = heldScopePath(rules, n.Scope, g.firstPath), ""
			rule := ruleFor(rules, pp.Key(g.firstPath))
			switch {
			case n.Reason == "owner" && rule.Mode == proto.ModeExclusive:
				n.Reason, n.Other = "exclusive", rule.Device
			case n.Reason == "owner":
				k := otherKey{n.Device, g.n.Scope}
				o, ok := others[k]
				if !ok {
					o = lastOtherWriter(db, n.Device, g.n.Scope)
					others[k] = o
				}
				n.Other = o
			}
			out = append(out, n)
		}
	}
	var ack int
	if err := s.db.QueryRow(`SELECT wc_ack FROM folders WHERE id=?`, folder).Scan(&ack); err == nil && ack == 0 {
		out = append(out, notice{Kind: kindNewFolder, Folder: folder, Link: "/folders/" + strconv.FormatInt(folder, 10) + "?wc="})
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

// srvNotice is a notification of the server's own state: the "Server" group of the popover.
type srvNotice struct {
	Error bool
	Kind  string // "disk", "gc", "cert" or "pushgone"
	Name  string // pushgone: the short name of the browser
}

// notifView is everything the popover and the bell show.
type notifView struct {
	Errors, Warnings int
	Groups           []noticeGroup
	Offline          []offlineDevice
	Old              []offlineDevice // devices whose client is too old: the server refuses its requests (9.3)
	Server           []srvNotice
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
	v.Old = s.oldDevices()
	v.Errors += len(v.Old)
	for _, n := range s.serverNotices() {
		v.Server = append(v.Server, n)
		if n.Error {
			v.Errors++
		} else {
			v.Warnings++
		}
	}
	return v
}

// oldDevices lists the devices whose last request was refused as too old (design 9.3), by name.
func (s *Server) oldDevices() []offlineDevice {
	s.mu.Lock()
	var ids []int64
	for id, rep := range s.reports {
		if rep.tooOld > 0 {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	var out []offlineDevice
	for _, id := range ids {
		var d offlineDevice
		d.ID = id
		if s.db.QueryRow(`SELECT name FROM devices WHERE id=?`, id).Scan(&d.Name) == nil {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// serverNotices lists the server's own problems and the browsers whose push subscription is invalid (design 4.3).
func (s *Server) serverNotices() []srvNotice {
	s.mu.Lock()
	out := s.serverProblemsLocked()
	s.mu.Unlock()
	for _, sub := range s.subscriptions() {
		if sub.Gone {
			out = append(out, srvNotice{Kind: "pushgone", Name: sub.Browser})
		}
	}
	return out
}

// serverProblemsLocked lists the server errors common to the bell and Web Push.
func (s *Server) serverProblemsLocked() []srvNotice {
	var out []srvNotice
	for _, c := range []struct {
		on   bool
		kind string
	}{{s.diskLow, "disk"}, {s.gcFailed, "gc"}, {s.certFailed, "cert"}} {
		if c.on {
			out = append(out, srvNotice{Error: true, Kind: c.kind})
		}
	}
	return out
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
		if e.Kind == proto.KindInUse || e.Kind == proto.KindSkipped || e.Kind == proto.KindReport {
			warnings++
		} else {
			errors++
		}
	}
	return
}

// lastOtherWriter is the device other than dev that wrote last in a write-control scope ("d:<key>" is a directory and
// what is under it, "f:<key>" one file): the other side of a concurrent write there.
func lastOtherWriter(db *sql.DB, dev, scope string) string {
	q, args := `SELECT device FROM files WHERE device<>? AND device<>?`, []any{dev, excludedDevice}
	switch {
	case strings.HasPrefix(scope, "f:"):
		q, args = q+` AND key=?`, append(args, scope[2:])
	case strings.TrimPrefix(scope, "d:") != "":
		k := strings.TrimPrefix(scope, "d:")
		q, args = q+` AND (key=? OR key>=? AND key<?)`, append(args, k, k+"/", k+"0") // "0" follows "/"
	}
	var other string
	db.QueryRow(q+` ORDER BY seq DESC LIMIT 1`, args...).Scan(&other)
	return other
}

// heldScopePath is the path a held scope is shown as: the path of its setting, or of the file for a file of a free
// directory; "" is the root.
func heldScopePath(rules []lockRow, scope, firstPath string) string {
	if strings.HasPrefix(scope, "f:") {
		return firstPath
	}
	key := strings.TrimPrefix(scope, "d:")
	for _, r := range rules {
		if r.key == key {
			return r.Path
		}
	}
	return ""
}

// heldWhy is the reason of held changes in words.
func heldWhy(t translator, reason, other string) string {
	return t.T("held.why."+reason, "other", cmp.Or(other, "?"))
}

// heldMsg is the text of the notice of a device's held changes in one scope (design 2.8.11).
func heldMsg(t translator, n notice) string {
	scope := n.Scope
	if scope == "" {
		scope = "/"
	}
	return t.T("notice.held", "device", n.Device, "n", n.N, "scope", scope, "why", heldWhy(t, n.Reason, n.Other))
}
