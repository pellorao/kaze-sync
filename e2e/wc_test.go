package main

import (
	"os"
	"path/filepath"
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
