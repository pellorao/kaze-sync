package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"path"
	"sort"
	"sync"

	"kaze-sync/internal/client/place"
	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// maxCommitBlocks is MaxCommitBlocks and stageSegment MaxStageBlocks; tests lower them to exercise big files with small ones.
var (
	maxCommitBlocks = proto.MaxCommitBlocks
	stageSegment    = proto.MaxStageBlocks
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
		n := chunkLen(rest)
		if err := f.commitChunk(ctx, rest[:n], rep); err != nil {
			return err
		}
		rest = rest[n:]
	}
	return nil
}

// chunkLen is how many leading items go into one commit: at most MaxCommitOps ops and, unless it is the first one
// alone, MaxCommitBlocks block hashes, so that the request body stays below MaxJSONBody.
func chunkLen(items []*item) int {
	n, blocks := 0, 0
	for n < len(items) && n < proto.MaxCommitOps {
		blocks += len(items[n].op.Blocks)
		if n > 0 && blocks > maxCommitBlocks {
			break
		}
		n++
	}
	return n
}

// stageItem sends the block list of a big file to the server in segments, uploading the blocks each segment reports
// missing before the next one (design 2.4). It reports false when the item cannot be committed now: the server
// refused it (noted like a rejection), or the file changed meanwhile.
func (f *Folder) stageItem(ctx context.Context, it *item, rep *Report) (bool, error) {
	var idb [16]byte
	rand.Read(idb[:])
	id := hex.EncodeToString(idb[:])
	blocks := it.op.Blocks
	for off := 0; off < len(blocks); {
		end := min(off+stageSegment, len(blocks))
		r, err := f.API.Stage(ctx, proto.StageReq{Folder: f.ID, ID: id, Offset: off, Size: it.op.Size, Blocks: blocks[off:end]})
		if err != nil {
			return false, err
		}
		switch r.Reason {
		case "":
		case proto.ReasonGap:
			if r.Have >= off { // cannot happen after our own segments: do not loop
				return false, errors.New("stage: server reports a gap")
			}
			off = r.Have
			continue
		default:
			f.note(it.path, proto.KindRejected, r.Reason)
			if f.rejected == nil {
				f.rejected = map[string]rejection{}
			}
			f.rejected[it.path] = rejection{it.op.Size, it.op.MtimeNs, r.Reason}
			return false, nil
		}
		if len(r.Need) > 0 {
			need := make(map[proto.Hash]bool, len(r.Need))
			for _, h := range r.Need {
				need[h] = true
			}
			kept, err := f.upload(ctx, []*item{it}, need, rep)
			if err != nil {
				return false, err
			}
			if len(kept) == 0 {
				return false, nil // the file changed or could not be read: the next scan retries
			}
		}
		off = end
	}
	it.stage = id
	return true, nil
}

// commitChunk commits up to MaxCommitOps ops, uploading missing blocks and
// committing again until the server has accepted or refused every op.
func (f *Folder) commitChunk(ctx context.Context, items []*item, rep *Report) error {
	for round := 0; round < maxRounds && len(items) > 0; round++ {
		var staged []*item
		for _, it := range items {
			if len(it.op.Blocks) <= maxCommitBlocks || it.stage != "" {
				staged = append(staged, it)
				continue
			}
			ok, err := f.stageItem(ctx, it, rep)
			if err != nil {
				return err
			}
			if ok {
				staged = append(staged, it)
			}
		}
		if items = staged; len(items) == 0 {
			break
		}
		ops := make([]proto.Op, len(items))
		for i, it := range items {
			ops[i] = it.op
			if it.stage != "" { // the block list is on the server already
				ops[i].Blocks, ops[i].Stage = nil, it.stage
			}
		}
		res, err := f.API.Commit(ctx, f.ID, ops)
		if err != nil {
			return err
		}
		var next []*item
		need := map[proto.Hash]bool{}
		var puts []localRow
		var dels []string
		var locked []*item
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
				if r.Reason == proto.ReasonLocked { // the configuration was behind: handled like a change in a read-only range
					locked = append(locked, it)
					continue
				}
				if r.Reason == proto.ReasonNoStage { // expired or lost: send the list again
					it.stage = ""
					next = append(next, it)
					continue
				}
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
		if err := f.rescue(ctx, locked); err != nil {
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

// ownCommit reports whether the conflict is with an entry this device itself committed after the base of the op. The
// base must be known (a base of 0 is a reset: design 5.5 wants a conflict copy) and older than the entry, and the
// retry happens once per item. The same content needs no retry, resolve adopts it.
func (f *Folder) ownCommit(it *item, cur *proto.Entry) bool {
	return !it.own && cur != nil && cur.Hash != nil && !cur.Excluded && f.DeviceName != "" && cur.Device == f.DeviceName &&
		it.op.BaseSeq > 0 && cur.Seq > it.op.BaseSeq && (it.del || it.op.Hash == nil || *cur.Hash != *it.op.Hash || cur.Link != it.op.Link)
}

// resolve handles a conflict result (design 6.1). Same content is adopted, a
// modification beats a deletion, a deletion of a deleted file is done, a
// deletion yields to a newer version, and two different versions leave the
// local one as a conflict copy and take the server's.
func (f *Folder) resolve(ctx context.Context, it *item, cur *proto.Entry) (outcome, error) {
	var src *blockSrc
	if f.ownCommit(it, cur) { // the server's entry is this device's own commit whose local row was lost (design 6.1)
		slog.Info("conflict with this device's own commit: committing on top of it", "folder", f.ID, "path", it.path, "old_base", it.op.BaseSeq, "new_base", cur.Seq)
		it.own, it.op.BaseSeq = true, cur.Seq
		return resubmit, nil
	}
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
	case *cur.Hash == *it.op.Hash && cur.Link == it.op.Link:
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
	if st, err := f.FS.Stat(it.path); err == nil && st.IsDir { // a deletion of a file whose place a directory took
		if wait, err := f.clearDir(it.path); err != nil {
			f.note(it.path, proto.KindIO, err.Error())
			return refused, nil
		} else if wait {
			f.retry = true // the directory still holds synced files: try again after the server's deletions
			return refused, nil
		}
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
	if it.op.Link {
		target, err := f.FS.Readlink(it.path)
		if err != nil || int64(len(target)) != it.op.Size || proto.Hash(sha256.Sum256([]byte(target))) != it.op.Blocks[0] {
			return errChanged
		}
		if err := emit(it.op.Blocks[0], []byte(target)); err != nil {
			return uploadError{err}
		}
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
