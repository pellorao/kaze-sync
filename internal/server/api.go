package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// versionOK refuses a client older than proto.MinAgentVersion with 426 and a message that says what to do (design 9.3).
// It reports the client's version; a request without the header comes from a client that predates it: version 1.
func versionOK(w http.ResponseWriter, r *http.Request) (int, bool) {
	v, err := strconv.Atoi(r.Header.Get(proto.VersionHeader))
	if err != nil {
		v = 1
	}
	if v >= proto.MinAgentVersion {
		return v, true
	}
	http.Error(w, fmt.Sprintf("client version %d is too old, the server needs %d or newer: upgrade kaze-syncc", v, proto.MinAgentVersion), http.StatusUpgradeRequired)
	return v, false
}

func (s *Server) auth(h func(http.ResponseWriter, *http.Request, device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		if !ok { // not counted for the lockout: a 256-bit token cannot be guessed, and a revoked device keeps asking (design 4.4)
			slog.Warn("device token rejected", "ip", clientIP(r))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if v, ok := versionOK(w, r); !ok {
			s.noteTooOld(d.id, v)
			return
		}
		h(w, r, d)
	}
}

// noteTooOld remembers a refused client for the overview page.
func (s *Server) noteTooOld(id int64, version int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := s.reports[id]
	if rep == nil {
		rep = &report{}
		s.reports[id] = rep
	}
	rep.tooOld, rep.at = version, s.now()
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	if s.locked(r) {
		http.Error(w, "too many failures", http.StatusTooManyRequests)
		return
	}
	if _, ok := versionOK(w, r); !ok {
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
	epoch := MetaString(s.db, "epoch")
	for {
		ch := s.waitCh() // taken before reading the state so no change is missed
		resp := proto.PollResp{Cursors: s.cursors(d.id), Epoch: epoch}
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
	rep.req, rep.at, rep.tooOld = req, s.now(), 0
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
	bw := s.blobs.NewWriter()
	var rerr error
	for {
		h, data, err := proto.ReadBlock(body, buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			rerr = err
			break
		}
		bw.Put(h, data)
		stored++
	}
	werr := bw.Wait() // the blocks that arrived completely are stored even when the rest is bad
	switch {
	case rerr != nil:
		http.Error(w, "bad block packet: "+rerr.Error(), http.StatusBadRequest)
	case errors.Is(werr, blobstore.ErrHashMismatch):
		http.Error(w, "bad block packet: "+werr.Error(), http.StatusBadRequest)
	case werr != nil:
		slog.Error("blocks", "err", werr)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		writeJSON(w, map[string]int{"stored": stored})
	}
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
	var total int64
	for _, h := range req.Hashes {
		n, ok, err := s.blobs.Size(h)
		if err != nil || !ok {
			http.Error(w, "missing block "+h.String(), http.StatusNotFound)
			return
		}
		if total += n + 36; total > proto.MaxBlockBody {
			http.Error(w, "too much data in one request", http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	buf := make([]byte, proto.BlockSize)
	for _, h := range req.Hashes {
		f, err := s.blobs.Open(h)
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
