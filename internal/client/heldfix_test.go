package client

import (
	"path/filepath"
	"testing"

	"kaze-sync/internal/proto"
)

// Settling a held deletion forgets the unlinked row of the path too, as applyDeleted does: a link the server deleted
// is not made later by retryUnlinked.
func TestSettleHeldDropsUnlinked(t *testing.T) {
	t.Parallel()
	db, err := openLocal(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	if _, err := db.db.Exec(`INSERT INTO held(path, scope, reason, base_seq, kind, size, mtime_ns, at) VALUES('l', '', 'r', 1, 'delete', 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if err := db.putUnlinked(unlinkedRow{Path: "l", Seq: 1, Hash: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.settleHeld([]string{"l"}, map[string]bool{}, map[string]heldDel{"l": {}}, nil); err != nil {
		t.Fatal(err)
	}
	if rows, _ := db.unlinked(); len(rows) != 0 {
		t.Fatalf("unlinked rows left: %+v", rows)
	}
}

// Until a conflict has shown the server leaves the block lists out, a probe is one path: an older server answers with
// the whole list of a file that may be far larger than its base.
func TestProbeBatchOneUntilBare(t *testing.T) {
	t.Parallel()
	ask := []string{"a", "b", "c"}
	if got := probeBatch(ask, map[string]heldDel{}, false); got != 1 {
		t.Errorf("unknown server: %d paths, want 1", got)
	}
	if got := probeBatch(ask[:0], nil, false); got != 0 {
		t.Errorf("nothing to ask: %d", got)
	}
	h := proto.Hash{1}
	for _, c := range []struct {
		r    proto.Result
		want bool
	}{
		{proto.Result{Status: proto.StatusConflict, Current: &proto.Entry{Size: 5, Hash: &h}}, true},
		{proto.Result{Status: proto.StatusConflict, Current: &proto.Entry{Size: 5, Hash: &h, Blocks: []proto.Hash{h}}}, false}, // older server
		{proto.Result{Status: proto.StatusConflict, Current: &proto.Entry{Hash: &h}}, false},                                   // empty file: no proof
		{proto.Result{Status: proto.StatusOK, Current: &proto.Entry{Size: 5}}, false},
		{proto.Result{Status: proto.StatusOK}, false},
	} {
		if got := bareAnswer(c.r); got != c.want {
			t.Errorf("%+v: %v, want %v", c.r, got, c.want)
		}
	}
}
