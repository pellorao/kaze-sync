package fs

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
)

// Test double: an in-memory FS with optional case-insensitive names, locked
// files and one-shot failure injection. Production code does not use it.

type MemOptions struct {
	CaseInsensitive bool
	NoSymlinks      bool  // Symlink fails with ErrLinkUnsupported, like Windows without Developer Mode
	Capacity        int64 // bytes the file system holds (0: no limit), checked by Reserve
	Clock           int64 // first logical mtime; give clients of one test different ranges so that mtimes never collide
}

type node struct {
	name  string
	dir   bool
	data  []byte
	mtime int64
	exec  bool
	link  bool // a symbolic link: data is the target; Open and OpenAppend refuse it (the real file system would follow it)
	kids  map[string]*node
}

type Mem struct {
	mu        sync.Mutex
	ci        bool
	noLinks   bool
	capacity  int64
	root      *node
	clock     int64 // logical time: every modification advances it
	locked    map[*node]bool
	failAfter int // calls left before the injected failure, -1 if none
	failErr   error
}

func NewMem(opts MemOptions) *Mem {
	if opts.Clock == 0 {
		opts.Clock = 1_000_000_000_000
	}
	return &Mem{
		ci:        opts.CaseInsensitive,
		noLinks:   opts.NoSymlinks,
		capacity:  opts.Capacity,
		root:      &node{dir: true, kids: map[string]*node{}},
		clock:     opts.Clock,
		locked:    map[*node]bool{},
		failAfter: -1,
	}
}

// FailAfter lets the next n calls succeed and makes the call after them fail
// with err (once). Calls are the FS methods, not Write or Close on handles.
func (m *Mem) FailAfter(n int, err error) {
	m.mu.Lock()
	m.failAfter, m.failErr = n, err
	m.mu.Unlock()
}

// Lock makes Open, OpenAppend, Rename and Remove of the existing file p fail
// with ErrLocked until Unlock, like a Windows file opened without sharing.
func (m *Mem) Lock(p string) { m.setLock(p, true) }

func (m *Mem) Unlock(p string) { m.setLock(p, false) }

func (m *Mem) setLock(p string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, _, n, err := m.find(p)
	if err != nil || n == nil {
		panic("fs.Mem: lock of missing path " + p)
	}
	if on {
		m.locked[n] = true
	} else {
		delete(m.locked, n)
	}
}

func (m *Mem) key(name string) string {
	if m.ci {
		return strings.ToLower(name)
	}
	return name
}

func pathErr(op, p string, err error) error { return &os.PathError{Op: op, Path: p, Err: err} }

// begin counts a call and returns the injected failure if it is due. Callers hold m.mu.
func (m *Mem) begin(op, p string) error {
	switch {
	case m.failAfter < 0:
	case m.failAfter == 0:
		m.failAfter = -1
		return pathErr(op, p, m.failErr)
	default:
		m.failAfter--
	}
	return nil
}

// find resolves p to its parent directory, last name and node (nil if only the last element is missing).
func (m *Mem) find(p string) (parent *node, base string, n *node, err error) {
	p = path.Clean(p)
	if p == "." || p == "/" {
		return nil, "", m.root, nil
	}
	n = m.root
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, seg := range segs {
		if !n.dir {
			return nil, "", nil, fs.ErrInvalid
		}
		parent, base = n, seg
		n = n.kids[m.key(seg)]
		if n == nil {
			if i < len(segs)-1 {
				return nil, "", nil, fs.ErrNotExist
			}
			return parent, base, nil, nil
		}
	}
	return parent, base, n, nil
}

func (m *Mem) lookup(op, p string) (*node, error) {
	if err := m.begin(op, p); err != nil {
		return nil, err
	}
	_, _, n, err := m.find(p)
	if err == nil && n == nil {
		err = fs.ErrNotExist
	}
	if err != nil {
		return nil, pathErr(op, p, err)
	}
	return n, nil
}

func (m *Mem) entry(n *node) Entry {
	return Entry{Name: n.name, IsDir: n.dir, IsSymlink: n.link, IsLink: n.link, Size: int64(len(n.data)), MtimeNs: n.mtime, Exec: n.exec}
}

func (m *Mem) tick() int64 { m.clock++; return m.clock }

func (m *Mem) ReadDir(dir string) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.lookup("readdir", dir)
	if err != nil {
		return nil, err
	}
	if !n.dir {
		return nil, pathErr("readdir", dir, fs.ErrInvalid)
	}
	out := make([]Entry, 0, len(n.kids))
	for _, k := range n.kids {
		out = append(out, m.entry(k))
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (m *Mem) Stat(p string) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.lookup("stat", p)
	if err != nil {
		return Entry{}, err
	}
	e := m.entry(n)
	e.Name = path.Base(p) // like os.Lstat
	return e, nil
}

func (m *Mem) Open(p string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.lookup("open", p)
	if err != nil {
		return nil, err
	}
	if n.dir || n.link {
		return nil, pathErr("open", p, fs.ErrInvalid)
	}
	if m.locked[n] {
		return nil, pathErr("open", p, ErrLocked)
	}
	return seekCloser{bytes.NewReader(bytes.Clone(n.data))}, nil
}

// seekCloser is a reader that can seek, like *os.File.
type seekCloser struct{ *bytes.Reader }

func (seekCloser) Close() error { return nil }

type memWriter struct {
	m *Mem
	n *node
}

func (w memWriter) Write(b []byte) (int, error) {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	w.n.data = append(w.n.data, b...)
	w.n.mtime = w.m.tick()
	return len(b), nil
}

func (w memWriter) Close() error { return nil }

func (m *Mem) OpenAppend(p string) (io.WriteCloser, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("open", p); err != nil {
		return nil, 0, err
	}
	parent, base, n, err := m.find(p)
	switch {
	case err != nil:
		return nil, 0, pathErr("open", p, err)
	case n == nil:
		if !parent.dir {
			return nil, 0, pathErr("open", p, fs.ErrInvalid)
		}
		n = &node{name: base, mtime: m.tick()}
		parent.kids[m.key(base)] = n
	case n.dir || n.link:
		return nil, 0, pathErr("open", p, fs.ErrInvalid)
	case m.locked[n]:
		return nil, 0, pathErr("open", p, ErrLocked)
	}
	return memWriter{m, n}, int64(len(n.data)), nil
}

func (m *Mem) Sync(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.lookup("sync", p)
	return err
}

func (m *Mem) Rename(oldp, newp string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("rename", oldp); err != nil {
		return err
	}
	op, ob, on, err := m.find(oldp)
	if err == nil && on == nil {
		err = fs.ErrNotExist
	}
	if err != nil {
		return pathErr("rename", oldp, err)
	}
	np, nb, nn, err := m.find(newp)
	if err == nil && np == nil {
		err = fs.ErrInvalid // renaming the root
	}
	if err != nil {
		return pathErr("rename", newp, err)
	}
	switch {
	case m.locked[on] || (nn != nil && m.locked[nn]):
		return pathErr("rename", oldp, ErrLocked)
	case nn != nil && nn != on && (nn.dir || on.dir):
		return pathErr("rename", newp, fs.ErrExist)
	}
	delete(op.kids, m.key(ob))
	on.name = nb
	np.kids[m.key(nb)] = on
	return nil
}

func (m *Mem) Remove(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("remove", p); err != nil {
		return err
	}
	parent, base, n, err := m.find(p)
	switch {
	case err != nil || n == nil || parent == nil:
		return pathErr("remove", p, fs.ErrNotExist)
	case m.locked[n]:
		return pathErr("remove", p, ErrLocked)
	case len(n.kids) > 0:
		return pathErr("remove", p, fmt.Errorf("directory not empty"))
	}
	delete(parent.kids, m.key(base))
	return nil
}

func (m *Mem) MkdirAll(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("mkdir", p); err != nil {
		return err
	}
	n := m.root
	for _, seg := range strings.Split(strings.TrimPrefix(path.Clean(p), "/"), "/") {
		if seg == "." {
			continue
		}
		k := n.kids[m.key(seg)]
		if k == nil {
			k = &node{name: seg, dir: true, kids: map[string]*node{}, mtime: m.tick()}
			n.kids[m.key(seg)] = k
		} else if !k.dir {
			return pathErr("mkdir", p, fs.ErrInvalid)
		}
		n = k
	}
	return nil
}

func (m *Mem) SetMeta(p string, mtimeNs int64, exec bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.lookup("setmeta", p)
	if err != nil {
		return err
	}
	n.mtime, n.exec = mtimeNs, exec
	return nil
}

func (m *Mem) Readlink(p string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.lookup("readlink", p)
	if err != nil {
		return "", err
	}
	if !n.link {
		return "", pathErr("readlink", p, fs.ErrInvalid)
	}
	return string(n.data), nil
}

func (m *Mem) Symlink(target, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("symlink", p); err != nil {
		return err
	}
	if m.noLinks {
		return pathErr("symlink", p, ErrLinkUnsupported)
	}
	parent, base, n, err := m.find(p)
	switch {
	case err != nil:
		return pathErr("symlink", p, err)
	case n != nil:
		return pathErr("symlink", p, fs.ErrExist)
	case !parent.dir:
		return pathErr("symlink", p, fs.ErrInvalid)
	}
	parent.kids[m.key(base)] = &node{name: base, link: true, data: []byte(target), mtime: m.tick()}
	return nil
}

// SetNoSymlinks switches the failure of Symlink on or off, as when the user turns Developer Mode on.
func (m *Mem) SetNoSymlinks(on bool) {
	m.mu.Lock()
	m.noLinks = on
	m.mu.Unlock()
}

// Reserve fails with ErrNoSpace when n more bytes would not fit in Capacity.
func (m *Mem) Reserve(p string, n int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.capacity == 0 {
		return nil
	}
	var used func(*node) int64
	used = func(n *node) (u int64) {
		u = int64(len(n.data))
		for _, k := range n.kids {
			u += used(k)
		}
		return u
	}
	if used(m.root)+n > m.capacity {
		return ErrNoSpace
	}
	return nil
}
