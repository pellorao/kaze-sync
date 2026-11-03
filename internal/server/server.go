// Package server implements kaze-syncd: the device API, the Web UI and the
// metadata and block stores behind them.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"kaze-sync/internal/blobstore"
	"kaze-sync/internal/proto"
)

// Tunables.
const (
	sessionLife   = 7 * 24 * time.Hour
	codeLife      = 15 * time.Minute
	lockFailures  = 10
	lockWindow    = time.Hour
	lastSeenEvery = time.Minute
)

type Server struct {
	db     *sql.DB // global database (meta.db)
	varDir string
	blobs  *blobstore.Store
	now    func() time.Time
	tmpl   map[string]*template.Template

	// Fingerprint is shown in the enrollment commands in self-signed mode; "" otherwise.
	Fingerprint string

	// PollTimeout is how long a poll hangs without news.
	PollTimeout time.Duration

	fmu  sync.Mutex        // guards fdbs
	fdbs map[int64]*sql.DB // open folder databases

	gcMu               sync.Mutex // guards the GC state: at most one runs
	gcRunning, gcAgain bool
	gcWG               sync.WaitGroup

	mu       sync.Mutex // guards everything below
	sessions map[string]time.Time
	codes    map[[32]byte]pendingCode
	fails    map[string]*failure
	reports  map[int64]*report
	notify   chan struct{} // closed (and replaced) when the config changes
}

type pendingCode struct {
	name    string
	expires time.Time
}

// report is the last state a device sent with a poll.
type report struct {
	req       proto.PollReq
	at        time.Time // when the last poll ended (or the report arrived)
	lastWrite time.Time
	pending   int // polls hanging now
	tooOld    int // the version of the client when its last request was refused as too old; 0 once it polls again
}

// onlineWindow is how long after its last poll a device still counts as online.
const onlineWindow = 90 * time.Second

func (r *report) online(now time.Time) bool { return r.pending > 0 || now.Sub(r.at) < onlineWindow }

// New opens the server state in <data>/var; meta.db must exist and be at the current schema.
func New(data string, now func() time.Time) (*Server, error) {
	data = filepath.Join(data, VarDir)
	db, err := OpenDB(filepath.Join(data, DBFile), false)
	if err != nil {
		return nil, err
	}
	if v, err := schemaVersion(db); err != nil || v != SchemaVersion {
		db.Close()
		if err == nil {
			err = &schemaError{v}
		}
		return nil, err
	}
	blobs, err := blobstore.Open(data)
	if err != nil {
		db.Close()
		return nil, err
	}
	return newServer(db, data, blobs, now), nil
}

type schemaError struct{ have int }

func (e *schemaError) Error() string {
	return "database schema version " + strconv.Itoa(e.have) + " differs from this binary's; run install"
}

func newServer(db *sql.DB, varDir string, blobs *blobstore.Store, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{
		db: db, varDir: varDir, fdbs: map[int64]*sql.DB{}, blobs: blobs, now: now, tmpl: parseTemplates(), PollTimeout: proto.PollTimeoutS * time.Second,
		sessions: map[string]time.Time{}, codes: map[[32]byte]pendingCode{}, fails: map[string]*failure{},
		reports: map[int64]*report{}, notify: make(chan struct{}),
	}
}

func (s *Server) Close() error {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	for _, db := range s.fdbs {
		db.Close()
	}
	return s.db.Close()
}

// folderDB returns the open database of a folder, opening it on first use. A
// missing file is an error: only createFolder creates folder databases.
func (s *Server) folderDB(id int64) (*sql.DB, error) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	if db := s.fdbs[id]; db != nil {
		return db, nil
	}
	db, err := OpenDB(FolderDBPath(s.varDir, id), false)
	if err != nil {
		return nil, err
	}
	if v, err := schemaVersion(db); err != nil || v != FolderSchemaVersion {
		db.Close()
		if err == nil {
			err = &schemaError{v}
		}
		return nil, err
	}
	s.fdbs[id] = db
	return db, nil
}

// createFolder adds a folder and its database; neither exists if either step fails.
func (s *Server) createFolder(name string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO folders(name, rules) VALUES(?, '')`, name)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	path := FolderDBPath(s.varDir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} { // the id is new (never used again), so a file is a leftover of a crashed create or delete
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	db, err := OpenDB(path, true)
	if err == nil {
		err = MigrateFolder(db)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		if db != nil {
			db.Close()
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			os.Remove(path + suffix)
		}
		return 0, err
	}
	s.fmu.Lock()
	s.fdbs[id] = db
	s.fmu.Unlock()
	return id, nil
}

// Handler returns the full HTTP handler: device API under /api/v1/, Web UI elsewhere.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.deviceRoutes(mux)
	s.webRoutes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	var h http.Handler = http.NewCrossOriginProtection().Handler(mux)
	return securityHeaders(h)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// broadcast wakes all hanging polls.
func (s *Server) broadcast() {
	s.mu.Lock()
	close(s.notify)
	s.notify = make(chan struct{})
	s.mu.Unlock()
}

func (s *Server) waitCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notify
}

func sha(b string) [32]byte { return sha256.Sum256([]byte(b)) }

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// randomBase32 returns n random base32 characters.
func randomBase32(n int) string {
	b := make([]byte, (n*5+7)/8)
	rand.Read(b)
	return b32.EncodeToString(b)[:n]
}

// group splits s into groups of n characters separated by "-".
func group(s string, n int) string {
	var parts []string
	for len(s) > n {
		parts, s = append(parts, s[:n]), s[n:]
	}
	return strings.Join(append(parts, s), "-")
}

func normalizeSecret(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "err", err)
	}
}
