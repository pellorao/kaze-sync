package client

import (
	"encoding/json"
	"testing"

	"kaze-sync/internal/proto"
)

func bigItem(blocks int) *item {
	h := proto.Hash{1}
	return &item{op: proto.Op{Path: "photos/some/long/directory/name/IMG_0001.jpg", Hash: &h, Blocks: make([]proto.Hash, blocks), Size: int64(blocks) * proto.BlockSize}}
}

// A commit chunk is limited by block hashes as well as by ops: 1000 files of 20 GiB must not make one 27 MB request.
func TestChunkLenLimitsBlocks(t *testing.T) {
	var items []*item
	for range 1000 {
		items = append(items, bigItem(80000)) // 20 GiB each
	}
	if n := chunkLen(items); n != 1 {
		t.Fatalf("files over the block limit go alone, got %d", n)
	}
	items = items[:0]
	for range 1000 {
		items = append(items, bigItem(3000)) // 750 MiB each
	}
	n := chunkLen(items)
	if n != 6 { // 6*3000 <= 20000 < 7*3000
		t.Fatalf("chunk of %d files", n)
	}
	ops := make([]proto.Op, n)
	for i := range ops {
		ops[i] = items[i].op
	}
	b, _ := json.Marshal(proto.CommitReq{Folder: 1, Ops: ops})
	if len(b) > proto.MaxJSONBody/2 {
		t.Fatalf("a full chunk is %d bytes of JSON", len(b))
	}
	small := make([]*item, 2500)
	for i := range small {
		small[i] = bigItem(1)
	}
	if n := chunkLen(small); n != proto.MaxCommitOps {
		t.Fatalf("small files: %d per chunk", n)
	}
	mixed := []*item{bigItem(19000), bigItem(2000), bigItem(1)}
	if n := chunkLen(mixed); n != 1 {
		t.Fatalf("mixed: %d", n)
	}
}
