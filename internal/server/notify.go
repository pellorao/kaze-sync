package server

import (
	"database/sql"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"kaze-sync/internal/proto"
)

// offlineAfter is how long a device may be silent before it raises a notice (design 4.3).
const offlineAfter = 7 * 24 * time.Hour

// notice is one line of a folder's notifications. Notices are computed from
// the current state on every request; none is stored (design 4.3).
type notice struct {
	Error                  bool // an error needs action to go away; otherwise it is a reminder
	Device, Path, Message  string
	Since                  string
	Advice, Link, Reported string // Reported: when the device last reported, if it is offline
}

var advice = map[string]string{
	proto.KindRejected: "在该客户端上改名 (大小写冲突时改成不冲突的名称, 非法名称时去掉非法字符), 或用规则排除",
	proto.KindIO:       "按消息检查该客户端的磁盘或权限",
	proto.KindFolder:   "挂载磁盘, 或在设备页修改路径或勾选接管",
	proto.KindInUse:    "关闭占用该文件的程序; SQLite 数据库需要先关闭应用",
	proto.KindSkipped:  "不需要时用排除规则排除",
}

const conflictAdvice = "在任一客户端上比较两份, 删除冲突副本, 或改名以同时保留"

// browseLink points at the directory holding p in the folder's file browser.
func browseLink(folder int64, p string) string {
	dir := ""
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		dir = p[:i]
	}
	return "/folders/" + strconv.FormatInt(folder, 10) + "?dir=" + url.QueryEscape(dir)
}

// folderNotices lists the notices of a folder: what the mapped devices report,
// and the conflict copies on the server.
func (s *Server) folderNotices(folder int64) ([]notice, error) {
	type dev struct {
		id   int64
		name string
	}
	devs, err := queryAll(s.db, func(rows *sql.Rows, d *dev) error { return rows.Scan(&d.id, &d.name) },
		`SELECT d.id, d.name FROM mappings m JOIN devices d ON d.id=m.device_id WHERE m.folder_id=? ORDER BY d.name`, folder)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []notice
	s.mu.Lock()
	for _, d := range devs {
		rep := s.reports[d.id]
		if rep == nil {
			continue
		}
		stale := ""
		if !rep.online(now) {
			stale = "最后上报于 " + formatTime(rep.at.Unix())
		}
		for _, fs := range rep.req.Folders {
			if fs.ID != folder {
				continue
			}
			for _, e := range fs.Errors {
				n := notice{Device: d.name, Path: e.Path, Message: e.Message, Since: formatTime(e.Since), Advice: advice[e.Kind], Reported: stale,
					Error: e.Kind != proto.KindInUse && e.Kind != proto.KindSkipped}
				if e.Kind == proto.KindFolder {
					n.Link = "/devices/" + strconv.FormatInt(d.id, 10)
				} else {
					n.Link = browseLink(folder, e.Path)
				}
				out = append(out, n)
			}
		}
	}
	s.mu.Unlock()
	db, err := s.folderDB(folder)
	if err != nil {
		return nil, err
	}
	conflicts, err := queryAll(db, func(rows *sql.Rows, n *notice) error {
		var created int64
		err := rows.Scan(&n.Path, &n.Device, &created)
		n.Since = formatTime(created)
		return err
	}, `SELECT path, device, created_at FROM files WHERE conflict AND hash IS NOT NULL ORDER BY path`)
	if err != nil {
		return nil, err
	}
	for _, n := range conflicts {
		n.Error, n.Message, n.Advice, n.Link = true, "冲突副本", conflictAdvice, browseLink(folder, n.Path)
		out = append(out, n)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Error && !out[j].Error })
	return out, nil
}

// offlineDevices lists the devices that have not connected for offlineAfter.
func (s *Server) offlineDevices() ([]offlineDevice, error) {
	type row struct {
		id            int64
		name          string
		seen, created int64
	}
	rows, err := queryAll(s.db, func(rows *sql.Rows, r *row) error { return rows.Scan(&r.id, &r.name, &r.seen, &r.created) },
		`SELECT id, name, last_seen, created_at FROM devices ORDER BY name`)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []offlineDevice
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		last := time.Unix(max(r.seen, r.created), 0)
		if rep := s.reports[r.id]; rep != nil && rep.at.After(last) {
			last = rep.at
		}
		if now.Sub(last) >= offlineAfter {
			out = append(out, offlineDevice{ID: r.id, Name: r.name, Days: int(now.Sub(last) / (24 * time.Hour))})
		}
	}
	return out, nil
}

type offlineDevice struct {
	ID   int64
	Name string
	Days int
}

// folderSummary is a folder's row of the overview's notification summary.
type folderSummary struct {
	ID               int64
	Name             string
	Errors, Warnings int
}

// deviceStatus is one folder line of a device in the overview.
type deviceStatus struct {
	Folder, State          string
	Synced                 string
	PendingUp, PendingDown int
}
