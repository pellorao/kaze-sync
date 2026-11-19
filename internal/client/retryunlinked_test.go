package client

import (
	"testing"
)

// A stale unlinked row of a path with a held deletion must not make retryUnlinked replace the file the user put back
// (same size and mtime as its base) with the link the server has since deleted: the scope is frozen, and settleHeld
// drops the row.
func TestRetryUnlinkedSkipsHeldDeletion(t *testing.T) {
	t.Parallel()
	_, _, a, b, _ := pair(t)
	a.put("x.txt", []byte("one"))
	a.sync()
	b.sync()
	row, have, _ := b.f.DB.get("x.txt")
	if !have {
		t.Fatal("setup: no base row")
	}
	b.fs.SetNoSymlinks(true)
	a.link("target", "x.txt") // the server's file becomes a link b cannot create
	a.sync()
	b.pull()
	if rows, _ := b.f.DB.unlinked(); len(rows) != 1 {
		t.Fatalf("setup: unlinked %+v", rows)
	}
	b.fs.Remove("x.txt")
	b.sync() // the deletion is held: the base is behind the server
	if rows, _ := b.f.DB.held(); len(rows) != 1 || rows[0].Kind != "delete" {
		t.Fatalf("setup: held %+v", rows)
	}
	a.fs.Remove("x.txt")
	a.sync()
	b.fs.SetNoSymlinks(false) // Developer Mode on, and the user puts the file back
	b.put("x.txt", []byte("one"))
	if err := b.fs.SetMeta("x.txt", row.MtimeNs, false); err != nil {
		t.Fatal(err)
	}
	b.pull()
	if s, ok := b.read("x.txt"); !ok || s != "one" {
		t.Fatalf("the restored file was replaced: %q %v", s, ok)
	}
	if _, ok := b.readlink("x.txt"); ok {
		t.Fatal("x.txt became a link")
	}
	b.sync()
	if s, ok := b.read("x.txt"); !ok || s != "one" {
		t.Fatalf("after sync: %q %v", s, ok)
	}
}
