package client

import (
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

// A single-writer setting on a file (left by a revoked exclusive owner, or set directly) is a scope of its own; an
// unsent change of that file is recorded as its directory, which dirScope maps to the directory's scope.
func TestCpufix4FileScopeDirtyCleared(t *testing.T) {
	a, _ := cpufix3Pair(t, []proto.WCRule{{Path: "d/f.txt", Mode: proto.ModeSingle}})
	id, mode := a.f.scopeOf("d/f.txt")
	if mode != proto.ModeSingle || id == "d:" {
		t.Fatalf("setup: %s %s", id, mode)
	}
	a.f.DB.setMeta("dirty:"+id, 1)
	a.f.unsentDirs = map[string]bool{"d": true}
	a.f.unsentFiles = map[string]bool{"d/f.txt": true}
	a.f.clearDirty()
	if _, ok := a.f.DB.metaPrefix("dirty:")[id]; !ok {
		t.Errorf("dirty of %s cleared while d/f.txt is unsent", id)
	}
	a.f.DB.delMeta("dirty:d:")
	a.f.DB.delMeta("dirty:" + id)
	a.f.DB.setMeta("dirty:d:", 1) // the root has a since, the file scope has none
	if !a.f.sinceHeldBack() {
		t.Errorf("sinceHeldBack=false while %s (no since) has an unsent change", id)
	}
}

// The same through a real scan: d/f.txt (single-writer scope of its own) is too fresh, so unsent; a partial scan of
// another directory leaves it, a clean scan covering d clears it.
func TestCpufix4FileScopeRecordedAndCleared(t *testing.T) {
	a, _ := cpufix3Pair(t, []proto.WCRule{{Path: "d/f.txt", Mode: proto.ModeSingle}})
	id, _ := a.f.scopeOf("d/f.txt")
	a.put("d/f.txt", []byte("zero"))
	a.put("e/g.txt", []byte("zero"))
	a.sync()
	now := time.Unix(0, 5_000_000_000_000)
	a.f.Now = func() time.Time { return now }
	a.f.Stable = 5 * time.Second
	a.put("d/f.txt", []byte("one"))
	a.fs.SetMeta("d/f.txt", now.Add(-time.Second).UnixNano(), false)
	a.scanHint("d")
	if !a.f.unsentFiles["d/f.txt"] {
		t.Fatalf("d/f.txt not recorded: %v", a.f.unsentFiles)
	}
	a.scanHint("e")
	if !a.f.unsentFiles["d/f.txt"] {
		t.Errorf("a scan of e cleared d/f.txt")
	}
	now = now.Add(10 * time.Second)
	a.scanHint("d")
	if len(a.f.unsentFiles) != 0 {
		t.Errorf("still unsent after a clean scan of d: %v (scope %s)", a.f.unsentFiles, id)
	}
}
