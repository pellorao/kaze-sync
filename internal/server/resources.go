package server

import (
	"database/sql"
	"net/http"
	"time"

	"kaze-sync/internal/resources"
)

type deviceResources struct {
	ID         int64           `json:"id"`
	Name       string          `json:"name"`
	OS         string          `json:"os"`
	ReceivedAt time.Time       `json:"received_at"`
	Days       []resources.Day `json:"days"`
}
type resourceResponse struct {
	At      time.Time         `json:"at"`
	Server  []resources.Day   `json:"server"`
	Devices []deviceResources `json:"devices"`
}

// clientResourceDays bounds retention and drops GC data, which only the server
// produces. Device clocks and local dates can differ; use absolute window ends
// for freshness, leaving the original timezone in each summary.
func clientResourceDays(days []resources.Day, now time.Time) []resources.Day {
	out := make([]resources.Day, 0, 2)
	for _, d := range days {
		if len(out) == 2 {
			break
		}
		if d.End.Before(now.Add(-48*time.Hour)) || d.Start.After(now.Add(24*time.Hour)) || !d.Start.Before(d.End) {
			continue
		}
		if d.WindowSeconds != d.End.Sub(d.Start).Seconds() || d.WindowSeconds < 23*3600 || d.WindowSeconds > 25*3600 {
			continue
		}
		if d.CoverageSeconds < 0 || d.CoverageSeconds > d.WindowSeconds+0.001 || d.CPUSeconds < 0 || d.Samples > 2000 {
			continue
		}
		if d.RSSMedianMB != nil && *d.RSSMedianMB < 0 {
			continue
		}
		d.GCCycles = nil
		d.GCCut = false
		d.GCSeconds = 0
		d.GCCPUSeconds = 0
		d.NonGCSeconds = d.CoverageSeconds
		d.NonGCCPUSeconds = d.CPUSeconds
		d.Complete = d.Complete && !now.Before(d.End) && d.CoverageSeconds >= d.WindowSeconds-0.000001 && d.Samples > 0 && d.RSSMedianMB != nil
		out = append(out, d)
	}
	return out
}
func (s *Server) resourceJSON(w http.ResponseWriter, r *http.Request) error {
	now := s.now()
	devices, err := queryAll(s.db, func(rows *sql.Rows, d *deviceResources) error { return rows.Scan(&d.ID, &d.Name, &d.OS) }, `SELECT id,name,os FROM devices ORDER BY id`)
	if err != nil {
		return err
	}
	s.mu.Lock()
	for i := range devices {
		if rep := s.reports[devices[i].ID]; rep != nil {
			devices[i].ReceivedAt = rep.resourcesAt
			devices[i].Days = clientResourceDays(rep.resources, now)
		}
		if devices[i].Days == nil {
			devices[i].Days = []resources.Day{}
		}
	}
	s.mu.Unlock()
	if devices == nil {
		devices = []deviceResources{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, resourceResponse{At: now, Server: s.resources.Days(now), Devices: devices})
	return nil
}
