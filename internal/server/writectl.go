package server

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// lockRow is one write-control setting (design 2.8.2): a path, its mode, and for an exclusive one the device that
// writes it. Path "" is the whole folder.
type lockRow struct {
	Path, key string
	Mode      string
	DeviceID  int64
	Device    string
	CreatedAt int64
}

// covers reports whether the setting covers a path whose key is k.
func (l lockRow) covers(k string) bool {
	return l.key == "" || k == l.key || strings.HasPrefix(k, l.key+"/")
}

// folderWC lists all settings of a folder, shortest path first; the device name comes from the devices table.
func folderWC(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, folder int64) ([]lockRow, error) {
	return queryAll(q, func(rows *sql.Rows, l *lockRow) error {
		return rows.Scan(&l.Path, &l.key, &l.Mode, &l.DeviceID, &l.Device, &l.CreatedAt)
	}, `SELECT w.path, w.key, w.mode, w.device_id, coalesce(d.name, ''), w.created_at FROM writectl w LEFT JOIN devices d ON d.id=w.device_id WHERE w.folder_id=? ORDER BY w.key`, folder)
}

// folderLocks lists the exclusive settings of a folder.
func folderLocks(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, folder int64) ([]lockRow, error) {
	all, err := folderWC(q, folder)
	out := all[:0]
	for _, l := range all {
		if l.Mode == proto.ModeExclusive {
			out = append(out, l)
		}
	}
	return out, err
}

// ruleFor is the setting that governs key k: the longest path that covers it; the root is single without a row.
func ruleFor(rules []lockRow, k string) lockRow {
	best := lockRow{Mode: proto.ModeSingle}
	for _, l := range rules { // ordered by key: the last cover is the longest
		if l.covers(k) {
			best = l
		}
	}
	return best
}

// wcScope is the unit of the ownership check (design 2.8.2): a setting and what it governs, or one file of a free
// directory.
type wcScope struct {
	rule lockRow
	id   string // "d:<key>" for a setting, "f:<key>" for a file of a free directory
	path string // the path shown to the user
}

func scopeOf(rules []lockRow, path string) wcScope {
	k := pp.Key(path)
	r := ruleFor(rules, k)
	if r.Mode == proto.ModeFree {
		return wcScope{r, "f:" + k, path}
	}
	return wcScope{r, "d:" + r.key, r.Path}
}

// ownerCheck is the ownership check of one scope for a batch from device (design 2.8.3): nil lets it through, else the
// reason. since is the smallest `since` of the batch's ops in the scope, window the time window in seconds.
func ownerCheck(q *sql.Tx, rules []lockRow, sc wcScope, devID int64, device string, since, window int64, now int64, firstPath string) (*proto.OwnerInfo, error) {
	switch sc.rule.Mode {
	case proto.ModeExclusive:
		if sc.rule.DeviceID == devID {
			return nil, nil
		}
		return &proto.OwnerInfo{Scope: sc.path, Mode: sc.rule.Mode, Device: sc.rule.Device, Cond: "exclusive"}, nil
	case proto.ModeFree: // one file: only the time window, the base check does the rest
		var dev string
		var created int64
		err := q.QueryRow(`SELECT device, created_at FROM files WHERE path=?`, firstPath).Scan(&dev, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if inWindow(dev, device, created, now, window) {
			return &proto.OwnerInfo{Scope: sc.path, Mode: sc.rule.Mode, Device: dev, Cond: "window", Ref: created + window}, nil
		}
		return nil, nil
	}
	rows, err := q.Query(`SELECT seq, key, device, created_at FROM files ORDER BY seq DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	first, saw := true, false
	for rows.Next() {
		var seq, created int64
		var key, dev string
		if err := rows.Scan(&seq, &key, &dev, &created); err != nil {
			return nil, err
		}
		if dev == excludedDevice {
			continue
		}
		if ruleFor(rules, key).key != sc.rule.key {
			if !saw && seq <= since && now-created >= window { // older rows are older still, unless the clock went back: then keep looking
				return nil, nil
			}
			continue
		}
		saw = true
		other := dev != device
		if first && other && inWindow(dev, device, created, now, window) {
			return &proto.OwnerInfo{Scope: sc.path, Mode: sc.rule.Mode, Device: dev, Cond: "window", Ref: created + window}, nil
		}
		first = false
		if !other || seq <= since {
			return nil, nil
		}
		return &proto.OwnerInfo{Scope: sc.path, Mode: sc.rule.Mode, Device: dev, Cond: "stale", Ref: seq}, nil
	}
	return nil, rows.Err()
}

// inWindow: the write of dev made at created is another device's and arrived less than window seconds ago. A clock
// that went back (negative age) counts as passed (design 2.8.3).
func inWindow(dev, device string, created, now, window int64) bool {
	age := now - created
	return dev != device && dev != excludedDevice && age >= 0 && age < window
}

// setWC sets the mode of path in a folder in one transaction (design 2.8.10); mode "inherit" removes the setting. It
// reports whether the configuration the devices get changed. The mapping check fails with an error message the
// dialog shows.
func setWC(tx *sql.Tx, folder int64, path, mode string, device int64, now int64) (changed bool, _ error) {
	key := pp.Key(path)
	var curMode string
	var curDev int64
	err := tx.QueryRow(`SELECT mode, device_id FROM writectl WHERE folder_id=? AND key=?`, folder, key).Scan(&curMode, &curDev)
	have := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if mode == "inherit" {
		if !have {
			return false, nil
		}
		_, err := tx.Exec(`DELETE FROM writectl WHERE folder_id=? AND key=?`, folder, key)
		return true, err
	}
	if mode != proto.ModeFree && mode != proto.ModeSingle && mode != proto.ModeExclusive {
		return false, errors.New("bad write-control mode")
	}
	if mode != proto.ModeExclusive {
		device = 0
	} else {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM mappings WHERE device_id=? AND folder_id=?`, device, folder).Scan(&n); err != nil {
			return false, err
		}
		if n == 0 {
			dn := strconv.FormatInt(device, 10) // a device that is not there is named by its id
			tx.QueryRow(`SELECT name FROM devices WHERE id=?`, device).Scan(&dn)
			return false, M("err.lock.unmapped", "device", dn)
		}
	}
	if have && curMode == mode && curDev == device {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO writectl(folder_id, path, key, mode, device_id, created_at) VALUES(?,?,?,?,?,?)`, folder, path, key, mode, device, now); err != nil {
		return false, err
	}
	// Saving the default for the root (single, no row before) confirms it: the devices get nothing new.
	return !(key == "" && !have && mode == proto.ModeSingle), nil
}

// releaseExclusive turns the exclusive settings of a device (in one folder, or in all with folder 0) into single.
func releaseExclusive(tx *sql.Tx, device, folder int64) (int64, error) {
	q, args := `UPDATE writectl SET mode='single', device_id=0 WHERE mode='exclusive' AND device_id=?`, []any{device}
	if folder != 0 {
		q, args = q+` AND folder_id=?`, append(args, folder)
	}
	res, err := tx.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// writectlHandler sets, moves or removes a setting (POST /folders/{id}/writectl; fields path, mode, device). Any save
// confirms the folder's write rules (design 2.8.11).
func (s *Server) writectlHandler(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	mode := r.FormValue("mode")
	p := strings.Trim(r.FormValue("path"), "/")
	back := "/folders/" + strconv.FormatInt(f.ID, 10)
	if p != "" && pp.CheckPortable(p) != nil || p == "" && mode == "inherit" {
		return fieldErr(s.folderPage, http.StatusBadRequest, "dlg-wc", "path", "err.lock.path")
	}
	if mode == proto.ModeFree && s.isFile(f.ID, p) { // design 2.8.2: a file is never unrestricted
		return fieldErr(s.folderPage, http.StatusBadRequest, "dlg-wc", "path", "err.lock.fileFree")
	}
	dev, _ := strconv.ParseInt(r.FormValue("device"), 10, 64)
	err = s.configChange(func(tx *sql.Tx) (bool, error) {
		changed, err := setWC(tx, f.ID, p, mode, dev, s.now().Unix())
		var m msg
		if errors.As(err, &m) {
			return false, fieldErr(s.folderPage, http.StatusBadRequest, "dlg-wc", "path", m.key, m.kv...)
		}
		if err == nil {
			_, err = tx.Exec(`UPDATE folders SET wc_ack=1 WHERE id=?`, f.ID)
		}
		return changed, err
	})
	if err != nil {
		return err
	}
	s.uiBump() // the new-folder reminder of the notification bar goes
	http.Redirect(w, r, back+"?dir="+url.QueryEscape(strings.Trim(r.FormValue("dir"), "/")), http.StatusSeeOther)
	return nil
}
