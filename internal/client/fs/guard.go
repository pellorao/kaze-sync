package fs

import (
	"io"
	"path"
	"strings"
	"sync"

	pp "kaze-sync/internal/proto/path"
)

// Guard wraps an FS whose read-only ranges the sync engine itself must still be able to write (design 2.8, 3.4):
// before an operation of the engine changes a path in a range, the guard gives the entries the operation needs
// (the file, and the directory it lives in) write permission, and takes it away again afterwards. An entry is
// restored once the last of the operations that unlocked it is done, since downloads of one directory overlap.
// Outside a range, and with no ranges, the guard passes everything through.
type Guard struct {
	FS
	mu   sync.Mutex
	keys []string         // the ranges as name keys ("" is the whole root)
	held map[string]*hold // entries unlocked now
}

type hold struct {
	n       int
	restore bool // it was read-only before the first operation unlocked it
}

// NewGuard guards the ranges, paths relative to the root.
func NewGuard(inner FS, ranges []string) *Guard {
	g := &Guard{FS: inner, held: map[string]*hold{}}
	g.SetRanges(ranges)
	return g
}

// SetRanges replaces the ranges.
func (g *Guard) SetRanges(ranges []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.keys = nil
	for _, r := range ranges {
		g.keys = append(g.keys, pp.Key(r))
	}
}

// In reports whether p is a range or below one.
func (g *Guard) In(p string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.in(p)
}

func (g *Guard) in(p string) bool {
	if len(g.keys) == 0 {
		return false
	}
	k := pp.Key(p)
	for _, r := range g.keys {
		if r == "" || k == r || strings.HasPrefix(k, r+"/") {
			return true
		}
	}
	return false
}

func parent(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// unlock makes the entries writable (those in a range that exist and are read-only) and returns the function that
// puts them back.
func (g *Guard) unlock(paths ...string) func() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.keys) == 0 {
		return func() {}
	}
	var taken []string
	for _, p := range paths {
		if !g.in(p) {
			continue
		}
		if h := g.held[p]; h != nil {
			h.n++
			taken = append(taken, p)
			continue
		}
		st, err := g.FS.Stat(p)
		if err != nil {
			continue
		}
		if st.ReadOnly {
			g.FS.SetWritable(p, true)
		}
		g.held[p] = &hold{n: 1, restore: st.ReadOnly}
		taken = append(taken, p)
	}
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		for i := len(taken) - 1; i >= 0; i-- {
			p := taken[i]
			h := g.held[p]
			if h.n--; h.n > 0 {
				continue
			}
			delete(g.held, p)
			if h.restore {
				g.FS.SetWritable(p, false) // gone: nothing to put back
			}
		}
	}
}

// protect makes p read-only if it is in a range.
func (g *Guard) protect(p string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.in(p) {
		if h := g.held[p]; h != nil {
			h.restore = true // an operation under way puts it back
		} else {
			g.FS.SetWritable(p, false)
		}
	}
}

func (g *Guard) OpenAppend(p string) (io.WriteCloser, int64, error) {
	defer g.unlock(p, parent(p))()
	return g.FS.OpenAppend(p)
}

func (g *Guard) Rename(oldp, newp string) error {
	done := g.unlock(oldp, newp, parent(oldp), parent(newp))
	err := g.FS.Rename(oldp, newp)
	if err == nil {
		g.protect(newp) // a downloaded file arrives writable
	}
	done()
	return err
}

func (g *Guard) Remove(p string) error {
	defer g.unlock(p, parent(p))()
	return g.FS.Remove(p)
}

func (g *Guard) Symlink(target, p string) error {
	defer g.unlock(parent(p))()
	return g.FS.Symlink(target, p)
}

// MkdirAll unlocks the nearest directory that exists and makes the ones it creates read-only.
func (g *Guard) MkdirAll(p string) error {
	g.mu.Lock()
	none := len(g.keys) == 0
	g.mu.Unlock()
	if none {
		return g.FS.MkdirAll(p)
	}
	var made []string
	var anchor string
	for q := p; q != ""; q = parent(q) {
		if _, err := g.FS.Stat(q); err == nil {
			anchor = q
			break
		}
		made = append(made, q)
	}
	defer g.unlock(anchor)()
	err := g.FS.MkdirAll(p)
	for _, q := range made { // deepest first
		if _, serr := g.FS.Stat(q); serr == nil {
			g.protect(q)
		}
	}
	return err
}
