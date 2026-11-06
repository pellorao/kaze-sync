package fs

import "testing"

func guardSetup(t *testing.T, windows bool) (*Mem, *Guard) {
	t.Helper()
	m := NewMem(MemOptions{Windows: windows})
	for _, d := range []string{"r/d", "free"} {
		m.MkdirAll(d)
	}
	for _, f := range []string{"r/a", "r/d/b", "free/c"} {
		w, _, _ := m.OpenAppend(f)
		w.Close()
	}
	g := NewGuard(m, []string{"r"})
	for _, p := range []string{"r/a", "r/d/b", "r/d", "r"} {
		m.SetWritable(p, false)
	}
	return m, g
}

func ro(m *Mem, p string) bool { st, _ := m.Stat(p); return st.ReadOnly }

func TestGuardUnlocksAroundEngineWrites(t *testing.T) {
	for _, windows := range []bool{false, true} {
		m, g := guardSetup(t, windows)
		if !windows {
			if err := m.Remove("r/a"); err == nil {
				t.Fatal("the Mem accepted a delete in a read-only directory")
			}
		}
		if err := g.Remove("r/a"); err != nil {
			t.Fatalf("windows=%v remove: %v", windows, err)
		}
		if !windows && !ro(m, "r") {
			t.Error("directory left writable")
		}
		w, _, err := g.OpenAppend("r/new")
		if err != nil {
			t.Fatalf("windows=%v create: %v", windows, err)
		}
		w.Close()
		if err := g.Rename("r/new", "r/d/b"); err != nil { // replaces a read-only file
			t.Fatalf("windows=%v rename: %v", windows, err)
		}
		if !ro(m, "r/d/b") {
			t.Errorf("windows=%v: a file the engine wrote is writable", windows)
		}
		if err := g.MkdirAll("r/x/y"); err != nil {
			t.Fatalf("windows=%v mkdir: %v", windows, err)
		}
		if !windows && (!ro(m, "r/x") || !ro(m, "r/x/y") || !ro(m, "r")) {
			t.Error("directories not read-only again")
		}
		if err := g.Remove("r/x/y"); err != nil || g.Remove("r/x") != nil {
			t.Fatalf("windows=%v remove dirs: %v", windows, err)
		}
		// Outside the range nothing changes, and a failure inside it restores too.
		if err := g.Remove("free/c"); err != nil {
			t.Fatal(err)
		}
		if err := g.Remove("r/zzz"); err == nil {
			t.Error("removing a missing file succeeded")
		}
		if !windows && !ro(m, "r") {
			t.Error("directory left writable after a failure")
		}
	}
}

func TestGuardNoRangesPassesThrough(t *testing.T) {
	m := NewMem(MemOptions{})
	g := NewGuard(m, nil)
	if err := g.MkdirAll("a/b"); err != nil || g.In("a") {
		t.Fatal("pass-through broken")
	}
}

// A directory the user made read-only outside every range does not stop the engine, and stays read-only.
func TestGuardGetsAroundForeignReadonlyDirs(t *testing.T) {
	m, g := guardSetup(t, false)
	m.SetWritable("free", false)
	if err := g.Remove("free/c"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	w, _, err := g.OpenAppend("free/n")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w.Close()
	if err := g.Rename("free/n", "free/m"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if !ro(m, "free") {
		t.Error("the user's read-only directory was left writable")
	}
}
