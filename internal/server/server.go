// Package server implements kaze-syncd: the device API, the Web UI and the
// metadata and block stores behind them.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
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
	db    *sql.DB
	blobs *blobstore.Store
	now   func() time.Time
	tmpl  map[string]*template.Template

	// Fingerprint is shown in the enrollment commands in self-signed mode; "" otherwise.
	Fingerprint string

	// PollTimeout is how long a poll hangs without news.
	PollTimeout time.Duration

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
	at        time.Time
	lastWrite time.Time
}

// New opens the server state in the data directory; meta.db must exist and be at the current schema.
func New(data string, now func() time.Time) (*Server, error) {
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
	return newServer(db, blobs, now), nil
}

type schemaError struct{ have int }

func (e *schemaError) Error() string {
	return "database schema version " + strconv.Itoa(e.have) + " differs from this binary's; run install"
}

func newServer(db *sql.DB, blobs *blobstore.Store, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{
		db: db, blobs: blobs, now: now, tmpl: parseTemplates(), PollTimeout: proto.PollTimeoutS * time.Second,
		sessions: map[string]time.Time{}, codes: map[[32]byte]pendingCode{}, fails: map[string]*failure{},
		reports: map[int64]*report{}, notify: make(chan struct{}),
	}
}

func (s *Server) Close() error { return s.db.Close() }

// Handler returns the full HTTP handler: device API under /api/v1/, Web UI elsewhere.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.deviceRoutes(mux)
	s.webRoutes(mux)
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
