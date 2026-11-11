package server

// The changes devices hold for the user's decision, and the decisions (design 2.8.7, 2.8.10, 2.8.11). Both live in
// memory: a device sends its list again when the server restarts (PollResp.Boot), and a decision lost with the restart
// is made again.

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// maxHeldItems bounds the list of held changes the server keeps per folder and device (design 2.8.10).
const maxHeldItems = proto.MaxHeldItems

// maxHeldRows is how many rows of one scope the decision dialog draws.
const maxHeldRows = 2000

// waitingFor is how long a decision counts as waiting for its device before the row shows as undecided again.
const waitingFor = 10 * time.Minute

// pendingAction is a decision that has not been seen to take effect: the device has it (delivered) or will get it
// with its next poll.
type pendingAction struct {
	proto.Action
	delivered bool
	at        time.Time
}

type actionKey struct {
	folder int64
	path   string
}

// heldHandler stores the list of held changes a device reports for a folder. The decisions the device has carried out
// are over: it reports after executing them, so the ones delivered before are cleared, and so are those for paths that
// are no longer held.
func (s *Server) heldHandler(w http.ResponseWriter, r *http.Request, d device) {
	var req proto.HeldReq
	if !readJSON(w, r, &req) {
		return
	}
	if !s.mapped(d.id, req.Folder) {
		http.Error(w, "folder not mapped to this device", http.StatusForbidden)
		return
	}
	cut := max(req.Cut, 0) // what the device itself left out
	if len(req.Items) > maxHeldItems {
		cut += len(req.Items) - maxHeldItems
		req.Items = req.Items[:maxHeldItems]
	}
	s.mu.Lock()
	rep := s.reportLocked(d.id)
	if rep.held == nil {
		rep.held = map[int64][]proto.HeldItem{}
	}
	rep.held[req.Folder] = req.Items
	if rep.heldCut == nil {
		rep.heldCut = map[int64]int{}
	}
	if cut > 0 {
		rep.heldCut[req.Folder] = cut
	} else {
		delete(rep.heldCut, req.Folder)
	}
	still := map[string]bool{}
	for _, it := range req.Items {
		still[it.Path] = true
	}
	for k, a := range rep.actions {
		if k.folder == req.Folder && (a.delivered || !still[k.path]) {
			delete(rep.actions, k)
		}
	}
	s.bumpLocked()
	s.mu.Unlock()
	writeJSON(w, map[string]int{"held": len(req.Items), "cut": cut})
}

// reportLocked returns the report of a device, made when there is none; the caller holds s.mu.
func (s *Server) reportLocked(id int64) *report {
	rep := s.reports[id]
	if rep == nil {
		rep = &report{}
		s.reports[id] = rep
	}
	return rep
}

// undelivered reports whether decisions wait for the device's next poll answer.
func (s *Server) undelivered(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rep := s.reports[id]; rep != nil {
		for _, a := range rep.actions {
			if !a.delivered {
				return true
			}
		}
	}
	return false
}

// deliver hands the device its decisions once.
func (s *Server) deliver(id int64) []proto.Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := s.reports[id]
	if rep == nil {
		return nil
	}
	var out []proto.Action
	for _, a := range rep.actions {
		if !a.delivered {
			a.delivered = true
			rep.actions[actionKey{a.Folder, a.Path}] = a
			out = append(out, a.Action)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// serverSeq is the seq of the server's entry of a path, 0 when it has none.
func (s *Server) serverSeq(folder int64, p string) int64 {
	db, err := s.folderDB(folder)
	if err != nil {
		return 0
	}
	var seq int64
	db.QueryRow(`SELECT seq FROM files WHERE path=?`, p).Scan(&seq)
	return seq
}

// resolveHandler takes the user's decisions (POST /folders/{id}/resolve). The rows of the dialog come as parallel
// fields r (expect, size, mtime and path, tab-separated) and a (the action, "" for none); a button with scope and
// action decides on every held change of that scope that the server has not changed since the dialog was drawn
// (field seen is the folder's latest seq then).
func (s *Server) resolveHandler(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	devID, _ := strconv.ParseInt(r.FormValue("device"), 10, 64)
	if !s.mapped(devID, f.ID) {
		return bad(http.StatusBadRequest, "/folders/"+strconv.FormatInt(f.ID, 10), "err.noDevice")
	}
	save := r.FormValue("save") != ""
	var acts []proto.Action
	valid := func(a string) bool { return a == proto.ActUpload || a == proto.ActOverwrite || a == proto.ActDiscard }
	if scope, act := r.FormValue("scope"), r.FormValue("action"); scope != "" && valid(act) {
		seen, _ := strconv.ParseInt(r.FormValue("seen"), 10, 64)
		s.mu.Lock()
		var items []proto.HeldItem
		if rep := s.reports[devID]; rep != nil {
			for _, it := range rep.held[f.ID] {
				if it.Scope == scope {
					items = append(items, it)
				}
			}
		}
		s.mu.Unlock()
		for _, it := range items {
			if cur := s.serverSeq(f.ID, it.Path); cur <= seen {
				acts = append(acts, proto.Action{Folder: f.ID, Path: it.Path, Action: act, Expect: &cur, Size: it.Size, MtimeNs: it.MtimeNs, Save: save})
			}
		}
	} else {
		rows, as := r.PostForm["r"], r.PostForm["a"]
		for i := 0; i < len(rows) && i < len(as); i++ {
			parts := strings.SplitN(rows[i], "\t", 4)
			if len(parts) != 4 || !valid(as[i]) {
				continue
			}
			exp, _ := strconv.ParseInt(parts[0], 10, 64)
			size, _ := strconv.ParseInt(parts[1], 10, 64)
			mtime, _ := strconv.ParseInt(parts[2], 10, 64)
			acts = append(acts, proto.Action{Folder: f.ID, Path: parts[3], Action: as[i], Expect: &exp, Size: size, MtimeNs: mtime, Save: save})
		}
	}
	s.mu.Lock()
	rep := s.reportLocked(devID)
	if rep.actions == nil {
		rep.actions = map[actionKey]pendingAction{}
	}
	now := s.now()
	for _, a := range acts {
		rep.actions[actionKey{a.Folder, a.Path}] = pendingAction{Action: a, at: now}
	}
	s.mu.Unlock()
	if len(acts) > 0 {
		s.broadcast() // wakes the device's poll and the open pages
	}
	http.Redirect(w, r, "/folders/"+strconv.FormatInt(f.ID, 10)+"?dir="+url.QueryEscape(strings.TrimSpace(r.FormValue("dir")))+"&held="+strconv.FormatInt(devID, 10), http.StatusSeeOther)
	return nil
}

// wcAckHandler confirms the default write rule of a folder (design 2.8.11: "keep the default").
func (s *Server) wcAckHandler(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE folders SET wc_ack=1 WHERE id=?`, f.ID); err != nil {
		return err
	}
	s.uiBump()
	http.Redirect(w, r, "/folders/"+strconv.FormatInt(f.ID, 10), http.StatusSeeOther)
	return nil
}

// heldRowView is one held change as the decision dialog shows it.
type heldRowView struct {
	Path, Kind, Reason, Note string
	Size, MtimeNs, At, Mtime int64
	BaseSeq, CurSeq          int64
	CurDevice                string
	CurTime                  int64
	CurGone, Changed         bool
	CanUpload, Waiting       bool
	Field                    string // the value of the row's r field
}

type heldGroupView struct {
	Scope, Path, Reason, Other string
	Rows                       []heldRowView
	Total                      int
	Decided                    int // how many of Total have a decision waiting for the device
}

type heldDevView struct {
	DeviceID int64
	Device   string
	Online   bool
	Seen     int64 // the folder's latest seq when drawn
	Groups   []heldGroupView
}

// heldView draws the decision dialog's data for one device of a folder.
func (s *Server) heldView(folder, devID int64) (*heldDevView, error) {
	var name string
	if err := s.db.QueryRow(`SELECT name FROM devices WHERE id=?`, devID).Scan(&name); err != nil {
		return nil, nil
	}
	db, err := s.folderDB(folder)
	if err != nil {
		return nil, err
	}
	rules, err := folderWC(s.db, folder)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var items []proto.HeldItem
	waiting := map[string]bool{}
	online := false
	if rep := s.reports[devID]; rep != nil {
		items = append(items, rep.held[folder]...)
		online = rep.online(s.now())
		for k, a := range rep.actions {
			if k.folder == folder && s.now().Sub(a.at) < waitingFor {
				waiting[k.path] = true
			}
		}
	}
	s.mu.Unlock()
	v := &heldDevView{DeviceID: devID, Device: name, Online: online, Seen: metaInt(db, "next_seq") - 1}
	byScope := map[string]*heldGroupView{}
	for _, it := range items {
		g := byScope[it.Scope]
		if g == nil {
			g = &heldGroupView{Scope: it.Scope, Path: heldScopePath(rules, it.Scope, it.Path), Reason: it.Reason}
			if rule := ruleFor(rules, pp.Key(it.Path)); it.Reason == "owner" && rule.Mode == proto.ModeExclusive {
				g.Reason, g.Other = "exclusive", rule.Device
			}
			if g.Reason == "owner" { // the other side of a concurrent write: the latest write of another device
				g.Other = lastOtherWriter(db, name, it.Scope)
			}
			byScope[it.Scope] = g
			v.Groups = append(v.Groups, *g)
		}
	}
	idx := map[string]int{}
	for i, g := range v.Groups {
		idx[g.Scope] = i
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	for _, it := range items {
		g := &v.Groups[idx[it.Scope]]
		g.Total++
		if waiting[it.Path] {
			g.Decided++
		}
		if len(g.Rows) >= maxHeldRows {
			continue
		}
		row := heldRowView{Mtime: it.MtimeNs / 1e9, Path: it.Path, Kind: it.Kind, Reason: it.Reason, Note: it.Note, Size: it.Size, MtimeNs: it.MtimeNs, At: it.At, BaseSeq: it.BaseSeq, Waiting: waiting[it.Path]}
		var hash []byte
		if err := db.QueryRow(`SELECT seq, hash, device, created_at FROM files WHERE path=?`, it.Path).Scan(&row.CurSeq, &hash, &row.CurDevice, &row.CurTime); err == nil {
			row.CurGone = hash == nil
		}
		row.Changed = row.CurSeq != it.BaseSeq
		row.CanUpload = !row.Changed && it.Reason != "type"
		row.Field = strconv.FormatInt(row.CurSeq, 10) + "\t" + strconv.FormatInt(it.Size, 10) + "\t" + strconv.FormatInt(it.MtimeNs, 10) + "\t" + it.Path
		g.Rows = append(g.Rows, row)
	}
	return v, nil
}
