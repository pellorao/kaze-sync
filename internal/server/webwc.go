package server

// The write-control side of the folder page (design 2.8.11): the effective mode of every row, the labels, and the line
// under the title.

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// wcInfo is the write control of a path: what governs it, whether the setting is on the path itself, and what the path
// would inherit without a setting of its own (the dialog's first option).
type wcInfo struct {
	Path     string
	Mode     string // the effective mode
	Explicit bool   // the setting is on this path
	From     string // the path of the governing setting; "" is the root
	Dev      string
	DevID    int64
	InhMode  string
	InhFrom  string
}

func wcFor(rules []lockRow, p string) wcInfo {
	k := pp.Key(p)
	gov, inh := lockRow{Mode: proto.ModeSingle}, lockRow{Mode: proto.ModeSingle}
	row := false              // gov is a stored setting, not the root's default
	for _, r := range rules { // ordered by key: the last cover is the longest
		if r.covers(k) {
			gov, row = r, true
			if r.key != k {
				inh = r
			}
		}
	}
	return wcInfo{Path: p, Mode: gov.Mode, Explicit: row && gov.key == k, From: gov.Path, Dev: gov.Device, DevID: gov.DeviceID, InhMode: inh.Mode, InhFrom: inh.Path}
}

// decorateWC gives the rows their mode (the icon shows it, wcIcon) and the held counts with their label, from memory
// and the settings (no query).
func (s *Server) decorateWC(t translator, entries []browseEntry, dir string, folder int64, rules []lockRow) {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	type heldKey struct {
		name string
		dev  int64
	}
	held := map[heldKey]int{}
	hdevs := map[string][]int64{}
	names := map[int64]string{}
	if devs, err := queryAll(s.db, func(r *sql.Rows, d *devTag) error { return r.Scan(&d.ID, &d.Name) }, `SELECT id, name FROM devices`); err == nil {
		for _, d := range devs {
			names[d.ID] = d.Name
		}
	}
	s.mu.Lock()
	for id, rep := range s.reports {
		for _, it := range rep.held[folder] {
			rest, ok := strings.CutPrefix(it.Path, prefix)
			if !ok {
				continue
			}
			name, _, _ := strings.Cut(rest, "/")
			k := heldKey{name, id}
			if held[k] == 0 {
				hdevs[name] = append(hdevs[name], id)
			}
			held[k]++
		}
	}
	s.mu.Unlock()
	for i := range entries {
		e := &entries[i]
		if e.Unsynced {
			continue
		}
		e.WC = wcFor(rules, e.Path)
		if !e.IsDir && e.WC.InhMode == proto.ModeFree {
			e.WC.InhMode = proto.ModeSingle // a file is never unrestricted: inside a free directory it is a single-writer scope of its own (design 2.8.2)
		}
		if ids := hdevs[e.Name]; len(ids) > 0 {
			slices.SortFunc(ids, func(a, b int64) int { return strings.Compare(names[a], names[b]) })
			var dn []string
			for _, id := range ids { // a tag per device: the held dialog is the device's
				n := held[heldKey{e.Name, id}]
				e.HeldN += n
				dn = append(dn, names[id])
				e.Tags = append(e.Tags, entryTag{Class: "err", Text: t.T("tag.held", "n", n, "device", names[id]), Title: t.T("tag.held.title"),
					Href: "/folders/" + strconv.FormatInt(folder, 10) + "?dir=" + url.QueryEscape(dir) + "&held=" + strconv.FormatInt(id, 10)})
			}
			e.HeldDev = strings.Join(dn, ", ")
		}
	}
}

// ownerNow is the device whose write still holds a single-writer scope (design 2.8.3), until the given Unix time.
type ownerNow struct {
	Path, Device string
	Until        int64
}

// activeOwners finds, per single-writer scope of a folder, the latest write if it is still inside the time window.
// It reads the newest 500 entries only, like the overview line.
func (s *Server) activeOwners(folder int64, rules []lockRow) map[string]ownerNow {
	out := map[string]ownerNow{}
	db, err := s.folderDB(folder)
	if err != nil {
		return out
	}
	window := metaInt(s.db, "write_window")
	now := s.now().Unix()
	rows, err := db.Query(`SELECT key, device, created_at FROM files ORDER BY seq DESC LIMIT 500`)
	if err != nil {
		return out
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var key, dev string
		var created int64
		if rows.Scan(&key, &dev, &created) != nil {
			break
		}
		rule := ruleFor(rules, key)
		if dev == excludedDevice || rule.Mode != proto.ModeSingle || seen[rule.key] {
			continue
		}
		seen[rule.key] = true // the latest write of the scope decides
		if until := created + window; until > now {
			out[rule.key] = ownerNow{rule.Path, dev, until}
		}
	}
	return out
}

// wcLine adds the overview of the current directory: its mode, where it comes from, and the single writer's owner.
func (s *Server) wcLine(t translator, data map[string]any, owners map[string]ownerNow, rules []lockRow, dir string) {
	w := wcFor(rules, dir)
	from := "/"
	if w.From != "" {
		from = "/" + w.From
	}
	data["WCMode"], data["WCFrom"] = t.T("wc.mode."+w.Mode), from
	if w.Mode == proto.ModeExclusive {
		data["WCMode"] = t.T("wc.tag.exclusive", "device", w.Dev)
	}
	if o, ok := owners[ruleFor(rules, pp.Key(dir)).key]; ok && w.Mode == proto.ModeSingle {
		data["WCOwner"], data["WCUntil"] = o.Device, o.Until
	}
}

// heldSpan is what the write-control dialog needs of the changes a device holds in one scope: the directory that
// contains all of them, and how many there are.
type heldSpan struct {
	P string // directory containing every held path of the group ("" is the folder root)
	N int
	D string // device name
}

// heldSpans groups the held changes of a folder by device and scope, as JSON for the page. The dialog asks for
// confirmation when a setting is saved over a span (design 2.8.11).
func (s *Server) heldSpans(folder int64) string {
	names := map[int64]string{}
	if devs, err := queryAll(s.db, func(r *sql.Rows, d *devTag) error { return r.Scan(&d.ID, &d.Name) }, `SELECT id, name FROM devices`); err == nil {
		for _, d := range devs {
			names[d.ID] = d.Name
		}
	}
	type gk struct {
		dev   int64
		scope string
	}
	groups := map[gk]*heldSpan{}
	s.mu.Lock()
	for id, rep := range s.reports {
		for _, it := range rep.held[folder] {
			k := gk{id, it.Scope}
			dir := ""
			if i := strings.LastIndex(it.Path, "/"); i >= 0 {
				dir = it.Path[:i]
			}
			g := groups[k]
			if g == nil {
				groups[k] = &heldSpan{dir, 1, names[id]}
				continue
			}
			g.N++
			for g.P != "" && g.P != dir && !strings.HasPrefix(dir, g.P+"/") {
				g.P = g.P[:max(strings.LastIndex(g.P, "/"), 0)]
			}
		}
	}
	s.mu.Unlock()
	out := []heldSpan{}
	for _, g := range groups {
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b heldSpan) int { return strings.Compare(a.D+"\x00"+a.P, b.D+"\x00"+b.P) })
	b, _ := json.Marshal(out)
	return string(b)
}
