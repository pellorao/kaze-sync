package server

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	pp "kaze-sync/internal/proto/path"
)

// lockRow is one exclusive-write claim (design 2.8): the device writes the path and everything below it; the other
// devices of the folder only receive it. Path "" is the whole folder.
type lockRow struct {
	Path, key string
	DeviceID  int64
	Device    string
	CreatedAt int64
}

// covers reports whether the lock covers a path whose key is k.
func (l lockRow) covers(k string) bool {
	return l.key == "" || k == l.key || strings.HasPrefix(k, l.key+"/")
}

// folderLocks lists the locks of a folder, oldest path first; the device name comes from the devices table.
func folderLocks(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, folder int64) ([]lockRow, error) {
	return queryAll(q, func(rows *sql.Rows, l *lockRow) error {
		return rows.Scan(&l.Path, &l.key, &l.DeviceID, &l.Device, &l.CreatedAt)
	}, `SELECT l.path, l.key, l.device_id, d.name, l.created_at FROM locks l JOIN devices d ON d.id=l.device_id WHERE l.folder_id=? ORDER BY l.key`, folder)
}

// lockedBy returns the lock that stops device (a device API id) from writing key, or nil.
func lockedBy(locks []lockRow, devID int64, key string) *lockRow {
	for i := range locks {
		if locks[i].DeviceID != devID && locks[i].covers(key) {
			return &locks[i]
		}
	}
	return nil
}

// setLock makes device (0: nobody) the exclusive writer of path in a folder, in one transaction (design 2.8). The
// mapping check fails with an error message the dialog shows.
func setLock(tx *sql.Tx, folder int64, path string, device int64, now int64) (changed bool, _ error) {
	key := pp.Key(path)
	if device == 0 {
		res, err := tx.Exec(`DELETE FROM locks WHERE folder_id=? AND key=?`, folder, key)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM mappings WHERE device_id=? AND folder_id=?`, device, folder).Scan(&n); err != nil {
		return false, err
	}
	if n == 0 {
		var dn string
		tx.QueryRow(`SELECT name FROM devices WHERE id=?`, device).Scan(&dn)
		return false, M("err.lock.unmapped", "device", dn)
	}
	var cur int64
	err := tx.QueryRow(`SELECT device_id FROM locks WHERE folder_id=? AND key=?`, folder, key).Scan(&cur)
	if err == nil && cur == device {
		return false, nil
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO locks(folder_id, path, key, device_id, created_at) VALUES(?,?,?,?,?)`, folder, path, key, device, now)
	return true, err
}

// lockHandler sets, moves or removes the exclusive writer of a path (POST /folders/{id}/lock; field device empty: remove).
func (s *Server) lockHandler(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	p := strings.Trim(r.FormValue("path"), "/")
	back := "/folders/" + strconv.FormatInt(f.ID, 10)
	if p != "" && pp.CheckPortable(p) != nil {
		return fieldErr(s.folderPage, http.StatusBadRequest, "dlg-lock", "path", "err.lock.path")
	}
	dev, _ := strconv.ParseInt(r.FormValue("device"), 10, 64)
	err = s.configChange(func(tx *sql.Tx) (bool, error) {
		changed, err := setLock(tx, f.ID, p, dev, s.now().Unix())
		var m msg
		if errors.As(err, &m) {
			return false, fieldErr(s.folderPage, http.StatusBadRequest, "dlg-lock", "path", m.key, m.kv...)
		}
		return changed, err
	})
	if err != nil {
		return err
	}
	http.Redirect(w, r, back+"?dir="+url.QueryEscape(strings.Trim(r.FormValue("dir"), "/")), http.StatusSeeOther)
	return nil
}
