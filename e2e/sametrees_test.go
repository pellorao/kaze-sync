package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSameSizes(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	w := func(root, p, data string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		if err := os.WriteFile(filepath.Join(root, p), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w(a, "x/f", "abc")
	w(a, ".syncroot", "")
	if ok, _ := sameSizes(a, b, 1); ok {
		t.Fatal("an empty side counts as the same")
	}
	w(b, "x/f", "abcd")
	if ok, _ := sameSizes(a, b, 1); ok {
		t.Fatal("different sizes count as the same")
	}
	w(b, "x/f", "xyz") // same size, other content: the size check passes, the hash check decides
	if ok, err := sameSizes(a, b, 1); !ok {
		t.Fatalf("same sizes: %v", err)
	}
	if ok, _ := sameSizes(a, b, 2); ok {
		t.Fatal("min not honoured")
	}
}
