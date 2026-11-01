package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"
)

const sessionCookie = "__Host-kaze-sync"

type failure struct {
	count int
	first time.Time
	until time.Time
}

// clientIP is the lockout identity of a request: the IP, or the /64 for IPv6.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// locked reports whether the request's IP may not authenticate now.
func (s *Server) locked(r *http.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.fails[clientIP(r)]
	return f != nil && s.now().Before(f.until)
}

// authFailed records a failed authentication and locks the IP after too many.
func (s *Server) authFailed(r *http.Request, what string) {
	ip := clientIP(r)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, f := range s.fails { // forget expired entries
		if now.Sub(f.first) > lockWindow && now.After(f.until) {
			delete(s.fails, k)
		}
	}
	f := s.fails[ip]
	if f == nil {
		f = &failure{first: now}
		s.fails[ip] = f
	}
	f.count++
	if f.count >= lockFailures {
		f.until = now.Add(lockWindow)
	}
	slog.Warn("authentication failed", "what", what, "ip", ip, "count", f.count)
}

func (s *Server) authOK(r *http.Request) {
	s.mu.Lock()
	delete(s.fails, clientIP(r))
	s.mu.Unlock()
}

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// checkAdminPassword compares a login attempt with the stored hash.
func (s *Server) checkAdminPassword(pw string) bool {
	want, err := hex.DecodeString(MetaString(s.db, "admin_password_hash"))
	if err != nil || len(want) == 0 {
		return false
	}
	got := sha(normalizeSecret(pw))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

func (s *Server) startSession(w http.ResponseWriter) {
	tok := newToken()
	exp := s.now().Add(sessionLife)
	s.mu.Lock()
	s.sessions[tok] = exp
	for t, e := range s.sessions {
		if s.now().After(e) {
			delete(s.sessions, t)
		}
	}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", Expires: exp, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (s *Server) hasSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	return ok && s.now().Before(exp)
}
