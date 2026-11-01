package fs

import (
	"io"
	"net/http"
	"sync"
)

// FaultTransport wraps an http.RoundTripper and injects one-shot failures. Test use only.
type FaultTransport struct {
	base   http.RoundTripper
	mu     sync.Mutex
	failIn int // requests left before the failing one, -1 if none
	failE  error
	cutIn  int // same for the request whose body is cut
	cutAt  int64
	cutE   error
}

func NewFaultTransport(base http.RoundTripper) *FaultTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &FaultTransport{base: base, failIn: -1, cutIn: -1}
}

// FailAfter lets the next n requests through and fails the one after them with err, without sending it.
func (t *FaultTransport) FailAfter(n int, err error) {
	t.mu.Lock()
	t.failIn, t.failE = n, err
	t.mu.Unlock()
}

// CutBodyAfter lets the next n requests through; on the one after them the
// body stops after size bytes with err, so the server sees a truncated upload.
func (t *FaultTransport) CutBodyAfter(n int, size int64, err error) {
	t.mu.Lock()
	t.cutIn, t.cutAt, t.cutE = n, size, err
	t.mu.Unlock()
}

// due counts one request against a countdown and reports whether it fires now.
func due(n *int) bool {
	switch {
	case *n < 0:
		return false
	case *n == 0:
		*n = -1
		return true
	}
	*n--
	return false
}

func (t *FaultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	fail, cut := due(&t.failIn), due(&t.cutIn)
	failE, cutAt, cutE := t.failE, t.cutAt, t.cutE
	t.mu.Unlock()
	switch {
	case fail:
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, failE
	case cut && req.Body != nil:
		req = req.Clone(req.Context())
		req.Body = &cutBody{ReadCloser: req.Body, left: cutAt, err: cutE}
	}
	return t.base.RoundTrip(req)
}

type cutBody struct {
	io.ReadCloser
	left int64
	err  error
}

func (b *cutBody) Read(p []byte) (int, error) {
	if b.left == 0 {
		return 0, b.err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}
