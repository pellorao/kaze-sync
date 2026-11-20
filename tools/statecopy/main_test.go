package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSnapshotIncludesWAL(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile(".syncroot", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "folder-#1%.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
		CREATE TABLE meta (cursor INTEGER); INSERT INTO meta VALUES (42)`); err != nil {
		t.Fatal(err)
	}
	if wal, err := os.Stat(dbPath + "-wal"); err != nil || wal.Size() == 0 {
		t.Fatalf("expected a non-empty source WAL: %v, %v", wal, err)
	}
	path, err := snapshot(temp, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	temp, err = filepath.EvalSymlinks(temp)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(filepath.Dir(path)) != temp {
		t.Fatalf("snapshot %q is outside %q", path, temp)
	}
	assertEntries(t, root, 1)
	assertEntries(t, filepath.Dir(path), 1)
	copiedDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { copiedDB.Close() })
	var cursor int
	if err := copiedDB.QueryRow(`SELECT cursor FROM meta`).Scan(&cursor); err != nil || cursor != 42 {
		t.Fatalf("snapshot cursor = %d, %v; want 42", cursor, err)
	}
}

func TestSnapshotRejectsUnsafeTemp(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile(".syncroot", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "scratch")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	type testCase struct{ name, dir, want string }
	cases := []testCase{
		{"missing", filepath.Join(t.TempDir(), "missing"), "missing"},
		{"relative", "scratch", "must be absolute"},
		{"sync root", root, "inside sync root"},
		{"below sync root", nested, "inside sync root"},
	}
	if runtime.GOOS != "windows" { // creating links needs privileges there
		link := filepath.Join(t.TempDir(), "scratch")
		if err := os.Symlink(nested, link); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, testCase{"symlink into sync root", link, "inside sync root"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, err := snapshot(tc.dir, "folder-1.db")
			if err == nil || !strings.Contains(err.Error(), tc.want) || path != "" {
				t.Fatalf("snapshot = %q, %v; want error containing %q", path, err, tc.want)
			}
			assertEntries(t, root, 2)
			assertEntries(t, nested, 0)
		})
	}
}

func TestSnapshotFailureLeavesNoFiles(t *testing.T) {
	root, temp := t.TempDir(), t.TempDir()
	missing := filepath.Join(root, "folder-1.db")
	if path, err := snapshot(temp, missing); err == nil || path != "" {
		t.Fatalf("snapshot of missing database = %q, %v", path, err)
	}
	assertEntries(t, root, 0)
	assertEntries(t, temp, 0)
}

func assertEntries(t *testing.T, dir string, want int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != want {
		t.Fatalf("entries in %s = %v, %v; want %d entries", dir, entries, err, want)
	}
}
