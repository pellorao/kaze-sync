package server

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A login body larger than loginBodyMax is cut off before parsing, so it cannot be used to tie up memory.
func TestLoginBodyLimited(t *testing.T) {
	e := newEnv(t)
	body := "password=" + strings.Repeat("a", 1<<20)
	req := httptest.NewRequest("POST", "/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cr := &countReader{r: req.Body}
	req.Body = io.NopCloser(cr)
	w := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
	if cr.n > loginBodyMax+4096 {
		t.Fatalf("read %d bytes of the login body, limit %d", cr.n, loginBodyMax)
	}
}

type countReader struct {
	r io.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// A client that declares a login body and never sends it is dropped at the read deadline.
// Exceeds 100 ms (about 0.5 s) on purpose: only a real connection proves the deadline, and net/http waits 500 ms
// before closing a connection whose request body was left unread.
func TestLoginSlowBodyDropped(t *testing.T) {
	e := newEnv(t)
	e.s.loginBodyTimeout = 20 * time.Millisecond
	conn, err := tls.Dial("tcp", e.ts.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	io.WriteString(conn, "POST /login HTTP/1.1\r\nHost: x\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\npassword=")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	start := time.Now()
	// The server answers once the deadline passes; without the deadline it would wait for the missing body.
	if _, err := conn.Read(make([]byte, 1)); err != nil {
		t.Fatalf("no answer: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("answered only after %v", d)
	}
}
