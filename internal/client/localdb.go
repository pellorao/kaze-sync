package client

import (
	"database/sql"
	"net/url"

	_ "modernc.org/sqlite"
)

// localRow is the state of a path the last time it matched the server: the
// base of the next commit (design 5.1).
type localRow struct {
	Path, Key string
	Seq       int64
	Hash      []byte
	Blocks    []byte
	Size      int64
	MtimeNs   int64
	Exec      bool
}

// localDB is the per-folder state database folder-<id>.db.
type localDB struct{ db *sql.DB }

const localSchema = `
CREATE TABLE IF NOT EXISTS local(path TEXT PRIMARY KEY, key TEXT NOT NULL, seq INTEGER NOT NULL, hash BLOB NOT NULL,
  blocks BLOB NOT NULL, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, exec INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value) WITHOUT ROWID;
INSERT OR IGNORE INTO meta VALUES('schema_version', 1);`

func openLocal(path string) (*localDB, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(localSchema); err != nil {
		db.Close()
		return nil, err
	}
	return &localDB{db}, nil
}

func (l *localDB) Close() error { return l.db.Close() }

func (l *localDB) all() (map[string]localRow, error) {
	rows, err := l.db.Query(`SELECT path, key, seq, hash, blocks, size, mtime_ns, exec FROM local`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]localRow{}
	for rows.Next() {
		var r localRow
		if err := rows.Scan(&r.Path, &r.Key, &r.Seq, &r.Hash, &r.Blocks, &r.Size, &r.MtimeNs, &r.Exec); err != nil {
			return nil, err
		}
		out[r.Path] = r
	}
	return out, rows.Err()
}

// apply stores and deletes rows in one transaction.
func (l *localDB) apply(puts []localRow, dels []string) error {
	if len(puts) == 0 && len(dels) == 0 {
		return nil
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range dels {
		if _, err := tx.Exec(`DELETE FROM local WHERE path=?`, p); err != nil {
			return err
		}
	}
	for _, r := range puts {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO local VALUES(?,?,?,?,?,?,?,?)`,
			r.Path, r.Key, r.Seq, r.Hash, r.Blocks, r.Size, r.MtimeNs, r.Exec); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (l *localDB) metaString(key string) string {
	var v sql.NullString
	l.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	return v.String
}

func (l *localDB) setMeta(key string, v any) error {
	_, err := l.db.Exec(`INSERT OR REPLACE INTO meta VALUES(?, ?)`, key, v)
	return err
}
