package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func touch(t *testing.T, root, p, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(p)), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// start watches root, adding every directory in dirs like a scanner would.
func start(t *testing.T, root string, dirs ...string) (*Watcher, chan string) {
	t.Helper()
	out := make(chan string, 1000)
	w, err := Start(root, out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	for _, d := range dirs {
		if err := w.Add(d); err != nil {
			t.Fatal(err)
		}
	}
	return w, out
}

func expect(t *testing.T, out <-chan string, want string) {
	t.Helper()
	timeout := time.After(time.Second)
	for {
		select {
		case got := <-out:
			if got == want {
				return
			}
		case <-timeout:
			t.Fatalf("no event for %q within 1s", want)
		}
	}
}

func TestChangesInNestedDirectories(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a/create", "a/modify", "a/remove", "a/rename")
	touch(t, root, "a/modify/f", "1")
	touch(t, root, "a/remove/f", "1")
	touch(t, root, "a/rename/f", "1")
	_, out := start(t, root, "a", "a/create", "a/modify", "a/remove", "a/rename")

	touch(t, root, "a/create/new", "1")
	expect(t, out, "a/create")
	touch(t, root, "a/modify/f", "22")
	expect(t, out, "a/modify")
	os.Remove(filepath.Join(root, "a", "remove", "f"))
	expect(t, out, "a/remove")
	os.Rename(filepath.Join(root, "a", "rename", "f"), filepath.Join(root, "a", "rename", "g"))
	expect(t, out, "a/rename")
	touch(t, root, "top", "1")
	expect(t, out, ".")
}

func TestNewSubdirectoryAfterAdd(t *testing.T) {
	root := t.TempDir()
	w, out := start(t, root)
	mkdirs(t, root, "new")
	expect(t, out, ".")
	if err := w.Add("new"); err != nil {
		t.Fatal(err)
	}
	touch(t, root, "new/f", "1")
	expect(t, out, "new")
}
