package fs

import "testing"

// Reserve allocates without changing the length that OpenAppend reports for resuming.
func TestReserveKeepsLength(t *testing.T) {
	o := OS(t.TempDir())
	w, _, err := o.OpenAppend("f")
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := o.Reserve("f", 1<<20); err != nil {
		t.Fatal(err)
	}
	if e, _ := o.Stat("f"); e.Size != 0 {
		t.Fatalf("length %d after Reserve", e.Size)
	}
}
