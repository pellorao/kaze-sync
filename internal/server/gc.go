package server

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"slices"

	"kaze-sync/internal/blobstore"
)

// GCMemoryBudget bounds the memory of the reference array of one GC pass (design 7).
const GCMemoryBudget = 64 << 20

const hashLen = 32

type hash = [hashLen]byte

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

func cmpHash(a, b hash) int {
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
func collectRefs(sources []refSource, lo, hi int, refs []hash) ([]hash, error) {
	refs = refs[:0]
	for _, src := range sources {
		err := src.each(func(blocks []byte) error {
			for ; len(blocks) >= hashLen; blocks = blocks[hashLen:] {
				if b := int(blocks[0]); b >= lo && b < hi {
					refs = append(refs, hash(blocks[:hashLen]))
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
	refs := make([]hash, 0, min(total, total/int64(k)+total/64+1024))
	for p := 0; p < k; p++ {
		lo, hi := p*256/k, (p+1)*256/k
		var err error
		if refs, err = collectRefs(sources, lo, hi, refs); err != nil {
			return p, err
		}
		err = sweep(lo, hi, func(h blobstore.Hash) bool {
			_, found := slices.BinarySearchFunc(refs, hash(h), cmpHash)
			return found
		})
		if err != nil {
			return p, err
		}
	}
	return k, nil
}
