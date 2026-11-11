package client

import (
	"testing"

	"kaze-sync/internal/proto"
)

// A single scope frozen by a type hold during a Pull (design 6.6) has no dirty: of its own. Its held files are
// unsent items in every scan, so sinceHeldBack stays true and the root's since stays old: the cross-scope freeze
// of cpureview2 (TestCpufix2FrozenScopeKeepsOtherDirty) is still there for this kind of freeze.
func TestCpufix3TypeHeldScopeKeepsOtherSince(t *testing.T) {
	for _, freezeT := range []bool{false, true} {
		t.Run(map[bool]string{false: "control", true: "typeHeldT"}[freezeT], func(t *testing.T) {
			a, b := cpufix3Pair(t, []proto.WCRule{{Path: "t", Mode: proto.ModeSingle}})
			a.put("t/f.txt", []byte("zero"))
			a.put("x.txt", []byte("zero"))
			a.sync()
			b.sync()
			if freezeT {
				b.put("t/z", []byte("a file"))
				b.sync()
				a.put("t/z/q.txt", []byte("a dir")) // not scanned yet
				a.pull()                            // t/z: a directory with unsent files is in the way: held, t frozen
				if _, ok := a.f.frozen["d:T"]; !ok {
					t.Fatalf("setup: t not frozen: %v", a.f.frozen)
				}
				if _, ok := a.f.DB.metaPrefix("dirty:")["d:T"]; ok {
					t.Fatalf("setup: t has a dirty")
				}
			}
			a.put("x.txt", []byte("one"))
			a.sync()
			b.sync()
			b.put("y.txt", []byte("from b"))
			b.sync()
			a.sync() // a pulls y.txt
			a.put("x.txt", []byte("two"))
			a.sync()
			if _, ok := a.f.frozen["d:"]; ok {
				t.Errorf("root frozen after pulling the other device's write, only because t is held: frozen %v dirty %v unsentDirs %v scanc %d cursor %d",
					a.f.frozen, a.f.DB.metaPrefix("dirty:"), a.f.unsentDirs, a.f.DB.metaInt("scanc"), a.f.DB.metaInt("cursor"))
			}
		})
	}
}
