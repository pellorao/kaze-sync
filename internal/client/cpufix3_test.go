package client

import (
	"errors"
	"strings"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// kaze-sync/cpufix3 reproductions (not part of the repository).

func cpufix3Pair(t *testing.T, extra []proto.WCRule) (a, b *engine) {
	t.Helper()
	e := newCEnv(t)
	f := e.newFolder("Docs")
	for _, r := range extra {
		if st := e.setRule(f, simRule{Path: r.Path, Mode: r.Mode}, nil); st >= 400 {
			t.Fatalf("setRule %d", st)
		}
	}
	var gs [2]*engine
	for i := range gs {
		dev, api := e.newDevice()
		e.mapFolder(dev, f, "/data/docs", true)
		gs[i] = e.engine(api, f, nil, true)
		gs[i].fs = cfs.NewMem(cfs.MemOptions{Clock: 1_000_000_000_000 + int64(i)*1_000_000_000})
		gs[i].restart(api, proto.FolderConfig{ID: f, Adopt: true, Writectl: append([]proto.WCRule{{Mode: proto.ModeSingle}}, extra...)}, gs[i].fs)
		gs[i].f.DeviceName = dev.Name
	}
	return gs[0], gs[1]
}

// clearDirty / sinceHeldBack map an unsent directory d to the scope of the made-up file "d/x". When d has a
// subdirectory "x" (or "X": keys fold case) with its own setting, d's unsent change is taken for that scope.
// a edits d/u (too fresh, unsent), then pulls b's write in the root scope: the root since must stay old.
func TestCpufix3UnsentDirNamedX(t *testing.T) {
	for _, name := range []string{"", "y", "x", "X"} {
		t.Run("sub="+name, func(t *testing.T) {
			var extra []proto.WCRule
			if name != "" {
				extra = []proto.WCRule{{Path: "d/" + name, Mode: proto.ModeFree}}
			}
			a, b := cpufix3Pair(t, extra)
			a.put("d/u.txt", []byte("zero"))
			a.put("r.txt", []byte("zero"))
			a.sync()
			b.sync()
			now := time.Unix(0, 5_000_000_000_000)
			a.f.Now = func() time.Time { return now }
			a.f.Stable = 5 * time.Second
			a.put("d/u.txt", []byte("one"))
			a.fs.SetMeta("d/u.txt", now.Add(-time.Second).UnixNano(), false)
			a.scanHint("d") // too fresh: unsent in d, root scope
			b.put("y.txt", []byte("from b"))
			b.sync()
			a.pull() // applies y.txt: must not become the since of a's d/u.txt change
			if held := a.f.sinceHeldBack(); !held {
				t.Logf("sinceHeldBack=false with unsentDirs %v (scanc %d cursor %d)", a.f.unsentDirs, a.f.DB.metaInt("scanc"), a.f.DB.metaInt("cursor"))
			}
			now = now.Add(10 * time.Second)
			a.scanHint("d") // stable now: sent
			if _, ok := a.f.frozen["d:"]; !ok {
				t.Errorf("d/u.txt was begun before b's y.txt was applied, yet the root was not frozen: frozen=%v", a.f.frozen)
			}
		})
	}
}

// The same mapping in clearDirty: d's unsent change does not keep the root's dirty when d has a setting "d/x".
func TestCpufix3ClearDirtyDirNamedX(t *testing.T) {
	a, _ := cpufix3Pair(t, []proto.WCRule{{Path: "d/x", Mode: proto.ModeFree}})
	a.f.DB.setMeta("dirty:d:", 1)
	a.f.unsentDirs = map[string]bool{"d": true}
	a.f.clearDirty()
	if _, ok := a.f.DB.metaPrefix("dirty:")["d:"]; !ok {
		t.Errorf("dirty of the root cleared while d (root scope) has an unsent change")
	}
}

// coversDir asks about the files directly in d: a scanned subdirectory named like the stand-in file does not count.
func TestCpufix3CoversDirNamedX(t *testing.T) {
	a, _ := cpufix3Pair(t, nil)
	a.f.scope = func(p string) bool { return p == "d/x" || strings.HasPrefix(p, "d/x/") } // a scan of d/x only
	if a.f.coversDir("d") {
		t.Fatalf("a scan of d/x covers the files of d")
	}
	if !a.f.coversDir("d/x") {
		t.Fatalf("a scan of d/x does not cover the files of d/x")
	}
}

// A failed held report is a reminder of its own kind: no push, no advice to exclude a file.
func TestCpufix3HeldReportFailureKind(t *testing.T) {
	a, _ := cpufix3Pair(t, nil)
	a.f.HeldReportFailed(errors.New("down"))
	var rep Report
	a.f.ShowHeldError(&rep)
	if len(rep.Errors) != 1 || rep.Errors[0].Kind != proto.KindReport {
		t.Fatalf("errors: %+v", rep.Errors)
	}
}
