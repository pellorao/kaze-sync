package client

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// Design 2.4: a file with more blocks than one commit may carry is committed through a staged block list, sent in
// segments. The limits are lowered so that a 10-block file stands for one past the 4 MiB request limit.
func TestBigFileCommitsThroughStage(t *testing.T) {
	maxCommitBlocks, stageSegment = 3, 4
	t.Cleanup(func() { maxCommitBlocks, stageSegment = 20000, 20000 })
	e, f, g1, g2, _ := pair(t)
	var stages int
	base := g1.f.API.HC.Transport
	g1.f.API.HC.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/stage") {
			stages++
		}
		return base.RoundTrip(r)
	})
	data := pseudoRandom(10*miB+1000, 5) // 11 blocks: three segments of 4
	g1.put("big.bin", data)
	g1.put("small.txt", []byte("small"))
	if rep := g1.scan(); rep.Committed != 2 {
		t.Fatalf("committed %d", rep.Committed)
	}
	if stages != 3 {
		t.Fatalf("%d stage requests, want 3", stages)
	}
	if got, ok := e.serverFile(f, "big.bin"); !ok || !bytes.Equal(got, data) {
		t.Fatal("server content differs")
	}
	g2.sync()
	if got, _ := g2.read("big.bin"); got != string(data) {
		t.Fatal("download differs")
	}
	if n := e.count(f, `SELECT count(*) FROM files WHERE hash IS NOT NULL`); n != 2 {
		t.Fatalf("%d files", n)
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
