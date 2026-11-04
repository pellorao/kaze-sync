package client

import (
	"database/sql"
	"errors"
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
	Link      bool // a symbolic link: Hash is the SHA-256 of its target (design 6.7)
}

// localDB is the per-folder state database folder-<id>.db.
type localDB struct{ db *sql.DB }

const localSchema = `
CREATE TABLE IF NOT EXISTS local(path TEXT PRIMARY KEY, key TEXT NOT NULL, seq INTEGER NOT NULL, hash BLOB NOT NULL,
  blocks BLOB NOT NULL, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, exec INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS local_hash ON local(hash);
INSERT OR IGNORE INTO meta VALUES('schema_version', 1);`

// unlinkedSchema: the links of the server this machine could not create (design 6.7). The rows are the state the
// Web UI notice is computed from: a row goes when the link is created, deleted or replaced.
const unlinkedSchema = `
CREATE TABLE IF NOT EXISTS unlinked(path TEXT PRIMARY KEY, seq INTEGER NOT NULL, hash BLOB NOT NULL, size INTEGER NOT NULL,
  mtime_ns INTEGER NOT NULL, message TEXT NOT NULL) WITHOUT ROWID;`

func openLocal(path string) (*localDB, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(localSchema + unlinkedSchema); err != nil {
		db.Close()
		return nil, err
	}
	if err := addLinkColumn(db); err != nil {
		db.Close()
		return nil, err
	}
	return &localDB{db}, nil
}

func (l *localDB) Close() error { return l.db.Close() }

// addLinkColumn adds the link column to a database made before symbolic links were synced.
func addLinkColumn(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('local') WHERE name='link'`).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := db.Exec(`ALTER TABLE local ADD COLUMN link INTEGER NOT NULL DEFAULT 0`)
	return err
}

const localCols = `path, key, seq, hash, blocks, size, mtime_ns, exec, link`

func (r *localRow) scan(s interface{ Scan(...any) error }) error {
	return s.Scan(&r.Path, &r.Key, &r.Seq, &r.Hash, &r.Blocks, &r.Size, &r.MtimeNs, &r.Exec, &r.Link)
}

// get returns the row of a path.
func (l *localDB) get(p string) (r localRow, ok bool, err error) {
	err = r.scan(l.db.QueryRow(`SELECT `+localCols+` FROM local WHERE path=?`, p))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// under returns the rows of all paths below dir ("" is everything).
func (l *localDB) under(dir string) (map[string]localRow, error) {
	q, args := `SELECT `+localCols+` FROM local`, []any(nil)
	if dir != "" {
		q += ` WHERE path >= ? AND path < ?` // "0" is the character after "/"
		args = []any{dir + "/", dir + "0"}
	}
	return l.query(q, args...)
}

// byHash returns up to limit rows whose whole-file hash is h.
func (l *localDB) byHash(h []byte, limit int) ([]localRow, error) {
	m, err := l.query(`SELECT `+localCols+` FROM local WHERE hash=? LIMIT ?`, h, limit)
	out := make([]localRow, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	return out, err
}

// rootFiles returns the rows of the files directly in the root.
func (l *localDB) rootFiles() (map[string]localRow, error) {
	return l.query(`SELECT ` + localCols + ` FROM local WHERE instr(path, '/') = 0`)
}

// topDirs returns the names of the root's directories that have rows below them.
func (l *localDB) topDirs() (map[string]bool, error) {
	rows, err := l.db.Query(`SELECT DISTINCT substr(path, 1, instr(path, '/') - 1) FROM local WHERE instr(path, '/') > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out[d] = true
	}
	return out, rows.Err()
}

func (l *localDB) all() (map[string]localRow, error) { return l.under("") }

func (l *localDB) query(q string, args ...any) (map[string]localRow, error) {
	rows, err := l.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]localRow{}
	for rows.Next() {
		var r localRow
		if err := r.scan(rows); err != nil {
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
		if _, err := tx.Exec(`INSERT OR REPLACE INTO local(`+localCols+`) VALUES(?,?,?,?,?,?,?,?,?)`,
			r.Path, r.Key, r.Seq, r.Hash, r.Blocks, r.Size, r.MtimeNs, r.Exec, r.Link); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// forgetSeqs makes the next Pull read the server's state from the start and drops the base seq of every row, so
// that no entry of a server with another history is taken for an echo of ours (design 5.5).
func (l *localDB) forgetSeqs() error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE local SET seq=0`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta VALUES('cursor', 0)`); err != nil {
		return err
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

func (l *localDB) metaInt(key string) int64 {
	var v int64
	l.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	return v
}

// unlinkedRow is a link the server has and this machine could not create.
type unlinkedRow struct {
	Path    string
	Seq     int64
	Hash    []byte
	Size    int64
	MtimeNs int64
	Message string
}

func (l *localDB) putUnlinked(r unlinkedRow) error {
	_, err := l.db.Exec(`INSERT OR REPLACE INTO unlinked VALUES(?,?,?,?,?,?)`, r.Path, r.Seq, r.Hash, r.Size, r.MtimeNs, r.Message)
	return err
}

func (l *localDB) dropUnlinked(p string) error {
	_, err := l.db.Exec(`DELETE FROM unlinked WHERE path=?`, p)
	return err
}

func (l *localDB) unlinked() ([]unlinkedRow, error) {
	rows, err := l.db.Query(`SELECT path, seq, hash, size, mtime_ns, message FROM unlinked ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []unlinkedRow
	for rows.Next() {
		var r unlinkedRow
		if err := rows.Scan(&r.Path, &r.Seq, &r.Hash, &r.Size, &r.MtimeNs, &r.Message); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
