package server

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Live updates of the Web UI (design 4.1): a version number of everything the pages show, a stream of its changes
// (/events) and the per-folder statistics the overview would otherwise recompute with a full scan on every update.

// uiBump says that something the Web UI shows has changed.
func (s *Server) uiBump() {
	s.mu.Lock()
	s.bumpLocked()
	s.mu.Unlock()
}

func (s *Server) bumpLocked() {
	s.uiGen++
	close(s.uiCh)
	s.uiCh = make(chan struct{})
}

// StopEvents ends the /events streams; the server calls it before it shuts down, which would wait for them.
func (s *Server) StopEvents() { s.evOnce.Do(func() { close(s.evDone) }) }

type folderStat struct {
	seq, files, bytes int64
	at                time.Time
}

// statsMaxAge is how long the statistics of a folder stay when it changes: counting is a scan of the whole table.
const statsMaxAge = 30 * time.Second

// folderStats returns the number and total size of a folder's files. They are recomputed only when the folder has
// changed and the last count is older than statsMaxAge.
func (s *Server) folderStats(id int64) (files, bytes int64, err error) {
	db, err := s.folderDB(id)
	if err != nil {
		return 0, 0, err
	}
	seq := metaInt(db, "next_seq")
	s.mu.Lock()
	st, ok := s.stats[id]
	s.mu.Unlock()
	if ok && (st.seq == seq || s.now().Sub(st.at) < statsMaxAge) {
		return st.files, st.bytes, nil
	}
	if err := db.QueryRow(`SELECT count(*), coalesce(sum(size), 0) FROM files WHERE hash IS NOT NULL`).Scan(&files, &bytes); err != nil {
		return 0, 0, err
	}
	s.mu.Lock()
	s.stats[id] = folderStat{seq, files, bytes, s.now()}
	s.mu.Unlock()
	return files, bytes, nil
}

// folderSeqs returns the latest seq of every folder.
func (s *Server) folderSeqs() map[int64]int64 {
	out := map[int64]int64{}
	ids, _ := queryAll(s.db, func(rows *sql.Rows, id *int64) error { return rows.Scan(id) }, `SELECT id FROM folders`)
	for _, id := range ids {
		if db, err := s.folderDB(id); err == nil {
			out[id] = metaInt(db, "next_seq") - 1
		}
	}
	return out
}

// changedScanLimit bounds the changes folderChanged reads; with more it says "changed" without looking further.
const changedScanLimit = 5000

// folderChanged answers a folder page that has been told the folder changed (the "content updated" hint, design
// 4.1): did the change touch what its listing shows? That is a direct child of the shown directory dir that changed,
// or a subdirectory that has appeared or gone. shown (the names of the subdirectories on the page) tells which. The
// query reads the rows with a seq above since from the index on seq and stops at the first change that matters; it
// is not the listing's scan of the whole directory.
func (s *Server) folderChanged(w http.ResponseWriter, r *http.Request) error {
	f, err := s.loadFolder(r)
	if err != nil {
		return err
	}
	db, err := s.folderDB(f.ID)
	if err != nil {
		return err
	}
	now := metaInt(db, "next_seq") - 1 // taken first: what changes during the query is seen by the next one
	since, err := strconv.ParseInt(r.FormValue("since"), 10, 64)
	if err != nil || since < 0 {
		http.Error(w, "bad since", http.StatusBadRequest)
		return nil
	}
	dir, deleted := strings.Trim(r.FormValue("dir"), "/"), r.FormValue("deleted") != ""
	shown := map[string]bool{}
	for _, n := range r.Form["d"] {
		shown[n] = true
	}
	changed := false
	if now > since {
		if changed, err = listingChanged(db, dir, deleted, shown, since); err != nil {
			return err
		}
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{"changed": changed, "seq": now})
}

func listingChanged(db *sql.DB, dir string, deleted bool, shown map[string]bool, since int64) (bool, error) {
	rows, err := db.Query(`SELECT path FROM files WHERE seq > ? ORDER BY seq LIMIT ?`, since, changedScanLimit)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	n, seen := 0, map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return false, err
		}
		n++
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		name, _, deeper := strings.Cut(p[len(prefix):], "/")
		if !deeper {
			return true, nil // a file of the directory
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		// A change below a subdirectory shows only when the subdirectory has appeared or gone.
		q := `SELECT count(*) FROM (SELECT 1 FROM files WHERE path >= ? AND path < ?`
		if !deleted {
			q += ` AND hash IS NOT NULL`
		}
		var have int
		if err := db.QueryRow(q+` LIMIT 1)`, prefix+name+"/", prefix+name+"0").Scan(&have); err != nil {
			return false, err
		}
		if (have > 0) != shown[name] {
			return true, nil
		}
	}
	return n == changedScanLimit, rows.Err()
}

// events streams the Web UI's version number as server-sent events: at once when the stream opens and after that
// when it has changed, at most once per EventGap. Nothing is sent, and nothing runs, while nothing changes.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !s.hasSession(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	io.WriteString(w, "retry: 10000\n\n")
	fl.Flush()
	sent := int64(-1)
	var sentAt time.Time
	for {
		s.mu.Lock()
		gen, ch := s.uiGen, s.uiCh // the channel is taken together with the version, so no change is missed
		s.mu.Unlock()
		if gen != sent {
			if wait := s.EventGap - time.Since(sentAt); !sentAt.IsZero() && wait > 0 {
				select {
				case <-time.After(wait):
				case <-r.Context().Done():
					return
				case <-s.evDone:
					return
				}
				continue // take the version again: changes during the wait go out together
			}
			data, _ := json.Marshal(map[string]any{"gen": gen, "seq": s.folderSeqs()})
			if _, err := io.WriteString(w, "data: "+string(data)+"\n\n"); err != nil {
				return
			}
			fl.Flush()
			sent, sentAt = gen, time.Now()
			continue
		}
		select {
		case <-ch:
		case <-r.Context().Done():
			return
		case <-s.evDone:
			return
		}
	}
}
