package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"path"
	"sort"
	"sync"

	"kaze-sync/internal/client/place"
	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// uploadMu serializes block uploads of all folders of the process.
var uploadMu sync.Mutex

// order puts additions that need no deletion first, then the deletions, then
// the additions that depend on one (same name ignoring case, or a file and a
// directory of one name; design 5.4, 6.4). A moved tree thus reaches the other
// clients as additions while the old files still exist there, so they copy
// the content locally instead of downloading it, whatever the batch sizes.
func order(items []*item) {
	keys := map[string]bool{} // deleted paths and their ancestor directories
	dirs := map[string]bool{}
	for _, it := range items {
		if !it.del {
			continue
		}
		keys[pp.Key(it.path)] = true
		for d := path.Dir(it.path); d != "."; d = path.Dir(d) {
			dirs[pp.Key(d)] = true
		}
	}
	dependent := func(it *item) bool {
		if it.del {
			return false
		}
		k := pp.Key(it.path)
		if keys[k] || dirs[k] {
			return true
		}
		for d := path.Dir(it.path); d != "."; d = path.Dir(d) {
			if keys[pp.Key(d)] {
				return true
			}
		}
		return false
	}
	rank := func(it *item) int {
		switch {
		case it.del:
			return 1
		case dependent(it):
			return 2
		}
		return 0
	}
	ranks := make(map[*item]int, len(items))
	for _, it := range items {
		ranks[it] = rank(it)
	}
	sort.SliceStable(items, func(i, j int) bool { return ranks[items[i]] < ranks[items[j]] })
}

// commitAll sends the changes in the order of order.
func (f *Folder) commitAll(ctx context.Context, items []*item, rep *Report) error {
	order(items)
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
				out, err := f.resolve(ctx, it, r.Current)
				if err != nil {
					return err
				}
				switch out {
				case handled:
					it.done = true
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
				if f.rejected == nil {
					f.rejected = map[string]rejection{}
				}
				f.rejected[it.path] = rejection{it.op.Size, it.op.MtimeNs, r.Reason}
				if r.Reason == proto.ReasonTypeConflict { // the blocking entry may be behind the cursor: read everything again
					if err := f.DB.setMeta("cursor", 0); err != nil {
						return err
					}
				}
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
	refused  outcome = iota
	resolved         // the local row to store is it.row
	resubmit
	handled // resolve already updated the local state
)

// resolve handles a conflict result (design 6.1). Same content is adopted, a
// modification beats a deletion, a deletion of a deleted file is done, a
// deletion yields to a newer version, and two different versions leave the
// local one as a conflict copy and take the server's.
func (f *Folder) resolve(ctx context.Context, it *item, cur *proto.Entry) (outcome, error) {
	var src *blockSrc
	switch {
	case it.del && (cur == nil || cur.Hash == nil):
		return resolved, nil
	case it.del:
	case cur == nil:
		it.op.BaseSeq = 0
		return resubmit, nil
	case cur.Hash == nil:
		it.op.BaseSeq = cur.Seq
		return resubmit, nil
	case *cur.Hash == *it.op.Hash:
		it.row.Seq = cur.Seq
		return resolved, nil
	default:
		q, err := f.renameAway(it.path)
		if err != nil {
			f.note(it.path, proto.KindIO, err.Error())
			return refused, nil
		}
		f.retry = true // the copy is a new file: scan again to upload it
		src = &blockSrc{path: q, blocks: it.op.Blocks, size: it.op.Size}
	}
	err := f.fetch(ctx, cur, src, place.Expect{})
	switch {
	case err == nil:
		return handled, nil
	case isRemote(err) || ctx.Err() != nil:
		return refused, err
	}
	f.note(it.path, proto.KindIO, err.Error())
	f.retry = true
	return refused, nil
}

// upload sends the needed blocks of the items, reading them from the files and
// verifying each hash. Items whose file changed meanwhile are dropped and
// retried by the next scan.
func (f *Folder) upload(ctx context.Context, items []*item, need map[proto.Hash]bool, rep *Report) ([]*item, error) {
	uploadMu.Lock() // one upload at a time over all folders bounds the buffers (design: low memory)
	defer uploadMu.Unlock()
	var pkt bytes.Buffer
	pkt.Grow(min(len(need)*(proto.BlockSize+36), packetTarget))
	buf := make([]byte, proto.BlockSize)
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
		err := f.readBlocks(it, idx, buf, func(h proto.Hash, data []byte) error {
			var err error
			if pkt.Len()+36+len(data) > packetTarget {
				err = flush()
			}
			proto.WriteBlock(&pkt, h, data)
			return err
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
func (f *Folder) readBlocks(it *item, idx []int, buf []byte, emit func(proto.Hash, []byte) error) error {
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
		buf := buf[:min(proto.BlockSize, it.op.Size-off)]
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
