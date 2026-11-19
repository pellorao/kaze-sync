package client

import (
	"database/sql"
	"errors"
	"fmt"
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

// heldSchema: the local changes that wait for the user's decision (design 2.8.6). hash is the content of the local
// file when it was last seen, nil for a deletion.
const heldSchema = `
CREATE TABLE IF NOT EXISTS held(path TEXT PRIMARY KEY, scope TEXT NOT NULL, reason TEXT NOT NULL, base_seq INTEGER NOT NULL,
  kind TEXT NOT NULL, hash BLOB, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, at INTEGER NOT NULL) WITHOUT ROWID;`

// localSchemaVersion is the version of the local database this build writes; each change of the tables raises it.
// 2: the link column. 3: the held table and the frozen:, dirty: and scanc meta values (design 2.8.6). A build that finds a higher version in a database stops with errNewerDB instead of failing
// on every scan.
const localSchemaVersion = 3

// errNewerDB: the local database was written by a newer kaze-syncc.
var errNewerDB = errors.New("the state database was made by a newer kaze-syncc; upgrade this client, or restore the database backup together with the older binary")

// localDSN is the data source name of the state database at path.
func localDSN(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	return "file:" + path + "?" + q.Encode()
}

func openLocal(path string) (*localDB, error) {
	db, err := sql.Open("sqlite", localDSN(path))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(localSchema + unlinkedSchema + heldSchema); err != nil {
		db.Close()
		return nil, err
	}
	l := &localDB{db}
	if v := l.metaInt("schema_version"); v > localSchemaVersion {
		db.Close()
		return nil, fmt.Errorf("%w (database version %d, this build %d)", errNewerDB, v, localSchemaVersion)
	}
	if err := addLinkColumn(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := l.setMeta("schema_version", localSchemaVersion); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
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

// liteCols is localCols without the block hashes (32 bytes per 256 KiB of every file): the scans read every row
// and never look at them. A lite row must not be written back; only get and byHash return the blocks.
const liteCols = `path, key, seq, hash, x'' AS blocks, size, mtime_ns, exec, link`

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

// under returns the rows of all paths below dir ("" is everything), without their block hashes.
func (l *localDB) under(dir string) (map[string]localRow, error) {
	q, args := `SELECT `+liteCols+` FROM local`, []any(nil)
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
	return l.query(`SELECT ` + liteCols + ` FROM local WHERE instr(path, '/') = 0`)
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
	if _, err := tx.Exec(lowerCursorSQL, 0); err != nil { // a frozen scope reads the new history from the start too
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
	_, err := l.db.Exec(`INSERT OR REPLACE INTO unlinked(path, seq, hash, size, mtime_ns, message) VALUES(?,?,?,?,?,?)`, r.Path, r.Seq, r.Hash, r.Size, r.MtimeNs, r.Message)
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

// heldRow is a change that waits for a decision.
type heldRow struct {
	Path, Scope, Reason, Kind string
	BaseSeq                   int64
	Hash                      []byte
	Size, MtimeNs, At         int64
}

// putHeld stores a held change; a path that is held already keeps its scope, reason, base and time and only gets the
// new snapshot of the local file.
func (l *localDB) putHeld(rows []heldRow) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		if _, err := tx.Exec(`INSERT INTO held(path, scope, reason, base_seq, kind, hash, size, mtime_ns, at) VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(path) DO UPDATE SET scope=excluded.scope, kind=excluded.kind, hash=excluded.hash, size=excluded.size, mtime_ns=excluded.mtime_ns`,
			r.Path, r.Scope, r.Reason, r.BaseSeq, r.Kind, r.Hash, r.Size, r.MtimeNs, r.At); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (l *localDB) heldCount() (n int) {
	l.db.QueryRow(`SELECT count(*) FROM held`).Scan(&n)
	return n
}

// heldDel is a held deletion: the scope it froze and the size of its base (0 without one).
type heldDel struct {
	scope string
	size  int64
}

// heldDeletes returns the held deletions by path.
func (l *localDB) heldDeletes() (map[string]heldDel, error) {
	rows, err := l.db.Query(`SELECT h.path, h.scope, coalesce(l.size, 0) FROM held h LEFT JOIN local l ON l.path=h.path WHERE h.kind='delete'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]heldDel{}
	for rows.Next() {
		var p string
		var d heldDel
		if err := rows.Scan(&p, &d.scope, &d.size); err != nil {
			return nil, err
		}
		out[p] = d
	}
	return out, rows.Err()
}

// settleHeld drops, in one transaction, the held deletions of paths (a path whose held change is no longer a
// deletion stays) with, unless keepBase has the path, their base rows, and thaws as thaw does the scopes of frozen
// (with their freeze cursors) that the settled deletions were in (dels) and that have nothing held left. It returns the
// paths settled, and the scopes thawed and the cursor they were read back to.
func (l *localDB) settleHeld(paths []string, keepBase map[string]bool, dels map[string]heldDel, frozen map[string]int64) (done, free []string, low int64, err error) {
	tx, err := l.db.Begin()
	if err != nil {
		return nil, nil, 0, err
	}
	defer tx.Rollback()
	cand := map[string]int64{}
	for _, p := range paths {
		r, err := tx.Exec(`DELETE FROM held WHERE path=? AND kind='delete'`, p)
		if err != nil {
			return nil, nil, 0, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			continue
		}
		done = append(done, p)
		if v, ok := frozen[dels[p].scope]; ok {
			cand[dels[p].scope] = v
		}
		if _, err := tx.Exec(`DELETE FROM unlinked WHERE path=?`, p); err != nil { // as applyDeleted: no link to retry, also when the base row stays
			return nil, nil, 0, err
		}
		if keepBase[p] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM local WHERE path=?`, p); err != nil {
			return nil, nil, 0, err
		}
	}
	if len(cand) > 0 {
		busy, err := heldScopes(tx)
		if err != nil {
			return nil, nil, 0, err
		}
		if free, low = freeScopes(cand, busy); len(free) > 0 {
			if err := thawTx(tx, free, false, low); err != nil {
				return nil, nil, 0, err
			}
		}
	}
	return done, free, low, tx.Commit()
}

// heldScopes returns the scopes that have held changes.
func (l *localDB) heldScopes() (map[string]bool, error) { return heldScopes(l.db) }

func heldScopes(q interface {
	Query(string, ...any) (*sql.Rows, error)
}) (map[string]bool, error) {
	rows, err := q.Query(`SELECT DISTINCT scope FROM held`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sc string
		if err := rows.Scan(&sc); err != nil {
			return nil, err
		}
		out[sc] = true
	}
	return out, rows.Err()
}

// lowerCursorSQL moves the cursor and the freeze cursors that are further on than ?1 back to it.
const lowerCursorSQL = `UPDATE meta SET value=?1 WHERE (key='cursor' OR key LIKE 'frozen:%') AND value>?1`

// lowerCursor moves the cursor, and the freeze cursors with it, back to v where they are further on.
func (l *localDB) lowerCursor(v int64) error {
	_, err := l.db.Exec(lowerCursorSQL, v)
	return err
}

// thaw drops the freezes of scopes, and unless keepDirty their dirty marks, and lowers the cursors to v, in one
// transaction.
func (l *localDB) thaw(scopes []string, keepDirty bool, v int64) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := thawTx(tx, scopes, keepDirty, v); err != nil {
		return err
	}
	return tx.Commit()
}

func thawTx(tx *sql.Tx, scopes []string, keepDirty bool, v int64) error {
	for _, sc := range scopes {
		if _, err := tx.Exec(`DELETE FROM meta WHERE key=?`, "frozen:"+sc); err != nil {
			return err
		}
		if keepDirty {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM meta WHERE key=?`, "dirty:"+sc); err != nil {
			return err
		}
	}
	_, err := tx.Exec(lowerCursorSQL, v)
	return err
}

func (l *localDB) dropHeld(paths ...string) error {
	for _, p := range paths {
		if _, err := l.db.Exec(`DELETE FROM held WHERE path=?`, p); err != nil {
			return err
		}
	}
	return nil
}

// held lists the held changes by path.
func (l *localDB) held() ([]heldRow, error) {
	rows, err := l.db.Query(`SELECT path, scope, reason, base_seq, kind, hash, size, mtime_ns, at FROM held ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []heldRow
	for rows.Next() {
		var r heldRow
		if err := rows.Scan(&r.Path, &r.Scope, &r.Reason, &r.BaseSeq, &r.Kind, &r.Hash, &r.Size, &r.MtimeNs, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// metaPrefix returns the meta values whose key starts with prefix, by the rest of the key.
func (l *localDB) metaPrefix(prefix string) map[string]int64 {
	out := map[string]int64{}
	rows, err := l.db.Query(`SELECT substr(key, ?), value FROM meta WHERE key LIKE ? ESCAPE '\'`, len(prefix)+1, prefix+"%")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v int64
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

func (l *localDB) delMeta(key string) error {
	_, err := l.db.Exec(`DELETE FROM meta WHERE key=?`, key)
	return err
}
