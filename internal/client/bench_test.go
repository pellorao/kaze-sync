package client

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// TestBench times the first upload of a synthetic tree and the first download of it to a second client, with the
// real server, TLS and file system and a per-endpoint request summary. It runs only with KAZE_BENCH=1.
// KAZE_BENCH_SHAPE=mixed (sizes drawn from the quantiles of a real source tree, default) or bulk (equal files),
// KAZE_BENCH_N (files), KAZE_BENCH_SRC (where the source tree lives; TMPDIR otherwise),
// KAZE_BENCH_PACKET_MB (upload packet size) and KAZE_BENCH_BATCH_MB (download batch size) override the defaults,
// KAZE_BENCH_PROF=<file> (CPU profile), KAZE_BENCH_BIG (MB of the one large file of mixed, default 64).
func TestBench(t *testing.T) {
	if os.Getenv("KAZE_BENCH") == "" {
		t.Skip("set KAZE_BENCH=1")
	}
	n := envInt("KAZE_BENCH_N", 4000)
	if v := envInt("KAZE_BENCH_PACKET_MB", 0); v > 0 {
		old := packetTarget
		packetTarget = v << 20
		t.Cleanup(func() { packetTarget = old })
	}
	if v := envInt("KAZE_BENCH_BATCH_MB", 0); v > 0 {
		old := getBatchBytes
		getBatchBytes = int64(v) << 20
		t.Cleanup(func() { getBatchBytes = old })
	}
	shape := os.Getenv("KAZE_BENCH_SHAPE")
	srcRoot := os.Getenv("KAZE_BENCH_SRC")
	if srcRoot == "" {
		srcRoot = t.TempDir()
	} else {
		srcRoot, _ = os.MkdirTemp(srcRoot, "kbench-")
		t.Cleanup(func() { os.RemoveAll(srcRoot) })
	}
	root := filepath.Join(srcRoot, "a")
	total := genTree(t, root, shape, n, int64(envInt("KAZE_BENCH_BIG", 64))<<20)
	t.Logf("dataset: %d files, %.1f MiB, shape %q", n, float64(total)/(1<<20), shape)

	e := newCEnv(t)
	fid := e.newFolder("Bench")
	mk := func(rootDir string) (*Folder, *httpStats) {
		dev, api := e.newDevice()
		e.mapFolder(dev, fid, rootDir, true)
		st := &httpStats{base: api.HC.Transport, m: map[string]*epStat{}}
		api.HC.Transport = st
		db, err := openLocal(filepath.Join(t.TempDir(), "folder.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		f := NewFolder(proto.FolderConfig{ID: fid, Adopt: true}, cfs.OS(rootDir), db, api, false)
		f.Stable = 0
		return f, st
	}
	a, sa := mk(root)
	broot := filepath.Join(t.TempDir(), "b")
	b, sb := mk(broot)
	ctx := context.Background()
	if p := os.Getenv("KAZE_BENCH_PROF"); p != "" {
		pf, _ := os.Create(p)
		pprof.StartCPUProfile(pf)
		defer func() { pprof.StopCPUProfile(); pf.Close() }()
	}

	phase := func(name string, st *httpStats, run func() error) {
		u0, s0 := cpuTime()
		t0 := time.Now()
		if err := run(); err != nil {
			t.Fatal(err)
		}
		wall := time.Since(t0)
		u1, s1 := cpuTime()
		t.Logf("%s: wall %.2fs, %.0f files/s, %.1f MiB/s; process cpu user %.2fs sys %.2fs (client and server share the process)\nrequests:\n%s",
			name, wall.Seconds(), float64(n)/wall.Seconds(), float64(total)/(1<<20)/wall.Seconds(), (u1 - u0).Seconds(), (s1 - s0).Seconds(), st.report())
		st.reset()
	}
	phase("UPLOAD (A scan+commit+upload)", sa, func() error {
		f := a
		rep, err := f.Scan(ctx)
		if err == nil && (rep.Committed != n || rep.PendingUp != 0 || rep.State != "idle") {
			err = fmt.Errorf("upload incomplete: %+v", rep)
		}
		return err
	})
	phase("DOWNLOAD (B pull)", sb, func() error {
		if _, err := b.Scan(ctx); err != nil { // sets up the root
			return err
		}
		sb.reset()
		rep, err := b.Pull(ctx)
		if err == nil && (rep.PendingDown != 0 || rep.State != "idle") {
			err = fmt.Errorf("download incomplete: %+v", rep)
		}
		return err
	})
	// verify the copy
	var bad int
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(p) == markerName {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		x, _ := os.ReadFile(p)
		y, err := os.ReadFile(filepath.Join(broot, rel))
		if err != nil || string(x) != string(y) {
			bad++
		}
		return nil
	})
	if bad > 0 {
		t.Fatalf("%d files differ", bad)
	}
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

// quantiles of file sizes in a real source tree (bytes)
var sizeQ = [][2]float64{{0, 1}, {0.1, 245}, {0.2, 697}, {0.3, 1317}, {0.4, 2363}, {0.5, 3489}, {0.6, 5123}, {0.7, 8320},
	{0.8, 12047}, {0.9, 27145}, {0.95, 60967}, {0.99, 202625}, {0.999, 5.6e6}, {1, 8e6}}

func sampleSize(u float64) int {
	for i := 1; i < len(sizeQ); i++ {
		if u <= sizeQ[i][0] {
			lo, hi := sizeQ[i-1], sizeQ[i]
			f := (u - lo[0]) / (hi[0] - lo[0])
			return int(math.Exp(math.Log(lo[1]) + f*(math.Log(hi[1])-math.Log(lo[1]))))
		}
	}
	return 1
}

func genTree(t *testing.T, root, shape string, n int, big int64) (total int64) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < n; i++ {
		var size int
		if shape == "bulk" {
			size = 200 << 10
		} else {
			size = sampleSize(rng.Float64())
		}
		if shape != "bulk" && i == 0 {
			size = int(big)
		}
		dir := filepath.Join(root, fmt.Sprintf("d%02d", i%40), fmt.Sprintf("s%d", i%7))
		os.MkdirAll(dir, 0o755)
		data := make([]byte, size)
		c := rand.NewChaCha8([32]byte{byte(i), byte(i >> 8), byte(i >> 16), 7})
		c.Read(data)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.dat", i)), data, 0o644); err != nil {
			t.Fatal(err)
		}
		total += int64(size)
	}
	return total
}

type epStat struct {
	n        int
	wall     time.Duration // until the response headers
	reqBytes int64
	resBytes int64
}

type httpStats struct {
	base http.RoundTripper
	mu   sync.Mutex
	m    map[string]*epStat
}

func (h *httpStats) RoundTrip(r *http.Request) (*http.Response, error) {
	t0 := time.Now()
	resp, err := h.base.RoundTrip(r)
	ep := filepath.Base(r.URL.Path)
	if ep == "get" {
		ep = "blocks/get"
	}
	h.mu.Lock()
	s := h.m[ep]
	if s == nil {
		s = &epStat{}
		h.m[ep] = s
	}
	s.n++
	s.wall += time.Since(t0)
	if r.ContentLength > 0 {
		s.reqBytes += r.ContentLength
	}
	h.mu.Unlock()
	if err == nil {
		resp.Body = &countBody{rc: resp.Body, add: func(n int) { h.mu.Lock(); s.resBytes += int64(n); h.mu.Unlock() }}
	}
	return resp, err
}

type countBody struct {
	rc  io.ReadCloser
	add func(int)
}

func (c *countBody) Read(p []byte) (int, error) { n, err := c.rc.Read(p); c.add(n); return n, err }
func (c *countBody) Close() error               { return c.rc.Close() }

func (h *httpStats) reset() { h.mu.Lock(); h.m = map[string]*epStat{}; h.mu.Unlock() }

func (h *httpStats) report() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var names []string
	for k := range h.m {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		s := h.m[k]
		fmt.Fprintf(&b, "  %-12s n=%-6d to-headers total=%8.1fms  req=%.2f MiB  resp=%.2f MiB\n", k, s.n, float64(s.wall)/1e6, float64(s.reqBytes)/(1<<20), float64(s.resBytes)/(1<<20))
	}
	return b.String()
}
