package client

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// CPU fixes, second round: the findings of a second review of the client's CPU use.

// A file held open by another program (in_use, a standing warning since scopefreeze) must not keep the since old.
func TestCpufix2InUseDoesNotFreezeScope(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"", "d/"} {
		a, b := singlePair(t)
		a.put("x.txt", []byte("one"))
		a.put(dir+"app.db", []byte("locked"))
		a.fs.Lock(dir + "app.db")
		a.sync()
		b.sync()
		b.put("y.txt", []byte("from b"))
		b.sync()
		a.sync() // a pulls y.txt
		a.put("x.txt", []byte("two"))
		a.sync()
		if len(a.f.frozen) != 0 {
			t.Errorf("dir=%q: frozen by a file in use: %v (unsentDirs %v unsentAll %v)", dir, a.f.frozen, a.f.unsentDirs, a.f.unsentAll)
		}
	}
}

// RetryDirs must name the directory of the file to rescan, not its parent.
func TestCpufix2RetryDirsAreTheFilesDirectory(t *testing.T) {
	t.Parallel()
	a, _ := singlePair(t)
	a.f.retryFile("d/x.txt")
	a.f.retryDir("e/f/y.txt")
	if got := a.f.RetryDirs(); !slices.Equal(got, []string{"d", "e/f"}) {
		t.Errorf("RetryDirs = %v, want [d e/f]", got)
	}
}

// A file in use in a top-level directory: the slow retry must upload it once it is free.
func TestCpufix2SlowRetryReachesTheFile(t *testing.T) {
	t.Parallel()
	a, b := singlePair(t)
	a.put("d/keep.txt", []byte("k"))
	a.sync()
	a.put("d/app.db", []byte("locked"))
	a.fs.Lock("d/app.db")
	a.scanHint("d")
	dirs := a.f.RetryDirs()
	a.fs.Unlock("d/app.db")
	a.scanHint(dirs...) // what the schedule's retry plan scans
	b.sync()
	if _, ok := b.read("d/app.db"); !ok {
		t.Errorf("the retry scanned %v and did not upload d/app.db", dirs)
	}
}

// Root single + "t" single: a frozen scope t (held changes waiting for the user) must not keep the root's since old.
func TestCpufix2FrozenScopeKeepsOtherDirty(t *testing.T) {
	t.Parallel()
	for _, freezeT := range []bool{false, true} {
		t.Run(map[bool]string{false: "control", true: "frozenT"}[freezeT], func(t *testing.T) { cpufix2Cross(t, freezeT) })
	}
}

func cpufix2Cross(t *testing.T, freezeT bool) {
	e := newCEnv(t)
	f := e.newFolder("Docs")
	if st := e.setRule(f, simRule{Path: "t", Mode: proto.ModeSingle}, nil); st >= 400 {
		t.Fatalf("setRule %d", st)
	}
	var gs [2]*engine
	for i := range gs {
		dev, api := e.newDevice()
		e.mapFolder(dev, f, "/data/docs", true)
		gs[i] = e.engine(api, f, nil, true)
		gs[i].fs = cfs.NewMem(cfs.MemOptions{Clock: 1_000_000_000_000 + int64(i)*1_000_000_000})
		gs[i].restart(api, proto.FolderConfig{ID: f, Adopt: true, Writectl: []proto.WCRule{{Mode: proto.ModeSingle}, {Path: "t", Mode: proto.ModeSingle}}}, gs[i].fs)
		gs[i].f.DeviceName = dev.Name
	}
	a, b := gs[0], gs[1]
	a.put("t/f.txt", []byte("zero"))
	a.put("x.txt", []byte("zero"))
	a.sync()
	b.sync()
	b.put("t/f.txt", []byte("b"))
	b.sync()
	if !freezeT {
		goto rest
	}
	a.put("t/f.txt", []byte("a")) // without pulling b's write: a is frozen on t
	a.scan()
	if _, ok := a.f.frozen["d:T"]; !ok {
		t.Fatalf("setup: t not frozen: %v", a.f.frozen)
	}
rest:
	a.put("x.txt", []byte("one"))
	a.sync()
	b.sync()
	b.put("y.txt", []byte("from b"))
	b.sync()
	a.sync() // a pulls y.txt
	a.put("x.txt", []byte("two"))
	a.sync()
	if _, ok := a.f.frozen["d:"]; ok {
		t.Errorf("root frozen after pulling the other device's write, only because t is frozen: %v dirty %v", a.f.frozen, a.f.DB.metaPrefix("dirty:"))
	}
}

// The held report is sized by its encoded length: "&" is 6 bytes in JSON.
// Over 100 ms (serial run): builds proto.MaxHeldItems held rows; the size is set by that product constant.
func TestCpufix2HeldReportCountsEscapes(t *testing.T) {
	t.Parallel()
	a, _ := singlePair(t)
	long := strings.Repeat("&", 250)
	rows := make([]heldRow, 0, proto.MaxHeldItems)
	for i := 0; i < proto.MaxHeldItems; i++ {
		rows = append(rows, heldRow{Path: long + strconv.Itoa(i), Scope: "d:", Reason: "owner", Kind: "add", Hash: make([]byte, 32)})
	}
	if err := a.f.DB.putHeld(rows); err != nil {
		t.Fatal(err)
	}
	items, cut, _ := a.f.HeldItems()
	b, _ := json.Marshal(proto.HeldReq{Folder: 1, Items: items, Cut: cut})
	if cut == 0 || len(b) > proto.MaxJSONBody {
		t.Fatalf("cut %d, the request is %d bytes, the server takes %d", cut, len(b), proto.MaxJSONBody)
	}
}

// A failing held report keeps the time of its first failure and is not a file error that pushes a notification.
func TestCpufix2HeldFailureKeepsStartAndKind(t *testing.T) {
	t.Parallel()
	a, _ := singlePair(t)
	now := time.Unix(1_000, 0)
	a.f.Now = func() time.Time { return now }
	a.f.HeldReportFailed(errors.New("x"))
	now = now.Add(time.Hour)
	a.f.HeldReportFailed(errors.New("x"))
	var rep Report
	a.f.ShowHeldError(&rep)
	if len(rep.Errors) != 1 || rep.Errors[0].Since != 1_000 || rep.Errors[0].Kind == proto.KindIO {
		t.Fatalf("%+v", rep.Errors)
	}
}
