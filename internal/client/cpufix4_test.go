package client

import (
	"testing"
	"time"
)

// CPU fixes, fourth round: a partial scan of only the subdirectory "x" of d (or of the root) must not count as covering
// the files directly in d (or the root): coversDir with a real scan scope, both the dirs branch and the root branch.
func TestCpufix4PartialScanOfSubdirX(t *testing.T) {
	for _, c := range []struct{ name, dir, file, sub string }{
		{"dir-y", "d", "d/u.txt", "d/y"},
		{"dir-x", "d", "d/u.txt", "d/x"},
		{"root-y", ".", "u.txt", "y"},
		{"root-x", ".", "u.txt", "x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, b := cpufix3Pair(t, nil)
			a.put(c.file, []byte("zero"))
			a.put(c.sub+"/v.txt", []byte("zero"))
			a.put("r.txt", []byte("zero"))
			a.sync()
			b.sync()
			now := time.Unix(0, 5_000_000_000_000)
			a.f.Now = func() time.Time { return now }
			a.f.Stable = 5 * time.Second
			a.put(c.file, []byte("one"))
			a.fs.SetMeta(c.file, now.Add(-time.Second).UnixNano(), false)
			a.scanHint(c.dir) // too fresh: unsent
			if !a.f.unsentDirs[c.dir] {
				t.Fatalf("setup: unsentDirs %v", a.f.unsentDirs)
			}
			a.put(c.sub+"/v.txt", []byte("one"))
			a.fs.SetMeta(c.sub+"/v.txt", now.Add(-time.Minute).UnixNano(), false)
			a.scanHint(c.sub) // a scan of the subdirectory only
			if !a.f.unsentDirs[c.dir] {
				t.Errorf("a scan of %s only dropped the unsent record of %s: %v (dirty %v)", c.sub, c.dir, a.f.unsentDirs, a.f.DB.metaPrefix("dirty:"))
			}
			t.Logf("after sub scan: dirty %v unsent %v scanc %d cursor %d", a.f.DB.metaPrefix("dirty:"), a.f.unsentDirs, a.f.DB.metaInt("scanc"), a.f.DB.metaInt("cursor"))
			b.sync() // b applies a's v.txt first, so that its own write is not refused
			b.put("y.txt", []byte("from b"))
			b.sync()
			a.pull()
			t.Logf("after pull: dirty %v unsent %v scanc %d cursor %d", a.f.DB.metaPrefix("dirty:"), a.f.unsentDirs, a.f.DB.metaInt("scanc"), a.f.DB.metaInt("cursor"))
			now = now.Add(10 * time.Second)
			rep := a.scanHint(c.dir)
			t.Logf("final: rep %+v dirty %v unsent %v", rep, a.f.DB.metaPrefix("dirty:"), a.f.unsentDirs)
			if _, ok := a.f.frozen["d:"]; !ok {
				t.Errorf("%s was begun before b's y.txt was applied, yet the root was not frozen: frozen=%v", c.file, a.f.frozen)
			}
		})
	}
}
