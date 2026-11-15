package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRequireEmptyDir(t *testing.T) {
	d := t.TempDir()
	if err := requireEmptyDir(filepath.Join(d, "missing")); err != nil {
		t.Fatalf("missing dir: %v", err)
	}
	if err := requireEmptyDir(d); err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	os.MkdirAll(filepath.Join(d, "root"), 0o755) // a name removeWinWork would delete recursively
	if err := requireEmptyDir(d); err == nil {
		t.Fatal("non-empty dir accepted")
	}
}

func TestWCCleanupAfterFailure(t *testing.T) {
	for _, stage := range []string{"setup", "scenario"} {
		t.Run(stage, func(t *testing.T) {
			d := testWeb(t)
			keep := testDevice(t, d, "wctkeep")
			keepFolder := testFolder(t, d, "wctkeep")
			a := testDevice(t, d, "wct-a")
			folder := testFolder(t, d, "wct-partial & folder")
			if err := d.web.mapFolder(strconv.FormatInt(a.DeviceID, 10), folder, "/test/partial", true); err != nil {
				t.Fatal(err)
			}
			g := &wcGroup{d: d, folders: map[string]string{}}
			if stage == "setup" {
				// Enrollment succeeded, but setup failed before idOf stored its id.
				g.a = &wcSide{name: "wct-a", ag: &agent{}}
			} else {
				b := testDevice(t, d, "wct-b")
				g.a = &wcSide{name: "wct-a", id: strconv.FormatInt(a.DeviceID, 10), ag: &agent{}}
				g.b = &wcSide{name: "wct-b", id: strconv.FormatInt(b.DeviceID, 10), ag: &agent{}}
				g.folders["wct-partial & folder"] = folder
				// The server created another folder, but the scenario failed before recording its id.
				testFolder(t, d, "wct-unrecorded")
			}
			g.cleanup()
			if len(d.fails) != 0 {
				t.Fatalf("cleanup failed: %v", d.fails)
			}
			for _, kind := range []string{"devices", "folders"} {
				links, err := d.listLinks(kind)
				if err != nil {
					t.Fatal(err)
				}
				wantID := keepFolder
				if kind == "devices" {
					wantID = strconv.FormatInt(keep.DeviceID, 10)
				}
				if len(links) != 1 || links[0].name != "wctkeep" || links[0].id != wantID {
					t.Errorf("remaining %s: %+v", kind, links)
				}
			}
		})
	}
}
