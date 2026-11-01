package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sort"

	"kaze-sync/internal/proto"
)

const errNewer = "the server has a newer version; resolved by two-way sync"

// commitAll sends the changes, deletions first (design 5.4, 6.4).
func (f *Folder) commitAll(ctx context.Context, items []*item, rep *Report) error {
	sort.SliceStable(items, func(i, j int) bool { return items[i].del && !items[j].del })
	for rest := items; len(rest) > 0; {
		n := min(len(rest), proto.MaxCommitOps)
		if err := f.commitChunk(ctx, rest[:n], rep); err != nil {
			return err
		}
		rest = rest[n:]
	}
	return nil
}

// commitChunk commits up to MaxCommitOps ops, uploading missing blocks and
// committing again until the server has accepted or refused every op.
func (f *Folder) commitChunk(ctx context.Context, items []*item, rep *Report) error {
	for round := 0; round < maxRounds && len(items) > 0; round++ {
		ops := make([]proto.Op, len(items))
		for i, it := range items {
			ops[i] = it.op
		}
		res, err := f.API.Commit(ctx, f.ID, ops)
		if err != nil {
			return err
		}
		var next []*item
		need := map[proto.Hash]bool{}
		var puts []localRow
		var dels []string
		for i, r := range res {
			it := items[i]
			switch r.Status {
			case proto.StatusOK:
				it.done = true
				rep.Committed++
				if it.del {
					dels = append(dels, it.path)
				} else {
					it.row.Seq = r.Seq
					puts = append(puts, it.row)
				}
			case proto.StatusNeed:
				next = append(next, it)
				for _, h := range r.Need {
					need[h] = true
				}
			case proto.StatusConflict:
				switch f.resolve(it, r.Current) {
				case resolved:
					it.done = true
					if it.del {
						dels = append(dels, it.path)
					} else {
						puts = append(puts, it.row)
					}
				case resubmit:
					next = append(next, it)
				}
			default:
				f.note(it.path, proto.KindRejected, r.Reason)
			}
		}
		if err := f.DB.apply(puts, dels); err != nil {
			return err
		}
		if len(need) > 0 {
			if items, err = f.upload(ctx, next, need, rep); err != nil {
				return err
			}
		} else {
			items = next
		}
	}
	for _, it := range items {
		f.note(it.path, proto.KindIO, "not committed: the server still reports missing blocks")
	}
	return nil
}

type outcome int

const (
	refused outcome = iota
	resolved
	resubmit
)

// resolve handles a conflict result with the cases that need no pulling
// (design 6.1): same content is adopted, a modification beats a deletion, a
// deletion of a deleted file is done. Anything else waits for two-way sync.
func (f *Folder) resolve(it *item, cur *proto.Entry) outcome {
	switch {
	case it.del && (cur == nil || cur.Hash == nil):
		return resolved
	case it.del:
	case cur == nil:
		it.op.BaseSeq = 0
		return resubmit
	case cur.Hash == nil:
		it.op.BaseSeq = cur.Seq
		return resubmit
	case *cur.Hash == *it.op.Hash:
		it.row.Seq = cur.Seq
		return resolved
	}
	f.note(it.path, proto.KindIO, errNewer)
	return refused
}

// upload sends the needed blocks of the items, reading them from the files and
// verifying each hash. Items whose file changed meanwhile are dropped and
// retried by the next scan.
func (f *Folder) upload(ctx context.Context, items []*item, need map[proto.Hash]bool, rep *Report) ([]*item, error) {
	var pkt bytes.Buffer
	flush := func() error {
		if pkt.Len() == 0 {
			return nil
		}
		err := f.API.UploadBlocks(ctx, pkt.Bytes())
		pkt.Reset()
		return err
	}
	queued := map[proto.Hash]bool{}
	var keep []*item
	for _, it := range items {
		var idx []int
		for i, h := range it.op.Blocks {
			if need[h] && !queued[h] {
				queued[h] = true
				idx = append(idx, i)
			}
		}
		err := f.readBlocks(it, idx, func(h proto.Hash, data []byte) error {
			proto.WriteBlock(&pkt, h, data)
			if pkt.Len() >= packetTarget {
				return flush()
			}
			return nil
		})
		var uerr uploadError
		switch {
		case errors.As(err, &uerr):
			return nil, uerr.err
		case errors.Is(err, errChanged):
			rep.Retry = true
			continue
		case err != nil:
			f.note(it.path, proto.KindIO, err.Error())
			continue
		}
		keep = append(keep, it)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return keep, nil
}

// uploadError marks a failure of the network side, as opposed to the file.
type uploadError struct{ err error }

func (e uploadError) Error() string { return e.err.Error() }

// readBlocks reads the blocks with the given indexes (ascending) of the item's file.
func (f *Folder) readBlocks(it *item, idx []int, emit func(proto.Hash, []byte) error) error {
	if len(idx) == 0 {
		return nil
	}
	r, err := f.FS.Open(it.path)
	if err != nil {
		return err
	}
	defer r.Close()
	var pos int64
	for _, i := range idx {
		off := int64(i) * proto.BlockSize
		if off != pos {
			if s, ok := r.(io.Seeker); ok {
				_, err = s.Seek(off, io.SeekStart)
			} else {
				_, err = io.CopyN(io.Discard, r, off-pos)
			}
			if err != nil {
				return errChanged
			}
		}
		buf := make([]byte, min(proto.BlockSize, it.op.Size-off))
		if _, err := io.ReadFull(r, buf); err != nil || proto.Hash(sha256.Sum256(buf)) != it.op.Blocks[i] {
			return errChanged
		}
		pos = off + int64(len(buf))
		if err := emit(it.op.Blocks[i], buf); err != nil {
			return uploadError{err}
		}
	}
	return nil
}
