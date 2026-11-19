package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// plantLink makes a victim file the service user could aim a link at, and returns its path.
func plantVictim(t *testing.T, dir string) string {
	t.Helper()
	v := filepath.Join(dir, "victim")
	if err := os.WriteFile(v, []byte("victim"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(v, 0o640); err != nil {
		t.Fatal(err)
	}
	return v
}

func checkVictim(t *testing.T, v string) {
	t.Helper()
	b, err := os.ReadFile(v)
	if err != nil || string(b) != "victim" {
		t.Fatalf("victim was written through a link: %q %v", b, err)
	}
	if fi, _ := os.Stat(v); fi.Mode().Perm() != 0o640 {
		t.Fatalf("victim mode changed through a link: %v", fi.Mode().Perm())
	}
}

// The service user owns var/tls, so root must not follow a link it plants there (symfix).
func TestRootWritesInTLSDirIgnorePlantedLinks(t *testing.T) {
	t.Parallel()
	data := caData(t)
	victim := plantVictim(t, t.TempDir())
	tls := filepath.Join(data, VarDir, TLSDir)
	if err := os.MkdirAll(tls, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(tls, serverFile+".tmp")); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureServerCert(data, []string{"localhost"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	checkVictim(t, victim)
	if fi, err := os.Lstat(serverPEMPath(data)); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("server.pem: %v %v", fi, err)
	}

	// The self-signed pair: dangling or not, a link at cert.pem or key.pem is replaced, never written through.
	st := filepath.Join(t.TempDir(), "tls")
	if err := os.MkdirAll(st, 0o755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(t.TempDir(), "created-by-root")
	if err := os.Symlink(gone, filepath.Join(st, "cert.pem")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(st, "key.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureSelfSigned(st); err != nil {
		t.Fatal(err)
	}
	checkVictim(t, victim)
	if _, err := os.Lstat(gone); err == nil {
		t.Fatal("a file was created through a dangling link")
	}
}

// var/tls itself may be a link the service user planted.
func TestRootWritesRefuseLinkedTLSDir(t *testing.T) {
	t.Parallel()
	data := caData(t)
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, VarDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(data, VarDir, TLSDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureServerCert(data, []string{"localhost"}, time.Now()); err == nil {
		t.Fatal("var/tls is a link: want an error")
	}
	if _, err := EnsureSelfSigned(filepath.Join(data, VarDir, TLSDir)); err == nil {
		t.Fatal("var/tls is a link: want an error")
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Fatalf("files were written through the linked directory: %v", ents)
	}
}

// SQLite opens -wal, -shm and -journal with O_CREAT and follows links, in a directory the service user owns.
func TestOpenDBIgnoresPlantedSidecarLinks(t *testing.T) {
	t.Parallel()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		dir := t.TempDir()
		victim := plantVictim(t, t.TempDir())
		path := filepath.Join(dir, "meta.db")
		if err := os.Symlink(victim, path+suffix); err != nil {
			t.Fatal(err)
		}
		db, err := OpenDB(path, true)
		if err == nil {
			err = Migrate(db)
			if err == nil {
				err = SetMeta(db, "x", "y")
			}
			db.Close()
		}
		checkVictim(t, victim)
		_ = err
	}
}
