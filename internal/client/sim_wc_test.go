package client

// The write-control part of the simulation (design 2.8.12): a model of the rules written independently of the
// server, a log of every commit the server accepted, and the checks of the invariants on that log.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	pp "kaze-sync/internal/proto/path"
)

// acc is one accepted write (a new row of the server's files table).
type acc struct {
	seq    int64
	path   string
	who    string
	at     time.Time // the server's clock when it was accepted
	since  int64
	system bool // a decision (resolve) or the Web UI: not bound by the check
}

// simScope is the model of the scope of a path: "d:<key>" for a setting, "f:<key>" for a file of a free directory.
func simScope(rules []simRule, path string) (id, mode string, owner int) {
	k := pp.Key(path)
	best := simRule{Mode: "single"}
	bestKey := ""
	for _, r := range rules {
		rk := pp.Key(r.Path)
		if (rk == "" || k == rk || strings.HasPrefix(k, rk+"/")) && len(rk) >= len(bestKey) {
			best, bestKey = r, rk
		}
	}
	if best.Mode == "free" {
		return "f:" + k, best.Mode, 0
	}
	return "d:" + bestKey, best.Mode, best.Owner
}

// randRules makes a random set of settings: the root, and a few of the directories the simulation uses. owners are
// the clients that map the folder.
func (s *sim) randRules(owners []int) []simRule {
	pick := func() simRule {
		switch x := s.rng.Intn(10); {
		case x < 4:
			return simRule{Mode: "single"}
		case x < 7:
			return simRule{Mode: "free"}
		default:
			return simRule{Mode: "exclusive", Owner: owners[s.rng.Intn(len(owners))]}
		}
	}
	root := pick()
	if root.Mode == "exclusive" && s.rng.Intn(2) == 0 { // an exclusive root is rarer: it makes the others do nothing
		root = simRule{Mode: "single"}
	}
	rules := []simRule{root}
	for _, d := range []string{"d", "g", "g/h", "j"} {
		if s.rng.Intn(4) == 0 {
			r := pick()
			r.Path = d
			rules = append(rules, r)
		}
	}
	return rules
}

// logCommit checks and records a commit request the server handled.
func (s *sim) logCommit(who string, reqBody, respBody []byte) {
	var req struct {
		Folder int64
		Ops    []struct {
			Path    string
			Hash    *string
			Since   int64
			Resolve bool
		}
	}
	var resp struct {
		Results []struct {
			Status string
			Seq    int64
		}
	}
	if json.Unmarshal(reqBody, &req) != nil || json.Unmarshal(respBody, &resp) != nil || len(resp.Results) != len(req.Ops) {
		return
	}
	now := s.srvNow()
	var fresh []int // the ops that wrote a new row
	for i, r := range resp.Results {
		if r.Status == "ok" && r.Seq > s.maxSeq[req.Folder] {
			fresh = append(fresh, i)
		}
	}
	if len(fresh) == 0 {
		return
	}
	rules := s.rules[req.Folder]
	type group struct {
		ops     []int
		since   int64
		resolve bool
	}
	groups := map[string]*group{}
	var ids []string
	for i, op := range req.Ops {
		id, _, _ := simScope(rules, op.Path)
		g := groups[id]
		if g == nil {
			g = &group{since: op.Since, resolve: true}
			groups[id] = g
			ids = append(ids, id)
		}
		g.ops, g.since, g.resolve = append(g.ops, i), min(g.since, op.Since), g.resolve && op.Resolve
	}
	sort.Strings(ids)
	for _, id := range ids {
		g := groups[id]
		wrote := false
		for _, i := range fresh {
			if _, ok := groupHas(g.ops, i); ok {
				wrote = true
			}
		}
		if !wrote || g.resolve {
			continue
		}
		s.checkGroup(req.Folder, who, id, g.since, req.Ops[g.ops[0]].Path, now)
		for _, i := range g.ops { // (1b) the since is not larger than the cursor the first unsent change began at
			op := req.Ops[i]
			if _, mode, _ := simScope(rules, op.Path); mode == "single" {
				if e, ok := s.edit[s.editKey(who, req.Folder, op.Path)]; ok && op.Since > e {
					s.fail("since too large: %s committed %s in folder %d with since %d, its change began at cursor %d", who, op.Path, req.Folder, op.Since, e)
				}
			}
		}
	}
	for _, i := range fresh {
		op := req.Ops[i]
		s.hist[req.Folder] = append(s.hist[req.Folder], acc{seq: resp.Results[i].Seq, path: op.Path, who: who, at: now, since: op.Since, system: op.Resolve})
		s.maxSeq[req.Folder] = max(s.maxSeq[req.Folder], resp.Results[i].Seq)
		delete(s.edit, s.editKey(who, req.Folder, op.Path))
	}
}

func groupHas(ops []int, i int) (int, bool) {
	for k, o := range ops {
		if o == i {
			return k, true
		}
	}
	return 0, false
}

// checkGroup is invariant (1) and (2): a write was accepted in a scope, so the rules must have allowed it.
func (s *sim) checkGroup(folder int64, who, id string, since int64, first string, now time.Time) {
	rules := s.rules[folder]
	_, mode, owner := simScope(rules, first)
	window := time.Duration(s.window) * time.Second
	inWindow := func(w acc) bool {
		age := now.Sub(w.at)
		return w.who != who && age >= 0 && age < window
	}
	switch mode {
	case "exclusive":
		if who != s.clients[owner].name {
			s.fail("exclusive scope %s of folder %d is owned by %s, but %s wrote it", id, folder, s.clients[owner].name, who)
		}
	case "free":
		var last *acc
		for i := range s.hist[folder] {
			if w := &s.hist[folder][i]; w.path == first && (last == nil || w.seq > last.seq) {
				last = w
			}
		}
		if last != nil && inWindow(*last) {
			s.fail("free file %s of folder %d: %s wrote it within the window after %s (seq %d)", first, folder, who, last.who, last.seq)
		}
	default:
		var last *acc
		lastOwn := int64(0)
		var rows []acc
		for _, w := range s.hist[folder] {
			if wid, _, _ := simScope(rules, w.path); wid == id {
				rows = append(rows, w)
			}
		}
		for i := range rows {
			if last == nil || rows[i].seq > last.seq {
				last = &rows[i]
			}
			if rows[i].who == who {
				lastOwn = max(lastOwn, rows[i].seq)
			}
		}
		low := max(since, lastOwn)
		for _, w := range rows {
			if w.who != who && w.seq > low {
				s.fail("single scope %s of folder %d: %s wrote with since %d (own %d) but %s wrote seq %d before (stale)", id, folder, who, since, lastOwn, w.who, w.seq)
				break
			}
		}
		rewound := false // a later row that looks older: the clock went back, and the walk may stop before the window's row (design: not checked while rewound)
		if last != nil {
			for _, w := range s.hist[folder] {
				if w.seq > last.seq && now.Sub(w.at) >= window {
					rewound = true
				}
			}
		}
		if last != nil && !rewound && inWindow(*last) {
			s.fail("single scope %s of folder %d: %s wrote %v after %s's write (seq %d), inside the window of %ds", id, folder, who, now.Sub(last.at), last.who, last.seq, s.window)
		}
	}
}

func (s *sim) editKey(who string, folder int64, path string) string {
	return fmt.Sprintf("%s|%d|%s", who, folder, path)
}

func (s *sim) fail(format string, a ...any) {
	if len(s.viol) < 10 {
		s.viol = append(s.viol, fmt.Sprintf(format, a...))
	}
}

// noteWeb records the writes the Web UI made (a restore) since the last look.
func (s *sim) noteWeb(fid int64) {
	db := s.e.folderDB(fid)
	defer db.Close()
	rows, err := db.Query(`SELECT seq, path FROM files WHERE seq > ? ORDER BY seq`, s.maxSeq[fid])
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var p string
		rows.Scan(&seq, &p)
		s.hist[fid] = append(s.hist[fid], acc{seq: seq, path: p, who: "web", at: s.srvNow(), system: true})
		s.maxSeq[fid] = seq
	}
}

// observe keeps, for every file of a single-writer scope that differs from the client's base, the cursor the scope
// had applied when the difference began (invariant 1b): the since of its commit must not be larger. It is called
// after each step on the folder; only the step that edits may begin a record.
func (s *sim) observe(c *simClient, sf *simFolder, edited bool) {
	sf.hw = max(sf.hw, sf.f.DB.metaInt("cursor"))
	tree := treeOf(sf.mem)
	rows, err := sf.f.DB.all()
	if err != nil {
		return
	}
	frozen := sf.frozenIDs()
	seen := map[string]bool{}
	note := func(p string, differs bool) {
		k := s.editKey(c.name, sf.id, p)
		id, mode, _ := simScope(sf.rules, p)
		seen[k] = true
		if !differs || mode != "single" || frozen[id] {
			delete(s.edit, k)
			return
		}
		if _, ok := s.edit[k]; !ok && edited { // a difference seen after a sync may be a pull cut short, not an edit
			s.edit[k] = sf.appliedCursor(id)
		}
	}
	for p, h := range tree {
		r, ok := rows[p]
		note(p, !ok || hex.EncodeToString(r.Hash) != h)
	}
	for p := range rows {
		if _, ok := tree[p]; !ok {
			note(p, true)
		}
	}
	prefix := fmt.Sprintf("%s|%d|", c.name, sf.id)
	for k := range s.edit {
		if strings.HasPrefix(k, prefix) && !seen[k] {
			delete(s.edit, k)
		}
	}
}

// bodyOf reads a request body and puts it back.
func bodyOf(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}
