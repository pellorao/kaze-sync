package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// getCounter counts the requests and requested hashes of /blocks/get.
type getCounter struct {
	base     http.RoundTripper
	requests atomic.Int64
	hashes   atomic.Int64
}

func (c *getCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/blocks/get") {
		c.requests.Add(1)
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		var req proto.BlocksGetReq
		json.Unmarshal(b, &req)
		c.hashes.Add(int64(len(req.Hashes)))
	}
	return c.base.RoundTrip(r)
}

// pair is two devices holding the same new folder, each over an empty in-memory file system.
func pair(t *testing.T) (e *cenv, f int64, g1, g2 *engine, gets *getCounter) {
	t.Helper()
	e = newCEnv(t)
	f = e.newFolder("Docs")
	e.setRule(f, simRule{Mode: proto.ModeFree}, nil) // these tests are not about write control: each file is its own scope
	var engines [2]*engine
	for i := range engines {
		dev, api := e.newDevice()
		if i == 1 {
			gets = &getCounter{base: api.HC.Transport}
			api.HC.Transport = gets
		}
		e.mapFolder(dev, f, "/data/docs", true)
		engines[i] = e.engine(api, f, nil, true)
		engines[i].f.DeviceName = dev.Name
		engines[i].fs = cfs.NewMem(cfs.MemOptions{Clock: 1_000_000_000_000 + int64(i)*1_000_000_000})
		engines[i].restart(api, proto.FolderConfig{ID: f, Adopt: true}, engines[i].fs)
		engines[i].f.DeviceName = dev.Name
	}
	return e, f, engines[0], engines[1], gets
}

func (g *engine) pull() Report {
	g.t.Helper()
	rep, err := g.f.Pull(context.Background())
	if err != nil {
		g.t.Fatal(err)
	}
	return rep
}

// sync runs Scan and Pull until neither has anything left to do.
func (g *engine) sync() {
	g.t.Helper()
	for i := 0; i < 6; i++ {
		rep := g.scan()
		rep = g.pull()
		if !rep.Retry && rep.PendingUp == 0 && rep.PendingDown == 0 {
			return
		}
	}
}

func (g *engine) read(p string) (string, bool) {
	g.t.Helper()
	r, err := g.fs.Open(p)
	if err != nil {
		return "", false
	}
	defer r.Close()
	var b bytes.Buffer
	b.ReadFrom(r)
	return b.String(), true
}

func TestPullAppliesAdditionsModificationsAndDeletions(t *testing.T) {
	t.Parallel()
	_, _, g1, g2, _ := pair(t)
	g1.put("a.txt", []byte("one"))
	g1.put("dir/sub/b.txt", []byte("bee"))
	g1.scan()
	g2.scan()
	if rep := g2.pull(); rep.PendingDown != 0 || len(rep.Errors) != 0 {
		t.Fatalf("pull: %+v", rep)
	}
	for p, want := range map[string]string{"a.txt": "one", "dir/sub/b.txt": "bee"} {
		if got, ok := g2.read(p); !ok || got != want {
			t.Fatalf("%s = %q %v", p, got, ok)
		}
	}
	if rep := g2.scan(); rep.Committed != 0 { // the downloaded files are not changes
		t.Fatalf("downloads committed back: %+v", rep)
	}
	if rep := g1.pull(); rep.PendingDown != 0 { // own changes coming back are skipped
		t.Fatalf("echo: %+v", rep)
	}

	g1.put("a.txt", []byte("two"))
	g1.fs.Remove("dir/sub/b.txt")
	g1.scan()
	g2.pull()
	if got, _ := g2.read("a.txt"); got != "two" {
		t.Fatalf("modified file = %q", got)
	}
	if _, ok := g2.read("dir/sub/b.txt"); ok {
		t.Fatal("deleted file still there")
	}
	if _, err := g2.fs.Stat("dir"); !errorsIsNotExist(err) { // emptied directories go with their last file
		t.Fatalf("empty directory kept: %v", err)
	}
}

func errorsIsNotExist(err error) bool {
	return err != nil && strings.Contains(err.Error(), fs.ErrNotExist.Error())
}

func TestPullLeavesLocalChangesAlone(t *testing.T) {
	t.Parallel()
	_, f, g1, g2, _ := pair(t)
	_ = f
	g1.put("a.txt", []byte("one"))
	g1.scan()
	g2.sync()
	g1.put("a.txt", []byte("server side"))
	g1.scan()
	g2.put("a.txt", []byte("local edit"))
	g2.pull()
	if got, _ := g2.read("a.txt"); got != "local edit" {
		t.Fatalf("Pull overwrote an unscanned local edit: %q", got)
	}
}

func TestPullHoldsFilesInUse(t *testing.T) {
	t.Parallel()
	_, _, g1, g2, _ := pair(t)
	g1.put("a.txt", []byte("one"))
	g1.scan()
	g2.sync()
	g1.put("a.txt", []byte("two"))
	g1.scan()
	g2.fs.Lock("a.txt")
	rep := g2.pull()
	if len(rep.Errors) != 1 || rep.Errors[0].Kind != proto.KindInUse || rep.Retry || !rep.Slow || rep.PendingDown != 1 {
		t.Fatalf("locked file: %+v", rep)
	}
	g2.fs.Unlock("a.txt")
	if rep := g2.pull(); len(rep.Errors) != 0 || rep.PendingDown != 0 {
		t.Fatalf("after unlock: %+v", rep)
	}
	if got, _ := g2.read("a.txt"); got != "two" {
		t.Fatalf("a.txt = %q", got)
	}
}

func TestMovesDownloadNoBlocks(t *testing.T) {
	t.Parallel()
	_, _, g1, g2, gets := pair(t)
	for i := 0; i < 40; i++ {
		g1.put("proj/f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".bin", pseudoRandom(1000+i, byte(i)))
	}
	g1.scan()
	g2.sync()
	before := gets.hashes.Load()
	g1.fs.MkdirAll("moved/deep")
	g1.fs.Rename("proj", "moved/deep/proj")
	g1.scan()
	g2.sync()
	if got := gets.hashes.Load() - before; got != 0 {
		t.Fatalf("moving a directory downloaded %d blocks", got)
	}
	if _, ok := g2.read("moved/deep/proj/faa.bin"); !ok {
		t.Fatalf("moved file missing: %v", treeOf(g2.fs))
	}
	if _, ok := g2.read("proj/faa.bin"); ok {
		t.Fatal("old path still there")
	}
}

func TestPullDownloadsOnlyChangedBlocks(t *testing.T) {
	t.Parallel()
	_, _, g1, g2, gets := pair(t)
	data := pseudoRandom(6*miB, 3)
	g1.put("big.db", data)
	g1.scan()
	g2.sync()
	if gets.hashes.Load() != 6 {
		t.Fatalf("first download requested %d blocks, want 6", gets.hashes.Load())
	}
	mod := bytes.Clone(data)
	copy(mod[2*miB+10:], pseudoRandom(5000, 8)) // block 2 only
	g1.put("big.db", mod)
	g1.scan()
	gets.hashes.Store(0)
	g2.sync()
	if gets.hashes.Load() != 1 {
		t.Fatalf("modified one block, downloaded %d", gets.hashes.Load())
	}
	if got, _ := g2.read("big.db"); got != string(mod) {
		t.Fatal("content differs after a partial download")
	}
}

// Two devices change a file from the same base: the first commit wins, the other change is held and no conflict copy
// is made (design 6.1, D3). The user's decision then ends it: discarding takes the server's version.
func TestConflictIsHeldNotCopied(t *testing.T) {
	t.Parallel()
	e, f, g1, g2, _ := pair(t)
	g1.put("doc.txt", []byte("base"))
	g1.scan()
	g2.sync()
	g1.put("doc.txt", []byte("from one"))
	g2.put("doc.txt", []byte("from two"))
	g1.sync() // dev1 commits first and wins
	g2.sync()
	g1.sync()
	if n := len(treeOf(g2.fs)); n != 1 {
		t.Fatalf("files on dev2: %v", treeOf(g2.fs))
	}
	if got, _ := g2.read("doc.txt"); got != "from two" {
		t.Fatalf("a held file was changed on dev2: %q", got)
	}
	if got, _ := e.serverFile(f, "doc.txt"); string(got) != "from one" {
		t.Fatalf("server doc.txt = %q", got)
	}
	if rows, _ := g2.f.DB.held(""); len(rows) != 1 || rows[0].Reason != "conflict" {
		t.Fatalf("held on dev2: %+v", rows)
	}
	if err := g2.f.Resolve(context.Background(), proto.Action{Path: "doc.txt", Action: ActDiscard, Save: true}); err != nil {
		t.Fatal(err)
	}
	g2.sync()
	if got, _ := g2.read("doc.txt"); got != "from one" {
		t.Fatalf("doc.txt on dev2 after the discard = %q", got)
	}
	if got, ok := g2.read(".syncsaved/doc.txt"); ok && got != "from two" {
		t.Fatalf("saved copy = %q", got)
	}
	if rows, _ := g2.f.DB.held(""); len(rows) != 0 {
		t.Fatalf("still held: %+v", rows)
	}
}

// A modification against a deletion is a conflict like any other: the later writer is held, and overwriting keeps
// the modification.
func TestDeleteAgainstModify(t *testing.T) {
	t.Parallel()
	e, f, g1, g2, _ := pair(t)
	ctx := context.Background()
	g1.put("a.txt", []byte("base"))
	g1.put("b.txt", []byte("base"))
	g1.scan()
	g2.sync()
	// dev2 deletes a.txt after dev1 modified it: the deletion is held, the file stays on both.
	g1.put("a.txt", []byte("edited"))
	g1.scan()
	g2.fs.Remove("a.txt")
	g2.sync()
	if rows, _ := g2.f.DB.held(""); len(rows) != 1 || rows[0].Kind != "delete" {
		t.Fatalf("held on dev2: %+v", rows)
	}
	if got, _ := e.serverFile(f, "a.txt"); string(got) != "edited" {
		t.Fatalf("server a.txt = %q", got)
	}
	if err := g2.f.Resolve(ctx, proto.Action{Path: "a.txt", Action: ActDiscard, Save: false}); err != nil {
		t.Fatal(err)
	}
	if got, _ := g2.read("a.txt"); got != "edited" {
		t.Fatalf("modify vs delete on the deleting side: %q", got)
	}
	// dev1 deletes b.txt, dev2 modifies it: the modification is held; overwriting brings the file back.
	g1.fs.Remove("b.txt")
	g1.scan()
	g2.put("b.txt", []byte("kept"))
	g2.sync()
	if err := g2.f.Resolve(ctx, overwriteAct(t, g2.f, "b.txt")); err != nil {
		t.Fatal(err)
	}
	g1.sync()
	if got, _ := g1.read("b.txt"); got != "kept" {
		t.Fatalf("delete vs modify: %q", got)
	}
}

func TestSecondDeviceAdoptsIdenticalFilesWithoutDownloads(t *testing.T) {
	t.Parallel()
	e := newCEnv(t)
	f := e.newFolder("Docs")
	files := map[string][]byte{"a.bin": pseudoRandom(3*miB+7, 1), "d/b.txt": []byte("bee"), "d/e/c.txt": []byte("sea")}
	var gets *getCounter
	var engines [2]*engine
	for i := range engines {
		dev, api := e.newDevice()
		if i == 1 {
			gets = &getCounter{base: api.HC.Transport}
			api.HC.Transport = gets
		}
		e.mapFolder(dev, f, "/data/docs", true)
		g := e.engine(api, f, nil, true)
		for p, b := range files {
			g.put(p, b) // the same files, with different mtimes
		}
		engines[i] = g
	}
	ct := &countTransport{base: engines[1].f.API.HC.Transport}
	engines[1].f.API.HC.Transport = ct
	engines[0].sync()
	engines[1].sync()
	if gets.hashes.Load() != 0 || ct.packets.Load() != 0 {
		t.Fatalf("adopting identical files moved blocks: downloaded %d, uploaded %d", gets.hashes.Load(), ct.packets.Load())
	}
	for p, b := range files {
		if got, _ := engines[1].read(p); got != string(b) {
			t.Fatalf("%s differs", p)
		}
	}
	if rep := engines[1].scan(); rep.Committed != 0 {
		t.Fatalf("adopted files committed again: %+v", rep)
	}
}

// A chmod alone (same size, same mtime) is a change: it commits and the other device gets the bit (design 6.4).
func TestChmodOnlyIsSynced(t *testing.T) {
	t.Parallel()
	e, f, g1, g2, _ := pair(t)
	g1.put("run.sh", []byte("#!/bin/sh\n"))
	st, err := g1.fs.Stat("run.sh")
	if err != nil {
		t.Fatal(err)
	}
	g1.scan()
	g2.sync()
	if rep := g1.scan(); rep.Committed != 0 {
		t.Fatalf("idle scan committed: %+v", rep)
	}
	if err := g1.fs.SetMeta("run.sh", st.MtimeNs, true); err != nil {
		t.Fatal(err)
	}
	if rep := g1.scan(); rep.Committed != 1 {
		t.Fatalf("chmod +x was not committed: %+v", rep)
	}
	if n := e.count(f, `SELECT count(*) FROM files WHERE path='run.sh' AND exec=1`); n != 1 {
		t.Fatalf("server exec bit not set (%d rows)", n)
	}
	g2.sync()
	if st2, err := g2.fs.Stat("run.sh"); err != nil || !st2.Exec {
		t.Fatalf("the other device did not get the bit: %+v %v", st2, err)
	}
	if rep := g2.scan(); rep.Committed != 0 {
		t.Fatalf("the downloaded bit came back as a change: %+v", rep)
	}
}
