package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"

	_ "modernc.org/sqlite"
)

const defaultRules = "~$*\n*.tmp\nThumbs.db\ndesktop.ini\n.DS_Store\n.sync_*.db*\n.owncloudsync.log\n.nextcloudsync.log"

const fileCols = "path, key, seq, hash, blocks, size, mtime_ns, exec, device, created_at, conflict"

// migrations[i] upgrades the schema from version i to i+1.
var migrations = []string{`
CREATE TABLE files(
  folder_id INTEGER NOT NULL, path TEXT NOT NULL, key TEXT NOT NULL, seq INTEGER NOT NULL,
  hash BLOB, blocks BLOB, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, exec INTEGER NOT NULL,
  device TEXT NOT NULL, created_at INTEGER NOT NULL, conflict INTEGER NOT NULL,
  PRIMARY KEY(folder_id, path)) WITHOUT ROWID;
CREATE UNIQUE INDEX files_key ON files(folder_id, key) WHERE hash IS NOT NULL;
CREATE INDEX files_conflict ON files(folder_id) WHERE conflict AND hash IS NOT NULL;
CREATE TABLE versions(
  id INTEGER PRIMARY KEY, folder_id INTEGER NOT NULL, path TEXT NOT NULL, key TEXT NOT NULL, seq INTEGER NOT NULL,
  hash BLOB, blocks BLOB, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, exec INTEGER NOT NULL,
  device TEXT NOT NULL, created_at INTEGER NOT NULL, conflict INTEGER NOT NULL, replaced_at INTEGER NOT NULL);
CREATE INDEX versions_path ON versions(folder_id, path);
CREATE TABLE folders(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, rules TEXT NOT NULL);
CREATE TABLE devices(
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, os TEXT NOT NULL, token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL, last_seen INTEGER NOT NULL);
CREATE TABLE mappings(
  device_id INTEGER NOT NULL, folder_id INTEGER NOT NULL, path TEXT NOT NULL, adopt INTEGER NOT NULL,
  PRIMARY KEY(device_id, folder_id));
CREATE TABLE meta(key TEXT PRIMARY KEY, value) WITHOUT ROWID;
INSERT INTO meta VALUES('next_seq', 1), ('config_version', 1), ('keep_days', 30), ('global_rules', '` + defaultRules + `');
`}

// SchemaVersion is the schema version this binary works with.
var SchemaVersion = len(migrations)

// OpenDB opens (creating if create) the metadata database at path.
func OpenDB(path string, create bool) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil && !create {
		return nil, fmt.Errorf("%s: %w (the data disk may not be mounted; run install first)", path, err)
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Migrate brings the schema to SchemaVersion, creating it in an empty database.
func Migrate(db *sql.DB) error {
	v, err := schemaVersion(db)
	if err != nil {
		return err
	}
	if v > SchemaVersion {
		return fmt.Errorf("database schema %d is newer than this binary (%d)", v, SchemaVersion)
	}
	for ; v < SchemaVersion; v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(migrations[v]); err == nil {
			_, err = tx.Exec(`INSERT OR REPLACE INTO meta VALUES('schema_version', ?)`, v+1)
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate to %d: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func schemaVersion(db *sql.DB) (int, error) {
	var v int
	err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	switch {
	case err == nil:
		return v, nil
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	}
	var n int
	if db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='meta'`).Scan(&n) == nil && n == 0 {
		return 0, nil
	}
	return 0, err
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// MetaString returns a meta value as text, "" when absent.
func MetaString(q querier, key string) string {
	var v sql.NullString
	q.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	return v.String
}

func metaInt(q querier, key string) int64 {
	var v int64
	q.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	return v
}

// SetMeta stores a meta value.
func SetMeta(db *sql.DB, key string, value any) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO meta VALUES(?, ?)`, key, value)
	return err
}
