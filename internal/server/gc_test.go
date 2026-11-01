package server

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"kaze-sync/internal/blobstore"
	"kaze-sync/internal/proto"
)

func TestGCPasses(t *testing.T) {
	const mb = 1 << 20
	for _, tc := range []struct {
		refs, budget int64
		want         int
	}{
		{0, 64 * mb, 1}, {1, 64 * mb, 1}, {2 * mb, 64 * mb, 1}, {2*mb + 1, 64 * mb, 2},
		{4 * mb, 64 * mb, 2}, {4_000_000, 64 * mb, 2}, {10 * mb, 64 * mb, 5}, {1 << 40, 64 * mb, 256},
	} {
		if got := gcPasses(tc.refs, tc.budget); got != tc.want {
			t.Errorf("gcPasses(%d, %d) = %d, want %d", tc.refs, tc.budget, got, tc.want)
		}
	}
}

func hashesOf(rng *rand.Rand, n int) []hash {
	out := make([]hash, n)
	for i := range out {
		for j := 0; j < hashLen; j += 8 {
			v := rng.Uint64()
			for k := 0; k < 8; k++ {
				out[i][j+k] = byte(v >> (8 * k))
			}
		}
	}
	return out
}

func memSource(lists ...[]hash) refSource {
	var n int64
	for _, l := range lists {
		n += int64(len(l))
	}
	return refSource{
		count: func() (int64, error) { return n, nil },
		each: func(visit func([]byte) error) error {
			for _, l := range lists {
				var b []byte
				for _, h := range l {
					b = append(b, h[:]...)
				}
				if err := visit(b); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func TestCollectRefsRangeSortDedup(t *testing.T) {
	hs := hashesOf(rand.New(rand.NewPCG(1, 2)), 2000)
	src := memSource(hs, hs[:500]) // duplicates across lists
	all, err := collectRefs([]refSource{src}, 0, 256, nil)
	if err != nil || len(all) != 2000 || !slices.IsSortedFunc(all, cmpHash) {
		t.Fatalf("all: %d refs, sorted=%v, %v", len(all), slices.IsSortedFunc(all, cmpHash), err)
	}
	part, _ := collectRefs([]refSource{src}, 64, 128, make([]hash, 0, 4))
	for _, h := range part {
		if h[0] < 64 || h[0] >= 128 {
			t.Fatalf("hash %x outside the range", h[0])
		}
	}
	want := 0
	for _, h := range all {
		if h[0] >= 64 && h[0] < 128 {
			want++
		}
	}
	if len(part) != want {
		t.Fatalf("range holds %d, want %d", len(part), want)
	}
}

// Whatever the budget, the same blobs are removed: only unreferenced old ones.
func TestSweepPassesRemovesOnlyUnreferenced(t *testing.T) {
	for _, budget := range []int64{GCMemoryBudget, 32 * 40, 32} { // one pass, a few passes, as many as possible
		e := newEnv(t)
		a, b := e.folder("a"), e.folder("b")
		var kept []proto.Hash
		for i := 0; i < 60; i++ {
			f := a
			if i%2 == 1 {
				f = b
			}
			op := e.op(fmt.Sprintf("f%d", i), 0, fmt.Sprintf("content %d", i))
			e.commit(f, op)
			kept = append(kept, op.Blocks...)
			if i%3 == 0 { // an overwritten version still references its blocks
				e.commit(f, e.op(fmt.Sprintf("f%d", i), 1, fmt.Sprintf("newer %d", i)))
			}
		}
		old := time.Now().Add(-48 * time.Hour)
		var doomed, fresh []proto.Hash
		for i := 0; i < 300; i++ {
			h := e.block([]byte(fmt.Sprintf("garbage %d", i)))
			if i%10 == 0 {
				fresh = append(fresh, h)
				continue
			}
			doomed = append(doomed, h)
			x := blobstore.Hash(h).String()
			os.Chtimes(filepath.Join(e.s.varDir, "blobs", x[:2], x[2:4], x), old, old)
		}
		// Referenced blobs are old too: age alone must not remove them.
		for _, h := range kept {
			x := blobstore.Hash(h).String()
			os.Chtimes(filepath.Join(e.s.varDir, "blobs", x[:2], x[2:4], x), old, old)
		}
		var srcs []refSource
		for _, f := range []int64{a, b} {
			db, _ := e.s.folderDB(f)
			srcs = append(srcs, folderRefs(db))
		}
		removed := 0
		passes, err := sweepPasses(srcs, budget, func(lo, hi int, ref func(blobstore.Hash) bool) error {
			n, err := e.s.blobs.SweepBlobs(lo, hi, ref, time.Hour)
			removed += n
			return err
		})
		if err != nil || removed != len(doomed) {
			t.Fatalf("budget %d: removed %d of %d, %v", budget, removed, len(doomed), err)
		}
		if budget == GCMemoryBudget && passes != 1 || budget == 32 && passes < 50 || budget == 32*40 && (passes < 2 || passes > 10) {
			t.Fatalf("budget %d: %d passes", budget, passes)
		}
		for h, want := range map[bool][]proto.Hash{true: append(slices.Clone(kept), fresh...), false: doomed} {
			for _, x := range h2(want) {
				if ok, _ := e.s.blobs.Has(x); ok != h {
					t.Fatalf("budget %d: blob %s present=%v, want %v", budget, x, ok, h)
				}
			}
		}
	}
}

func h2(hs []proto.Hash) []blobstore.Hash {
	out := make([]blobstore.Hash, len(hs))
	for i, h := range hs {
		out[i] = blobstore.Hash(h)
	}
	return out
}

// synthetic is a source of n pseudo-random hashes that stores none of them, so a
// benchmark measures only the collection's own memory.
func synthetic(n int) refSource {
	return refSource{
		count: func() (int64, error) { return int64(n), nil },
		each: func(visit func([]byte) error) error {
			rng := rand.New(rand.NewPCG(7, 8))
			buf := make([]byte, 0, 4096*hashLen) // one file of 4096 blocks (1 GB)
			for i := 0; i < n; i++ {
				for j := 0; j < 4; j++ {
					v := rng.Uint64()
					buf = append(buf, byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
				}
				if len(buf) == cap(buf) || i == n-1 {
					if err := visit(buf); err != nil {
						return err
					}
					buf = buf[:0]
				}
			}
			return nil
		},
	}
}

// BenchmarkGCCollect compares passes for 1M and 4M block references. peakMB is the
// highest live heap seen while collecting, above the heap before the run.
func BenchmarkGCCollect(b *testing.B) {
	for _, n := range []int{1_000_000, 4_000_000} {
		for _, k := range []int{1, 2, 4} {
			b.Run(fmt.Sprintf("blocks=%dM/k=%d", n/1_000_000, k), func(b *testing.B) {
				src := []refSource{synthetic(n)}
				budget := (int64(n)*hashLen + int64(k) - 1) / int64(k) // exactly k passes
				var peak uint64
				for i := 0; i < b.N; i++ {
					runtime.GC()
					var m runtime.MemStats
					runtime.ReadMemStats(&m)
					base := m.HeapAlloc
					stop, done := make(chan struct{}), sync.WaitGroup{}
					done.Add(1)
					go func() {
						defer done.Done()
						var ms runtime.MemStats
						for {
							runtime.ReadMemStats(&ms)
							peak = max(peak, ms.HeapAlloc-min(ms.HeapAlloc, base))
							select {
							case <-stop:
								return
							case <-time.After(2 * time.Millisecond):
							}
						}
					}()
					found := 0
					passes, err := sweepPasses(src, budget, func(lo, hi int, ref func(blobstore.Hash) bool) error {
						found++
						return nil
					})
					close(stop)
					done.Wait()
					if err != nil || passes != k || found != k {
						b.Fatalf("passes %d, %v", passes, err)
					}
				}
				b.ReportMetric(float64(peak)/(1<<20), "peakMB")
			})
		}
	}
}
