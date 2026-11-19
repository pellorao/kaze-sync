package client

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func noTempFiles(t *testing.T, g *engine) {
	t.Helper()
	for _, p := range treeOf(g.fs) {
		if strings.Contains(p, ".syncpart-") {
			t.Errorf("temporary file left: %s", p)
		}
	}
}

// Many files share one request; files of one directory with the same content do not share a temporary file.
func TestPullFetchesManyFilesWithOneRequest(t *testing.T) {
	t.Parallel()
	_, _, g1, g2, gets := pair(t)
	want := map[string]string{}
	for i := 0; i < 80; i++ {
		p := "d" + strconv.Itoa(i%4) + "/f" + strconv.Itoa(i) + ".bin"
		data := pseudoRandom(500+i*13, byte(i))
		if i%10 == 0 {
			data = []byte("same content in a few files") // three files per directory hold it
		}
		g1.put(p, data)
		want[p] = string(data)
	}
	g1.put("empty.txt", nil)
	want["empty.txt"] = ""
	g1.scan()
	g2.sync()
	if n := gets.requests.Load(); n != 1 {
		t.Errorf("%d /blocks/get requests, want 1", n)
	}
	for p, w := range want {
		if got, ok := g2.read(p); !ok || got != w {
			t.Errorf("%s: ok=%v, content differs=%v", p, ok, got != w)
		}
	}
	noTempFiles(t, g2)
}

// cutBody ends the response of the first /blocks/get with an error after n bytes.
type cutBody struct {
	io.ReadCloser
	left int
}

func (c *cutBody) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n, err := c.ReadCloser.Read(p[:min(len(p), c.left)])
	c.left -= n
	return n, err
}

type cutTransport struct {
	base  http.RoundTripper
	n     int
	armed atomic.Bool
}

func (c *cutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(r)
	if err == nil && strings.HasSuffix(r.URL.Path, "/blocks/get") && c.armed.CompareAndSwap(true, false) {
		resp.Body = &cutBody{ReadCloser: resp.Body, left: c.n}
	}
	return resp, err
}

// A response that breaks in the middle ends the batch: the files before the break are in place, the rest
// comes with the next pull.
func TestPullBatchContinuesAfterABrokenResponse(t *testing.T) {
	t.Parallel()
	_, _, g1, g2, _ := pair(t)
	want := map[string]string{}
	for i := 0; i < 30; i++ {
		p := "f" + strconv.Itoa(100+i) + ".bin"
		data := pseudoRandom(2000, byte(i))
		g1.put(p, data)
		want[p] = string(data)
	}
	g1.scan()
	g2.scan() // sets up the root
	cut := &cutTransport{base: g2.f.API.HC.Transport, n: 36*10 + 2000*10 + 1000}
	g2.f.API.HC.Transport = cut
	cut.armed.Store(true)
	if _, err := g2.f.Pull(context.Background()); err == nil {
		t.Fatal("the broken response did not fail the pull")
	}
	got := 0
	for p, w := range want {
		if c, ok := g2.read(p); ok {
			got++
			if c != w {
				t.Errorf("%s differs after the break", p)
			}
		}
	}
	if got < 8 || got >= len(want) {
		t.Errorf("%d of %d files in place after the break, want the ones before it", got, len(want))
	}
	g2.sync()
	for p, w := range want {
		if c, ok := g2.read(p); !ok || c != w {
			t.Errorf("%s missing or wrong after the next pull", p)
		}
	}
	noTempFiles(t, g2)
}

// A file larger than a batch is fetched alone, in windows, between the batches of the small ones.
//
// Not parallel: it lowers the package variable getBatchBytes, and a parallel test that pulls files while it is
// lowered gets its batch cut in two (about 3 runs in 100 of TestPullFetchesManyFilesWithOneRequest next to it).
// Sequential tests run before the parallel ones resume, so the value is back before any of them reads it.
func TestPullFetchesLargeFileAlone(t *testing.T) {
	old := getBatchBytes
	getBatchBytes = 40 << 10
	t.Cleanup(func() { getBatchBytes = old })
	_, _, g1, g2, gets := pair(t)
	want := map[string]string{}
	for i := 0; i < 20; i++ {
		p := "f" + strconv.Itoa(100+i) + ".bin"
		size := 3000
		if i == 7 {
			size = 3*miB + 5
		}
		data := pseudoRandom(size, byte(i))
		g1.put(p, data)
		want[p] = string(data)
	}
	g1.scan()
	g2.sync()
	for p, w := range want {
		if c, ok := g2.read(p); !ok || c != w {
			t.Errorf("%s missing or wrong", p)
		}
	}
	if n := gets.requests.Load(); n < 3 {
		t.Errorf("%d requests: the large file and the batches around it should need more", n)
	}
	noTempFiles(t, g2)
}
