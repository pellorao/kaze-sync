package place

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	cfs "kaze-sync/internal/client/fs"
)

// The target is open without FILE_SHARE_DELETE: Place keeps the temporary file and
// reports ErrLocked; after the other program closes the file a second call succeeds.
func TestPlaceTargetOpenOnNTFS(t *testing.T) {
	root := t.TempDir()
	f := cfs.OS(root)
	old := put(t, f, "doc.txt", "old")
	name, _ := windows.UTF16PtrFromString(filepath.Join(root, "doc.txt"))
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	src := &source{data: content, limit: -1}
	exp := Expect{true, old.Size, old.MtimeNs}
	if err := Place(f, "doc.txt", hash, src.open, Meta{MtimeNs: 1_700_000_000_000_000_000}, exp); !errors.Is(err, ErrLocked) {
		windows.CloseHandle(h)
		t.Fatalf("err = %v", err)
	}
	if got := get(t, f, "doc.txt"); got != "old" {
		t.Errorf("target changed while locked: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".syncpart-"+hash.String()[:16])); err != nil {
		t.Errorf("temporary file not kept: %v", err)
	}
	windows.CloseHandle(h)
	if err := Place(f, "doc.txt", hash, src.open, Meta{MtimeNs: 1_700_000_000_000_000_000}, exp); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	if got := get(t, f, "doc.txt"); got != content {
		t.Errorf("content = %q", got)
	}
}
