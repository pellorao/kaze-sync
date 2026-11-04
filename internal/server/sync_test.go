package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

func (e *env) getAPI(token, path string) (int, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/"+path, nil)
	req.Header.Set(proto.VersionHeader, strconv.Itoa(proto.AgentVersion))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (e *env) changes(tok string, folder, since int64) proto.ChangesResp {
	e.t.Helper()
	st, out := e.getAPI(tok, "changes?folder="+itoa(folder)+"&since="+itoa(since))
	if st != 200 {
		e.t.Fatalf("changes: %d %s", st, out)
	}
	var r proto.ChangesResp
	if err := json.Unmarshal(out, &r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestChangesPagesBySeqAndChecksMapping(t *testing.T) {
	e := newEnv(t)
	f, other := e.folder("A"), e.folder("B")
	dev, tok := e.enrollDevice("d1", "linux")
	e.mapFolder(dev, f, "/a")
	h := e.block([]byte("x"))
	for c := 0; c < 6; c++ { // 5001 entries
		var ops []proto.Op
		for i := 0; i < 1000 && c*1000+i <= 5000; i++ {
			ops = append(ops, proto.Op{Path: "f" + strconv.Itoa(c*1000+i), Hash: &h, Blocks: []proto.Hash{h}, Size: 1, MtimeNs: 1})
		}
		e.commit(f, ops...)
	}
	e.commit(f, proto.Op{Path: "f0", BaseSeq: 1}) // delete: a tombstone with a new seq
	first := e.changes(tok, f, 0)
	if len(first.Entries) != proto.MaxChangesPage || !first.More || first.Cursor != first.Entries[len(first.Entries)-1].Seq {
		t.Fatalf("first page: %d entries, more=%v, cursor=%d", len(first.Entries), first.More, first.Cursor)
	}
	second := e.changes(tok, f, first.Cursor)
	if len(second.Entries) != 1 || second.More { // f0 now holds its tombstone, at the newest seq
		t.Fatalf("second page: %d entries, more=%v", len(second.Entries), second.More)
	}
	if last := second.Entries[0]; last.Path != "f0" || last.Hash != nil {
		t.Fatalf("tombstone entry: %+v", last)
	}
	if idle := e.changes(tok, f, second.Cursor); len(idle.Entries) != 0 || idle.Cursor != second.Cursor {
		t.Fatalf("nothing new: %+v", idle)
	}
	if st, _ := e.getAPI(tok, "changes?folder="+itoa(other)+"&since=0"); st != http.StatusForbidden {
		t.Fatalf("unmapped folder: %d", st)
	}
	if st, _ := e.getAPI(tok, "changes?folder="+itoa(f)+"&since=-1"); st != http.StatusBadRequest {
		t.Fatalf("bad since: %d", st)
	}
}

func TestBlocksGetStreamsRequestedBlocks(t *testing.T) {
	e := newEnv(t)
	_, tok := e.enrollDevice("d1", "linux")
	h1, h2 := e.block([]byte("first block")), e.block([]byte("second"))
	st, body := e.api(tok, "blocks/get", proto.BlocksGetReq{Hashes: []proto.Hash{h2, h1}})
	if st != 200 {
		t.Fatalf("blocks/get: %d %s", st, body)
	}
	r := bytes.NewReader(body)
	buf := make([]byte, proto.BlockSize)
	for _, want := range []struct {
		h proto.Hash
		s string
	}{{h2, "second"}, {h1, "first block"}} {
		got, data, err := proto.ReadBlock(r, buf)
		if err != nil || got != want.h || string(data) != want.s {
			t.Fatalf("block: %v %v %q", got, err, data)
		}
	}
	missing := proto.Hash{9}
	if st, _ := e.api(tok, "blocks/get", proto.BlocksGetReq{Hashes: []proto.Hash{h1, missing}}); st == 200 {
		t.Fatal("a request with an unknown block succeeded")
	}
	tooMany := make([]proto.Hash, proto.MaxGetBlocks+1)
	if st, _ := e.api(tok, "blocks/get", proto.BlocksGetReq{Hashes: tooMany}); st != http.StatusBadRequest {
		t.Fatalf("too many hashes: %d", st)
	}
	if st, _ := e.api("", "blocks/get", proto.BlocksGetReq{Hashes: []proto.Hash{h1}}); st != http.StatusUnauthorized {
		t.Fatalf("no token: %d", st)
	}
}

// A request is bounded by the bytes of its response, not by the number of blocks: many small blocks fit, too
// much data does not.
func TestBlocksGetBoundsTheResponse(t *testing.T) {
	e := newEnv(t)
	_, tok := e.enrollDevice("d1", "linux")
	var small, big []proto.Hash
	for i := 0; i < 1000; i++ {
		small = append(small, e.block([]byte("small block "+itoa(int64(i)))))
	}
	for i := 0; i < proto.MaxBlockBody/proto.BlockSize+1; i++ { // sparse files: only their size counts
		h := proto.Hash(sha256.Sum256([]byte("big " + itoa(int64(i)))))
		x := h.String()
		p := filepath.Join(e.s.varDir, "blobs", x[:2], x[2:4], x)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, nil, 0o644); err != nil || os.Truncate(p, proto.BlockSize) != nil {
			t.Fatal(err)
		}
		big = append(big, h)
	}
	if st, body := e.api(tok, "blocks/get", proto.BlocksGetReq{Hashes: small}); st != 200 {
		t.Fatalf("1000 small blocks: %d %s", st, body)
	}
	if st, _ := e.api(tok, "blocks/get", proto.BlocksGetReq{Hashes: big}); st != http.StatusBadRequest {
		t.Fatalf("more than MaxBlockBody bytes: %d", st)
	}
}

func TestPollWakesOnCommitAndOnlyForNews(t *testing.T) {
	e := newEnv(t)
	e.s.PollTimeout = 2 * time.Second
	f := e.folder("A")
	dev, tok := e.enrollDevice("d1", "linux")
	e.mapFolder(dev, f, "/a")
	poll := func(cursor int64) chan proto.PollResp {
		ch := make(chan proto.PollResp, 1)
		go func() {
			_, out := e.api(tok, "poll", proto.PollReq{ConfigVersion: metaInt(e.s.db, "config_version"), Cursors: map[int64]int64{f: cursor}})
			var pr proto.PollResp
			json.Unmarshal(out, &pr)
			ch <- pr
		}()
		return ch
	}
	ch := poll(0)
	select {
	case <-ch:
		t.Fatal("poll returned although nothing happened")
	case <-time.After(200 * time.Millisecond):
	}
	e.commit(f, e.op("a", 0, "hello"))
	select {
	case pr := <-ch:
		if pr.Cursors[f] != 1 || pr.Config != nil {
			t.Fatalf("poll answer %+v", pr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not wake on a commit")
	}
	select { // a client that already has the latest seq keeps waiting
	case <-poll(1):
		t.Fatal("poll returned for a client that is up to date")
	case <-time.After(200 * time.Millisecond):
	}
	if pr := <-poll(0); pr.Cursors[f] != 1 { // a client that is behind gets the answer at once
		t.Fatalf("behind: %+v", pr)
	}
}

func (e *env) report(tok string, folder int64, errs ...proto.FileError) {
	e.t.Helper()
	st, out := e.api(tok, "poll", proto.PollReq{ConfigVersion: metaInt(e.s.db, "config_version"), Cursors: map[int64]int64{folder: e.seq(folder)},
		Folders: []proto.FolderState{{ID: folder, State: "error", Errors: errs}}})
	if st != 200 {
		e.t.Fatalf("poll: %d %s", st, out)
	}
}

func (e *env) seq(folder int64) int64 {
	db, err := e.s.folderDB(folder)
	if err != nil {
		e.t.Fatal(err)
	}
	var n int64
	db.QueryRow(`SELECT coalesce(max(seq), 0) FROM files`).Scan(&n)
	return n
}

func TestNotificationsFromEverySource(t *testing.T) {
	e := newEnv(t)
	e.s.PollTimeout = 50 * time.Millisecond
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	dev, tok := e.enrollDevice("laptop", "linux")
	e.mapFolder(dev, f, "/docs")
	e.report(tok, f,
		proto.FileError{Path: "dir/bad:name.txt", Kind: proto.KindRejected, Message: "illegal name", Since: e.clk.Now().Unix()},
		proto.FileError{Path: "db/x.sqlite", Kind: proto.KindIO, Message: "disk full", Since: e.clk.Now().Unix()},
		proto.FileError{Kind: proto.KindFolder, Message: "marker file missing", Since: e.clk.Now().Unix()},
		proto.FileError{Path: "open.db", Kind: proto.KindInUse, Message: "in use", Since: e.clk.Now().Unix()},
		proto.FileError{Path: "huge.iso", Kind: proto.KindSkipped, Message: "too large", Since: e.clk.Now().Unix()})
	e.commit(f, e.op("a.conflict-laptop-20261101-120000.txt", 0, "copy"))

	_, over := e.get(c, "/")
	// Errors: rejected, io, folder, conflict copy; warnings: in use and skipped.
	if !strings.Contains(over, `<a href="/folders/`+itoa(f)+`">Docs</a></td><td>4</td><td>2</td>`) {
		t.Fatalf("overview summary:\n%s", over)
	}
	_, page := e.get(c, "/folders/"+itoa(f))
	for _, want := range []string{"illegal name", "disk full", "marker file missing", "in use", "too large", "冲突副本",
		`href="/folders/` + itoa(f) + `?dir=dir"`, `href="/folders/` + itoa(f) + `?dir=db"`, `href="/devices/` + itoa(dev) + `"`,
		`href="/folders/` + itoa(f) + `?dir="`, "laptop"} {
		if !strings.Contains(page, want) {
			t.Errorf("folder page lacks %q", want)
		}
	}

	// A conflict copy survives a restart of the server: nothing is stored apart from the file rows.
	e.restart()
	e.login(c, "pw") // sessions are in memory
	_, over = e.get(c, "/")
	if !strings.Contains(over, `Docs</a></td><td>1</td><td>0</td>`) {
		t.Fatalf("after restart the reports are gone but the conflict copy stays:\n%s", over)
	}

	// Removing the cause removes the notice.
	e.report(tok, f)
	e.commit(f, proto.Op{Path: "a.conflict-laptop-20261101-120000.txt", BaseSeq: 1})
	_, over = e.get(c, "/")
	if strings.Contains(over, "<h2>通知</h2>") {
		t.Fatalf("notices remain:\n%s", over)
	}
}

func TestOfflineDeviceNoticeSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	dev, tok := e.enrollDevice("old", "linux")
	e.mapFolder(dev, f, "/docs")
	e.report(tok, f)
	e.clk.Add(8 * 24 * time.Hour)
	e.restart()
	e.login(c, "pw") // sessions are in memory
	_, over := e.get(c, "/")
	if !strings.Contains(over, `/devices/`+itoa(dev)+`">old</a> (8 天未连接)`) {
		t.Fatalf("offline notice missing:\n%s", over)
	}
	e.report(tok, f) // the device is back
	_, over = e.get(c, "/")
	if strings.Contains(over, "天未连接") {
		t.Fatalf("offline notice stays after the device polled:\n%s", over)
	}
}

// restart replaces the server by a new one over the same data directory, like a process restart.
func (e *env) restart() {
	e.t.Helper()
	poll := e.s.PollTimeout
	e.s.Close()
	s2, err := New(filepath.Dir(e.s.varDir), e.clk.Now)
	if err != nil {
		e.t.Fatal(err)
	}
	s2.PollTimeout = poll
	e.s = s2
	e.ts.Config.Handler = s2.Handler()
	e.t.Cleanup(func() { s2.Close() })
}
