package server

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"kaze-sync/internal/proto"
)

func pollEpoch(t *testing.T, e *env, token string) string {
	t.Helper()
	st, out := e.api(token, "poll", proto.PollReq{ConfigVersion: 1})
	var r proto.PollResp
	if err := json.Unmarshal(out, &r); st != 200 || err != nil {
		t.Fatalf("poll: %d %s", st, out)
	}
	return r.Epoch
}

// A database has an epoch from its creation, polls carry it, only `install --new-epoch` changes it (design 2.4, 9.4).
func TestEpochInPollAndNewEpoch(t *testing.T) {
	e := newEnv(t)
	_, tok := e.enrollDevice("dev", "linux")
	first := pollEpoch(t, e, tok)
	if first == "" || pollEpoch(t, e, tok) != first {
		t.Fatalf("epoch %q is not stable", first)
	}

	f := newFakeSystem(t)
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		db, err := OpenDB(filepath.Join(f.data, "var", "meta.db"), false)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		return MetaString(db, "epoch")
	}
	ep := read()
	if ep == "" {
		t.Fatal("install left no epoch")
	}
	if err := f.install(InstallOptions{}); err != nil || read() != ep {
		t.Fatalf("a plain reinstall changed the epoch: %v", err)
	}
	if err := f.install(InstallOptions{NewEpoch: true}); err != nil || read() == ep {
		t.Fatalf("--new-epoch kept the epoch: %v", err)
	}
}

// Version 1 of meta.db (folders without AUTOINCREMENT, no epoch) migrates in place: the folders stay, the epoch
// appears, and the ids of folders deleted afterwards are never handed out again.
func TestMigrateMetaToAutoincrementAndEpoch(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "meta.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrate(db, migrations[:1]); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO folders(name, rules) VALUES('a', 'x'), ('b', 'y')`)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if v, _ := schemaVersion(db); v != SchemaVersion {
		t.Fatalf("schema %d", v)
	}
	if MetaString(db, "epoch") == "" {
		t.Fatal("no epoch after the migration")
	}
	var n int
	var rules string
	db.QueryRow(`SELECT count(*), max(rules) FROM folders`).Scan(&n, &rules)
	if n != 2 || rules != "y" {
		t.Fatalf("folders lost: %d %q", n, rules)
	}
	db.Exec(`DELETE FROM folders WHERE name='b'`)
	res, err := db.Exec(`INSERT INTO folders(name, rules) VALUES('c', '')`)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 3 {
		t.Fatalf("the id of a deleted folder was used again: %d", id)
	}
}
