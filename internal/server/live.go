package server

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
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
