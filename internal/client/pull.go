package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"path"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/client/place"
	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// Results of applying one server entry.
type applied int

const (
	done  applied = iota // applied, or nothing to do here (excluded, echo, local changes that Scan resolves)
	hold                 // could not be applied now: keep the cursor behind it
	later                // needs another entry of the same page first
)

// Pull applies the server's changes after the saved cursor (design 5.5). Three
// passes per page: case-only renames, additions and modifications, deletions.
// The cursor stays behind the first entry that could not be applied.
func (f *Folder) Pull(ctx context.Context) (Report, error) {
	var rep Report
	f.pullErrs, f.sink, f.pendingDown, f.linkProbe = nil, &f.pullErrs, 0, 0
	defer func() { f.sink = &f.errs }()
	if !f.ready {
		f.finish(&rep)
		return rep, nil
	}
	if err := f.checkEpoch(); err != nil {
		return rep, err
	}
	cursor := f.DB.metaInt("cursor")
	since, held := cursor, int64(0)
	var err error
	for more := true; more && err == nil; {
		var page proto.ChangesResp
		if page, err = f.API.Changes(ctx, f.ID, since); err != nil {
			break
		}
		if err = f.applyPage(ctx, page.Entries, &held); err != nil {
			break
		}
		since, more = page.Cursor, page.More
		keep := since
		if held > 0 {
			keep = min(keep, held-1)
		}
		if keep > cursor {
			if err = f.DB.setMeta("cursor", keep); err != nil {
				break
			}
			cursor = keep
		}
	}
	if err == nil {
		f.seen = since
		err = f.retryUnlinked(ctx)
	}
	waiting := 0 // entries held for files in use wait for the slow schedule, not the quick retry
	for _, e := range f.pullErrs {
		if e.Kind == proto.KindInUse {
			waiting++
		}
	}
	f.retry = f.retry || f.pendingDown > waiting
	f.finish(&rep)
	return rep, err
}

func (f *Folder) excluded(p string) bool { return f.Matcher.ExcludedPath(p, false) || builtinPath(p) }

func (f *Folder) applyPage(ctx context.Context, entries []proto.Entry, held *int64) error {
	var adds, dels []proto.Entry
	for _, e := range entries {
		if e.Hash == nil {
			dels = append(dels, e)
		} else {
			adds = append(adds, e)
		}
	}
	if f.CaseFold {
		adds, dels = f.mergeRenames(adds, dels)
	}
	var again []proto.Entry
	check := func(e proto.Entry, r applied) {
		if r == hold && (*held == 0 || e.Seq < *held) {
			*held = e.Seq
		}
		if r != done {
			f.pendingDown++
		}
	}
	var q fetchQueue
	flush := func() error {
		for _, j := range f.fetchBatch(ctx, q.jobs) {
			r, err := f.finishLive(ctx, j.e, j.err)
			if err != nil {
				return err
			}
			if r == later {
				again = append(again, j.e)
			} else {
				check(j.e, r)
			}
		}
		q = fetchQueue{}
		return nil
	}
	for _, e := range adds {
		r, job, err := f.prepareLive(e)
		if err != nil {
			return err
		}
		if job == nil {
			if r == later {
				again = append(again, e)
			} else {
				check(e, r)
			}
			continue
		}
		if e.Link { // one block, but made by one call of its own, and the queue's files are written by workers
			if err := flush(); err != nil {
				return err
			}
			job.err = f.fetch(ctx, &job.e, job.src, job.expect)
			if r, err = f.finishLive(ctx, e, job.err); err != nil {
				return err
			}
			check(e, r)
			continue
		}
		if !q.fits(e) { // too big to share a request with others (or the queue is full): the queue goes first
			if err := flush(); err != nil {
				return err
			}
		}
		if !q.fits(e) {
			job.err = f.fetch(ctx, &job.e, job.src, job.expect)
			if r, err = f.finishLive(ctx, e, job.err); err != nil {
				return err
			}
			check(e, r)
			continue
		}
		q.add(job)
	}
	if err := flush(); err != nil {
		return err
	}
	for _, e := range dels {
		r, err := f.applyDeleted(e)
		if err != nil {
			return err
		}
		check(e, r)
	}
	for _, e := range again {
		r, err := f.applyLive(ctx, e)
		if err != nil {
			return err
		}
		if r == later {
			r = hold
		}
		check(e, r)
	}
	if f.changed {
		f.rejected, f.changed = nil, false // the server moved on: ask again about refused files
	}
	return nil
}

// mergeRenames turns a deletion and an addition with the same key into a
// rename of the local file, so that a case-insensitive file system never
// holds the new name on top of the old one (design 5.5, pass 1).
func (f *Folder) mergeRenames(adds, dels []proto.Entry) ([]proto.Entry, []proto.Entry) {
	gone := map[string]int{}
	for i, d := range dels {
		gone[pp.Key(d.Path)] = i
	}
	var keepAdds []proto.Entry
	drop := map[int]bool{}
	for _, a := range adds {
		i, ok := gone[pp.Key(a.Path)]
		if !ok || dels[i].Path == a.Path || drop[i] || !f.renameLocal(dels[i].Path, a) {
			keepAdds = append(keepAdds, a)
			continue
		}
		drop[i] = true
	}
	var keepDels []proto.Entry
	for i, d := range dels {
		if !drop[i] {
			keepDels = append(keepDels, d)
		}
	}
	return keepAdds, keepDels
}

func (f *Folder) renameLocal(from string, a proto.Entry) bool {
	row, ok, err := f.DB.get(from)
	if err != nil || !ok || !bytes.Equal(row.Hash, a.Hash[:]) || row.Link != a.Link || f.excluded(a.Path) {
		return false
	}
	if st, err := f.FS.Stat(from); err != nil || !consistent(row, st) {
		return false
	}
	if err := f.FS.Rename(from, a.Path); err != nil {
		return false
	}
	row.Path, row.Key, row.Seq = a.Path, pp.Key(a.Path), a.Seq
	f.changed = true
	return f.DB.apply([]localRow{row}, []string{from}) == nil
}

// applyLive applies an added or modified file.
func (f *Folder) applyLive(ctx context.Context, e proto.Entry) (applied, error) {
	r, job, err := f.prepareLive(e)
	if job == nil || err != nil {
		return r, err
	}
	return f.finishLive(ctx, e, f.fetch(ctx, &job.e, job.src, job.expect))
}

// prepareLive decides what to do with an added or modified file. It returns the outcome, or a job for the
// download when the file must be fetched.
func (f *Folder) prepareLive(e proto.Entry) (applied, *fetchJob, error) {
	f.DB.dropUnlinked(e.Path) // a link that fails again is recorded again
	if f.excluded(e.Path) {
		return done, nil, nil
	}
	row, have, err := f.DB.get(e.Path)
	if err != nil {
		return done, nil, err
	}
	if have && row.Seq == e.Seq {
		return done, nil, nil // our own change coming back
	}
	st, err := f.FS.Stat(e.Path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Usually a file where a parent directory belongs (ENOTDIR): make room like fetch does.
		renamed, pending, cerr := f.clearAncestors(e.Path)
		switch {
		case cerr != nil:
			f.note(e.Path, proto.KindIO, cerr.Error())
			return hold, nil, nil
		case pending:
			return later, nil, nil
		case !renamed:
			f.note(e.Path, proto.KindIO, err.Error())
			return hold, nil, nil
		}
	}
	if exists && st.IsDir {
		pending, err := f.clearDir(e.Path)
		switch {
		case pending:
			return later, nil, nil
		case err != nil:
			f.note(e.Path, proto.KindIO, err.Error())
			return hold, nil, nil
		}
		exists = false
	}
	switch {
	case have && !exists, !have && exists:
		return done, nil, nil // a local change: Scan commits it and resolves the conflict
	case have && !consistent(row, st):
		return done, nil, nil
	}
	if have && bytes.Equal(row.Hash, e.Hash[:]) && row.Link == e.Link {
		row.Seq = e.Seq // same content: only the base moves
		return done, nil, f.DB.apply([]localRow{row}, nil)
	}
	if exists && !st.IsLink && f.inUse(e.Path) {
		f.note(e.Path, proto.KindInUse, errInUse.Error())
		return hold, nil, nil
	}
	var src *blockSrc
	if have && !row.Link && !e.Link {
		src = &blockSrc{row.Path, proto.SplitHashes(row.Blocks), row.Size}
	}
	return done, &fetchJob{e: e, src: src, expect: place.Expect{Exists: exists, Size: st.Size, MtimeNs: st.MtimeNs}}, nil
}

// finishLive turns the result of fetching an added or modified file into the outcome for the page.
func (f *Folder) finishLive(ctx context.Context, e proto.Entry, err error) (applied, error) {
	switch {
	case err == nil:
		f.changed = true
		return done, nil
	case errors.Is(err, place.ErrTargetChanged):
		f.retry = true // changed meanwhile: a local change for Scan
		return done, nil
	case errors.Is(err, errWait):
		return later, nil
	case isRemote(err) || ctx.Err() != nil:
		return hold, err
	case errors.Is(err, cfs.ErrLocked):
		f.note(e.Path, proto.KindInUse, err.Error())
	default:
		f.note(e.Path, proto.KindIO, err.Error())
	}
	return hold, nil
}

// applyDeleted applies a deletion.
func (f *Folder) applyDeleted(e proto.Entry) (applied, error) {
	f.DB.dropUnlinked(e.Path)
	if e.Excluded || f.excluded(e.Path) { // the rules deleted it on the server: the local copy stays (design 8.3)
		return done, nil
	}
	row, have, err := f.DB.get(e.Path)
	if err != nil || !have {
		return done, err
	}
	st, err := f.FS.Stat(e.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		f.changed = true
		return done, f.DB.apply(nil, []string{e.Path}) // deleted on both sides
	case err != nil:
		f.note(e.Path, proto.KindIO, err.Error())
		return hold, nil
	case !consistent(row, st):
		return done, nil // modified locally: the modification wins (Scan)
	case !st.IsLink && f.inUse(e.Path):
		f.note(e.Path, proto.KindInUse, errInUse.Error())
		return hold, nil
	}
	if err := f.FS.Remove(e.Path); err != nil {
		kind := proto.KindIO
		if errors.Is(err, cfs.ErrLocked) {
			kind = proto.KindInUse
		}
		f.note(e.Path, kind, err.Error())
		return hold, nil
	}
	f.changed = true
	for d := path.Dir(e.Path); d != "." && d != "/"; d = path.Dir(d) {
		if f.FS.Remove(d) != nil { // not empty
			break
		}
	}
	return done, f.DB.apply(nil, []string{e.Path})
}

// inUse reports whether p is a SQLite database with a non-empty -wal or -journal (design 5.6).
func (f *Folder) inUse(p string) bool {
	busy := false
	for _, sfx := range []string{"-wal", "-journal"} {
		if st, err := f.FS.Stat(p + sfx); err == nil && st.Size > 0 {
			busy = true
		}
	}
	if !busy {
		return false
	}
	r, err := f.FS.Open(p)
	if err != nil {
		return false
	}
	defer r.Close()
	head := make([]byte, len(sqliteMagic))
	_, err = io.ReadFull(r, head)
	return err == nil && string(head) == sqliteMagic
}
