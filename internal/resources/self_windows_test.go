package resources

import (
	"testing"
	"unsafe"
)

func TestWorkingSetCounterLayout(t *testing.T) {
	var m memoryCounters
	if got, want := unsafe.Sizeof(m), uintptr(8)+8*unsafe.Sizeof(uintptr(0)); got != want {
		t.Fatalf("counter size %d want %d", got, want)
	}
	if got, want := unsafe.Offsetof(m.WorkingSet), uintptr(8)+unsafe.Sizeof(uintptr(0)); got != want {
		t.Fatalf("working set offset %d want %d", got, want)
	}
}
