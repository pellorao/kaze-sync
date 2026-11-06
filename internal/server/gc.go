package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"kaze-sync/internal/blobstore"
	"kaze-sync/internal/proto"
)

// GC schedule (design 7).
const (
	gcEvery = 24 * time.Hour
	gcGrace = time.Hour // a block or tmp/ file must be older than this to be removed: uploads wait for their commit
)

// GCMemoryBudget bounds the memory of the reference array of one GC pass (design 7).
const GCMemoryBudget = 64 << 20

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

type gcStats struct{ versions, blobs, tmp, passes int }

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
		sources = append(sources, folderRefs(db))
	}
	st.passes, err = sweepPasses(sources, GCMemoryBudget, func(lo, hi int, referenced func(blobstore.Hash) bool) error {
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
			st, err := s.gc()
			slog.Info("gc", "versions", st.versions, "blocks", st.blobs, "tmp", st.tmp, "passes", st.passes, "took", time.Since(start).Round(time.Millisecond), "err", err)
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
