package server

import (
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

const bigBlocks = 70000 // past the 4 MiB request limit the old single commit hit (about 60000 blocks)

type stageEnv struct {
	*env
	folder  int64
	dev     [2]int64
	tok     [2]string
	h       proto.Hash
	hashes  []proto.Hash
	size    int64
	id, id2 string
}

func newStageEnv(t *testing.T) *stageEnv {
	e := newEnv(t)
	se := &stageEnv{env: e, folder: e.folder("f")}
	for i, n := range []string{"a", "b"} {
		se.dev[i], se.tok[i] = e.enrollDevice(n, "linux")
		e.mapFolder(se.dev[i], se.folder, "/x"+n)
	}
	// One stored block stands for all of them: the server checks the number of blocks, not their length.
	se.h = e.block([]byte("x"))
	se.hashes = make([]proto.Hash, bigBlocks)
	for i := range se.hashes {
		se.hashes[i] = se.h
	}
	se.size = int64(bigBlocks) * proto.BlockSize
	se.id = strings.Repeat("ab", 16)
	se.id2 = strings.Repeat("cd", 16)
	return se
}

func (se *stageEnv) stage(dev int, id string, off int, hs []proto.Hash) proto.StageResp {
	se.t.Helper()
	code, out := se.api(se.tok[dev], "stage", proto.StageReq{Folder: se.folder, ID: id, Offset: off, Size: se.size, Blocks: hs})
	if code != 200 {
		se.t.Fatalf("stage: %d %s", code, out)
	}
	var r proto.StageResp
	json.Unmarshal(out, &r)
	return r
}

func (se *stageEnv) commitStage(dev int, id string) proto.Result {
	se.t.Helper()
	h := proto.Hash(sha256.Sum256([]byte("whole")))
	code, out := se.api(se.tok[dev], "commit", proto.CommitReq{Folder: se.folder, Ops: []proto.Op{{Path: "big", Hash: &h, Size: se.size, MtimeNs: 1, Stage: id}}})
	if code != 200 {
		se.t.Fatalf("commit: %d %s", code, out)
	}
	var r proto.CommitResp
	json.Unmarshal(out, &r)
	return r.Results[0]
}

func (se *stageEnv) sendAll(dev int, id string) {
	for off := 0; off < bigBlocks; off += proto.MaxStageBlocks {
		r := se.stage(dev, id, off, se.hashes[off:min(off+proto.MaxStageBlocks, bigBlocks)])
		if r.Reason != "" || r.Have != min(off+proto.MaxStageBlocks, bigBlocks) {
			se.t.Fatalf("segment at %d: %+v", off, r)
		}
	}
}

func TestStageBigFileCommits(t *testing.T) {
	t.Parallel()
	se := newStageEnv(t)
	se.sendAll(0, se.id)
	r := se.commitStage(0, se.id)
	if r.Status != proto.StatusOK {
		t.Fatalf("commit: %+v", r)
	}
	db, _ := se.s.folderDB(se.folder)
	var n int
	db.QueryRow(`SELECT length(blocks) FROM files WHERE path='big'`).Scan(&n)
	if n != bigBlocks*proto.HashLen {
		t.Fatalf("stored %d bytes of block list", n)
	}
	if _, ok := stageHave(se.s.stagePath(se.dev[0], se.folder, se.id)); ok {
		t.Fatal("staged file kept after the commit")
	}
}

func TestStageResendAndNeed(t *testing.T) {
	t.Parallel()
	se := newStageEnv(t)
	missing := proto.Hash{9}
	hs := append([]proto.Hash{missing}, se.hashes[:10]...)
	r := se.stage(0, se.id, 0, hs)
	if len(r.Need) != 1 || r.Need[0] != missing || r.Have != 11 {
		t.Fatalf("%+v", r)
	}
	if r = se.stage(0, se.id, 0, hs); r.Have != 11 { // a resent segment overwrites, it does not grow the list
		t.Fatalf("resend: %+v", r)
	}
}

func TestStageGapRefused(t *testing.T) {
	t.Parallel()
	se := newStageEnv(t)
	if r := se.stage(0, se.id, 5, se.hashes[:5]); r.Reason != proto.ReasonGap || r.Have != 0 {
		t.Fatalf("new stage at offset 5: %+v", r)
	}
	se.stage(0, se.id, 0, se.hashes[:100])
	if r := se.stage(0, se.id, 200, se.hashes[:5]); r.Reason != proto.ReasonGap || r.Have != 100 {
		t.Fatalf("hole: %+v", r)
	}
	if r := se.commitStage(0, se.id); r.Status != proto.StatusRejected || r.Reason != proto.ReasonNoStage {
		t.Fatalf("incomplete list committed: %+v", r)
	}
}

func TestStageBelongsToDevice(t *testing.T) {
	t.Parallel()
	se := newStageEnv(t)
	se.sendAll(0, se.id)
	if r := se.commitStage(1, se.id); r.Status != proto.StatusRejected || r.Reason != proto.ReasonNoStage {
		t.Fatalf("device b committed device a's stage: %+v", r)
	}
	if r := se.stage(1, se.id, 20000, se.hashes[:5]); r.Reason != proto.ReasonGap || r.Have != 0 {
		t.Fatalf("device b appended to device a's stage: %+v", r)
	}
	if r := se.commitStage(0, se.id); r.Status != proto.StatusOK {
		t.Fatalf("owner: %+v", r)
	}
}

func TestStageExpires(t *testing.T) {
	t.Parallel()
	se := newStageEnv(t)
	se.sendAll(0, se.id)
	old := time.Now().Add(-stageTTL - time.Hour)
	p := se.s.stagePath(se.dev[0], se.folder, se.id)
	os.Chtimes(p, old, old)
	if r := se.commitStage(0, se.id); r.Status != proto.StatusRejected || r.Reason != proto.ReasonNoStage {
		t.Fatalf("expired stage committed: %+v", r)
	}
	if n := se.s.sweepStages(); n != 1 {
		t.Fatalf("swept %d", n)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("expired stage kept")
	}
}

func TestStageLimits(t *testing.T) {
	t.Parallel()
	se := newStageEnv(t)
	for i := 0; i < proto.MaxStagesPerDevice; i++ {
		id := strings.Repeat(string(rune('a'+i)), 32)
		if r := se.stage(0, id, 0, se.hashes[:1]); r.Reason != "" {
			t.Fatalf("stage %d: %+v", i, r)
		}
	}
	if r := se.stage(0, se.id, 0, se.hashes[:1]); r.Reason != proto.ReasonTooManyStages {
		t.Fatalf("fifth stage: %+v", r)
	}
	if r := se.stage(1, se.id, 0, se.hashes[:1]); r.Reason != "" { // the limit is per device
		t.Fatalf("other device: %+v", r)
	}
	se.size = (proto.MaxFileBlocks + 1) * proto.BlockSize
	if r := se.stage(1, se.id2, 0, se.hashes[:1]); r.Reason != proto.ReasonTooLarge {
		t.Fatalf("too large: %+v", r)
	}
	if code, _ := se.api(se.tok[1], "stage", proto.StageReq{Folder: se.folder, ID: "short", Size: se.size}); code != 400 {
		t.Fatalf("bad id: %d", code)
	}
}

func TestStageNoSpace(t *testing.T) {
	se := newStageEnv(t)
	freeSpace = func(string) (uint64, bool) { return 1 << 30, true }
	t.Cleanup(func() { freeSpace = freeBytes })
	// The first segment is all missing blocks, so the whole file (70000 x 256 KiB) is needed: far above 1 GiB.
	missing := make([]proto.Hash, 100)
	for i := range missing {
		missing[i] = proto.Hash{byte(i + 1), 1}
	}
	if r := se.stage(0, se.id, 0, missing); r.Reason != proto.ReasonNoSpace {
		t.Fatalf("%+v", r)
	}
	if _, ok := stageHave(se.s.stagePath(se.dev[0], se.folder, se.id)); ok {
		t.Fatal("a refused segment was stored")
	}
	if r := se.stage(0, se.id, 0, se.hashes[:100]); r.Reason != "" { // blocks the server has need no space
		t.Fatalf("%+v", r)
	}
}

func TestGCKeepsStagedBlocks(t *testing.T) {
	t.Parallel()
	se := newStageEnv(t)
	old := time.Now().Add(-3 * time.Hour)
	blobPathOf(t, se.s.varDir, se.h, old)
	se.stage(0, se.id, 0, se.hashes[:10])
	if _, err := se.s.gc(); err != nil {
		t.Fatal(err)
	}
	if ok, _ := se.s.blobs.Use(se.h); !ok {
		t.Fatal("GC removed a block a staged list references")
	}
	blobPathOf(t, se.s.varDir, se.h, old)
	os.Chtimes(se.s.stagePath(se.dev[0], se.folder, se.id), old.Add(-30*time.Hour), old.Add(-30*time.Hour))
	if _, err := se.s.gc(); err != nil {
		t.Fatal(err)
	}
	if ok, _ := se.s.blobs.Use(se.h); ok {
		t.Fatal("block of an expired stage kept")
	}
}

// blobPathOf sets the modification time of the stored block h.
func blobPathOf(t *testing.T, varDir string, h proto.Hash, when time.Time) {
	t.Helper()
	name := hexOf(h[:])
	found := false
	filepath.WalkDir(filepath.Join(varDir, "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == name {
			found = true
			os.Chtimes(p, when, when)
		}
		return nil
	})
	if !found {
		t.Fatal("block file not found")
	}
}
