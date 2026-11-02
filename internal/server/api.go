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
	"strconv"
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
	mux.HandleFunc("GET /api/v1/changes", s.auth(s.changes))
	mux.HandleFunc("POST /api/v1/blocks", s.auth(s.blocks))
	mux.HandleFunc("POST /api/v1/blocks/get", s.auth(s.blocksGet))
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
	writeJSON(w, proto.EnrollResp{DeviceID: id, Name: pc.name, Token: tok})
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

// poll hangs until the device's configuration or the latest seq of one of its
// folders differs from what it reports, or PollTimeout passes. It also carries
// the device's status report; a device is online while it has a poll hanging.
func (s *Server) poll(w http.ResponseWriter, r *http.Request, d device) {
	var req proto.PollReq
	if !readJSON(w, r, &req) {
		return
	}
	s.pollStart(d.id, req)
	defer s.pollEnd(d.id)
	timer := time.NewTimer(s.PollTimeout)
	defer timer.Stop()
	for {
		ch := s.waitCh() // taken before reading the state so no change is missed
		resp := proto.PollResp{Cursors: s.cursors(d.id)}
		news := metaInt(s.db, "config_version") != req.ConfigVersion
		for id, seq := range resp.Cursors {
			if have, ok := req.Cursors[id]; !ok || have != seq {
				news = true
			}
		}
		if news {
			if metaInt(s.db, "config_version") != req.ConfigVersion {
				cfg, err := s.effectiveConfig(d.id)
				if err != nil {
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				resp.Config = &cfg
			}
			writeJSON(w, resp)
			return
		}
		select {
		case <-ch:
		case <-timer.C:
			writeJSON(w, resp)
			return
		case <-r.Context().Done():
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

func (s *Server) pollStart(id int64, req proto.PollReq) {
	s.mu.Lock()
	rep := s.reports[id]
	if rep == nil {
		rep = &report{}
		s.reports[id] = rep
	}
	rep.req, rep.at = req, s.now()
	rep.pending++
	s.mu.Unlock()
}

// pollEnd records the end of a poll and persists last_seen, at most once per lastSeenEvery per device.
func (s *Server) pollEnd(id int64) {
	now := s.now()
	s.mu.Lock()
	rep := s.reports[id]
	if rep == nil { // revoked meanwhile
		s.mu.Unlock()
		return
	}
	rep.pending--
	rep.at = now
	due := now.Sub(rep.lastWrite) >= lastSeenEvery
	if due {
		rep.lastWrite = now
	}
	s.mu.Unlock()
	if due {
		s.db.Exec(`UPDATE devices SET last_seen=? WHERE id=?`, now.Unix(), id)
	}
}

// mapped reports whether the folder is mapped to the device.
func (s *Server) mapped(deviceID, folder int64) bool {
	var n int
	return s.db.QueryRow(`SELECT count(*) FROM mappings WHERE device_id=? AND folder_id=?`, deviceID, folder).Scan(&n) == nil && n > 0
}

// changes returns the next page of a folder's entries after seq "since" (design 2.4).
func (s *Server) changes(w http.ResponseWriter, r *http.Request, d device) {
	folder, _ := strconv.ParseInt(r.FormValue("folder"), 10, 64)
	since, err := strconv.ParseInt(r.FormValue("since"), 10, 64)
	if err != nil || since < 0 {
		http.Error(w, "bad since", http.StatusBadRequest)
		return
	}
	if !s.mapped(d.id, folder) {
		http.Error(w, "folder not mapped to this device", http.StatusForbidden)
		return
	}
	db, err := s.folderDB(folder)
	var rows *sql.Rows
	if err == nil {
		rows, err = db.Query(`SELECT `+fileCols+` FROM files WHERE seq > ? ORDER BY seq LIMIT ?`, since, proto.MaxChangesPage+1)
	}
	if err != nil {
		slog.Error("changes", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	resp := proto.ChangesResp{Entries: []proto.Entry{}, Cursor: since}
	nblocks := 0
	for rows.Next() {
		var f fileRow
		if err := f.scan(rows); err != nil {
			slog.Error("changes", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if len(resp.Entries) >= proto.MaxChangesPage || (len(resp.Entries) > 0 && nblocks+len(f.blocks)/32 > proto.MaxChangeBlocks) {
			resp.More = true
			break
		}
		nblocks += len(f.blocks) / 32
		resp.Entries = append(resp.Entries, *f.entry())
		resp.Cursor = f.seq
	}
	writeJSON(w, resp)
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
	if !s.mapped(d.id, req.Folder) {
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
	buf := make([]byte, proto.BlockSize)
	for {
		h, data, err := proto.ReadBlock(body, buf)
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

// blocksGet returns the requested blocks as a block packet.
func (s *Server) blocksGet(w http.ResponseWriter, r *http.Request, _ device) {
	var req proto.BlocksGetReq
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Hashes) > proto.MaxGetBlocks {
		http.Error(w, "too many blocks", http.StatusBadRequest)
		return
	}
	for _, h := range req.Hashes {
		if ok, err := s.blobs.Has(blobstore.Hash(h)); err != nil || !ok {
			http.Error(w, "missing block "+h.String(), http.StatusNotFound)
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	buf := make([]byte, proto.BlockSize)
	for _, h := range req.Hashes {
		f, err := s.blobs.Open(blobstore.Hash(h))
		if err == nil {
			var n int
			n, err = io.ReadFull(f, buf)
			f.Close()
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				err = nil
			}
			if err == nil {
				err = proto.WriteBlock(w, h, buf[:n])
			}
		}
		if err != nil {
			panic(http.ErrAbortHandler) // headers are gone: break the connection so the client sees a truncated body
		}
	}
}
