package client

// The simulation with exclusive writes (design 7): exclusives are set, moved and released at random, clients get
// the new configuration at random times, people lift the read-only flags by hand. On top of the invariants of
// sim_test.go: refused changes are not lost (they wait in .syncsaved), and while an exclusive stands, only its device
// writes new versions below it.

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	cfs "kaze-sync/internal/client/fs"
)

// simLock is an exclusive: the folder's path is written by client owner from seq from until seq to (0: still).
type simLock struct {
	folder   int64
	path     string
	owner    int
	from, to int64
}

var simLockPaths = []string{"d", "g", "g/h", "j", "d/e.txt"}

// readonly is what the server's configuration lists for a client: the exclusives of the other clients.
func (s *sim) readonly(c *simClient, sf *simFolder) []string {
	var out []string
	for _, l := range s.locks {
		if l.to == 0 && l.folder == sf.id && s.clients[l.owner] != c {
			out = append(out, l.path)
		}
	}
	return out
}

func (s *sim) nextSeq(folder int64) int64 {
	db := s.e.folderDB(folder)
	defer db.Close()
	var n int64
	db.QueryRow(`SELECT value FROM meta WHERE key='next_seq'`).Scan(&n)
	return n
}

// lockStep sets, moves or releases an exclusive of a random folder through the Web UI.
func (s *sim) lockStep() {
	c := s.clients[s.rng.Intn(len(s.clients))]
	sf := c.folders[s.rng.Intn(len(c.folders))]
	p := simLockPaths[s.rng.Intn(len(simLockPaths))]
	owner := s.clients[s.rng.Intn(len(s.clients))]
	form := url.Values{"path": {p}}
	release := s.rng.Intn(3) == 0
	if !release {
		form.Set("device", strconv.FormatInt(owner.devID, 10))
	}
	seq := s.nextSeq(sf.id)
	if st, _ := s.e.post("/folders/"+strconv.FormatInt(sf.id, 10)+"/lock", form); st != 303 {
		return // overlap or the device does not map the folder
	}
	for i := range s.locks {
		if l := &s.locks[i]; l.to == 0 && l.folder == sf.id && l.path == p {
			if release || s.clients[l.owner] != owner {
				l.to = seq
			}
			if !release && s.clients[l.owner] == owner {
				return // the same device again
			}
		}
	}
	if !release {
		for i, cl := range s.clients {
			if cl == owner {
				s.locks = append(s.locks, simLock{folder: sf.id, path: p, owner: i, from: seq})
			}
		}
	}
}

// checkWriters: below a standing exclusive only its device (or the Web UI) wrote new versions.
func (s *sim) checkWriters() {
	for fid := int64(1); fid <= simFolders; fid++ {
		db := s.e.folderDB(fid)
		for _, q := range []string{`SELECT path, seq, device FROM files`, `SELECT path, seq, device FROM versions`} {
			rows, err := db.Query(q)
			if err != nil {
				s.t.Fatal(err)
			}
			for rows.Next() {
				var p, dev string
				var seq int64
				rows.Scan(&p, &seq, &dev)
				for _, l := range s.locks {
					if l.folder != fid || seq < l.from || l.to != 0 && seq >= l.to || !(p == l.path || strings.HasPrefix(p, l.path+"/")) {
						continue
					}
					if dev != s.clients[l.owner].name && dev != "web" {
						s.t.Errorf("folder %d: %s (seq %d) written by %s under the exclusive of %s", fid, p, seq, dev, s.clients[l.owner].name)
					}
				}
			}
			rows.Close()
		}
		db.Close()
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

func TestSimulationExclusive(t *testing.T) {
	steps := 300
	if v, err := strconv.Atoi(os.Getenv("SIM_STEPS")); err == nil {
		steps = v
	}
	n := simSeeds() / 4
	for seed := 1; seed <= n; seed++ {
		t.Run(strconv.Itoa(seed), func(t *testing.T) {
			t.Parallel()
			s := newSim(t, int64(5000+seed))
			s.excl = true
			s.run(steps, true)
			s.settle()
			saved := 0
			for _, c := range s.clients {
				for _, sf := range c.folders {
					saved += len(savedTree(sf.mem))
				}
			}
			t.Logf("locks %d, files in .syncsaved %d", len(s.locks), saved)
		})
	}
}
