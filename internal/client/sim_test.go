package client

// Deterministic multi-client simulation (docs/plan.md M4): three clients, each
// subscribed to two of three folders, run random operations against one real
// server in the process. Everything is in memory or on tmpfs, nothing sleeps,
// and a seed fully determines a run. Invariants at the end of a run:
//   - convergence: after enough sync rounds every client tree equals the
//     server's current state of its folders;
//   - no data loss: the content of every file a clean Scan saw on disk is
//     still held by the server (current or version) or by some client.
// Crashes are injected at FS and HTTP calls: randomly in every seed, and at
// every single call (before and after it) in the sweep test.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
)

// simCrash is the panic value of an injected crash.
type simCrash struct{}

// crasher counts the FS and HTTP calls of all clients and crashes at one of them.
type crasher struct {
	n, at int  // at: the call to crash at, 0 for none
	after bool // crash after the call took effect instead of before it
}

// hit is called around every call; it panics before the call or returns true to panic after it.
func (c *crasher) hit() bool {
	c.n++
	if c.at == 0 || c.n != c.at {
		return false
	}
	c.at = 0
	if !c.after {
		panic(simCrash{})
	}
	return true
}

type crashFS struct {
	in cfs.FS
	c  *crasher
}

func call[T any](c *crasher, f func() (T, error)) (T, error) {
	post := c.hit()
	v, err := f()
	if post {
		panic(simCrash{})
	}
	return v, err
}

func (w crashFS) ReadDir(d string) ([]cfs.Entry, error) {
	return call(w.c, func() ([]cfs.Entry, error) { return w.in.ReadDir(d) })
}
func (w crashFS) Stat(p string) (cfs.Entry, error) {
	return call(w.c, func() (cfs.Entry, error) { return w.in.Stat(p) })
}
func (w crashFS) Open(p string) (io.ReadCloser, error) {
	return call(w.c, func() (io.ReadCloser, error) { return w.in.Open(p) })
}
func (w crashFS) OpenAppend(p string) (io.WriteCloser, int64, error) {
	post := w.c.hit()
	wc, n, err := w.in.OpenAppend(p)
	if post {
		panic(simCrash{})
	}
	return wc, n, err
}
func (w crashFS) Reserve(p string, n int64) error { return w.in.Reserve(p, n) }
func (w crashFS) Sync(p string) error {
	_, err := call(w.c, func() (struct{}, error) { return struct{}{}, w.in.Sync(p) })
	return err
}
func (w crashFS) Rename(a, b string) error {
	_, err := call(w.c, func() (struct{}, error) { return struct{}{}, w.in.Rename(a, b) })
	return err
}
func (w crashFS) Remove(p string) error {
	_, err := call(w.c, func() (struct{}, error) { return struct{}{}, w.in.Remove(p) })
	return err
}
func (w crashFS) MkdirAll(p string) error {
	_, err := call(w.c, func() (struct{}, error) { return struct{}{}, w.in.MkdirAll(p) })
	return err
}
func (w crashFS) SetMeta(p string, m int64, x bool) error {
	_, err := call(w.c, func() (struct{}, error) { return struct{}{}, w.in.SetMeta(p, m, x) })
	return err
}
func (w crashFS) Readlink(p string) (string, error) {
	return call(w.c, func() (string, error) { return w.in.Readlink(p) })
}
func (w crashFS) Symlink(t, p string) error {
	_, err := call(w.c, func() (struct{}, error) { return struct{}{}, w.in.Symlink(t, p) })
	return err
}
func (w crashFS) SetWritable(p string, ok bool) error { return w.in.SetWritable(p, ok) }

// simTransport hands requests to the server's handler in the process; with crash set, the request is
// processed and the response lost ("after") or the request never sent ("before").
type simTransport struct {
	h http.Handler
	c *crasher
}

func (t simTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := call(t.c, func() (*http.Response, error) {
		rec := httptest.NewRecorder()
		t.h.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	return resp, err
}

type simFolder struct {
	applied string // the read-only ranges the engine was built with (exclusive writes, sim_excl_test.go)
	id      int64
	mem     *cfs.Mem
	dbPath  string
	f       *Folder
	synced  bool // a clean Scan has run since the last crash or restart
}

type simClient struct {
	name    string
	devID   int64
	api     *API
	folders []*simFolder
}

type sim struct {
	t       *testing.T
	e       *cenv
	rng     *rand.Rand
	crash   *crasher
	clients []*simClient
	now     time.Time
	keep    map[string]bool // content hashes a clean Scan saw on disk
	where   map[string]string
	crashes int

	excl  bool      // exclusive writes are part of the run
	locks []simLock // every exclusive set so far (sim_excl_test.go)
	force bool      // clients take the newest configuration before each sync
}

const (
	simFolders = 3
	simClients = 3
)

func newSim(t *testing.T, seed int64) *sim {
	oldIO, oldHash := ioWorkers, hashWorkers
	ioWorkers, hashWorkers = 1, 1 // the crash points are numbered in the order of the calls: keep it deterministic
	t.Cleanup(func() { ioWorkers, hashWorkers = oldIO, oldHash })
	e := newCEnv(t)
	s := &sim{t: t, e: e, rng: rand.New(rand.NewSource(seed)), crash: &crasher{}, now: time.Unix(1_700_000_000, 0), keep: map[string]bool{}, where: map[string]string{}}
	var fids []int64
	for i := 0; i < simFolders; i++ {
		fids = append(fids, e.newFolder("f"+strconv.Itoa(i)))
	}
	for ci := 0; ci < simClients; ci++ {
		dev, api := e.newDevice()
		api.HC = &http.Client{Transport: simTransport{h: e.srv.Handler(), c: s.crash}}
		c := &simClient{name: dev.Name, devID: dev.DeviceID, api: api}
		for k := 0; k < 2; k++ { // client ci holds folders ci and ci+1 (mod 3)
			fid := fids[(ci+k)%simFolders]
			e.mapFolder(dev, fid, "/sim/"+strconv.Itoa(ci)+"/"+strconv.Itoa(k), true)
			sf := &simFolder{id: fid, dbPath: filepath.Join(t.TempDir(), "folder.db"),
				mem: cfs.NewMem(cfs.MemOptions{Clock: 1_000_000_000_000 + int64(ci*2+k)*1_000_000_000})}
			c.folders = append(c.folders, sf)
			s.open(c, sf)
		}
		s.clients = append(s.clients, c)
	}
	return s
}

// open (re)builds a folder engine over the same file system and state file, like a process restart.
func (s *sim) open(c *simClient, sf *simFolder) {
	if sf.f != nil {
		sf.f.DB.Close()
	}
	db, err := openLocal(sf.dbPath)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { db.Close() })
	ro := s.readonly(c, sf)
	sf.applied = strings.Join(ro, "\n")
	f := NewFolder(proto.FolderConfig{ID: sf.id, Adopt: true, Readonly: ro}, crashFS{sf.mem, s.crash}, db, c.api, false)
	f.Stable, f.DeviceName = 0, c.name
	f.Now = func() time.Time { return s.now }
	sf.f, sf.synced = f, false
}

// sync is one agent cycle of a folder: Scan, then Pull; a crash restarts the engine.
func (s *sim) sync(c *simClient, sf *simFolder) (rep Report, crashed bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(simCrash); !ok {
				panic(r)
			}
			s.crashes++
			rep, crashed = Report{}, true
			s.open(c, sf)
		}
	}()
	ctx := context.Background()
	var err error
	if s.excl && sf.applied != strings.Join(s.readonly(c, sf), "\n") && (s.force || s.rng.Intn(100) < 70) {
		s.open(c, sf) // the configuration with the new exclusives arrived: the engine restarts with it
	}
	if rep, err = sf.f.Scan(ctx); err != nil {
		s.t.Fatalf("scan: %v", err)
	}
	if !rep.Retry && rep.PendingUp == 0 {
		s.record(sf) // nothing was postponed: every file on disk is on the server
	}
	if sf.f.Ready() {
		if rep, err = sf.f.Pull(ctx); err != nil {
			s.t.Fatalf("pull: %v", err)
		}
	}
	return rep, false
}

// record notes the content of every file on disk right after a Scan that left nothing undone.
func (s *sim) record(sf *simFolder) {
	if !sf.f.Ready() {
		return
	}
	for p, h := range treeOf(sf.mem) {
		if !s.keep[h] {
			s.where[h] = fmt.Sprintf("folder %d path %s at %s (crashes so far %d)", sf.id, p, s.now.Format("15:04:05"), s.crashes)
		}
		s.keep[h] = true
	}
}

func treeOf(m cfs.FS) map[string]string {
	out := map[string]string{}
	var walk func(dir string)
	walk = func(dir string) {
		ents, err := m.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			p := e.Name
			if dir != "" {
				p = dir + "/" + e.Name
			}
			if builtinPath(p) {
				continue
			}
			if e.IsDir {
				walk(p)
				continue
			}
			r, err := m.Open(p)
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(r)
			r.Close()
			sum := sha256.Sum256(b)
			out[p] = hex.EncodeToString(sum[:])
		}
	}
	walk("")
	return out
}

var simPaths = []string{"a.txt", "b.txt", "c.txt", "d/e.txt", "d/f.txt", "g/h/i.txt", "j/k.txt"}

func (s *sim) content() []byte {
	if s.rng.Intn(25) == 0 { // a file of three blocks, mostly shared between versions
		b := pseudoRandom(2*proto.BlockSize+50, 7)
		copy(b[10:], fmt.Sprintf("v%d", s.rng.Intn(3)))
		return b
	}
	return []byte(fmt.Sprintf("content %d", s.rng.Intn(8)))
}

// op applies one random local change (never a sync) to a folder of a client.
func (s *sim) op(sf *simFolder) {
	m, rng := sf.mem, s.rng
	put := func(p string, data []byte) {
		if i := strings.LastIndexByte(p, '/'); i > 0 {
			if m.MkdirAll(p[:i]) != nil {
				return // a file is in the way
			}
		}
		m.Remove(p)
		w, _, err := m.OpenAppend(p)
		if err != nil {
			return
		}
		w.Write(data)
		w.Close()
	}
	files := func() []string {
		var out []string
		for p := range treeOf(m) {
			out = append(out, p)
		}
		sort.Strings(out)
		return out
	}
	if s.excl && len(sf.f.Readonly) > 0 && rng.Intn(4) == 0 {
		sf.f.Unprotect() // the person lifts the read-only flags by hand
	}
	switch k := rng.Intn(100); {
	case k < 45:
		put(simPaths[rng.Intn(len(simPaths))], s.content())
	case k < 65:
		if fs := files(); len(fs) > 0 {
			m.Remove(fs[rng.Intn(len(fs))])
		}
	case k < 80:
		if fs := files(); len(fs) > 0 {
			dst := simPaths[rng.Intn(len(simPaths))]
			if i := strings.LastIndexByte(dst, '/'); i > 0 && m.MkdirAll(dst[:i]) != nil {
				return
			}
			m.Rename(fs[rng.Intn(len(fs))], dst)
		}
	case k < 87: // move a directory
		pairs := [][2]string{{"d", "dd"}, {"dd", "d"}, {"g", "gg"}, {"gg", "g"}, {"j", "d"}}
		pr := pairs[rng.Intn(len(pairs))]
		m.Rename(pr[0], pr[1])
	case k < 92: // a file where a directory is
		for _, d := range []string{"d", "g", "j"} {
			if rng.Intn(2) == 0 {
				continue
			}
			for p := range treeOf(m) {
				if strings.HasPrefix(p, d+"/") {
					m.Remove(p)
				}
			}
			for _, sub := range []string{"h", ""} {
				m.Remove(d + "/" + sub)
			}
			m.Remove(d)
			put(d, s.content())
			break
		}
	case k < 96: // a directory where a file is
		put("c.txt/x", s.content())
	default: // touch
		if fs := files(); len(fs) > 0 {
			m.SetMeta(fs[rng.Intn(len(fs))], 1_900_000_000_000+int64(rng.Intn(1000)), false)
		}
	}
}

func (s *sim) pick() (*simClient, *simFolder) {
	c := s.clients[s.rng.Intn(len(s.clients))]
	return c, c.folders[s.rng.Intn(len(c.folders))]
}

// run does n random steps; with randomCrash, a share of the syncs crash at a random call.
func (s *sim) run(n int, randomCrash bool) {
	calls := 40
	for i := 0; i < n; i++ {
		s.now = s.now.Add(time.Second)
		c, sf := s.pick()
		if s.excl && s.rng.Intn(100) < 4 {
			s.lockStep()
			continue
		}
		if s.rng.Intn(100) < 60 {
			s.op(sf)
			continue
		}
		if randomCrash && s.rng.Intn(100) < 12 {
			s.crash.at, s.crash.after = s.crash.n+1+s.rng.Intn(calls), s.rng.Intn(2) == 0
		}
		before := s.crash.n
		_, crashed := s.sync(c, sf)
		if randomCrash {
			s.crash.at = 0
		}
		if !crashed {
			calls = max(calls, s.crash.n-before)
		}
	}
}

// settle syncs everything until all trees equal the server, and checks both invariants.
func (s *sim) settle() {
	t := s.t
	s.crash.at = 0
	s.force = true
	ok := false
	var diff string
	for round := 0; round < 40 && !ok; round++ {
		s.now = s.now.Add(time.Second)
		for _, c := range s.clients {
			for _, sf := range c.folders {
				if rep, _ := s.sync(c, sf); len(rep.Errors) > 0 && round >= 39 {
					t.Errorf("client %s folder %d: errors %+v", c.name, sf.id, rep.Errors)
				}
			}
		}
		ok, diff = s.converged()
	}
	if !ok {
		t.Fatalf("not converged after 40 rounds:\n%s", diff)
	}
	for _, c := range s.clients { // one more round must change nothing
		for _, sf := range c.folders {
			before := treeOf(sf.mem)
			s.sync(c, sf)
			if after := treeOf(sf.mem); fmt.Sprint(before) != fmt.Sprint(after) {
				t.Fatalf("tree of %s folder %d still changing after convergence", c.name, sf.id)
			}
		}
	}
	// No data loss.
	held := map[string]bool{}
	for fid := int64(1); fid <= simFolders; fid++ {
		db := s.e.folderDB(fid)
		for _, q := range []string{`SELECT hash FROM files WHERE hash IS NOT NULL`, `SELECT hash FROM versions WHERE hash IS NOT NULL`} {
			rows, err := db.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var h []byte
				rows.Scan(&h)
				held[hex.EncodeToString(h)] = true
			}
			rows.Close()
		}
		db.Close()
	}
	for _, c := range s.clients {
		for _, sf := range c.folders {
			for _, h := range treeOf(sf.mem) {
				held[h] = true
			}
			for _, h := range savedTree(sf.mem) { // refused changes wait in .syncsaved
				held[h] = true
			}
		}
	}
	s.checkWriters()
	lost := 0
	for h := range s.keep {
		if !held[h] {
			lost++
			t.Logf("lost %.8s first seen: %s", h, s.where[h])
		}
	}
	if lost > 0 {
		t.Fatalf("%d of %d file contents that were on disk at a clean scan are lost", lost, len(s.keep))
	}
}

// converged compares every client tree with the server's current files of that folder.
func (s *sim) converged() (bool, string) {
	var diff strings.Builder
	for _, c := range s.clients {
		for _, sf := range c.folders {
			db := s.e.folderDB(sf.id)
			want := map[string]string{}
			rows, err := db.Query(`SELECT path, hash FROM files WHERE hash IS NOT NULL`)
			if err != nil {
				s.t.Fatal(err)
			}
			for rows.Next() {
				var p string
				var h []byte
				rows.Scan(&p, &h)
				want[p] = hex.EncodeToString(h)
			}
			rows.Close()
			db.Close()
			got := treeOf(sf.mem)
			for p, h := range want {
				if got[p] != h {
					fmt.Fprintf(&diff, "%s folder %d: %s server %.8s, client %.8s\n", c.name, sf.id, p, h, got[p])
				}
			}
			for p, h := range got {
				if _, ok := want[p]; !ok {
					fmt.Fprintf(&diff, "%s folder %d: %s only on the client (%.8s)\n", c.name, sf.id, p, h)
				}
			}
		}
	}
	return diff.Len() == 0, diff.String()
}

func simSeeds() int {
	if v, err := strconv.Atoi(os.Getenv("SIM_SEEDS")); err == nil {
		return v
	}
	if testing.Short() {
		return 40
	}
	return 1000
}

func TestSimulation(t *testing.T) {
	steps := 300
	if v, err := strconv.Atoi(os.Getenv("SIM_STEPS")); err == nil {
		steps = v
	}
	for seed := 1; seed <= simSeeds(); seed++ {
		t.Run(strconv.Itoa(seed), func(t *testing.T) {
			t.Parallel()
			s := newSim(t, int64(seed))
			s.run(steps, true)
			s.settle()
		})
	}
}

// TestCrashAtEveryCall replays short scripts and crashes once at each FS or HTTP call of the whole run, before
// and after the call took effect; every run must still converge without data loss.
func TestCrashAtEveryCall(t *testing.T) {
	scripts := 5
	if v, err := strconv.Atoi(os.Getenv("SIM_SCRIPTS")); err == nil {
		scripts = v
	} else if testing.Short() {
		scripts = 1
	}
	for seed := 1; seed <= scripts; seed++ {
		probe := newSim(t, int64(1000+seed))
		probe.run(70, false)
		total := probe.crash.n
		for at := 1; at <= total; at++ {
			for _, after := range []bool{false, true} {
				t.Run(fmt.Sprintf("script%d/call%d/after=%v", seed, at, after), func(t *testing.T) {
					t.Parallel()
					s := newSim(t, int64(1000+seed))
					s.crash.at, s.crash.after = at, after
					s.run(70, false)
					s.crash.at = 0
					s.settle()
				})
			}
		}
	}
}
