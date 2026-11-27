package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Source file metadata must not leak into release archives.
func TestArchiveReproducible(t *testing.T) {
	dir := t.TempDir()
	names := []string{"a", "b"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, linux := range []bool{true, false} {
		first := filepath.Join(dir, "first")
		second := filepath.Join(dir, "second")
		if err := pack(first, dir, names, linux); err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			path := filepath.Join(dir, name)
			if err := os.Chmod(path, 0777); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		if err := pack(second, dir, names, linux); err != nil {
			t.Fatal(err)
		}
		a, err := os.ReadFile(first)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(second)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("linux=%v: archive changed with source metadata", linux)
		}
		if linux {
			gz, err := gzip.NewReader(bytes.NewReader(a))
			if err != nil {
				t.Fatal(err)
			}
			tr := tar.NewReader(gz)
			for _, name := range names {
				h, err := tr.Next()
				if err != nil {
					t.Fatal(err)
				}
				if h.Name != name || h.Mode != 0755 || h.Uid != 0 || h.Gid != 0 || !h.ModTime.Equal(archiveTime) {
					t.Fatalf("unexpected tar header: %+v", h)
				}
			}
			if _, err := tr.Next(); err != io.EOF {
				t.Fatalf("tar end: %v", err)
			}
			gz.Close()
		} else {
			zr, err := zip.NewReader(bytes.NewReader(a), int64(len(a)))
			if err != nil {
				t.Fatal(err)
			}
			if len(zr.File) != len(names) {
				t.Fatal("unexpected zip entry count")
			}
			for i, f := range zr.File {
				if f.Name != names[i] || f.Mode().Perm() != 0755 || !f.Modified.Equal(archiveTime) {
					t.Fatalf("unexpected zip header: %+v", f.FileHeader)
				}
			}
		}
	}
}

func TestPruneDist(t *testing.T) {
	dist := t.TempDir()
	for _, n := range []string{"v0.7.1", "v0.7.4", "v0.7.5", "v0.7.10", "v0.7.9", "notes", ".release-x"} {
		if err := os.MkdirAll(filepath.Join(dist, n), 0755); err != nil {
			t.Fatal(err)
		}
	}
	pruneDist(dist, 2, func(tag string) bool { return tag != "v0.7.4" })
	var got []string
	entries, _ := os.ReadDir(dist)
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := ".release-x notes v0.7.10 v0.7.4 v0.7.9"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}
