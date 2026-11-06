package server

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"database/sql"
)

// tunables of the push checks (design 4.3)
const (
	pushEvery        = 5 * time.Minute // how often the state is looked at
	pushErrorsAfter  = 2               // device-reported errors must be seen this many checks in a row (a retry may clear them)
	pushDiskLow      = 256 << 20       // free bytes on the data disk below which the user is told
	pushDiskLowLabel = "256 MB"
)

// pushLoop looks at the state every pushEvery and pushes what needs the user. The states are the same ones the
// notification popover shows, computed again; nothing is stored (design 4.3).
func (s *Server) pushLoop(ctx context.Context) {
	if s.Push == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(pushEvery):
		}
		s.pushCheck()
	}
}

// pushCheck is one look: changes on hold, errors devices report, devices offline for long, and the free disk.
// Messages name a folder or a device, never a file.
func (s *Server) pushCheck() {
	p := s.Push
	if p == nil {
		return
	}
	fols, err := queryAll(s.db, func(rows *sql.Rows, f *folderRow) error { return rows.Scan(&f.ID, &f.Name) }, `SELECT id, name FROM folders`)
	if err != nil {
		slog.Warn("push check", "err", err)
		return
	}
	for _, f := range fols {
		notes, err := s.folderNotices(f.ID)
		if err != nil {
			continue
		}
		held, errs := 0, 0
		for _, n := range notes {
			switch {
			case n.Kind == kindHeld:
				held += n.N
			case n.Error:
				errs++
			}
		}
		id := strconv.FormatInt(f.ID, 10)
		if held > 0 {
			p.Fail("held-"+id, strconv.Itoa(held)+" changes in folder "+f.Name+" are on hold for your decision: open the web page and choose", 1)
		} else {
			p.Ok("held-"+id, "No more changes on hold in folder "+f.Name)
		}
		if errs > 0 {
			p.Fail("errors-"+id, strconv.Itoa(errs)+" errors reported by devices in folder "+f.Name+": see the notifications on the web page", pushErrorsAfter)
		} else {
			p.Ok("errors-"+id, "Devices report no more errors in folder "+f.Name)
		}
	}
	if off, err := s.offlineDevices(); err == nil {
		now := map[int64]bool{}
		for _, d := range off {
			now[d.ID] = true
			p.Fail("offline-"+strconv.FormatInt(d.ID, 10), "Device "+d.Name+" has not connected for "+strconv.Itoa(d.Days)+" days", 1)
		}
		if devs, err := queryAll(s.db, func(rows *sql.Rows, d *offlineDevice) error { return rows.Scan(&d.ID, &d.Name) }, `SELECT id, name FROM devices`); err == nil {
			for _, d := range devs {
				if !now[d.ID] {
					p.Ok("offline-"+strconv.FormatInt(d.ID, 10), "Device "+d.Name+" is back")
				}
			}
		}
	}
	if free, ok := freeSpace(s.varDir); ok {
		if free < pushDiskLow {
			p.Fail("disk", "Free disk space on the server is below "+pushDiskLowLabel, 1)
		} else {
			p.Ok("disk", "Free disk space on the server is fine again")
		}
	}
}
