package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"
)

const fileCols = "path, key, seq, hash, blocks, size, mtime_ns, exec, device, created_at, conflict, link"

// defaultRules is the factory global rule set: the original eight plus the system and
// temporary files of Nextcloud's sync-exclude.lst (design 8.2).
const defaultRules = `~$*
*.tmp
Thumbs.db
desktop.ini
.DS_Store
.sync_*.db*
.owncloudsync.log
.nextcloudsync.log
*~
.~lock.*
.*.sw?
*.kate-swp
*.part
*.filepart
*.crdownload
System Volume Information/
$RECYCLE.BIN/
.Trash-*/
.Trashes/
.Spotlight-V100/
.TemporaryItems/
.DocumentRevisions-V100/
.fseventd/
.apdisk
.directory
*.textClipping
photothumb.db
.fuse_hidden*
.nfs*
._*
.stfolder/
.stversions/
.stignore
*.unison
.sync.ffs_db`

// migrations[i] upgrades the global database (meta.db) from version i to i+1. Version 2 makes folder ids
// never reused (AUTOINCREMENT) and adds the epoch (design 2.4): the folder ids and seqs the clients know belong to it.
// Version 3 adds the exclusive-write locks; version 4 turns them into the write-control settings and adds the time window (design 2.8).
var migrations = []string{`
CREATE TABLE folders(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, rules TEXT NOT NULL);
CREATE TABLE devices(
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, os TEXT NOT NULL, token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL, last_seen INTEGER NOT NULL);
CREATE TABLE mappings(
  device_id INTEGER NOT NULL, folder_id INTEGER NOT NULL, path TEXT NOT NULL, adopt INTEGER NOT NULL,
  PRIMARY KEY(device_id, folder_id));
CREATE TABLE meta(key TEXT PRIMARY KEY, value) WITHOUT ROWID;
INSERT INTO meta VALUES('config_version', 1), ('keep_days', 30), ('global_rules', '` + defaultRules + `');
`, `
CREATE TABLE folders_new(id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, rules TEXT NOT NULL);
INSERT INTO folders_new SELECT id, name, rules FROM folders;
DROP TABLE folders;
ALTER TABLE folders_new RENAME TO folders;
INSERT OR IGNORE INTO meta VALUES('epoch', lower(hex(randomblob(8))));
`, `
CREATE TABLE locks(
  folder_id INTEGER NOT NULL, path TEXT NOT NULL, key TEXT NOT NULL, device_id INTEGER NOT NULL, created_at INTEGER NOT NULL,
  PRIMARY KEY(folder_id, key));
`, `
ALTER TABLE locks RENAME TO writectl;
ALTER TABLE writectl ADD COLUMN mode TEXT NOT NULL DEFAULT 'exclusive';
INSERT OR IGNORE INTO meta VALUES('write_window', 60);
`}

// folderMigrations[i] upgrades a folder database (folders/<id>.db) from version i to i+1.
var folderMigrations = []string{`
CREATE TABLE files(
  path TEXT PRIMARY KEY, key TEXT NOT NULL, seq INTEGER NOT NULL,
  hash BLOB, blocks BLOB, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, exec INTEGER NOT NULL,
  device TEXT NOT NULL, created_at INTEGER NOT NULL, conflict INTEGER NOT NULL) WITHOUT ROWID;
CREATE UNIQUE INDEX files_key ON files(key) WHERE hash IS NOT NULL;
CREATE INDEX files_conflict ON files(path) WHERE conflict AND hash IS NOT NULL;
CREATE TABLE versions(
  id INTEGER PRIMARY KEY, path TEXT NOT NULL, key TEXT NOT NULL, seq INTEGER NOT NULL,
  hash BLOB, blocks BLOB, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, exec INTEGER NOT NULL,
  device TEXT NOT NULL, created_at INTEGER NOT NULL, conflict INTEGER NOT NULL, replaced_at INTEGER NOT NULL);
CREATE INDEX versions_path ON versions(path);
CREATE TABLE meta(key TEXT PRIMARY KEY, value) WITHOUT ROWID;
INSERT INTO meta VALUES('next_seq', 1);
`, `
CREATE INDEX files_seq ON files(seq);
`, `
ALTER TABLE files ADD COLUMN link INTEGER NOT NULL DEFAULT 0;
ALTER TABLE versions ADD COLUMN link INTEGER NOT NULL DEFAULT 0;
`}

// Schema versions this binary works with.
var (
	SchemaVersion       = len(migrations)
	FolderSchemaVersion = len(folderMigrations)
)

// FolderDir is the directory of the folder databases below var/.
const FolderDir = "folders"

// FolderDBPath is the database file of a folder.
func FolderDBPath(varDir string, id int64) string {
	return filepath.Join(varDir, FolderDir, strconv.FormatInt(id, 10)+".db")
}

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

// Migrate brings the global schema to SchemaVersion, creating it in an empty database.
func Migrate(db *sql.DB) error { return migrate(db, migrations) }

// MigrateFolder does the same for a folder database.
func MigrateFolder(db *sql.DB) error { return migrate(db, folderMigrations) }

// migrate runs all missing steps in one transaction: a failing step leaves the database as it was.
func migrate(db *sql.DB, steps []string) error {
	v, err := schemaVersion(db)
	if err != nil {
		return err
	}
	if v > len(steps) {
		return &newerError{have: v, want: len(steps)}
	}
	if v == len(steps) {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for ; v < len(steps); v++ {
		if _, err := tx.Exec(steps[v]); err != nil {
			return fmt.Errorf("migrate to %d: %w", v+1, err)
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta VALUES('schema_version', ?)`, len(steps)); err != nil {
		return err
	}
	return tx.Commit()
}

// newerError is a database written by a newer binary: never opened, never changed (a downgrade).
type newerError struct{ have, want int }

func (e *newerError) Error() string {
	return fmt.Sprintf("database schema %d is newer than this binary's (%d): run the newer kaze-syncd, or put back the backup from before the upgrade (<data>/%s/%s)", e.have, e.want, VarDir, BackupDir)
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
