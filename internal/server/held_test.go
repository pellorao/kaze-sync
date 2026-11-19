package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

// heldReport is what a device sends when its held list changes.
func (e *env) heldReport(tok string, folder int64, items ...proto.HeldItem) {
	e.t.Helper()
	if st, out := e.api(tok, "held", proto.HeldReq{Folder: folder, Items: items}); st != 200 {
		e.t.Fatalf("held: %d %s", st, out)
	}
}

// pollActions polls once and returns the decisions and the boot id of the answer.
func (e *env) pollActions(tok string, folder int64) ([]proto.Action, string) {
	e.t.Helper()
	st, out := e.api(tok, "poll", proto.PollReq{ConfigVersion: metaInt(e.s.db, "config_version"), Cursors: map[int64]int64{folder: e.seq(folder)}})
	if st != 200 {
		e.t.Fatalf("poll: %d %s", st, out)
	}
	var r proto.PollResp
	json.Unmarshal(out, &r)
	return r.Actions, r.Boot
}

func heldEnv(t *testing.T) (*lockEnv, proto.HeldItem) {
	l := newLockEnv(t)
	l.s.PollTimeout = 20 * 1e6
	l.apiCommit(l.ta, l.f, l.op("d/x.txt", 0, "one"))
	return l, proto.HeldItem{Path: "d/x.txt", Scope: "d:D", Reason: "owner", BaseSeq: 1, Kind: "modify", Size: 5, MtimeNs: 7e9, At: l.clk.Now().Unix()}
}

// A decision reaches the device once through its poll, and the next held report ends it.
func TestResolveDeliversActionOnce(t *testing.T) {
	t.Parallel()
	l, it := heldEnv(t)
	l.heldReport(l.tb, l.f, it)
	row := "1\t5\t7000000000\td/x.txt"
	_, dlg := l.get(l.web, "/folders/"+itoa(l.f)+"?held="+itoa(l.b))
	if !strings.Contains(dlg, "与 A 同时写入") {
		t.Error("the dialog does not name the other writer")
	}
	if st, body := l.post(l.web, "/folders/"+itoa(l.f)+"/resolve", url.Values{"device": {itoa(l.b)}, "r": {row}, "a": {"overwrite"}, "save": {"1"}}); st != 303 {
		t.Fatalf("resolve: %d %s", st, body)
	}
	acts, boot := l.pollActions(l.tb, l.f)
	if len(acts) != 1 || acts[0].Path != "d/x.txt" || acts[0].Action != proto.ActOverwrite || !acts[0].Save || acts[0].Expect == nil || *acts[0].Expect != 1 || acts[0].Size != 5 {
		t.Fatalf("actions %+v", acts)
	}
	if boot == "" {
		t.Fatal("no boot id")
	}
	if acts, _ := l.pollActions(l.tb, l.f); len(acts) != 0 {
		t.Fatalf("delivered twice: %+v", acts)
	}
	// While undecided-looking, the dialog shows the wait; once the device reports the list again it is gone.
	_, page := l.get(l.web, "/folders/"+itoa(l.f)+"?held="+itoa(l.b))
	if !strings.Contains(page, "等待 B 执行") {
		t.Errorf("the dialog does not show the waiting decision:\n%.3000s", page)
	}
	l.heldReport(l.tb, l.f)
	if n := len(l.s.reports[l.b].actions); n != 0 {
		t.Fatalf("%d decisions left after the report", n)
	}
}

// A batch button acts only on what the server has not changed since the dialog was drawn.
func TestResolveBatchSkipsChangedPaths(t *testing.T) {
	t.Parallel()
	l, it := heldEnv(t)
	l.apiCommit(l.ta, l.f, l.op("d/y.txt", 0, "y"))
	it2 := it
	it2.Path, it2.BaseSeq = "d/y.txt", 2
	l.heldReport(l.tb, l.f, it, it2)
	seen := l.seq(l.f)
	l.apiCommit(l.ta, l.f, l.op("d/y.txt", 2, "yy")) // changes after the dialog was drawn
	l.post(l.web, "/folders/"+itoa(l.f)+"/resolve", url.Values{"device": {itoa(l.b)}, "scope": {"d:D"}, "action": {"discard"}, "seen": {itoa(seen)}})
	acts, _ := l.pollActions(l.tb, l.f)
	if len(acts) != 1 || acts[0].Path != "d/x.txt" || acts[0].Action != proto.ActDiscard {
		t.Fatalf("actions %+v", acts)
	}
}

// The boot id changes with the process, which tells the devices to send their held lists again.
func TestBootChangesWithRestart(t *testing.T) {
	t.Parallel()
	l, _ := heldEnv(t)
	_, b1 := l.pollActions(l.ta, l.f)
	_, b2 := l.pollActions(l.ta, l.f)
	l.restart()
	_, b3 := l.pollActions(l.ta, l.f)
	if b1 == "" || b1 != b2 || b3 == "" || b3 == b1 {
		t.Fatalf("boot ids %q %q %q", b1, b2, b3)
	}
}

// changes?blocking= lists the live entries that stand in the way of a file at a path.
func TestChangesBlockingEntries(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	l.apiCommit(l.ta, l.f, l.op("a", 0, "file"), l.op("b/c/d", 0, "deep"), l.op("b/e", 0, "e"))
	blocking := func(p string) []string {
		req, _ := http.NewRequest("GET", l.ts.URL+"/api/v1/changes?folder="+itoa(l.f)+"&since=0&blocking="+url.QueryEscape(p), nil)
		req.Header.Set(proto.VersionHeader, strconv.Itoa(proto.AgentVersion))
		req.Header.Set("Authorization", "Bearer "+l.ta)
		resp, err := l.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r proto.ChangesResp
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&r) != nil {
			t.Fatalf("changes: %d", resp.StatusCode)
		}
		var ps []string
		for _, e := range r.Entries {
			ps = append(ps, e.Path)
		}
		return ps
	}
	if got := blocking("a/x"); len(got) != 1 || got[0] != "a" { // a file stands where a directory should be
		t.Fatalf("ancestor: %v", got)
	}
	if got := blocking("b"); len(got) != 2 { // a directory's files stand where a file should be
		t.Fatalf("below: %v", got)
	}
	if got := blocking("z"); len(got) != 0 {
		t.Fatalf("none: %v", got)
	}
}

// A folder shows the new-folder reminder until its write rules are saved or kept.
func TestNewFolderReminderUntilAcknowledged(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	has := func() bool { _, p := l.get(l.web, "/"); return strings.Contains(p, "还没有设置写入规则") }
	if !has() {
		t.Fatal("no reminder for a new folder")
	}
	if st, _ := l.post(l.web, "/folders/"+itoa(l.f)+"/wc-ack", url.Values{}); st != 303 || has() {
		t.Fatal("keeping the default did not end the reminder")
	}
	f2 := l.folder("G")
	if !has() {
		t.Fatal("no reminder for the second folder")
	}
	l.post(l.web, "/folders/"+itoa(f2)+"/writectl", url.Values{"path": {""}, "mode": {"free"}})
	if has() {
		t.Fatal("saving the write rules did not end the reminder")
	}
}

func TestWriteWindowSetting(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	if st, _ := l.post(l.web, "/settings/window", url.Values{"window": {"5"}}); st != 303 || metaInt(l.s.db, "write_window") != 300 {
		t.Fatalf("window = %d", metaInt(l.s.db, "write_window"))
	}
	for _, bad := range []string{"61", "-1", "x", ""} {
		if st, _ := l.post(l.web, "/settings/window", url.Values{"window": {bad}}); st != 400 {
			t.Errorf("%q accepted: %d", bad, st)
		}
	}
	if metaInt(l.s.db, "write_window") != 300 {
		t.Fatal("a refused value changed the window")
	}
}

// Migrations: the conflict column goes from the folder databases, wc_ack comes to the folders.
func TestMigrationsDropConflictAddAck(t *testing.T) {
	t.Parallel()
	l := newLockEnv(t)
	db := l.s.folderDBMust(t, l.f)
	for _, tb := range []string{"files", "versions"} {
		rows, err := db.Query(`SELECT name FROM pragma_table_info('` + tb + `')`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var n string
			rows.Scan(&n)
			if n == "conflict" {
				t.Errorf("%s still has the conflict column", tb)
			}
		}
		rows.Close()
	}
	if l.count(`SELECT count(*) FROM pragma_table_info('folders') WHERE name='wc_ack'`) != 1 {
		t.Fatal("no wc_ack")
	}
}

// manyHeld is n held changes, all in one scope.
func manyHeld(it proto.HeldItem, n int) []proto.HeldItem {
	out := make([]proto.HeldItem, n)
	for i := range out {
		out[i] = it
		out[i].Path = "d/f" + strconv.Itoa(i)
	}
	return out
}

// A device cannot make the server keep more than maxHeldItems held changes for a folder: the rest is dropped.
func TestHeldListIsCapped(t *testing.T) {
	t.Parallel()
	l, it := heldEnv(t)
	l.heldReport(l.tb, l.f, manyHeld(it, maxHeldItems+5)...)
	l.s.mu.Lock()
	n := len(l.s.reports[l.b].held[l.f])
	l.s.mu.Unlock()
	if n != maxHeldItems {
		t.Fatalf("%d held changes kept, want %d", n, maxHeldItems)
	}
}

// The decision dialog draws at most maxHeldRows rows of a scope and says how many there are in all.
func TestHeldDialogDrawsAtMostMaxRows(t *testing.T) {
	t.Parallel()
	l, it := heldEnv(t)
	items := manyHeld(it, maxHeldRows)
	for range 30 {
		items = append(items, items[0])
	}
	l.heldReport(l.tb, l.f, items...)
	v, err := l.s.heldView(l.f, l.b)
	if err != nil || v == nil || len(v.Groups) != 1 {
		t.Fatalf("view: %v %+v", err, v)
	}
	if g := v.Groups[0]; len(g.Rows) != maxHeldRows || g.Total != maxHeldRows+30 {
		t.Fatalf("rows %d total %d", len(g.Rows), g.Total)
	}
	_, page := l.get(l.web, "/folders/"+itoa(l.f)+"?held="+itoa(l.b))
	if n := strings.Count(page, `name="r"`); n != maxHeldRows {
		t.Errorf("the page has %d row fields, want %d", n, maxHeldRows)
	}
	if !strings.Contains(page, "2030") { // the note under the rows names the total
		t.Error("the page does not name the total")
	}
}

// A report over the limit is cut, and the cut is not silent: the answer counts what was dropped and the folder's
// notices say so, until a report fits again.
func TestHeldReportCutIsShown(t *testing.T) {
	t.Parallel()
	l, _ := heldEnv(t)
	items := make([]proto.HeldItem, maxHeldItems+3)
	for i := range items {
		items[i] = proto.HeldItem{Path: "p" + strconv.Itoa(i), Scope: "d:", Reason: "conflict", Kind: "modify", At: 1}
	}
	st, out := l.api(l.tb, "held", proto.HeldReq{Folder: l.f, Items: items})
	if st != 200 {
		t.Fatalf("held: %d %s", st, out)
	}
	var r map[string]int
	json.Unmarshal(out, &r)
	if r["held"] != maxHeldItems || r["cut"] != 3 {
		t.Fatalf("answer %v", r)
	}
	notes, err := l.s.folderNotices(l.f)
	if err != nil {
		t.Fatal(err)
	}
	cut := 0
	for _, n := range notes {
		if n.Kind == kindHeldCut {
			cut = n.N
		}
	}
	if cut != 3 {
		t.Fatalf("notices %+v", notes)
	}
	_, page := l.get(l.web, "/folders/"+itoa(l.f))
	if !strings.Contains(page, "3 个未显示") {
		t.Errorf("the page does not say that 3 changes are not shown")
	}
	l.heldReport(l.tb, l.f)
	notes, _ = l.s.folderNotices(l.f)
	for _, n := range notes {
		if n.Kind == kindHeldCut {
			t.Errorf("the cut notice stays: %+v", n)
		}
	}
}

// What the device itself left out (HeldReq.Cut) is counted with what the server drops.
func TestHeldReportCutAddsDeviceCut(t *testing.T) {
	t.Parallel()
	l, it := heldEnv(t)
	st, out := l.api(l.tb, "held", proto.HeldReq{Folder: l.f, Items: []proto.HeldItem{it}, Cut: 9000})
	if st != 200 {
		t.Fatalf("held: %d %s", st, out)
	}
	var r map[string]int
	json.Unmarshal(out, &r)
	if r["held"] != 1 || r["cut"] != 9000 {
		t.Fatalf("answer %v", r)
	}
	notes, _ := l.s.folderNotices(l.f)
	for _, n := range notes {
		if n.Kind == kindHeldCut && n.N == 9000 {
			return
		}
	}
	t.Fatalf("notices %+v", notes)
}

// A new device whose file equals the server's is not refused for ownership; a different one still is.
func TestOwnerCheckLetsSameContentThrough(t *testing.T) {
	t.Parallel()
	l, _ := heldEnv(t) // A committed d/x.txt = "one"
	same := l.op("d/x.txt", 0, "one")
	r := l.apiCommit(l.tb, l.f, same)
	if r[0].Reason == proto.ReasonOwner {
		t.Fatalf("same content refused for ownership: %+v", r[0])
	}
	if r[0].Status != proto.StatusConflict {
		t.Fatalf("status %+v", r[0])
	}
	if r := l.apiCommit(l.tb, l.f, l.op("d/x.txt", 0, "two")); r[0].Reason != proto.ReasonOwner {
		t.Fatalf("other content: %+v", r[0])
	}
}

// The page the user returns to after a decision keeps the directory he was in, whatever characters its name has.
func TestResolveRedirectKeepsDirectory(t *testing.T) {
	t.Parallel()
	l, _ := heldEnv(t)
	resp, err := l.web.PostForm(l.ts.URL+"/folders/"+itoa(l.f)+"/resolve", url.Values{"device": {itoa(l.b)}, "dir": {"a&b#c"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc == nil || loc.Query().Get("dir") != "a&b#c" || loc.Query().Get("held") != itoa(l.b) {
		t.Fatalf("returned to %q", resp.Header.Get("Location"))
	}
}

// After "discard all" the dialog shows at once that the decision is sent (the device carries it out on its own time),
// and the group buttons are off, so a second click is not needed nor offered (discard2x).
func TestGroupDecisionShowsWaiting(t *testing.T) {
	t.Parallel()
	l, it := heldEnv(t)
	l.heldReport(l.tb, l.f, it)
	seen := l.seq(l.f)
	_, before := l.get(l.web, "/folders/"+itoa(l.f)+"?held="+itoa(l.b))
	if strings.Contains(before, "data-waiting") {
		t.Fatal("waiting note before any decision")
	}
	l.post(l.web, "/folders/"+itoa(l.f)+"/resolve", url.Values{"device": {itoa(l.b)}, "scope": {"d:D"}, "action": {"discard"}, "seen": {itoa(seen)}})
	_, page := l.get(l.web, "/folders/"+itoa(l.f)+"?held="+itoa(l.b))
	if !strings.Contains(page, "data-waiting") || !strings.Contains(page, `value="discard" disabled`) {
		t.Errorf("the group does not show the decision as sent:\n%.3000s", page)
	}
	l.heldReport(l.tb, l.f)
	_, after := l.get(l.web, "/folders/"+itoa(l.f)+"?held="+itoa(l.b))
	if strings.Contains(after, "data-waiting") || strings.Contains(after, `class="held-grp"`) {
		t.Error("the group is still shown after the device reported")
	}
}
