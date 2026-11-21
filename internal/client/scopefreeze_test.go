package client

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
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
	t.Parallel()
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

// A new device adopting a directory whose files equal the server's is not frozen for them: nothing differs, so
// nothing needs the user's decision (design 2.8.3).
func TestAdoptSameContentIsNotHeld(t *testing.T) {
	t.Parallel()
	a, b := singlePair(t)
	for _, p := range []string{"x.txt", "d/y.txt"} {
		a.put(p, []byte("same "+p))
		b.put(p, []byte("same "+p))
	}
	a.sync()
	a.put("z.txt", []byte("later")) // the server has writes the new device has not seen
	a.sync()
	b.sync()
	rows, _ := b.f.DB.held()
	if len(rows) != 0 || len(b.f.frozen) != 0 {
		t.Fatalf("held %+v frozen %v", rows, b.f.frozen)
	}
}

// A list of held changes longer than the report may be is cut to what fits, the cut is counted, and the report of a
// full-size list stays within the request limit of the server.
// Over 100 ms (serial run): builds proto.MaxHeldItems+50 held rows; the size is set by that product constant.
func TestHeldReportIsCutToFit(t *testing.T) {
	t.Parallel()
	a, _ := singlePair(t)
	long := strings.Repeat("d", 200)
	rows := make([]heldRow, 0, proto.MaxHeldItems+50)
	for i := 0; i < proto.MaxHeldItems+50; i++ {
		rows = append(rows, heldRow{Path: long + "/" + strconv.Itoa(i), Scope: "d:", Reason: "owner", Kind: "add", Hash: make([]byte, 32), Size: 1 << 40, MtimeNs: 1 << 60, At: 1 << 31})
	}
	if err := a.f.DB.putHeld(rows); err != nil {
		t.Fatal(err)
	}
	items, cut, changed := a.f.HeldItems()
	if !changed || cut == 0 || len(items)+cut != len(rows) {
		t.Fatalf("items %d cut %d changed %v", len(items), cut, changed)
	}
	b, _ := json.Marshal(proto.HeldReq{Folder: 1, Items: items, Cut: cut})
	if len(b) > proto.MaxJSONBody {
		t.Fatalf("the request is %d bytes, the server takes %d", len(b), proto.MaxJSONBody)
	}
	// Short paths: the count limit decides.
	a, _ = singlePair(t)
	for i := range rows {
		rows[i].Path = "p" + strconv.Itoa(i)
	}
	a.f.DB.putHeld(rows)
	items, cut, _ = a.f.HeldItems()
	if len(items) != proto.MaxHeldItems || cut == 0 {
		t.Fatalf("short paths: items %d cut %d", len(items), cut)
	}
}

// A report that failed shows in the folder's status, not only in the log, until one succeeds.
func TestHeldReportFailureIsShown(t *testing.T) {
	t.Parallel()
	a, _ := singlePair(t)
	var rep Report
	a.f.HeldReportFailed(errors.New("held: 400 too large"))
	a.f.ShowHeldError(&rep)
	if rep.State != "error" || len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0].Message, "400 too large") {
		t.Fatalf("report %+v", rep)
	}
	rep = Report{}
	a.f.HeldReportFailed(nil)
	a.f.ShowHeldError(&rep)
	if len(rep.Errors) != 0 {
		t.Fatalf("still shown: %+v", rep)
	}
}
