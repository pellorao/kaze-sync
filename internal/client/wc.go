package client

// Write control on the client (design 2.8): the folder is cut into scopes, a scope whose local changes the server
// refused or found to conflict is frozen, and its changes wait in the held table until the user decides.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"path"
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

// thaw unfreezes those of scopes that have no held changes left: the next Pull reads them again from the cursor of
// the freeze. With keepDirty the since of their next changes stays what it was (a requeue after a configuration
// change, design 2.8.6). One query finds the scopes still holding something, and one transaction drops the freezes
// and moves the cursor back, so that a crash cannot leave a scope unfrozen with its skipped entries behind the cursor.
func (f *Folder) thaw(keepDirty bool, scopes ...string) error {
	var free []string
	var busy map[string]bool
	low := int64(math.MaxInt64)
	for _, sc := range scopes {
		v, ok := f.frozen[sc]
		if !ok || slices.Contains(free, sc) {
			continue
		}
		if busy == nil {
			var err error
			if busy, err = f.DB.heldScopes(); err != nil {
				return err
			}
		}
		if !busy[sc] {
			free = append(free, sc)
			low = min(low, v)
		}
	}
	if len(free) == 0 {
		return nil
	}
	if err := f.DB.thaw(free, keepDirty, low); err != nil {
		return err
	}
	for _, sc := range free {
		delete(f.frozen, sc)
	}
	f.lowered(low)
	f.seen = -1
	f.rejected = nil
	return nil
}

// lowerCursor moves the Pull cursor back to v when it is further on. The pulls to come skip the entries of the scopes
// still frozen from there on, so those read from v too when they are unfrozen.
func (f *Folder) lowerCursor(v int64) error {
	if err := f.DB.lowerCursor(v); err != nil {
		return err
	}
	f.lowered(v)
	return nil
}

// lowered brings the freeze cursors kept in memory down to v, as localDB.lowerCursor did in the database.
func (f *Folder) lowered(v int64) {
	for scope, fv := range f.frozen {
		if fv > v {
			f.frozen[scope] = v
		}
	}
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
	return f.thaw(true, slices.Collect(maps.Keys(scopes))...)
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
// The folder remembers where changes were left unsent (unsentAll for "somewhere", else directories). A scan replaces
// the record for what it covered, the whole folder or its directories, so a partial retry that sends the last
// unsent file is enough to clear the since; without this the first change after another device's write would
// carry the old since and be refused (design 2.8.6).
func (f *Folder) endScan(items []*item, whole bool, failed error) {
	if c := f.DB.metaInt("cursor"); c != f.DB.metaInt("scanc") {
		f.DB.setMeta("scanc", c)
	}
	unsent := map[string]bool{}
	unsentF := map[string]bool{} // unsent single-writer files whose scope is not their directory's (a setting on the file itself)
	note := func(p string) {
		d := path.Dir(p)
		unsent[d] = true
		if a, m := f.scopeOf(p); m == proto.ModeSingle {
			if b, _ := f.dirScope(d); a != b {
				unsentF[p] = true
			}
		}
	}
	for _, it := range items {
		if _, refused := f.rejected[it.path]; !it.done && !refused { // a refused path (name, size, space...) cannot be sent: not unsent
			note(it.path)
		}
	}
	for p := range f.youngFiles {
		note(p)
	}
	for d := range f.changedDirs { // a file in use (retryDirs only) is a standing warning: not unsent
		unsent[d] = true
	}
	// Standing warnings (refused paths, files in use, skipped entries, unreadable files) are paths that cannot be sent
	// now: they do not hold the since back (design 2.8.6). Only a folder-level error does.
	folderErr := false
	for _, e := range f.errs {
		folderErr = folderErr || e.Kind == proto.KindFolder
	}
	// Anything not clean that names no place (a failed scan, a folder error, a retry without a directory) is
	// unsent everywhere until a whole scan is clean.
	somewhere := failed != nil || folderErr || (f.retry && len(unsent) == 0)
	if whole {
		f.unsentAll, f.unsentDirs, f.unsentFiles = false, nil, nil
	} else {
		for p := range f.unsentFiles {
			if f.coversDir(path.Dir(p)) {
				delete(f.unsentFiles, p)
			}
		}
		for d := range f.unsentDirs {
			if f.coversDir(d) {
				delete(f.unsentDirs, d)
			}
		}
	}
	f.unsentAll = f.unsentAll || somewhere
	for d := range unsent {
		if f.unsentDirs == nil {
			f.unsentDirs = map[string]bool{}
		}
		f.unsentDirs[d] = true
	}
	for p := range unsentF {
		if f.unsentFiles == nil {
			f.unsentFiles = map[string]bool{}
		}
		f.unsentFiles[p] = true
	}
	f.clearDirty()
}

// clearDirty clears the since of each scope with no unsent directory of its own and no freeze, so that a scope
// kept back by unsent changes (or frozen) does not keep the others (design 2.8.6). With unsentAll nothing clears.
func (f *Folder) clearDirty() {
	if f.unsentAll {
		return
	}
	busy := map[string]bool{}
	f.eachUnsentScope(func(id, _ string) bool { busy[id] = true; return true })
	for id := range f.DB.metaPrefix("dirty:") {
		if _, fr := f.frozen[id]; !fr && !busy[id] {
			f.DB.delMeta("dirty:" + id)
		}
	}
}

// dirScope is the scope of the files directly in directory d ("." is the root): the setting whose key is d or the
// longest ancestor of it. The directory itself is looked up, not a made-up file in it, which a subdirectory of
// that name (with a setting of its own) would take for its own.
func (f *Folder) dirScope(d string) (id, mode string) {
	if d == "." {
		d = ""
	}
	return f.scopeOf(d)
}

// sinceHeldBack reports whether the cursor of a Pull may not become the since of the scopes' next changes: some change
// is unsent (or may be) in a single-writer scope that has no since yet. A scope with a since keeps it, so unsent
// changes or a freeze elsewhere do not stop the others (design 2.8.6).
func (f *Folder) sinceHeldBack() bool {
	if f.unsentAll {
		return true
	}
	dirty := f.DB.metaPrefix("dirty:")
	held := false
	f.eachUnsentScope(func(id, mode string) bool {
		if mode != proto.ModeSingle {
			return true
		}
		if _, ok := dirty[id]; ok {
			return true
		}
		if _, fr := f.frozen[id]; fr { // frozen: its changes go to held, not to a since (design 2.8.6)
			return true
		}
		held = true
		return false
	})
	return held
}

// eachUnsentScope calls fn with the scope of each unsent directory and of each unsent file that has a setting of its
// own, until fn returns false.
func (f *Folder) eachUnsentScope(fn func(id, mode string) bool) {
	for d := range f.unsentDirs {
		if id, mode := f.dirScope(d); !fn(id, mode) {
			return
		}
	}
	for p := range f.unsentFiles {
		if id, mode := f.scopeOf(p); !fn(id, mode) {
			return
		}
	}
}

// coversDir reports whether the partial scan that just ran looked at the files directly in directory d.
func (f *Folder) coversDir(d string) bool {
	if f.scope == nil {
		return true
	}
	// A file name no directory can have, so that a scanned subdirectory of d (named like the stand-in) is not taken
	// for d itself.
	if d == "." {
		return f.scope("\x00")
	}
	return f.scope(d + "/\x00")
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
		}
	}
	f.thaw(false, slices.Collect(maps.Keys(scopes))...)
}

// HeldItems is the list of held changes for the server (POST /held), and whether it differs from the last one sent.
// At most proto.MaxHeldItems are returned, the rest is counted in cut: a longer list would not fit the request.
func (f *Folder) HeldItems() (items []proto.HeldItem, cut int, changed bool) {
	rows, _ := f.DB.held("")
	items = []proto.HeldItem{}
	live := make(map[string]bool, len(f.heldNote))
	for _, h := range rows {
		live[h.Path] = true
	}
	for p := range f.heldNote { // a path that is no longer held leaves no note for the day it is held again
		if !live[p] {
			delete(f.heldNote, p)
		}
	}
	for _, h := range rows {
		items = append(items, proto.HeldItem{Path: h.Path, Scope: h.Scope, Reason: h.Reason, BaseSeq: h.BaseSeq, Kind: h.Kind, Size: h.Size, MtimeNs: h.MtimeNs, At: h.At, Note: f.heldNote[h.Path]})
	}
	b, _ := json.Marshal(items)
	sum := sha256.Sum256(b)
	f.heldSig = string(sum[:])
	// Paths and notes make an item longer: keep the request well within proto.MaxJSONBody. The whole list is already
	// encoded (b, with a comma between items): only a list that may be too long is measured item by item.
	if limit := proto.MaxJSONBody * 7 / 8; len(items) > proto.MaxHeldItems || len(b) > limit {
		size := 0
		for i, it := range items {
			eb, _ := json.Marshal(it) // the encoded length, escapes included ("&" is 6 bytes)
			if size += len(eb) + 1; i >= proto.MaxHeldItems || size > limit {
				cut, items = len(items)-i, items[:i]
				break
			}
		}
	}
	return items, cut, f.heldSig != f.heldSent
}

// HeldReportFailed records that the list could not reach the server (err), or that it did (nil), so that the status
// shows it; a log line alone is not seen (design 2.8.10).
func (f *Folder) HeldReportFailed(err error) {
	if err == nil {
		f.heldErr = ""
		return
	}
	if f.heldErr == "" {
		f.heldErrAt = f.Now().Unix() // the first failure, not the latest
	}
	f.heldErr = "cannot report the held changes: " + err.Error()
}

// ShowHeldError adds the failure of the last held report to a report that was made before it.
func (f *Folder) ShowHeldError(rep *Report) {
	if f.heldErr == "" {
		return
	}
	// KindReport, not KindIO: a reminder without a push, and without the advice to exclude a file
	rep.Errors = append(slices.Clone(rep.Errors), proto.FileError{Kind: proto.KindReport, Message: f.heldErr, Since: f.heldErrAt})
	rep.State = "error"
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

// probe asks the server for its current state of each path with a deletion that cannot match any base: it changes
// nothing and answers ok where the server has no live file (a tombstone, or nothing), else a conflict with the
// current entry. At most proto.MaxCommitOps paths.
func (f *Folder) probe(ctx context.Context, paths ...string) ([]proto.Result, error) {
	ops := make([]proto.Op, len(paths))
	for i, p := range paths {
		ops[i] = proto.Op{Path: p, BaseSeq: -1, Resolve: true}
	}
	res, err := f.API.Commit(ctx, f.ID, ops)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	return res, nil
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
	if err := f.DB.dropHeld(p); err != nil {
		return err
	}
	return f.thaw(false, h.Scope)
}

// failNote records why a decision was not carried out, for the held list the Web UI shows.
func (f *Folder) failNote(p, note string, err error) error {
	if f.heldNote == nil {
		f.heldNote = map[string]string{}
	}
	f.heldNote[p] = note
	return err
}

// uploadHeld commits the held change with the resolve flag. Upload keeps the base of the change, so the server must
// not have changed the path since; overwrite takes the seq the user saw (else the server's current one) as its base.
// A change against a file or directory of the other kind first deletes what is in its way (design 2.8.7).
func (f *Folder) uploadHeld(ctx context.Context, h heldRow, st cfs.Entry, act proto.Action) error {
	it := &item{path: h.Path, del: h.Kind == "delete", decided: true, op: proto.Op{Path: h.Path, BaseSeq: h.BaseSeq, Resolve: true}}
	var pre []*item
	if act.Action == ActOverwrite {
		if act.Expect == nil { // the server always sends the version the user saw
			return errors.New("overwrite without the version the user saw")
		}
		it.op.BaseSeq = *act.Expect
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
	res, err := f.probe(ctx, h.Path)
	if err != nil {
		return err
	}
	cur := res[0].Current
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
