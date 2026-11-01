package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"kaze-sync/internal/blobstore"
	"kaze-sync/internal/proto"
)

type device struct {
	id   int64
	name string
	os   string
}

func (s *Server) deviceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/enroll", s.enroll)
	mux.HandleFunc("POST /api/v1/poll", s.auth(s.poll))
	mux.HandleFunc("POST /api/v1/commit", s.auth(s.commitHandler))
	mux.HandleFunc("POST /api/v1/blocks", s.auth(s.blocks))
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, proto.MaxJSONBody))
	if err := dec.Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) auth(h func(http.ResponseWriter, *http.Request, device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.locked(r) {
			http.Error(w, "too many failures", http.StatusTooManyRequests)
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		var d device
		if ok {
			hash := sha256.Sum256([]byte(tok))
			err := s.db.QueryRow(`SELECT id, name, os FROM devices WHERE token_hash=?`, hash[:]).Scan(&d.id, &d.name, &d.os)
			ok = err == nil
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if !ok {
			s.authFailed(r, "device token")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r, d)
	}
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	if s.locked(r) {
		http.Error(w, "too many failures", http.StatusTooManyRequests)
		return
	}
	var req proto.EnrollReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.OS != "linux" && req.OS != "windows" {
		http.Error(w, "unsupported os", http.StatusBadRequest)
		return
	}
	key := sha(normalizeSecret(req.Code))
	s.mu.Lock()
	pc, ok := s.codes[key]
	delete(s.codes, key)
	s.mu.Unlock()
	if !ok || s.now().After(pc.expires) {
		s.authFailed(r, "enrollment code")
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}
	tok := newToken()
	hash := sha256.Sum256([]byte(tok))
	res, err := s.db.Exec(`INSERT INTO devices(name, os, token_hash, created_at, last_seen) VALUES(?,?,?,?,0)`, pc.name, req.OS, hash[:], s.now().Unix())
	if err != nil {
		http.Error(w, "cannot register device: "+err.Error(), http.StatusConflict)
		return
	}
	id, _ := res.LastInsertId()
	slog.Info("device enrolled", "device", pc.name, "os", req.OS)
	writeJSON(w, proto.EnrollResp{DeviceID: id, Token: tok})
}

// effectiveConfig is the device's mappings with global and folder rules joined.
func (s *Server) effectiveConfig(deviceID int64) (proto.Config, error) {
	cfg := proto.Config{Version: metaInt(s.db, "config_version"), Folders: []proto.FolderConfig{}}
	global := splitRules(MetaString(s.db, "global_rules"))
	rows, err := s.db.Query(`SELECT f.id, f.name, m.path, m.adopt, f.rules FROM mappings m JOIN folders f ON f.id=m.folder_id WHERE m.device_id=? ORDER BY f.id`, deviceID)
	if err != nil {
		return cfg, err
	}
	defer rows.Close()
	for rows.Next() {
		var fc proto.FolderConfig
		var rules string
		if err := rows.Scan(&fc.ID, &fc.Name, &fc.Path, &fc.Adopt, &rules); err != nil {
			return cfg, err
		}
		fc.Rules = append(append([]string{}, global...), splitRules(rules)...)
		cfg.Folders = append(cfg.Folders, fc)
	}
	return cfg, rows.Err()
}

func splitRules(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

// poll hangs until the device's configuration differs from the version it
// reports, or PollTimeout passes. It also carries the device's status report.
func (s *Server) poll(w http.ResponseWriter, r *http.Request, d device) {
	var req proto.PollReq
	if !readJSON(w, r, &req) {
		return
	}
	s.noteReport(d.id, req)
	timer := time.NewTimer(s.PollTimeout)
	defer timer.Stop()
	for {
		ch := s.waitCh() // taken before reading the version so no change is missed
		if v := metaInt(s.db, "config_version"); v != req.ConfigVersion {
			cfg, err := s.effectiveConfig(d.id)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			writeJSON(w, proto.PollResp{Cursors: s.cursors(d.id), Config: &cfg})
			s.touch(d.id)
			return
		}
		select {
		case <-ch:
		case <-timer.C:
			writeJSON(w, proto.PollResp{Cursors: s.cursors(d.id)})
			s.touch(d.id)
			return
		case <-r.Context().Done():
			s.touch(d.id)
			return
		}
	}
}

// cursors returns the latest seq of every folder mapped to the device.
func (s *Server) cursors(deviceID int64) map[int64]int64 {
	out := map[int64]int64{}
	ids, err := queryAll(s.db, func(rows *sql.Rows, id *int64) error { return rows.Scan(id) },
		`SELECT folder_id FROM mappings WHERE device_id=?`, deviceID)
	if err != nil {
		slog.Warn("cursors", "err", err)
	}
	for _, id := range ids {
		if db, err := s.folderDB(id); err != nil {
			slog.Warn("cursors", "folder", id, "err", err)
		} else {
			out[id] = metaInt(db, "next_seq") - 1
		}
	}
	return out
}

func (s *Server) noteReport(id int64, req proto.PollReq) {
	s.mu.Lock()
	rep := s.reports[id]
	if rep == nil {
		rep = &report{}
		s.reports[id] = rep
	}
	rep.req, rep.at = req, s.now()
	s.mu.Unlock()
}

// touch persists last_seen, at most once per lastSeenEvery per device.
func (s *Server) touch(id int64) {
	now := s.now()
	s.mu.Lock()
	rep := s.reports[id]
	due := rep != nil && now.Sub(rep.lastWrite) >= lastSeenEvery
	if due {
		rep.lastWrite = now
	}
	s.mu.Unlock()
	if due {
		s.db.Exec(`UPDATE devices SET last_seen=? WHERE id=?`, now.Unix(), id)
	}
}

func (s *Server) commitHandler(w http.ResponseWriter, r *http.Request, d device) {
	var req proto.CommitReq
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Ops) > proto.MaxCommitOps {
		http.Error(w, "too many ops", http.StatusBadRequest)
		return
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM mappings WHERE device_id=? AND folder_id=?`, d.id, req.Folder).Scan(&n); err != nil || n == 0 {
		http.Error(w, "folder not mapped to this device", http.StatusForbidden)
		return
	}
	res, err := s.commit(req.Folder, d.name, req.Ops)
	var br badRequest
	switch {
	case errors.As(err, &br):
		http.Error(w, br.msg, http.StatusBadRequest)
	case err != nil:
		slog.Error("commit", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		writeJSON(w, proto.CommitResp{Results: res})
	}
}

// blocks stores the blocks of an upload packet. Blocks that arrived completely
// stay stored when the connection breaks.
func (s *Server) blocks(w http.ResponseWriter, r *http.Request, _ device) {
	body := http.MaxBytesReader(w, r.Body, proto.MaxBlockBody)
	stored := 0
	for {
		h, data, err := proto.ReadBlock(body)
		if err == io.EOF {
			break
		}
		if err == nil {
			err = s.blobs.Put(blobstore.Hash(h), bytes.NewReader(data))
		}
		if err != nil {
			http.Error(w, "bad block packet: "+err.Error(), http.StatusBadRequest)
			return
		}
		stored++
	}
	writeJSON(w, map[string]int{"stored": stored})
}
