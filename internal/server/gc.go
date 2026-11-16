package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"kaze-sync/internal/blobstore"
	"kaze-sync/internal/proto"
	"kaze-sync/internal/resources"
)

// GC schedule (design 7).
const (
	gcEvery = 24 * time.Hour
	gcGrace = time.Hour // a block or tmp/ file must be older than this to be removed: uploads wait for their commit
)

// GCMemoryBudget bounds the memory of the reference array of one GC pass (design 7).
var GCMemoryBudget = gcMemoryBudget("/proc/meminfo", gcMemoryLimitPaths("/proc/self/cgroup", "/sys/fs/cgroup")...)

// gcMemoryLimitPaths includes the current cgroup and its ancestors: a service
// can inherit a finite limit even when its own memory.max says "max".
func gcMemoryLimitPaths(membership, root string) []string {
	paths := []string{filepath.Join(root, "memory.max"), filepath.Join(root, "memory", "memory.limit_in_bytes")}
	data, err := os.ReadFile(membership)
	if err != nil {
		return paths
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 || !strings.HasPrefix(fields[2], "/") {
			continue
		}
		base, name := root, "memory.max"
		if fields[0] != "0" || fields[1] != "" {
			if !slices.Contains(strings.Split(fields[1], ","), "memory") {
				continue
			}
			base, name = filepath.Join(root, "memory"), "memory.limit_in_bytes"
		}
		for group := filepath.Clean(fields[2]); group != "/"; group = filepath.Dir(group) {
			paths = append(paths, filepath.Join(base, strings.TrimPrefix(group, "/"), name))
		}
	}
	return paths
}

// gcMemoryBudget is computed once at startup. Missing or unlimited cgroups do
// not constrain MemTotal; unreadable physical memory keeps the 64 MiB fallback.
// Paths are arguments so tests never depend on the host's memory or cgroups.
func gcMemoryBudget(meminfo string, limits ...string) int64 {
	const floor = 64 << 20
	total := readMemTotal(meminfo)
	if total == 0 {
		return floor
	}
	for _, path := range limits {
		if limit := readMemoryLimit(path); limit >= 0 {
			total = min(total, limit)
		}
	}
	// Divide before multiplying to avoid overflow, preserving floor(total*3/8).
	return max(floor, total/8*3+total%8*3/8)
}

func readMemTotal(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err == nil && n > 0 && n <= (1<<63-1)/1024 {
				return n * 1024
			}
			return 0
		}
	}
	return 0
}

func readMemoryLimit(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	// v2's "max" is nonnumeric. v1 uses a value close to MaxInt64 for
	// unlimited memory; values of at least 1 EiB cannot be a real limit here.
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n < 0 || n >= 1<<60 {
		return -1
	}
	return n
}

const hashLen = proto.HashLen

// refSource lists the blocks referenced by one folder: count is an upper bound
// of the number of references, each calls visit with every blocks value (a
// concatenation of 32-byte hashes) of its files and versions.
type refSource struct {
	count func() (int64, error)
	each  func(visit func(blocks []byte) error) error
}

// folderRefs is the refSource of a folder database.
func folderRefs(db *sql.DB) refSource {
	return refSource{
		count: func() (n int64, err error) {
			err = db.QueryRow(`SELECT (SELECT coalesce(sum(length(blocks)), 0) FROM files) +
				(SELECT coalesce(sum(length(blocks)), 0) FROM versions)`).Scan(&n)
			return n / hashLen, err
		},
		each: func(visit func([]byte) error) error {
			rows, err := db.Query(`SELECT blocks FROM files WHERE blocks IS NOT NULL UNION ALL SELECT blocks FROM versions WHERE blocks IS NOT NULL`)
			if err != nil {
				return err
			}
			defer rows.Close()
			var b []byte
			for rows.Next() {
				if err := rows.Scan(&b); err != nil {
					return err
				}
				if err := visit(b); err != nil {
					return err
				}
			}
			return rows.Err()
		},
	}
}

// gcPasses is the number of passes that keep the reference array within budget
// bytes: the hash space is split by first byte, so at most 256.
func gcPasses(refs, budget int64) int {
	k := (refs*hashLen + budget - 1) / budget
	return int(max(1, min(256, k)))
}

func cmpHash(a, b proto.Hash) int {
	if x, y := binary.BigEndian.Uint64(a[:8]), binary.BigEndian.Uint64(b[:8]); x != y {
		if x < y {
			return -1
		}
		return 1
	}
	return bytes.Compare(a[8:], b[8:])
}

// collectRefs returns the sorted, distinct referenced hashes whose first byte is
// in [lo, hi), stored in refs[:0] (grown if it is too small, never truncated:
// dropping a reference would let GC delete a live block).
func collectRefs(sources []refSource, lo, hi int, refs []proto.Hash) ([]proto.Hash, error) {
	refs = refs[:0]
	for _, src := range sources {
		err := src.each(func(blocks []byte) error {
			for ; len(blocks) >= hashLen; blocks = blocks[hashLen:] {
				if b := int(blocks[0]); b >= lo && b < hi {
					refs = append(refs, proto.Hash(blocks[:hashLen]))
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(refs, cmpHash)
	return slices.Compact(refs), nil
}

// sweepPasses runs the reference collection of a GC in as many passes as the
// budget needs. Each pass scans the block lists of all sources, keeps only the
// hashes of its first-byte range [lo, hi) as a sorted array and calls sweep with
// that range and a membership test; sweep removes the unreferenced blobs of the
// range (blobstore.SweepBlobs). With a small data set there is one pass and the
// whole reference set is in memory. It returns the number of passes.
func sweepPasses(sources []refSource, budget int64, sweep func(lo, hi int, referenced func(blobstore.Hash) bool) error) (int, error) {
	var total int64
	for _, src := range sources {
		n, err := src.count()
		if err != nil {
			return 0, err
		}
		total += n
	}
	k := gcPasses(total, budget)
	// One array serves every pass. Its size is the even share of a pass plus a margin for the
	// uneven spread of hashes over first bytes; collectRefs grows it in the rare case it is short.
	refs := make([]proto.Hash, 0, min(total, total/int64(k)+total/64+1024))
	for p := 0; p < k; p++ {
		lo, hi := p*256/k, (p+1)*256/k
		var err error
		if refs, err = collectRefs(sources, lo, hi, refs); err != nil {
			return p, err
		}
		err = sweep(lo, hi, func(h blobstore.Hash) bool {
			_, found := slices.BinarySearchFunc(refs, h, cmpHash)
			return found
		})
		if err != nil {
			return p, err
		}
	}
	return k, nil
}

type gcStats struct {
	versions, blobs, tmp, passes int
	blockRefs                    int64
	refsKnown                    bool
}

// gc removes the versions older than keep_days, then the blocks no file or version references and the
// leftovers in tmp/ (design 7). It must not run twice at once: use requestGC.
func (s *Server) gc() (st gcStats, err error) {
	// A keep_days that cannot be read must not default to a short retention: this deletes for good.
	var days int64
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key='keep_days'`).Scan(&days); err != nil || days < 1 {
		return st, fmt.Errorf("gc: keep_days unreadable or below 1: %d %v", days, err)
	}
	cutoff := s.now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	ids, err := queryAll(s.db, func(rows *sql.Rows, id *int64) error { return rows.Scan(id) }, `SELECT id FROM folders`)
	if err != nil {
		return st, err
	}
	sources := []refSource{s.stageRefs()}
	for _, id := range ids {
		db, err := s.folderDB(id)
		if err != nil {
			return st, err
		}
		res, err := db.Exec(`DELETE FROM versions WHERE replaced_at < ?`, cutoff)
		if err != nil {
			return st, err
		}
		n, _ := res.RowsAffected()
		st.versions += int(n)
		src := folderRefs(db)
		n, countErr := src.count()
		if countErr != nil {
			return st, countErr
		}
		st.blockRefs += n
		src.count = func() (int64, error) { return n, nil }
		sources = append(sources, src)
	}
	st.refsKnown = true
	st.passes, err = sweepPasses(sources, GCMemoryBudget, func(lo, hi int, referenced func(blobstore.Hash) bool) error {
		s.resources.Checkpoint() // reference array remains live until sweep completes
		n, err := s.blobs.SweepBlobs(lo, hi, referenced, gcGrace)
		st.blobs += n
		return err
	})
	if err == nil {
		st.tmp, err = s.blobs.SweepTmp(gcGrace)
		st.tmp += s.sweepStages()
	}
	return st, err
}

// requestGC runs a GC in the background. Only one runs at a time: a request made while one runs makes it
// run once more when it ends (the keep_days of the first may be stale).
func (s *Server) requestGC() {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if s.gcRunning {
		s.gcAgain = true
		return
	}
	s.gcRunning = true
	s.gcWG.Add(1)
	go func() {
		defer s.gcWG.Done()
		for again := true; again; {
			start := time.Now()
			before, readErr := resources.ReadSelf()
			if readErr != nil {
				before = resources.Reading{CPU: -1}
			}
			s.resources.BeginGC(start, before, GCMemoryBudget)
			st, err := s.gc()
			after, readErr := resources.ReadSelf()
			if readErr != nil {
				after = resources.Reading{CPU: -1}
			}
			cycle := s.resources.EndGC(time.Now(), after, st.blockRefs, st.refsKnown)
			var blockRefs any
			if cycle.BlockRefs != nil {
				blockRefs = *cycle.BlockRefs
			}
			slog.Info("gc", "versions", st.versions, "blocks", st.blobs, "tmp", st.tmp, "passes", st.passes, "budget", GCMemoryBudget, "block_refs", blockRefs, "peak_rss_MB", cycle.PeakRSSMB, "peak_exact", cycle.PeakExact, "cpu_s", cycle.CPUSeconds, "seconds", cycle.Seconds, "coverage_seconds", cycle.CoverageSeconds, "took", time.Since(start).Round(time.Millisecond), "err", err)
			if err != nil {
				s.Push.Fail("gc", "The server's daily cleanup failed", 1)
			} else {
				s.Push.Ok("gc", "The server's daily cleanup works again")
			}
			s.gcMu.Lock()
			again, s.gcAgain = s.gcAgain, false
			s.gcRunning = again
			s.gcMu.Unlock()
		}
	}()
}

// gcWait is how long to wait before the next daily GC; the first minute after a start is left alone.
func gcWait(last, now time.Time) time.Duration { return max(last.Add(gcEvery).Sub(now), time.Minute) }

// gcLoop requests a GC once a day; the time of the last request survives restarts in meta.
func (s *Server) gcLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(gcWait(time.Unix(metaInt(s.db, "last_gc"), 0), s.now())):
		}
		if err := SetMeta(s.db, "last_gc", s.now().Unix()); err != nil {
			slog.Error("gc", "err", err)
		}
		s.requestGC()
	}
}
