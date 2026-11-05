package server

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BackupDir is the directory below var/ that holds the copies taken before a schema migration (design 9.3).
const BackupDir = "backups"

// keepBackups is how many pre-migration copies of one database are kept (the newest ones).
const keepBackups = 3

const backupTimeFormat = "20060102T150405Z"

// migrateState brings meta.db and every folder database to the current schema (design 9.3). It is the one
// migration path: kaze-syncd run (New) and install both come here. A database that is older than this binary
// is copied to var/backups first; one that is newer, or a migration that fails, stops it with the database unchanged.
func migrateState(db *sql.DB, varDir string, now time.Time) error {
	if err := migrateBacked(db, migrations, varDir, "meta", now); err != nil {
		return fmt.Errorf("%s: %w", DBFile, err)
	}
	return migrateFolders(db, varDir, now)
}

// migrateFolders migrates the database of every folder; a missing one is an error, never recreated.
func migrateFolders(db *sql.DB, varDir string, now time.Time) error {
	ids, err := queryAll(db, func(rows *sql.Rows, id *int64) error { return rows.Scan(id) }, `SELECT id FROM folders ORDER BY id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		fdb, err := OpenDB(FolderDBPath(varDir, id), false)
		if err == nil {
			err = migrateBacked(fdb, folderMigrations, varDir, folderBackupName(id), now)
			fdb.Close()
		}
		if err != nil {
			return fmt.Errorf("folder %d: %w", id, err)
		}
	}
	return nil
}

func folderBackupName(id int64) string { return "folder-" + strconv.FormatInt(id, 10) }

// migrateBacked migrates one database, after a backup when it holds an older schema. An empty database
// (a new one) has nothing to back up.
func migrateBacked(db *sql.DB, steps []string, varDir, name string, now time.Time) error {
	v, err := schemaVersion(db)
	if err != nil {
		return err
	}
	if v >= len(steps) {
		return migrate(db, steps) // current: nothing; newer: the refusal
	}
	if v > 0 {
		if err := backupDB(db, varDir, name, v, now); err != nil {
			return fmt.Errorf("backup before migrating from schema %d failed, nothing was changed: %w", v, err)
		}
	}
	if err := migrate(db, steps); err != nil {
		return fmt.Errorf("%w (the database is unchanged)", err)
	}
	if v > 0 {
		pruneBackups(varDir, name)
	}
	return nil
}

// backupDB writes a consistent copy of the database (WAL included) as var/backups/<name>-v<version>-<time>.db.
// It is complete or absent: written under a temporary name first.
func backupDB(db *sql.DB, varDir, name string, version int, now time.Time) error {
	dir := filepath.Join(varDir, BackupDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	final := filepath.Join(dir, fmt.Sprintf("%s-v%d-%s.db", name, version, now.UTC().Format(backupTimeFormat)))
	tmp := final + ".tmp"
	os.Remove(tmp)
	if _, err := db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// backupsOf lists the backups of one database, oldest first (by the time in the name).
func backupsOf(varDir, name string) []string {
	ents, _ := os.ReadDir(filepath.Join(varDir, BackupDir))
	type item struct{ file, at string }
	var items []item
	for _, e := range ents {
		f := e.Name()
		rest, ok := strings.CutPrefix(f, name+"-v")
		rest, ok2 := strings.CutSuffix(rest, ".db")
		// rest is <version>-<time>; the name is followed by -v, so folder-1 never matches folder-12
		if v, at, found := strings.Cut(rest, "-"); ok && ok2 && found && isDigits(v) {
			items = append(items, item{f, at})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].at < items[j].at || items[i].at == items[j].at && items[i].file < items[j].file
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = filepath.Join(varDir, BackupDir, it.file)
	}
	return out
}

func isDigits(s string) bool {
	_, err := strconv.ParseUint(s, 10, 32)
	return err == nil
}

// pruneBackups keeps the newest keepBackups copies of one database. A failure only leaves old copies behind.
func pruneBackups(varDir, name string) {
	all := backupsOf(varDir, name)
	for len(all) > keepBackups {
		os.Remove(all[0])
		all = all[1:]
	}
}

// removeBackups deletes every backup of a database (its folder was deleted: the data must go with it).
func removeBackups(varDir, name string) error {
	var errs []error
	for _, p := range backupsOf(varDir, name) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
