package server

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var migrateNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// oldState builds a data directory whose meta.db is at schema 1 and whose folder 1 is at schema 2 and holds a row.
func oldState(t *testing.T) (data, varDir string) {
	t.Helper()
	data = t.TempDir()
	varDir = filepath.Join(data, VarDir)
	os.MkdirAll(filepath.Join(varDir, FolderDir), 0o700)
	db, err := OpenDB(filepath.Join(varDir, DBFile), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	SetMeta(db, "schema_version", 1)
	db.Exec(`INSERT INTO folders(id, name, rules) VALUES(1, 'docs', '')`)
	fdb, err := OpenDB(FolderDBPath(varDir, 1), true)
	if err != nil {
		t.Fatal(err)
	}
	defer fdb.Close()
	for _, s := range folderMigrations[:2] {
		if _, err := fdb.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	SetMeta(fdb, "schema_version", 2)
	if _, err := fdb.Exec(`INSERT INTO files VALUES('a.txt', 'a.txt', 1, x'00', x'00', 5, 7, 0, 'dev', 9, 0)`); err != nil {
		t.Fatal(err)
	}
	return data, varDir
}

func openAt(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := OpenDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func hasColumn(db *sql.DB, table, col string) bool {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		rows.Scan(&n)
		if n == col {
			return true
		}
	}
	return false
}

func listBackups(varDir string) []string {
	ents, _ := os.ReadDir(filepath.Join(varDir, BackupDir))
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestStartMigratesOldDatabasesAfterBackup(t *testing.T) {
	data, varDir := oldState(t)
	s, err := New(data, func() time.Time { return migrateNow })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := schemaVersion(s.db); v != SchemaVersion {
		t.Fatalf("meta schema %d", v)
	}
	fdb, err := s.folderDB(1)
	if err != nil {
		t.Fatal(err)
	}
	var path string
	var link int
	if err := fdb.QueryRow(`SELECT path, link FROM files`).Scan(&path, &link); err != nil || path != "a.txt" || link != 0 {
		t.Fatalf("row after migration: %q %d %v", path, link, err)
	}
	want := []string{"folder-1-v2-20261105T120000Z.db", "meta-v1-20261105T120000Z.db"}
	if got := listBackups(varDir); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("backups %v, want %v", got, want)
	}
	b := openAt(t, filepath.Join(varDir, BackupDir, want[0]))
	if v, _ := schemaVersion(b); v != 2 || hasColumn(b, "files", "link") {
		t.Fatalf("folder backup is not the old schema 2 (version %d)", v)
	}
	if err := b.QueryRow(`SELECT path FROM files`).Scan(&path); err != nil || path != "a.txt" {
		t.Fatalf("backup lost the data: %q %v", path, err)
	}
	if v, _ := schemaVersion(openAt(t, filepath.Join(varDir, BackupDir, want[1]))); v != 1 {
		t.Fatalf("meta backup schema %d, want 1", v)
	}
	if fi, _ := os.Stat(filepath.Join(varDir, BackupDir, want[0])); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", fi.Mode())
	}
}

func TestCurrentSchemaIsNotBackedUp(t *testing.T) {
	e := newEnv(t)
	e.folder("docs")
	e.s.Close()
	s, err := New(filepath.Dir(e.s.varDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(filepath.Join(e.s.varDir, BackupDir)); err == nil {
		t.Fatalf("a backup was taken of current databases: %v", listBackups(e.s.varDir))
	}
}

func TestFailedMigrationLeavesDatabaseUnchanged(t *testing.T) {
	data, varDir := oldState(t)
	orig := folderMigrations
	t.Cleanup(func() { folderMigrations = orig })
	folderMigrations = append(append([]string{}, orig...), `ALTER TABLE files ADD COLUMN extra INTEGER; INSERT INTO no_such_table VALUES(1);`)
	if _, err := New(data, nil); err == nil || !strings.Contains(err.Error(), "folder 1") || !strings.Contains(err.Error(), "unchanged") {
		t.Fatalf("New: %v", err)
	}
	fdb := openAt(t, FolderDBPath(varDir, 1))
	if v, _ := schemaVersion(fdb); v != 2 || hasColumn(fdb, "files", "link") || hasColumn(fdb, "files", "extra") {
		t.Fatalf("half-migrated folder database (schema %d)", v)
	}
	var n int
	if fdb.QueryRow(`SELECT count(*) FROM files`).Scan(&n); n != 1 {
		t.Fatalf("rows %d", n)
	}
}

func TestMigrationStopsWhenBackupFails(t *testing.T) {
	data, varDir := oldState(t)
	os.WriteFile(filepath.Join(varDir, BackupDir), []byte("in the way"), 0o600) // a file where the directory belongs
	if _, err := New(data, nil); err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("New: %v", err)
	}
	if v, _ := schemaVersion(openAt(t, filepath.Join(varDir, DBFile))); v != 1 {
		t.Fatalf("meta migrated without a backup (schema %d)", v)
	}
}

func TestNewerSchemaRefusesToStart(t *testing.T) {
	for _, target := range []string{"folder", "meta"} {
		data, varDir := oldState(t)
		path := FolderDBPath(varDir, 1)
		if target == "meta" {
			path = filepath.Join(varDir, DBFile)
		}
		db := openAt(t, path)
		SetMeta(db, "schema_version", 99)
		_, err := New(data, nil)
		if err == nil || !strings.Contains(err.Error(), "newer than this binary") {
			t.Fatalf("%s at schema 99: %v", target, err)
		}
		if v, _ := schemaVersion(db); v != 99 {
			t.Fatalf("%s changed to %d", target, v)
		}
		if b := listBackups(varDir); len(b) > 0 && target == "meta" {
			t.Fatalf("backups of a newer database: %v", b)
		}
	}
}

func TestBackupsKeepTheNewestAndStayPerDatabase(t *testing.T) {
	_, varDir := oldState(t)
	fdb := openAt(t, FolderDBPath(varDir, 1))
	for i := 0; i < keepBackups+2; i++ {
		if err := backupDB(fdb, varDir, "folder-1", 2, migrateNow.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := backupDB(fdb, varDir, "folder-12", 2, migrateNow); err != nil { // another database whose name starts the same
		t.Fatal(err)
	}
	pruneBackups(varDir, "folder-1")
	got := backupsOf(varDir, "folder-1")
	if len(got) != keepBackups || !strings.Contains(got[0], "T140000Z") || !strings.Contains(got[len(got)-1], "T160000Z") {
		t.Fatalf("kept %v", got)
	}
	if len(backupsOf(varDir, "folder-12")) != 1 {
		t.Fatal("pruning folder-1 touched folder-12")
	}
	if err := removeBackups(varDir, "folder-1"); err != nil {
		t.Fatal(err)
	}
	if len(backupsOf(varDir, "folder-1")) != 0 || len(backupsOf(varDir, "folder-12")) != 1 {
		t.Fatalf("remove: %v", listBackups(varDir))
	}
}
