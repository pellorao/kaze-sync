package client

import (
	"testing"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// singlePair is two devices on a folder whose root is single-writer (the default of a new folder).
func singlePair(t *testing.T) (a, b *engine) {
	t.Helper()
	e := newCEnv(t)
	f := e.newFolder("Docs")
	var gs [2]*engine
	for i := range gs {
		dev, api := e.newDevice()
		e.mapFolder(dev, f, "/data/docs", true)
		gs[i] = e.engine(api, f, nil, true)
		gs[i].fs = cfs.NewMem(cfs.MemOptions{Clock: 1_000_000_000_000 + int64(i)*1_000_000_000})
		gs[i].restart(api, proto.FolderConfig{ID: f, Adopt: true, Writectl: []proto.WCRule{{Mode: proto.ModeSingle}}}, gs[i].fs)
		gs[i].f.DeviceName = dev.Name
	}
	return gs[0], gs[1]
}

// A standing warning (a name the server refuses, design 2.8.6) is not an unsent change: it must not keep the
// scope's since at an old value, or every later write of another device freezes this one.
func TestStandingWarningDoesNotFreezeScope(t *testing.T) {
	for _, bad := range []bool{true, false} {
		a, b := singlePair(t)
		if bad {
			a.put("bad<name", []byte("x"))
		}
		a.put("x.txt", []byte("one"))
		a.sync()
		if bad && len(a.f.errs) == 0 {
			t.Fatal("the refused name was not reported")
		}
		b.sync()
		b.put("y.txt", []byte("from b"))
		b.sync()
		a.sync() // a pulls y.txt
		a.put("x.txt", []byte("two"))
		a.sync()
		if len(a.f.frozen) != 0 {
			t.Errorf("bad=%v: scope frozen by another device's write after a pull: %v", bad, a.f.frozen)
		}
	}
}
