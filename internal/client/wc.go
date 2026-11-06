package client

// Write control on the client (design 2.8): the folder is cut into scopes, a scope whose local changes the server
// refused or found to conflict is frozen, and its changes wait in the held table until the user decides.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"strings"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/client/place"
	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// wcRule is a write-control setting by the key of its path; the root (key "") always comes first.
type wcRule struct{ key, mode string }

func buildWC(rs []proto.WCRule) []wcRule {
	out := []wcRule{{"", proto.ModeSingle}}
	for _, r := range rs {
		if k := pp.Key(r.Path); k == "" {
			out[0].mode = r.Mode
		} else {
			out = append(out, wcRule{k, r.Mode})
		}
	}
	slices.SortFunc(out[1:], func(a, b wcRule) int { return strings.Compare(a.key, b.key) })
	return out
}

// scopeOf names the scope of a file and gives its mode: a setting and everything it governs ("d:<key>"), or one file
// of a free directory ("f:<key>"), whose freeze concerns that file only.
func (f *Folder) scopeOf(p string) (id, mode string) {
	k := pp.Key(p)
	best := f.wc[0]
	for _, r := range f.wc[1:] { // sorted: a later cover is a longer one
		if k == r.key || strings.HasPrefix(k, r.key+"/") {
			best = r
		}
	}
	if best.mode == proto.ModeFree {
		return "f:" + k, best.mode
	}
	return "d:" + best.key, best.mode
}

func (f *Folder) frozenPath(p string) bool {
	if len(f.frozen) == 0 {
		return false
	}
	id, _ := f.scopeOf(p)
	_, ok := f.frozen[id]
	return ok
}

// freeze stops the sync of a scope on this device, remembering the cursor its pulls stop at (design 2.8.5).
func (f *Folder) freeze(scope string) error {
	if _, ok := f.frozen[scope]; ok {
		return nil
	}
	cur := f.DB.metaInt("cursor")
	if err := f.DB.setMeta("frozen:"+scope, cur); err != nil {
		return err
	}
	f.frozen[scope] = cur
	return nil
}

// thaw unfreezes a scope with no held changes left: the next Pull reads it again from the cursor of the freeze. With
// keepDirty the since of its next changes stays what it was (a requeue after a configuration change, design 2.8.6).
func (f *Folder) thaw(scope string, keepDirty bool) error {
	v, ok := f.frozen[scope]
	if !ok {
		return nil
	}
	if rest, err := f.DB.held(scope); err != nil || len(rest) > 0 {
		return err
	}
	delete(f.frozen, scope)
	if err := f.DB.delMeta("frozen:" + scope); err != nil {
		return err
	}
	if !keepDirty {
		f.DB.delMeta("dirty:" + scope)
	}
	if err := f.lowerCursor(v); err != nil {
		return err
	}
	f.seen = -1
	f.rejected = nil
	return nil
}

// lowerCursor moves the Pull cursor back to v when it is further on. The pulls to come skip the entries of the scopes
// still frozen from there on, so those read from v too when they are unfrozen.
func (f *Folder) lowerCursor(v int64) error {
	if v >= f.DB.metaInt("cursor") {
		return nil
	}
	if err := f.DB.setMeta("cursor", v); err != nil {
		return err
	}
	for scope, fv := range f.frozen {
		if fv > v {
			f.frozen[scope] = v
			if err := f.DB.setMeta("frozen:"+scope, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// Requeue is for a configuration change: the changes held for a refused write are sent again under the new settings
// (design 2.8.6), the others wait as before.
func (f *Folder) Requeue() error {
	rows, err := f.DB.held("")
	if err != nil {
		return err
	}
	scopes := map[string]bool{}
	for _, h := range rows {
		if h.Reason == "owner" {
			scopes[h.Scope] = true
			if err := f.DB.dropHeld(h.Path); err != nil {
				return err
			}
		}
	}
	for sc := range scopes {
		if err := f.thaw(sc, true); err != nil {
			return err
		}
	}
	f.heldDirty = true
	return nil
}

// hold stores the items as held and freezes their scopes. An item keeps the reason it was given, else why.
func (f *Folder) hold(items []*item, why string) error {
	rows := make([]heldRow, 0, len(items))
	at := f.Now().Unix()
	for _, it := range items {
		id, _ := f.scopeOf(it.path)
		if err := f.freeze(id); err != nil {
			return err
		}
		h := heldRow{Path: it.path, Scope: id, Reason: cmpOr(it.reason, why), BaseSeq: it.op.BaseSeq, Kind: "modify", Hash: it.row.Hash,
			Size: it.row.Size, MtimeNs: it.row.MtimeNs, At: at}
		switch {
		case it.del:
			h.Kind = "delete"
		case it.op.BaseSeq == 0:
			h.Kind = "add"
		}
		rows = append(rows, h)
	}
	f.heldDirty = true
	return f.DB.putHeld(rows)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// holdTree holds the local files at p, or below it when p is a directory, which a server entry cannot replace (design 6.6).
func (f *Folder) holdTree(p string) (int, error) {
	var items []*item
	var walk func(string)
	walk = func(q string) {
		st, err := f.FS.Stat(q)
		if err != nil {
			return
		}
		if !st.IsDir {
			items = append(items, &item{path: q, reason: "type", row: localRow{Size: st.Size, MtimeNs: st.MtimeNs}})
			return
		}
		ents, _ := f.FS.ReadDir(q)
		for _, e := range ents {
			if !builtinPath(q + "/" + e.Name) {
				walk(q + "/" + e.Name)
			}
		}
	}
	walk(p)
	return len(items), f.hold(items, "type")
}

// setSince gives the ops of single-writer scopes the cursor their scope had applied when the device began changing
// it (design 2.8.6): the value of the last scan, kept while changes of the scope stay unsent. Smaller is safe.
func (f *Folder) setSince(items []*item) {
	dirty := f.DB.metaPrefix("dirty:")
	base := min(f.DB.metaInt("scanc"), f.DB.metaInt("cursor"))
	for _, it := range items {
		id, mode := f.scopeOf(it.path)
		if mode != proto.ModeSingle {
			continue
		}
		v, ok := dirty[id]
		if !ok {
			v = base
			dirty[id] = v
			f.DB.setMeta("dirty:"+id, v)
		}
		it.op.Since = v
	}
}

// endScan records the cursor the scan ended at, and clears the since of the scopes that have nothing left to send.
// Only a whole scan can show that nothing is left unsent anywhere; once it has, a partial scan that sends everything it
// finds is enough, or the first change after another device's write would always carry the old since and be refused.
func (f *Folder) endScan(items []*item, whole bool, failed error) {
	if c := f.DB.metaInt("cursor"); c != f.DB.metaInt("scanc") {
		f.DB.setMeta("scanc", c)
	}
	busy := map[string]bool{}
	for _, it := range items {
		if !it.done {
			id, _ := f.scopeOf(it.path)
			busy[id] = true
		}
	}
	clean := failed == nil && len(busy) == 0 && f.young.IsZero() && !f.retry && len(f.errs) == 0
	if !clean || !whole && f.unsent {
		f.unsent = f.unsent || !clean
		return
	}
	f.unsent = false
	for id := range f.DB.metaPrefix("dirty:") {
		if _, fr := f.frozen[id]; !fr {
			f.DB.delMeta("dirty:" + id)
		}
	}
}

// cleanHeld drops the held changes whose files went back to the base (reverted) or are gone with no base to go
// back to, within what the scan covered, and unfreezes the scopes left without any.
func (f *Folder) cleanHeld(files map[string]found, local map[string]localRow, reverted map[string]bool, failed map[string]bool) {
	rows, err := f.DB.held("")
	if err != nil {
		return
	}
	scopes := map[string]bool{}
	for _, h := range rows {
		_, there := files[h.Path]
		_, based := local[h.Path]
		gone := !there && !based && (f.scope == nil || f.scope(h.Path)) && !underFailed(failed, h.Path)
		same := there && based && files[h.Path].e.Size == local[h.Path].Size && files[h.Path].e.MtimeNs == local[h.Path].MtimeNs
		if reverted[h.Path] || gone || same { // same: the base was written over it, like a fetch of equal content
			f.DB.dropHeld(h.Path)
			scopes[h.Scope] = true
			f.heldDirty = true
		}
	}
	for sc := range scopes {
		f.thaw(sc, false)
	}
}

// HeldItems is the list of held changes for the server (POST /held), and whether it differs from the last one sent.
func (f *Folder) HeldItems() (items []proto.HeldItem, changed bool) {
	rows, _ := f.DB.held("")
	items = []proto.HeldItem{}
	for _, h := range rows {
		items = append(items, proto.HeldItem{Path: h.Path, Scope: h.Scope, Reason: h.Reason, BaseSeq: h.BaseSeq, Kind: h.Kind, Size: h.Size, MtimeNs: h.MtimeNs, At: h.At, Note: f.heldNote[h.Path]})
	}
	b, _ := json.Marshal(items)
	sum := sha256.Sum256(b)
	f.heldSig = string(sum[:])
	return items, f.heldSig != f.heldSent
}

// HeldSent records that the list HeldItems returned last reached the server.
func (f *Folder) HeldSent() { f.heldSent = f.heldSig }

// Decisions on a held change (design 2.8.7).
const (
	ActUpload    = proto.ActUpload
	ActOverwrite = proto.ActOverwrite
	ActDiscard   = proto.ActDiscard
)

var (
	errLocalChanged  = errors.New("the local file changed after it was held: decide again")
	errServerChanged = errors.New("the server version changed: decide again")
)

// probe returns the server's current entry of p, nil when it has none (a deletion that cannot match any base).
func (f *Folder) probe(ctx context.Context, p string) (*proto.Entry, error) {
	res, err := f.API.Commit(ctx, f.ID, []proto.Op{{Path: p, BaseSeq: -1, Resolve: true}})
	if err != nil || len(res) != 1 {
		return nil, errors.Join(err, errors.New("probe: bad answer"))
	}
	return res[0].Current, nil
}

// Resolve carries out the user's decision on the held change of act.Path. It checks first that the local file is
// still the one that was held, and that the held change and the server are what the user saw (act.Expect, Size and
// MtimeNs); a failed check or a changed server leaves everything as it was and leaves a note for the Web UI.
func (f *Folder) Resolve(ctx context.Context, act proto.Action) error {
	p := act.Path
	rows, err := f.DB.held("")
	if err != nil {
		return err
	}
	i := slices.IndexFunc(rows, func(h heldRow) bool { return h.Path == p })
	if i < 0 {
		return nil
	}
	h := rows[i]
	delete(f.heldNote, p)
	st, serr := f.FS.Stat(p)
	if h.Kind == "delete" && serr == nil && !st.IsDir || h.Kind != "delete" && (serr != nil || (!st.IsDir && (st.Size != h.Size || st.MtimeNs != h.MtimeNs))) ||
		act.Expect != nil && (act.Size != h.Size || act.MtimeNs != h.MtimeNs) {
		return f.failNote(p, "local", errLocalChanged)
	}
	if act.Action == ActDiscard {
		err = f.discard(ctx, h, serr == nil, act.Save)
	} else {
		err = f.uploadHeld(ctx, h, st, act)
	}
	switch {
	case errors.Is(err, errServerChanged):
		return f.failNote(p, "server", err)
	case err != nil:
		return f.failNote(p, "failed", err)
	}
	f.heldDirty = true
	if err := f.DB.dropHeld(p); err != nil {
		return err
	}
	return f.thaw(h.Scope, false)
}

// failNote records why a decision was not carried out, for the held list the Web UI shows.
func (f *Folder) failNote(p, note string, err error) error {
	if f.heldNote == nil {
		f.heldNote = map[string]string{}
	}
	f.heldNote[p] = note
	f.heldDirty = true
	return err
}

// uploadHeld commits the held change with the resolve flag. Upload keeps the base of the change, so the server must
// not have changed the path since; overwrite takes the seq the user saw (else the server's current one) as its base.
// A change against a file or directory of the other kind first deletes what is in its way (design 2.8.7).
func (f *Folder) uploadHeld(ctx context.Context, h heldRow, st cfs.Entry, act proto.Action) error {
	it := &item{path: h.Path, del: h.Kind == "delete", decided: true, op: proto.Op{Path: h.Path, BaseSeq: h.BaseSeq, Resolve: true}}
	var pre []*item
	if act.Action == ActOverwrite {
		switch {
		case act.Expect != nil:
			it.op.BaseSeq = *act.Expect
		default:
			cur, err := f.probe(ctx, h.Path)
			if err != nil {
				return err
			}
			it.op.BaseSeq = 0
			if cur != nil {
				it.op.BaseSeq = cur.Seq
			}
		}
		if h.Reason == "type" {
			blocking, err := f.API.Blocking(ctx, f.ID, h.Path)
			if err != nil {
				return err
			}
			for _, e := range blocking {
				pre = append(pre, &item{path: e.Path, del: true, decided: true, op: proto.Op{Path: e.Path, BaseSeq: e.Seq, Resolve: true}})
			}
		}
	}
	if !it.del {
		buf := make([]byte, proto.BlockSize)
		hs, err := f.hashFile(h.Path, found{e: st, link: st.IsLink}, buf)
		if err != nil {
			return err
		}
		if h.Hash != nil && !slices.Equal(hs.whole[:], h.Hash) {
			return errLocalChanged
		}
		wh := hs.whole
		it.row = localRow{Path: h.Path, Key: pp.Key(h.Path), Hash: wh[:], Blocks: proto.JoinHashes(hs.blocks), Size: st.Size, MtimeNs: st.MtimeNs, Exec: st.Exec && !st.IsLink, Link: st.IsLink}
		it.op = proto.Op{Path: h.Path, BaseSeq: it.op.BaseSeq, Hash: &wh, Blocks: hs.blocks, Size: hs.size, MtimeNs: st.MtimeNs, Exec: it.row.Exec, Link: st.IsLink, Resolve: true}
	}
	var rep Report
	if err := f.commitChunk(ctx, append(pre, it), &rep); err != nil {
		return err
	}
	if !it.done {
		return errServerChanged
	}
	return nil
}

// discard sets the local version of the held path aside (or removes it) and takes the server's.
func (f *Folder) discard(ctx context.Context, h heldRow, exists, save bool) error {
	cur, err := f.probe(ctx, h.Path)
	if err != nil {
		return err
	}
	if exists {
		if st, _ := f.FS.Stat(h.Path); save || st.IsDir {
			err = f.saveAway(h.Path)
		} else {
			err = f.FS.Remove(h.Path)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	rows, err := f.DB.under(h.Path)
	if err != nil {
		return err
	}
	dels := []string{h.Path}
	for p := range rows {
		dels = append(dels, p)
	}
	if err := f.DB.apply(nil, dels); err != nil {
		return err
	}
	if err := f.lowerCursor(0); err != nil { // the rows are gone: the server's entries at and below p come again
		return err
	}
	f.seen = -1
	if cur != nil && cur.Hash != nil {
		return f.fetch(ctx, cur, nil, place.Expect{})
	}
	return nil
}
