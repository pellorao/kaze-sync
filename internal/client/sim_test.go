package client

// Deterministic multi-client simulation (docs/plan.md M4): three clients, each
// subscribed to two of three folders, run random operations against one real
// server in the process. Everything is in memory or on tmpfs, nothing sleeps,
// and a seed fully determines a run. Invariants at the end of a run:
//   - convergence: after enough sync rounds every client tree equals the
//     server's current state of its folders;
//   - no data loss: the content of every file a clean Scan saw on disk is
//     still held by the server (current or version), by some client (also in
//     .syncsaved), or was given up by a user's discard.
// Write control (design 2.8, 2.8.12): every folder gets random settings (free,
// single, exclusive) that change during the run, the server a fake clock that
// jumps, clients go offline, write groups and web restores happen, and the
// user's decisions on held changes are random. A log of every commit the
// server accepted is checked against the ownership rules (sim_wc_test.go).
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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
	"kaze-sync/internal/server"
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

// simTransport hands requests to the server's handler in the process; with crash set, the request is
// processed and the response lost ("after") or the request never sent ("before").
type simTransport struct {
	h   http.Handler
	c   *crasher
	s   *sim
	who string // the client, for the log of accepted commits
}

func (t simTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := call(t.c, func() (*http.Response, error) {
		var body []byte
		if strings.HasSuffix(r.URL.Path, "/commit") {
			body = bodyOf(r)
		}
		rec := httptest.NewRecorder()
		t.h.ServeHTTP(rec, r)
		if body != nil { // logged whether or not the client gets the answer
			t.s.logCommit(t.who, body, rec.Body.Bytes())
		}
		return rec.Result(), nil
	})
	return resp, err
}

type simFolder struct {
	id     int64
	mem    *cfs.Mem
	dbPath string
	f      *Folder
	synced bool // a clean Scan has run since the last crash or restart
	rules  []simRule
	cfgVer int   // the version of the rules the engine was built with
	hw     int64 // the highest cursor seen: a re-read lowers the cursor, not what the device has applied
}

type simClient struct {
	name    string
	idx     int
	devID   int64
	api     *API
	folders []*simFolder
	off     int           // steps this client stays offline
	skew    time.Duration // its clock against the simulation's
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

	skew    time.Duration // the server's clock against the simulation's
	window  int64         // write_window, seconds
	devIDs  []int64
	rules   map[int64][]simRule
	cfgVer  map[int64]int
	hist    map[int64][]acc
	maxSeq  map[int64]int64
	edit    map[string]int64 // see noteEdit
	waived  map[string]bool  // contents a user gave up by discarding them without saving
	viol    []string         // broken invariants
	steps   []string         // the last steps, for the failure report
	decided int
	frozens int
}

const (
	simFolders = 3
	simClients = 3
)

func (s *sim) srvNow() time.Time { return s.now.Add(s.skew) }

// oneWorker makes the engine single-threaded, as the crash points are numbered in the order of the calls: it is
// called once by the test that owns the simulations, never by each (parallel) simulation, whose cleanups would
// restore each other's values and leave the setting behind for the tests that run later.
func oneWorker(t *testing.T) {
	oldIO, oldHash := ioWorkers, hashWorkers
	ioWorkers, hashWorkers = 1, 1
	t.Cleanup(func() { ioWorkers, hashWorkers = oldIO, oldHash })
}

func newSim(t *testing.T, seed int64) *sim {
	s := &sim{t: t, rng: rand.New(rand.NewSource(seed)), crash: &crasher{}, now: time.Now().Truncate(time.Hour), keep: map[string]bool{}, where: map[string]string{},
		rules: map[int64][]simRule{}, cfgVer: map[int64]int{}, hist: map[int64][]acc{}, maxSeq: map[int64]int64{}, edit: map[string]int64{}, waived: map[string]bool{}}
	e := newCEnvAt(t, s.srvNow)
	s.e = e
	s.window = []int64{0, 60, 600}[s.rng.Intn(3)]
	s.setWindow(s.window)
	var fids []int64
	for i := 0; i < simFolders; i++ {
		fids = append(fids, e.newFolder("f"+strconv.Itoa(i)))
	}
	devs := make([]Device, simClients)
	for ci := 0; ci < simClients; ci++ {
		dev, api := e.newDevice()
		devs[ci] = dev
		s.devIDs = append(s.devIDs, dev.DeviceID)
		api.HC = &http.Client{Transport: simTransport{h: e.srv.Handler(), c: s.crash, s: s, who: dev.Name}}
		c := &simClient{name: dev.Name, idx: ci, devID: dev.DeviceID, api: api}
		for k := 0; k < 2; k++ { // client ci holds folders ci and ci+1 (mod 3)
			fid := fids[(ci+k)%simFolders]
			e.mapFolder(dev, fid, "/sim/"+strconv.Itoa(ci)+"/"+strconv.Itoa(k), true)
			sf := &simFolder{id: fid, dbPath: filepath.Join(t.TempDir(), "folder.db"),
				mem: cfs.NewMem(cfs.MemOptions{Clock: 1_000_000_000_000 + int64(ci*2+k)*1_000_000_000})}
			c.folders = append(c.folders, sf)
		}
		s.clients = append(s.clients, c)
	}
	for _, fid := range fids {
		s.reconfigure(fid, true)
	}
	for _, c := range s.clients {
		for _, sf := range c.folders {
			s.open(c, sf)
		}
	}
	return s
}

// setWindow stores the server's write window.
func (s *sim) setWindow(sec int64) {
	db, err := server.OpenDB(filepath.Join(s.e.data, server.VarDir, "meta.db"), false)
	if err != nil {
		s.t.Fatal(err)
	}
	defer db.Close()
	if err := server.SetMeta(db, "write_window", sec); err != nil {
		s.t.Fatal(err)
	}
}

// owners are the clients that map a folder.
func (s *sim) owners(fid int64) []int {
	var out []int
	for _, c := range s.clients {
		for _, sf := range c.folders {
			if sf.id == fid {
				out = append(out, c.idx)
			}
		}
	}
	return out
}

// reconfigure gives a folder new random settings and applies the difference on the server; the clients take them at
// their next sync, like a poll would bring them.
func (s *sim) reconfigure(fid int64, first bool) {
	old := s.rules[fid]
	rules := s.randRules(s.owners(fid))
	in := func(rs []simRule, p string) (simRule, bool) {
		for _, r := range rs {
			if r.Path == p {
				return r, true
			}
		}
		return simRule{}, false
	}
	for _, r := range old {
		if _, ok := in(rules, r.Path); !ok && r.Path != "" {
			s.e.setRule(fid, simRule{Path: r.Path, Mode: "inherit"}, s.devIDs)
		}
	}
	kept := rules[:0]
	for _, r := range rules {
		if o, ok := in(old, r.Path); !ok || o != r {
			st := s.e.setRule(fid, r, s.devIDs)
			if st == 400 && r.Mode == "free" && r.Path != "" { // the path is a file now: a file is never unrestricted (design 2.8.2)
				s.e.setRule(fid, simRule{Path: r.Path, Mode: "inherit"}, s.devIDs)
				continue
			}
			if st != 303 && st != 404 {
				s.t.Fatalf("set rule %+v of folder %d: %d", r, fid, st)
			}
		}
		kept = append(kept, r)
	}
	rules = kept
	s.rules[fid] = rules
	s.cfgVer[fid]++
	_ = first
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
	cleanupOnce(s.t, func() { db.Close() })
	sf.rules, sf.cfgVer = s.rules[sf.id], s.cfgVer[sf.id]
	f := NewFolder(simConfig(sf.id, sf.rules), crashFS{sf.mem, s.crash}, db, c.api, false)
	f.Stable, f.DeviceName = 0, c.name
	f.Now = func() time.Time { return s.now.Add(c.skew) }
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
	if sf.cfgVer != s.cfgVer[sf.id] { // the poll brought new settings: the engine starts again with them
		s.open(c, sf)
	}
	if rep, err = sf.f.Scan(ctx); err != nil {
		s.t.Fatalf("scan: %v", err)
	}
	if !rep.Retry && rep.PendingUp == 0 && rep.Held == 0 {
		s.record(sf) // nothing was postponed or held: every file on disk is on the server
	}
	if sf.f.Ready() {
		frozen, before := sf.frozenIDs(), treeOf(sf.mem)
		if rep, err = sf.f.Pull(ctx); err != nil {
			s.t.Fatalf("pull: %v", err)
		}
		if len(frozen) > 0 { // (4) a pull does not touch a scope that was frozen when it began
			after := treeOf(sf.mem)
			for p, h := range before {
				if id, _, _ := simScope(sf.rules, p); frozen[id] && after[p] != h {
					s.fail("pull changed %s of frozen scope %s on %s folder %d", p, id, c.name, sf.id)
				}
			}
			for p := range after {
				if _, ok := before[p]; !ok {
					if id, _, _ := simScope(sf.rules, p); frozen[id] {
						s.fail("pull added %s to frozen scope %s on %s folder %d", p, id, c.name, sf.id)
					}
				}
			}
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

// edit applies one random local change, or a group of them (a "git commit": several files at once), and notes the
// cursor the change began at.
func (s *sim) edit_(c *simClient, sf *simFolder) {
	for n := 1 + s.rng.Intn(2)*s.rng.Intn(5); n > 0; n-- {
		s.op(sf)
	}
}

// tick advances the clocks: the simulation's by a second, the server's and a client's now and then by a jump, the
// server's sometimes back; a client may go offline.
func (s *sim) tick() {
	s.now = s.now.Add(time.Second)
	switch x := s.rng.Intn(200); {
	case x < 4:
		s.skew += time.Duration(1+s.rng.Intn(300)) * time.Second
	case x == 4:
		s.skew -= time.Duration(1+s.rng.Intn(120)) * time.Second
	case x < 8:
		s.clients[s.rng.Intn(len(s.clients))].skew += time.Duration(s.rng.Intn(3000)-1500) * time.Second
	case x < 11:
		s.clients[s.rng.Intn(len(s.clients))].off = 3 + s.rng.Intn(60)
	}
	for _, c := range s.clients {
		c.off = max(c.off-1, 0)
	}
}

// decide takes a random user decision on a random held change of a client's folder.
func (s *sim) decide(c *simClient, sf *simFolder) {
	defer func() { // a crash in the middle of carrying a decision out: the process starts again
		if r := recover(); r != nil {
			if _, ok := r.(simCrash); !ok {
				panic(r)
			}
			s.crashes++
			s.open(c, sf)
		}
	}()
	list := sf.heldList()
	if len(list) == 0 {
		return
	}
	h := list[s.rng.Intn(len(list))]
	action, save := simActions[s.rng.Intn(len(simActions))], s.rng.Intn(3) > 0
	if h.Reason == "type" {
		action = ActDiscard // a file against a directory is decided by taking the server's
	}
	var hash string
	if r, err := sf.mem.Open(h.Path); err == nil {
		b, _ := io.ReadAll(r)
		r.Close()
		sum := sha256.Sum256(b)
		hash = hex.EncodeToString(sum[:])
	}
	rerr := sf.resolve(context.Background(), h.Path, action, save)
	if rerr != nil && os.Getenv("SIM_DEBUG") != "" {
		fmt.Printf("RESOLVE %s f%d %s %s save=%v: %v\n", c.name, sf.id, h.Path, action, save, rerr)
	}
	if rerr == nil {
		s.decided++
		if action == ActDiscard && !save && hash != "" {
			s.waived[hash] = true
		}
	}
}

// restore makes the Web UI restore a random old version, a write by "web" (design 2.8.3).
func (s *sim) restore() {
	fid := int64(1 + s.rng.Intn(simFolders))
	db := s.e.folderDB(fid)
	rows, err := db.Query(`SELECT id FROM versions ORDER BY id`)
	var ids []int64
	for err == nil && rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	if rows != nil {
		rows.Close()
	}
	db.Close()
	if len(ids) == 0 {
		return
	}
	s.e.post("/folders/"+strconv.FormatInt(fid, 10)+"/restore", url.Values{"version": {strconv.FormatInt(ids[s.rng.Intn(len(ids))], 10)}})
	s.noteWeb(fid)
}

// run does n random steps; with randomCrash, a share of the syncs crash at a random call.
func (s *sim) run(n int, randomCrash bool) {
	calls := 40
	for i := 0; i < n; i++ {
		s.tick()
		c, sf := s.pick()
		switch x := s.rng.Intn(100); {
		case x < 42:
			s.edit_(c, sf)
			s.observe(c, sf, true)
			continue
		case x < 48:
			s.decide(c, sf)
			s.observe(c, sf, false)
			continue
		case x < 50:
			s.reconfigure(sf.id, false)
			continue
		case x < 52:
			s.restore()
			continue
		}
		if c.off > 0 {
			continue
		}
		if randomCrash && s.rng.Intn(100) < 12 {
			s.crash.at, s.crash.after = s.crash.n+1+s.rng.Intn(calls), s.rng.Intn(2) == 0
		}
		before := s.crash.n
		_, crashed := s.sync(c, sf)
		s.observe(c, sf, false)
		if randomCrash {
			s.crash.at = 0
		}
		if !crashed {
			calls = max(calls, s.crash.n-before)
		}
	}
}

// pending is how many held changes the clients have.
func (s *sim) pending() int {
	n := 0
	for _, c := range s.clients {
		for _, sf := range c.folders {
			n += len(sf.heldList())
		}
	}
	return n
}

// settle brings every client online, decides all held changes and syncs until all trees equal the server, then
// checks the invariants.
func (s *sim) settle() {
	t := s.t
	s.crash.at = 0
	for _, c := range s.clients {
		c.off = 0
	}
	ok := false
	var diff string
	for round := 0; round < 60 && !(ok && s.pending() == 0); round++ {
		s.now = s.now.Add(11 * time.Minute) // every window has passed
		for _, c := range s.clients {
			for _, sf := range c.folders {
				if rep, _ := s.sync(c, sf); len(rep.Errors) > 0 && round >= 59 {
					t.Errorf("client %s folder %d: errors %+v", c.name, sf.id, rep.Errors)
				}
				for k := len(sf.heldList()); k > 0; k-- { // decide one by one: a decision can release the others
					s.decide(c, sf)
				}
			}
		}
		ok, diff = s.converged()
	}
	if len(s.viol) > 0 {
		t.Fatalf("invariants broken (decisions %d, crashes %d):\n%s", s.decided, s.crashes, strings.Join(s.viol, "\n"))
	}
	if !ok || s.pending() > 0 {
		for _, c := range s.clients {
			for _, sf := range c.folders {
				rr, _ := sf.f.DB.all()
				for p, r := range rr {
					t.Logf("ROW %s f%d %s seq=%d hash=%x", c.name, sf.id, p, r.Seq, r.Hash)
				}
				t.Logf("DBG %s f%d held=%+v frozen=%v rules=%+v meta=%v", c.name, sf.id, sf.heldList(), sf.frozenIDs(), sf.rules, sf.f.DB.metaPrefix(""))
			}
		}
		t.Fatalf("not converged after 60 rounds (held %d):\n%s", s.pending(), diff)
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
	if len(s.viol) > 0 {
		t.Fatalf("invariants broken:\n%s", strings.Join(s.viol, "\n"))
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
	conflictNames := 0
	for _, c := range s.clients {
		for _, sf := range c.folders {
			for p, h := range treeOf(sf.mem) {
				held[h] = true
				if strings.Contains(p, ".conflict-") {
					conflictNames++
				}
			}
			for _, h := range savedTree(sf.mem) { // changes set aside by a discard wait in .syncsaved
				held[h] = true
			}
		}
	}
	for h := range s.waived {
		held[h] = true
	}
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
	if conflictNames > 0 {
		t.Fatalf("%d conflict copies: write control holds the changes instead", conflictNames)
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
	return 16 // a quick random sample; SIM_SEEDS=1000 SIM_STEPS=300 is the deep run
}

// Over 100 ms (serial run): random simulation of 16 seeds; some seeds run longer than 100 ms each.
func TestSimulation(t *testing.T) {
	oneWorker(t)
	steps := 60
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
	oneWorker(t)
	scripts, steps := 1, 8 // every call of a short script; SIM_SCRIPTS=5 SIM_CRASH_STEPS=70 is the deep sweep
	if v, err := strconv.Atoi(os.Getenv("SIM_SCRIPTS")); err == nil {
		scripts = v
	}
	if v, err := strconv.Atoi(os.Getenv("SIM_CRASH_STEPS")); err == nil {
		steps = v
	}
	for seed := 1; seed <= scripts; seed++ {
		probe := newSim(t, int64(1000+seed))
		probe.run(steps, false)
		total := probe.crash.n
		for at := 1; at <= total; at++ {
			for _, after := range []bool{false, true} {
				t.Run(fmt.Sprintf("script%d/call%d/after=%v", seed, at, after), func(t *testing.T) {
					t.Parallel()
					s := newSim(t, int64(1000+seed))
					s.crash.at, s.crash.after = at, after
					s.run(steps, false)
					s.crash.at = 0
					s.settle()
				})
			}
		}
	}
}

// savedTree is the content hashes of the files in .syncsaved.
func savedTree(m cfs.FS) map[string]string {
	out := map[string]string{}
	var walk func(string)
	walk = func(dir string) {
		ents, err := m.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			p := dir + "/" + e.Name
			if e.IsDir {
				walk(p)
				continue
			}
			if r, err := m.Open(p); err == nil {
				b, _ := io.ReadAll(r)
				r.Close()
				sum := sha256.Sum256(b)
				out[p] = hex.EncodeToString(sum[:])
			}
		}
	}
	walk(savedName)
	return out
}
