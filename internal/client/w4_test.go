package client

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// A decision is carried out only on the file the user saw: when the local file or the server's version changed since,
// nothing happens and the held list says why.
func TestResolveSnapshotGuards(t *testing.T) {
	e, f, g1, g2, _ := pair(t)
	ctx := context.Background()
	g1.put("doc.txt", []byte("base"))
	g1.scan()
	g2.sync()
	g1.put("doc.txt", []byte("from one"))
	g2.put("doc.txt", []byte("from two"))
	g1.sync()
	g2.sync()
	rows, _ := g2.f.DB.held()
	if len(rows) != 1 {
		t.Fatalf("held: %+v", rows)
	}
	h := rows[0]
	cur := int64(2)
	// The local file is not the one the user saw.
	err := g2.f.Resolve(ctx, proto.Action{Path: "doc.txt", Action: ActOverwrite, Expect: &cur, Size: h.Size + 1, MtimeNs: h.MtimeNs})
	if err == nil || items(g2.f)["doc.txt"] != "local" {
		t.Fatalf("local snapshot: %v %v", err, items(g2.f))
	}
	// The server's version is not the one the user saw.
	stale := cur - 1
	err = g2.f.Resolve(ctx, proto.Action{Path: "doc.txt", Action: ActOverwrite, Expect: &stale, Size: h.Size, MtimeNs: h.MtimeNs})
	if err == nil || items(g2.f)["doc.txt"] != "server" {
		t.Fatalf("server snapshot: %v %v", err, items(g2.f))
	}
	if got, _ := e.serverFile(f, "doc.txt"); string(got) != "from one" {
		t.Fatalf("a refused decision reached the server: %q", got)
	}
	if err := g2.f.Resolve(ctx, proto.Action{Path: "doc.txt", Action: ActOverwrite, Expect: &cur, Size: h.Size, MtimeNs: h.MtimeNs}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.serverFile(f, "doc.txt"); string(got) != "from two" {
		t.Fatalf("overwrite: %q", got)
	}
	if len(items(g2.f)) != 0 {
		t.Fatalf("still held: %v", items(g2.f))
	}
}

// items is the held list as the server gets it: path -> note.
func items(f *Folder) map[string]string {
	out := map[string]string{}
	list, _, _ := f.HeldItems()
	for _, it := range list {
		out[it.Path] = it.Note
	}
	return out
}

// Overwriting a change of the other kind removes what stands in its way on the server first: a file where the
// held change has a directory, and the files below a name where it has a file.
func TestOverwriteTypeClearsBlockingEntries(t *testing.T) {
	e, f, g1, g2, _ := pair(t)
	ctx := context.Background()
	g1.put("out/x", []byte("below"))
	g1.put("out/sub/y", []byte("deeper"))
	g1.put("flat", []byte("file"))
	g1.scan()
	g2.put("out", []byte("a file now"))
	g2.put("flat/inner", []byte("a directory now"))
	g2.scan()
	var held []heldRow
	held, _ = g2.f.DB.held()
	types := map[string]bool{}
	for _, h := range held {
		if h.Reason == "type" {
			types[h.Path] = true
		}
	}
	if !types["out"] || !types["flat/inner"] {
		t.Fatalf("held: %+v", held)
	}
	for _, p := range []string{"out", "flat/inner"} {
		if err := g2.f.Resolve(ctx, overwriteAct(t, g2.f, p)); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	for p, want := range map[string]string{"out": "a file now", "flat/inner": "a directory now"} {
		if got, ok := e.serverFile(f, p); !ok || string(got) != want {
			t.Errorf("%s = %q %v", p, got, ok)
		}
	}
	for _, p := range []string{"out/x", "out/sub/y", "flat"} {
		if _, ok := e.serverFile(f, p); ok {
			t.Errorf("%s is still on the server", p)
		}
	}
}

// The time directories of .syncsaved go after 30 days, by the scan.
func TestScanCleansSavedAfter30Days(t *testing.T) {
	_, _, _, g := setup(t)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	g.f.Now = func() time.Time { return now }
	old, recent := now.Add(-31*24*time.Hour).Format("20060102-150405"), now.Add(-29*24*time.Hour).Format("20060102-150405")
	g.put(".syncsaved/"+old+"/a/b.txt", []byte("old"))
	g.put(".syncsaved/"+recent+"/c.txt", []byte("recent"))
	g.scan()
	if _, err := g.fs.Stat(".syncsaved/" + old); err == nil {
		t.Error("a 31-day-old directory stays")
	}
	if _, err := g.fs.Stat(".syncsaved/" + recent + "/c.txt"); err != nil {
		t.Errorf("a 29-day-old directory went: %v", err)
	}
}

// A new run of the server (its boot id) makes every folder send the held list again; decisions are queued for the
// folder they name.
func TestHandlePollBootAndActions(t *testing.T) {
	a := &Agent{Dir: t.TempDir(), kick: make(chan struct{}, 1), runners: map[int64]*runner{1: {ready: true}, 2: {ready: true}}}
	req := proto.PollReq{}
	a.handlePoll(req, proto.PollResp{Boot: "A"})
	if a.runners[1].resend || a.runners[2].resend {
		t.Fatal("the first boot id is not a restart")
	}
	a.handlePoll(req, proto.PollResp{Boot: "A", Actions: []proto.Action{{Folder: 2, Path: "x", Action: ActDiscard}, {Folder: 9, Path: "y", Action: ActDiscard}}})
	if a.runners[1].resend || len(a.runners[2].actions) != 1 || !a.runners[2].s.full {
		t.Fatalf("actions: %+v %+v", a.runners[1], a.runners[2])
	}
	a.handlePoll(req, proto.PollResp{Boot: "B"})
	if !a.runners[1].resend || !a.runners[2].resend || !a.runners[1].s.pull {
		t.Fatal("a new boot id did not wake the folders")
	}
}

// The whole way: a device holds a change, the user decides in the Web UI, the poll brings the decision, the device
// carries it out and reports the list empty.
func TestAgentCarriesOutAWebDecision(t *testing.T) {
	e := newCEnv(t)
	f := e.newFolder("One")
	devA, apiA := e.newDevice()
	devB, apiB := e.newDevice()
	roots := [2]string{filepath.Join(t.TempDir(), "one"), filepath.Join(t.TempDir(), "one")}
	e.mapFolder(devA, f, roots[0], true)
	e.mapFolder(devB, f, roots[1], true)
	if st := e.setRule(f, simRule{Mode: proto.ModeExclusive, Owner: 0}, []int64{devA.DeviceID}); st != 303 {
		t.Fatalf("rule: %d", st)
	}
	for i, d := range []struct {
		dev Device
		api *API
	}{{devA, apiA}, {devB, apiB}} {
		a := &Agent{Dir: t.TempDir(), API: d.api, NewFS: cfs.OS, OS: runtime.GOOS, DeviceName: d.dev.Name, Stable: 0,
			ScanInterval: time.Hour, RetryAfter: 50 * time.Millisecond, Debounce: 50 * time.Millisecond}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		_ = i
	}
	eventually(t, "both roots", func() bool {
		_, e1 := os.Stat(filepath.Join(roots[0], markerName))
		_, e2 := os.Stat(filepath.Join(roots[1], markerName))
		return e1 == nil && e2 == nil
	})
	writeFile(t, filepath.Join(roots[1], "mine.txt"), "not allowed")
	// The exclusive owner is another device: the write is held. The Web UI lists it.
	page := ""
	eventually(t, "the held change on the page", func() bool {
		_, page = e.get("/folders/" + strconv.FormatInt(f, 10) + "?held=" + strconv.FormatInt(devB.DeviceID, 10))
		return strings.Contains(page, "mine.txt")
	})
	st, body := e.post("/folders/"+strconv.FormatInt(f, 10)+"/resolve", url.Values{"device": {strconv.FormatInt(devB.DeviceID, 10)}, "scope": {"d:"}, "action": {"discard"}, "seen": {"1000000"}, "save": {"1"}})
	if st != 303 {
		t.Fatalf("resolve: %d %s", st, body)
	}
	eventually(t, "the file to be set aside", func() bool {
		_, err := os.Stat(filepath.Join(roots[1], "mine.txt"))
		return err != nil
	})
	eventually(t, "the held list to empty", func() bool {
		_, page = e.get("/folders/" + strconv.FormatInt(f, 10) + "?held=" + strconv.FormatInt(devB.DeviceID, 10))
		return !strings.Contains(page, "mine.txt")
	})
	if m, _ := filepath.Glob(filepath.Join(roots[1], ".syncsaved", "*", "mine.txt")); len(m) != 1 {
		t.Fatalf("saved copies: %v", m)
	}
}

func (e *cenv) get(path string) (int, string) {
	e.t.Helper()
	resp, err := e.admin.Get(e.ts.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The server keeps the held lists in memory only. After a restart (a new boot id in the poll answer) a real agent
// sends its list again, so the Web UI shows the held change without anyone touching the file.
func TestAgentSendsHeldListAgainAfterServerRestart(t *testing.T) {
	e := newCEnv(t)
	e.srv.PollTimeout = 300 * time.Millisecond // a poll hanging on the old server ends soon
	f := e.newFolder("One")
	devA, apiA := e.newDevice()
	devB, apiB := e.newDevice()
	roots := [2]string{filepath.Join(t.TempDir(), "one"), filepath.Join(t.TempDir(), "one")}
	e.mapFolder(devA, f, roots[0], true)
	e.mapFolder(devB, f, roots[1], true)
	if st := e.setRule(f, simRule{Mode: proto.ModeExclusive, Owner: 0}, []int64{devA.DeviceID}); st != 303 {
		t.Fatalf("rule: %d", st)
	}
	for _, d := range []struct {
		dev Device
		api *API
	}{{devA, apiA}, {devB, apiB}} {
		a := &Agent{Dir: t.TempDir(), API: d.api, NewFS: cfs.OS, OS: runtime.GOOS, DeviceName: d.dev.Name, Stable: 0,
			ScanInterval: time.Hour, RetryAfter: 50 * time.Millisecond, Debounce: 50 * time.Millisecond}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	eventually(t, "both roots", func() bool {
		_, e1 := os.Stat(filepath.Join(roots[0], markerName))
		_, e2 := os.Stat(filepath.Join(roots[1], markerName))
		return e1 == nil && e2 == nil
	})
	url := "/folders/" + strconv.FormatInt(f, 10) + "?held=" + strconv.FormatInt(devB.DeviceID, 10)
	listed := func() bool { _, page := e.get(url); return strings.Contains(page, "mine.txt") }
	writeFile(t, filepath.Join(roots[1], "mine.txt"), "not allowed")
	eventually(t, "the held change on the page", listed)
	e.restart()
	if listed() {
		t.Fatal("the restarted server still knows the held list; nothing was lost, so nothing is tested")
	}
	eventually(t, "the held change on the page again", listed)
}

// A note about a decision that was not carried out goes with the held change: when the path is no longer held, a
// later held change of the same path starts without it.
func TestHeldNoteDropsWithTheHeldChange(t *testing.T) {
	_, _, _, g2, _ := pair(t)
	g2.f.heldNote = map[string]string{"gone.txt": "local"}
	if got := items(g2.f); len(got) != 0 || len(g2.f.heldNote) != 0 {
		t.Fatalf("items %v, notes %v", got, g2.f.heldNote)
	}
}

// overwriteAct is the overwrite decision the server sends for the held change of p: the version and the local file
// the user saw are the current ones.
func overwriteAct(t *testing.T, f *Folder, p string) proto.Action {
	t.Helper()
	rows, _ := f.DB.held()
	for _, h := range rows {
		if h.Path == p {
			res, err := f.probe(context.Background(), false, p)
			if err != nil {
				t.Fatal(err)
			}
			cur, seq := res[0].Current, int64(0)
			if cur != nil {
				seq = cur.Seq
			}
			return proto.Action{Path: p, Action: ActOverwrite, Expect: &seq, Size: h.Size, MtimeNs: h.MtimeNs}
		}
	}
	t.Fatalf("%s is not held", p)
	return proto.Action{}
}
