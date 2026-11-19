package client

import "testing"

// A held deletion whose path the other device then deleted too is settled: both ends lack the file and the server
// has it deleted, so nothing is left to decide (design 2.8.6 "same as the server", 2.8.9 "delete vs deleted").
func TestHeldDeleteClearsWhenServerDeletedToo(t *testing.T) {
	a, b := singlePair(t)
	a.put("x.txt", []byte("one"))
	a.sync()
	b.sync()
	a.put("x.txt", []byte("two")) // b has not seen this when it deletes
	a.sync()
	b.fs.Remove("x.txt")
	b.sync()
	rows, _ := b.f.DB.held("")
	if len(rows) != 1 || rows[0].Kind != "delete" {
		t.Fatalf("setup: held %+v", rows)
	}
	a.fs.Remove("x.txt")
	a.sync()
	for i := 0; i < 3; i++ {
		b.sync()
	}
	rows, _ = b.f.DB.held("")
	if len(rows) != 0 || len(b.f.frozen) != 0 {
		t.Fatalf("stale held %+v frozen %v", rows, b.f.frozen)
	}
}
