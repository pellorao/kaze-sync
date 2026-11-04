package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/client/place"
	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// window is how many blocks of a file are requested with one /blocks/get; a
// block that repeats inside a window is requested once and kept for its repeats.
const window = 64

var errWait = errors.New("waiting for pending deletions")

// remoteError marks a failure of the network side while reading blocks.
type remoteError struct{ err error }

func (e remoteError) Error() string { return e.err.Error() }
func (e remoteError) Unwrap() error { return e.err }

// localReadError marks a failure to read the local file that supplies blocks.
type localReadError struct{ err error }

func (e localReadError) Error() string { return e.err.Error() }
func (e localReadError) Unwrap() error { return e.err }

func isRemote(err error) bool { return errors.As(err, new(remoteError)) }

// blockSrc is a local file whose blocks may be reused.
type blockSrc struct {
	path   string
	blocks []proto.Hash
	size   int64
}

func consistent(r localRow, st cfs.Entry) bool {
	return !st.IsDir && st.IsLink == r.Link && st.Size == r.Size && st.MtimeNs == r.MtimeNs
}

// fetch makes the file at e.Path equal to entry e (design 5.5) and records it
// as the local base. Blocks come from a local file with the same content, else
// from src or the server, which sends only the blocks no local file has.
// expect is the state the target must still be in.
func (f *Folder) fetch(ctx context.Context, e *proto.Entry, src *blockSrc, expect place.Expect) error {
	if e.Link {
		return f.fetchLink(ctx, e, expect)
	}
	src = f.localSource(e, src)
	err := f.placeEntry(ctx, e, src, expect)
	if src != nil && (errors.Is(err, place.ErrHashMismatch) || errors.As(err, new(localReadError))) {
		err = f.placeEntry(ctx, e, nil, expect) // the local source changed under us
	}
	if err != nil && !errors.Is(err, place.ErrTargetChanged) && !errors.Is(err, place.ErrHashMismatch) &&
		!errors.Is(err, cfs.ErrLocked) && !isRemote(err) && ctx.Err() == nil {
		// Perhaps a file sits where a directory is needed.
		if renamed, wait, cerr := f.clearAncestors(e.Path); cerr != nil {
			return cerr
		} else if wait {
			return errWait
		} else if renamed {
			err = f.placeEntry(ctx, e, src, expect)
		}
	}
	return err
}

// localSource is the block source for e: a local file with the same content if there is an unchanged one, else src.
func (f *Folder) localSource(e *proto.Entry, src *blockSrc) *blockSrc {
	if rows, err := f.DB.byHash(e.Hash[:], 8); err == nil {
		for _, r := range rows {
			if r.Link {
				continue // its content is a link target, not a file
			}
			if st, err := f.FS.Stat(r.Path); err == nil && consistent(r, st) {
				return &blockSrc{r.Path, proto.SplitHashes(r.Blocks), r.Size}
			}
		}
	}
	return src
}

func (f *Folder) placeEntry(ctx context.Context, e *proto.Entry, src *blockSrc, expect place.Expect) error {
	open := func(offset int64) (io.ReadCloser, error) { return newBlockReader(ctx, f, e, src, offset), nil }
	if err := place.Place(f.FS, e.Path, *e.Hash, open, place.Meta{Size: e.Size, MtimeNs: e.MtimeNs, Exec: e.Exec}, expect); err != nil {
		return err
	}
	row, err := f.placedRow(e)
	if err != nil {
		return err
	}
	return f.DB.apply([]localRow{row}, nil)
}

// placedRow is the base row for entry e after its file was placed: the state the file system reports.
func (f *Folder) placedRow(e *proto.Entry) (localRow, error) {
	st, err := f.FS.Stat(e.Path)
	if err != nil {
		return localRow{}, err
	}
	return localRow{Path: e.Path, Key: pp.Key(e.Path), Seq: e.Seq, Hash: e.Hash[:], Blocks: proto.JoinHashes(e.Blocks),
		Size: st.Size, MtimeNs: st.MtimeNs, Exec: e.Exec && !e.Link, Link: e.Link}, nil
}

// maxConflictNames is how many names (one per second) conflictName tries before it gives up.
const maxConflictNames = 1000

// conflictName returns a free name for the conflict copy of p (design 6.1).
func (f *Folder) conflictName(p string) (string, error) {
	dir, base := path.Split(p)
	ext := path.Ext(base)
	dev := f.DeviceName
	if dev == "" {
		dev = "device"
	}
	t := f.Now()
	for range maxConflictNames {
		q := dir + strings.TrimSuffix(base, ext) + ".conflict-" + dev + "-" + t.Format("20060102-150405") + ext
		if _, err := f.FS.Stat(q); errors.Is(err, fs.ErrNotExist) {
			return q, nil
		} else if err != nil {
			return "", err
		}
		t = t.Add(time.Second)
	}
	return "", fmt.Errorf("no free conflict copy name for %s: %d are taken", p, maxConflictNames)
}

// renameAway moves the file or directory p to its conflict copy name.
func (f *Folder) renameAway(p string) (string, error) {
	q, err := f.conflictName(p)
	if err == nil {
		err = f.FS.Rename(p, q)
	}
	return q, err
}

// clearDir makes room for a file where the directory p is. A directory whose
// files are all still the synced ones waits for the server's deletions;
// anything else is moved to a conflict copy name (design 6.6).
func (f *Folder) clearDir(p string) (wait bool, err error) {
	if ents, err := f.FS.ReadDir(p); err == nil && len(ents) == 0 {
		return false, f.FS.Remove(p)
	}
	rows, err := f.DB.under(p)
	if err != nil {
		return false, err
	}
	synced := len(rows) > 0
	for _, r := range rows {
		if st, err := f.FS.Stat(r.Path); err != nil || !consistent(r, st) {
			synced = false
		}
	}
	if synced {
		return true, nil
	}
	f.retry = true
	_, err = f.renameAway(p)
	return false, err
}

// clearAncestors handles a file where a parent directory of p is needed, like clearDir.
func (f *Folder) clearAncestors(p string) (renamed, wait bool, err error) {
	for a := path.Dir(p); a != "." && a != "/"; a = path.Dir(a) {
		st, err := f.FS.Stat(a)
		if err != nil || st.IsDir {
			continue
		}
		if r, ok, err := f.DB.get(a); err != nil {
			return false, false, err
		} else if ok && consistent(r, st) {
			return false, true, nil
		}
		f.retry = true
		_, err = f.renameAway(a)
		return err == nil, false, err
	}
	return false, false, nil
}

// blockReader supplies the content of entry e from block start on, block by block.
type blockReader struct {
	ctx    context.Context
	e      *proto.Entry
	f      *Folder
	i      int    // next block
	skip   int64  // bytes of the first block to drop (resuming inside a block)
	buf    []byte // storage of the current block
	cur    []byte // unread part of it
	pbuf   []byte
	srcIdx map[proto.Hash]int
	src    *blockSrc
	srcR   io.ReadSeeker
	srcC   io.Closer

	winEnd int
	count  map[proto.Hash]int // remote hashes of the window and how often each occurs
	cache  map[proto.Hash][]byte
	body   io.ReadCloser

	prefetch map[proto.Hash][]byte // remote blocks already read from a batch response (see fetchBatch)
}

func newBlockReader(ctx context.Context, f *Folder, e *proto.Entry, src *blockSrc, offset int64) *blockReader {
	r := &blockReader{ctx: ctx, e: e, f: f, i: int(offset / proto.BlockSize), skip: offset % proto.BlockSize, src: src}
	if src != nil {
		r.srcIdx = map[proto.Hash]int{}
		for j := len(src.blocks) - 1; j >= 0; j-- {
			r.srcIdx[src.blocks[j]] = j
		}
	}
	return r
}

func (r *blockReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		if r.i >= len(r.e.Blocks) {
			return 0, io.EOF
		}
		if err := r.load(r.i); err != nil {
			return 0, err
		}
		r.i++
		r.cur = r.cur[min(r.skip, int64(len(r.cur))):]
		r.skip = 0
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

func (r *blockReader) Close() error {
	if r.body != nil {
		r.body.Close()
	}
	if r.srcC != nil {
		r.srcC.Close()
	}
	return nil
}

func (r *blockReader) local(h proto.Hash) (int, bool) {
	j, ok := r.srcIdx[h]
	return j, ok
}

func (r *blockReader) load(i int) error {
	if r.buf == nil {
		r.buf = make([]byte, proto.BlockSize)
	}
	h := r.e.Blocks[i]
	n := int(min(proto.BlockSize, r.e.Size-int64(i)*proto.BlockSize))
	dst := r.buf[:n]
	if j, ok := r.local(h); ok {
		if err := r.readSrc(j, dst); err != nil {
			return localReadError{err}
		}
		r.cur = dst
		return nil
	}
	if data, ok := r.prefetch[h]; ok {
		if len(data) != n {
			return remoteError{fmt.Errorf("server sent block %s with %d bytes, want %d", h, len(data), n)}
		}
		copy(dst, data)
		r.cur = dst
		return nil
	}
	if c, ok := r.cache[h]; ok {
		copy(dst, c)
		if r.count[h]--; r.count[h] <= 1 {
			delete(r.cache, h)
		}
		r.cur = dst
		return nil
	}
	if i >= r.winEnd {
		if err := r.openWindow(i); err != nil {
			return remoteError{err}
		}
	}
	if r.pbuf == nil {
		r.pbuf = make([]byte, proto.BlockSize)
	}
	got, data, err := proto.ReadBlock(r.body, r.pbuf)
	if err == nil && (got != h || len(data) != n) {
		err = fmt.Errorf("server sent block %s (%d bytes), want %s (%d)", got, len(data), h, n)
	}
	if err != nil {
		return remoteError{err}
	}
	copy(dst, data)
	if r.count[h] > 1 {
		if r.cache == nil {
			r.cache = map[proto.Hash][]byte{}
		}
		r.cache[h] = append([]byte(nil), data...)
	}
	r.cur = dst
	return nil
}

func (r *blockReader) readSrc(j int, dst []byte) error {
	if r.srcR == nil {
		rc, err := r.f.FS.Open(r.src.path)
		if err != nil {
			return err
		}
		s, ok := rc.(io.ReadSeeker)
		if !ok {
			rc.Close()
			return errors.New("file system cannot seek")
		}
		r.srcR, r.srcC = s, rc
	}
	if _, err := r.srcR.Seek(int64(j)*proto.BlockSize, io.SeekStart); err != nil {
		return err
	}
	_, err := io.ReadFull(r.srcR, dst)
	return err
}

// openWindow requests the blocks of positions i to i+window that no local file has.
func (r *blockReader) openWindow(i int) error {
	if r.body != nil {
		r.body.Close()
		r.body = nil
	}
	want := r.plan(i, min(i+window, len(r.e.Blocks)))
	var err error
	r.body, err = r.f.API.GetBlocks(r.ctx, want)
	return err
}

// plan sets the reader to take the blocks of positions i to end from one response and returns the hashes that
// response must hold, in order: those no local file has, each once (a repeat is kept for its later positions).
func (r *blockReader) plan(i, end int) []proto.Hash {
	r.winEnd = end
	r.count, r.cache = map[proto.Hash]int{}, nil
	var want []proto.Hash
	for k := i; k < r.winEnd; k++ {
		h := r.e.Blocks[k]
		if _, ok := r.local(h); ok {
			continue
		}
		if r.count[h]++; r.count[h] == 1 {
			want = append(want, h)
		}
	}
	return want
}

// linkTarget reads the target of the link entry e from the server: its content is one block of at most
// MaxLinkTarget bytes, which hashes to the whole-file hash.
func (f *Folder) linkTarget(ctx context.Context, e *proto.Entry) (string, error) {
	if e.Size < 1 || e.Size > proto.MaxLinkTarget || len(e.Blocks) != 1 || e.Blocks[0] != *e.Hash {
		return "", fmt.Errorf("the server's link entry is malformed (%d bytes, %d blocks)", e.Size, len(e.Blocks))
	}
	body, err := f.API.GetBlocks(ctx, e.Blocks)
	if err != nil {
		return "", remoteError{err}
	}
	defer body.Close()
	got, data, err := proto.ReadBlock(body, make([]byte, proto.BlockSize))
	if err == nil && (got != *e.Hash || int64(len(data)) != e.Size) {
		err = fmt.Errorf("server sent block %s (%d bytes), want %s (%d)", got, len(data), e.Hash, e.Size)
	}
	if err != nil {
		return "", remoteError{err}
	}
	return string(data), nil
}

// linksOK probes once per Pull whether this machine can create symbolic links, so that a machine that cannot does not
// fetch the target of every link first.
func (f *Folder) linksOK() bool {
	if f.linkProbe == 0 {
		probe := ".syncpart-linkprobe"
		f.FS.Remove(probe)
		f.linkProbe = 1
		if err := f.FS.Symlink("probe", probe); errors.Is(err, cfs.ErrLinkUnsupported) {
			f.linkProbe = -1
		} else if err == nil {
			f.FS.Remove(probe)
		}
	}
	return f.linkProbe > 0
}

// fetchLink makes the path of the link entry e a symbolic link to its target and records it as the local base
// (design 6.7). When this machine cannot create links the link is left on the server, the path is recorded in the
// unlinked table that the Web UI notice is computed from, and the entry counts as applied.
func (f *Folder) fetchLink(ctx context.Context, e *proto.Entry, expect place.Expect) error {
	var err error
	if f.linksOK() {
		var target string
		if target, err = f.linkTarget(ctx, e); err == nil {
			err = place.PlaceLink(f.FS, e.Path, target, *e.Hash, expect)
		}
	} else {
		err = cfs.ErrLinkUnsupported
	}
	if errors.Is(err, cfs.ErrLinkUnsupported) {
		return f.DB.putUnlinked(unlinkedRow{Path: e.Path, Seq: e.Seq, Hash: e.Hash[:], Size: e.Size, MtimeNs: e.MtimeNs,
			Message: proto.LinkNotCreated + " (on Windows turn on Developer Mode in Settings, then the link appears by itself)"})
	}
	if err != nil {
		return err
	}
	row, err := f.placedRow(e)
	if err != nil {
		return err
	}
	if err := f.DB.dropUnlinked(e.Path); err != nil {
		return err
	}
	return f.DB.apply([]localRow{row}, nil)
}

// retryUnlinked tries the links that could not be created again, as soon as the machine can create them.
func (f *Folder) retryUnlinked(ctx context.Context) error {
	rows, err := f.DB.unlinked()
	if err != nil || len(rows) == 0 || !f.linksOK() {
		return err
	}
	for _, r := range rows {
		e := proto.Entry{Path: r.Path, Seq: r.Seq, Size: r.Size, MtimeNs: r.MtimeNs, Link: true, Hash: (*proto.Hash)(r.Hash)}
		e.Blocks = []proto.Hash{*e.Hash}
		if _, err := f.applyLive(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
