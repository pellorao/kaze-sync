package fs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// lockFile opens abs with the given share mode and keeps it open until the test ends.
func lockFile(t *testing.T, abs string, share uint32) {
	t.Helper()
	name, _ := windows.UTF16PtrFromString(abs)
	h, err := windows.CreateFile(name, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
}

// A file opened without sharing behaves like Mem.Lock: every access reports ErrLocked.
func TestOSExclusiveLock(t *testing.T) {
	root := t.TempDir()
	f := OS(root)
	write(t, f, "f", "data")
	write(t, f, "g", "other")
	lockFile(t, filepath.Join(root, "f"), 0)
	if _, err := f.Open("f"); !errors.Is(err, ErrLocked) {
		t.Errorf("Open = %v", err)
	}
	if _, _, err := f.OpenAppend("f"); !errors.Is(err, ErrLocked) {
		t.Errorf("OpenAppend = %v", err)
	}
	if err := f.Rename("g", "f"); !errors.Is(err, ErrLocked) {
		t.Errorf("Rename onto locked = %v", err)
	}
	if err := f.Rename("f", "h"); !errors.Is(err, ErrLocked) {
		t.Errorf("Rename of locked = %v", err)
	}
	if err := f.Remove("f"); !errors.Is(err, ErrLocked) {
		t.Errorf("Remove = %v", err)
	}
}

// A file opened for reading with sharing of read and write (but not delete) can be read, not replaced or deleted.
func TestOSOpenWithoutShareDelete(t *testing.T) {
	root := t.TempDir()
	f := OS(root)
	write(t, f, "f", "data")
	write(t, f, "g", "other")
	lockFile(t, filepath.Join(root, "f"), windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if got := read(t, f, "f"); got != "data" {
		t.Errorf("read = %q", got)
	}
	if err := f.Rename("g", "f"); !errors.Is(err, ErrLocked) {
		t.Errorf("Rename onto open file = %v", err)
	}
	if err := f.Remove("f"); !errors.Is(err, ErrLocked) {
		t.Errorf("Remove of open file = %v", err)
	}
}

func TestOSLongPath(t *testing.T) {
	f := OS(t.TempDir())
	dir := strings.TrimSuffix(strings.Repeat("0123456789abcdef/", 20), "/") // 339 characters
	if err := f.MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	write(t, f, dir+"/file.txt", "deep")
	if err := f.Rename(dir+"/file.txt", dir+"/File.txt"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, f, dir); got != "File.txt" {
		t.Errorf("ReadDir = %q", got)
	}
	if err := f.Remove(dir + "/File.txt"); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(f.(osFS).abs(dir)); err != nil {
		t.Error(err)
	}
}
