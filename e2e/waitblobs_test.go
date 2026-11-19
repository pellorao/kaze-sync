package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The wait counts whole seconds (date +%s), so giving up takes up to one second whatever the limit.
func TestWaitBlobsCmd(t *testing.T) {
	dir := t.TempDir()
	blobs := filepath.Join(dir, "var", "blobs")
	os.MkdirAll(blobs, 0o755)
	os.WriteFile(filepath.Join(blobs, "a"), nil, 0o644)
	if out, err := exec.Command("sh", "-c", waitBlobsCmd(dir, 1, 5)).CombinedOutput(); err != nil {
		t.Fatalf("enough files: %v %s", err, out)
	}
	start := time.Now()
	if err := exec.Command("sh", "-c", waitBlobsCmd(dir, 2, 0)).Run(); err == nil {
		t.Fatal("the wait never gives up")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("gave up after %v", time.Since(start))
	}
}
