package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kaze-sync/e2e/vmctl"
	"kaze-sync/internal/proto"
)

// Sizes of the M4 scenarios. The plan's variant of scenario 5 uses a 1 GB file; 1 GB writes the SSD about
// 3 GB (server blocks, NTFS copy, VM redo log), so the default is smaller and the variant needs approval.
const (
	m4RestartSize = 24 << 20 // scenario 4: file uploaded while the server restarts
	m4BigSize     = 32 << 20 // scenario 5: 1 MiB rewritten inside it
	m4MoveFiles   = 1000     // scenario 6
	m4MoveSize    = 16 << 10
)

// m4 is the state of the M4 scenarios: a Linux and a Windows client with the same folders.
type m4 struct {
	d        *driver
	lin, win *agent
	linID    string
	winID    string
	ids      map[string]string // folder name -> id
	linRoot  string            // WSL path holding the Linux client's folders
	winRoot  string            // WSL path of the Windows client's folders (below /mnt/c)
	winWin   string            // the Windows spelling of winRoot
	winWork  string            // WSL path of the Windows scratch directory
	written  int64             // bytes the scenarios put on disks, for the report
}

func (m *m4) lroot(f string) string { return filepath.Join(m.linRoot, f) }
func (m *m4) wroot(f string) string { return filepath.Join(m.winRoot, f) }

// until polls every 300 ms.
func until(what string, timeout time.Duration, f func() (bool, error)) (time.Duration, error) {
	start := time.Now()
	var last error
	for {
		ok, err := f()
		if ok {
			return time.Since(start), nil
		}
		last = err
		if time.Since(start) > timeout {
			return 0, fmt.Errorf("timeout waiting for %s (last: %v)", what, last)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// tree maps the relative path of every synced file below root to the hash of its content.
func tree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		if n := e.Name(); n == ".syncroot" || strings.HasPrefix(n, ".syncpart-") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	return out, err
}

// sameTrees reports whether the folder holds the same files on both clients (and at least min of them).
func (m *m4) sameTrees(f string, min int) (bool, error) {
	a, err := tree(m.lroot(f))
	if err != nil {
		return false, err
	}
	b, err := tree(m.wroot(f))
	if err != nil {
		return false, err
	}
	if len(a) < min {
		return false, fmt.Errorf("%d files on Linux, want at least %d", len(a), min)
	}
	for p, h := range a {
		if b[p] != h {
			return false, fmt.Errorf("%s differs (linux %.8s, windows %.8s); %d vs %d files", p, h, b[p], len(a), len(b))
		}
	}
	if len(a) != len(b) {
		return false, fmt.Errorf("%d files on Linux, %d on Windows", len(a), len(b))
	}
	return true, nil
}

func (m *m4) converge(f string, min int, timeout time.Duration) (time.Duration, error) {
	return until("both clients to hold the same files of "+f, timeout, func() (bool, error) { return m.sameTrees(f, min) })
}

func readOr(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// conflictCopies lists the conflict copies below root.
func conflictCopies(root string) []string {
	t, _ := tree(root)
	var out []string
	for p := range t {
		if strings.Contains(p, ".conflict-") {
			out = append(out, p)
		}
	}
	return out
}

// guest counters
func (d *driver) txBytes() int {
	out, _ := d.vm.SSH("cat /sys/class/net/eth0/statistics/tx_bytes")
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// diskWritten is the number of bytes the guest has written to its block devices so far.
func (d *driver) diskWritten() int64 {
	out, _ := d.vm.SSH(`awk '$3 ~ /^(sd[a-z]|vd[a-z]|nvme[0-9]n[0-9])$/ {s+=$10} END {print s*512}' /proc/diskstats`)
	n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return n
}

// summary returns the overview's notification summary cells of a folder ("" when it has no notices).
func (d *driver) summary(folder string) (errs, warns string, err error) {
	_, b, err := d.web.get("/")
	if err != nil {
		return "", "", err
	}
	mm := regexp.MustCompile(`<a href="/folders/\d+">` + regexp.QuoteMeta(folder) + `</a></td><td>(\d+)</td><td>(\d+)</td>`).FindStringSubmatch(b)
	if mm == nil {
		return "", "", nil
	}
	return mm[1], mm[2], nil
}

func (d *driver) m4Scenarios(skipM4 bool) {
	m := &m4{d: d, ids: map[string]string{}}
	if !d.step("M4 setup: a Linux and a Windows client, two folders each", m.setup) {
		return
	}
	defer m.teardown()
	disk0 := d.diskWritten()
	if skipM4 {
		m.m5Scenarios()
		return
	}
	d.step("M4 1: a new file on one side appears on the other within 10 s", m.newFile)
	d.step("M4 2: both sides edit one file: the same conflict copy on both, folder error count 1, deleting it clears the notice", m.conflict)
	d.step("M4 3: stop one client, edit both sides, resume: converge per the conflict table", m.offlineEdits)
	d.step("M4 4: the server restarts during a large upload: only the remaining blocks are sent again", m.restartUpload)
	d.step("M4 5: rewriting 1 MiB inside a large file moves 4 blocks", m.bigFile)
	d.step("M4 6: moving a directory of 1000 files moves no blocks", m.moveDir)
	d.step("M4 7: a third mapping added in the Web UI is picked up within one long poll", m.thirdMapping)
	d.step("M4 8: the firewall drops the port for 1 minute: the clients reconnect and converge", m.firewall)
	d.step("M4 migration drill: two clients holding the same files join one after the other; the second downloads nothing", m.migration)
	d.step("M4 resident memory and one idle minute with two clients", m.idle)
	m.m5Scenarios()
	written := d.diskWritten() - disk0
	d.metric("M4 and M5 disk written: the VM wrote %.0f MB to its disks; the scenarios wrote about %.0f MB on the Windows disk", float64(written)/1e6, float64(m.written)/1e6)
}

func (d *driver) dropAgent(a *agent) {
	for i, x := range d.agents {
		if x == a {
			d.agents = append(d.agents[:i], d.agents[i+1:]...)
			return
		}
	}
}

func (m *m4) teardown() {
	for _, a := range []*agent{m.lin, m.win} {
		if a != nil {
			a.stop(false)
			m.d.dropAgent(a)
		}
	}
	if m.winWork != "" {
		os.RemoveAll(m.winWork)
	}
}

func (m *m4) setup() error {
	d := m.d
	m.linRoot = filepath.Join(d.tmp, "wsl4-files")
	m.lin = d.newAgent("wsl4", filepath.Join(d.tmp, "wsl4-state"))
	winWin := d.cfg.WindowsWorkDir + "-m4"
	work, err := vmctl.WSLPath(winWin)
	if err != nil {
		return err
	}
	os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	m.winWork = work
	exe := filepath.Join(work, "kaze-syncc-m4.exe")
	b, err := os.ReadFile(d.bins.windows)
	if err != nil {
		return err
	}
	if err := os.WriteFile(exe, b, 0o755); err != nil {
		return err
	}
	m.win = &agent{name: "host4", exe: exe, windows: true, logs: d.logs, state: filepath.Join(work, "state"), stateWin: winWin + `\state`}
	os.MkdirAll(m.win.state, 0o755)
	d.agents = append(d.agents, m.win)
	m.winRoot, m.winWin = filepath.Join(work, "root"), winWin+`\root`
	for _, x := range []struct {
		a    *agent
		name string
		id   *string
	}{{m.lin, "wsl4", &m.linID}, {m.win, "host4", &m.winID}} {
		code, err := d.web.addDevice(x.name)
		if err != nil {
			return err
		}
		if err := x.a.enroll(d.base, code, d.fp); err != nil {
			return err
		}
		if *x.id, err = d.web.idOf("devices", x.name); err != nil {
			return err
		}
	}
	for _, n := range []string{"m4a", "m4b"} {
		if m.ids[n], err = d.web.newFolder(n); err != nil {
			return err
		}
		os.MkdirAll(m.lroot(n), 0o755)
		os.MkdirAll(m.wroot(n), 0o755)
		if err := d.web.mapFolder(m.linID, m.ids[n], m.lroot(n), true); err != nil {
			return err
		}
		if err := d.web.mapFolder(m.winID, m.ids[n], m.winWin+`\`+n, true); err != nil {
			return err
		}
	}
	// A folder that cannot be used (non-empty directory, not adopted) must not make the long poll spin.
	if m.ids["m4x"], err = d.web.newFolder("m4x"); err != nil {
		return err
	}
	writeOld(filepath.Join(m.lroot("m4x"), "mine.txt"), []byte("not yours"))
	if err := d.web.mapFolder(m.linID, m.ids["m4x"], m.lroot("m4x"), false); err != nil {
		return err
	}
	if err := m.lin.start(); err != nil {
		return err
	}
	if err := m.win.start(); err != nil {
		return err
	}
	for _, n := range []string{"m4a", "m4b"} {
		for _, root := range []string{m.lroot(n), m.wroot(n)} {
			p := filepath.Join(root, ".syncroot")
			if _, err := until("the marker in "+root, 60*time.Second, func() (bool, error) { _, err := os.Stat(p); return err == nil, err }); err != nil {
				return err
			}
		}
	}
	return nil
}

// put writes a file whose mtime is old enough to be stable at once.
func (m *m4) put(root, rel string, data []byte) {
	writeOld(filepath.Join(root, filepath.FromSlash(rel)), data)
	if strings.HasPrefix(root, m.winRoot) {
		m.written += int64(len(data))
	}
}

func (m *m4) newFile() error {
	for _, x := range []struct{ name, from, to string }{{"lin2win.txt", m.lroot("m4a"), m.wroot("m4a")}, {"win2lin.txt", m.wroot("m4a"), m.lroot("m4a")}} {
		m.put(x.from, x.name, []byte("hello "+x.name))
		took, err := until(x.name+" on the other side", 10*time.Second, func() (bool, error) {
			return readOr(filepath.Join(x.to, x.name)) == "hello "+x.name, nil
		})
		if err != nil {
			return err
		}
		m.d.metric("M4 1: %s appeared on the other side after %.1f s", x.name, took.Seconds())
	}
	return nil
}

func (m *m4) conflict() error {
	d := m.d
	m.put(m.lroot("m4a"), "doc.txt", []byte("base"))
	if _, err := m.converge("m4a", 3, 30*time.Second); err != nil {
		return err
	}
	m.win.stop(false)
	m.put(m.lroot("m4a"), "doc.txt", []byte("edited on linux"))
	if err := d.waitFile(m.ids["m4a"], "", "doc.txt", "15 B", 60*time.Second); err != nil {
		return err
	}
	m.put(m.wroot("m4a"), "doc.txt", []byte("edited on windows!"))
	if err := m.win.start(); err != nil {
		return err
	}
	var copies []string
	if _, err := until("the conflict copy on both sides", 90*time.Second, func() (bool, error) {
		l, w := conflictCopies(m.lroot("m4a")), conflictCopies(m.wroot("m4a"))
		copies = l
		return len(l) == 1 && len(w) == 1 && l[0] == w[0], fmt.Errorf("linux %v windows %v", l, w)
	}); err != nil {
		return err
	}
	if _, err := m.converge("m4a", 4, 60*time.Second); err != nil {
		return err
	}
	if got := readOr(filepath.Join(m.lroot("m4a"), "doc.txt")); got != "edited on linux" {
		return fmt.Errorf("doc.txt = %q on Linux, the server version should win", got)
	}
	if got := readOr(filepath.Join(m.lroot("m4a"), copies[0])); got != "edited on windows!" {
		return fmt.Errorf("conflict copy = %q", got)
	}
	if _, err := until("folder error count 1 in the overview", 30*time.Second, func() (bool, error) {
		e, w, err := d.summary("m4a")
		return e == "1" && w == "0", fmt.Errorf("errors %q warnings %q %v", e, w, err)
	}); err != nil {
		return err
	}
	page, err := d.web.folderPage(m.ids["m4a"], "")
	if err != nil || !strings.Contains(page, "冲突副本") || !strings.Contains(page, `href="/folders/`+m.ids["m4a"]+`?dir="`) {
		return fmt.Errorf("folder page lacks the conflict notice or its link (%v)", err)
	}
	if err := os.Remove(filepath.Join(m.wroot("m4a"), copies[0])); err != nil {
		return err
	}
	if _, err := until("the notice to disappear", 60*time.Second, func() (bool, error) {
		e, _, err := d.summary("m4a")
		return e == "" && err == nil, fmt.Errorf("errors %q %v", e, err)
	}); err != nil {
		return err
	}
	_, err = m.converge("m4a", 3, 60*time.Second)
	return err
}

func (m *m4) offlineEdits() error {
	a, b := m.lroot("m4a"), m.wroot("m4a")
	for _, n := range []string{"both.txt", "winmod.txt", "lindel.txt", "windel.txt", "base-del.txt"} {
		m.put(a, "t/"+n, []byte("base "+n))
	}
	if _, err := m.converge("m4a", 8, 60*time.Second); err != nil {
		return err
	}
	m.lin.stop(false)
	// The Windows client edits and the server has it before the Linux client comes back (server first).
	m.put(b, "t/both.txt", []byte("windows both"))
	m.put(b, "t/winmod.txt", []byte("windows only"))
	m.put(b, "t/lindel.txt", []byte("windows beats a delete"))
	os.Remove(filepath.Join(b, "t", "windel.txt"))
	os.Remove(filepath.Join(b, "t", "base-del.txt"))
	m.put(b, "t/new-diff.txt", []byte("new on windows"))
	m.put(b, "t/new-same.txt", []byte("same everywhere"))
	if err := m.d.waitFile(m.ids["m4a"], "t", "new-same.txt", "15 B", 60*time.Second); err != nil {
		return err
	}
	m.put(a, "t/both.txt", []byte("linux both"))
	os.Remove(filepath.Join(a, "t", "lindel.txt"))
	m.put(a, "t/windel.txt", []byte("linux beats a delete"))
	m.put(a, "t/new-diff.txt", []byte("new on linux"))
	m.put(a, "t/new-same.txt", []byte("same everywhere"))
	if err := m.lin.start(); err != nil {
		return err
	}
	if _, err := m.converge("m4a", 9, 120*time.Second); err != nil {
		return err
	}
	want := map[string]string{"t/both.txt": "windows both", "t/winmod.txt": "windows only", "t/lindel.txt": "windows beats a delete",
		"t/windel.txt": "linux beats a delete", "t/new-diff.txt": "new on windows", "t/new-same.txt": "same everywhere"}
	for p, c := range want {
		for _, root := range []string{a, b} {
			if got := readOr(filepath.Join(root, p)); got != c {
				return fmt.Errorf("%s = %q in %s, want %q", p, got, root, c)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(a, "t", "base-del.txt")); err == nil {
		return fmt.Errorf("base-del.txt came back")
	}
	copies := conflictCopies(a)
	if len(copies) != 2 {
		return fmt.Errorf("conflict copies %v, want two (both.txt and new-diff.txt)", copies)
	}
	for _, c := range copies {
		got := readOr(filepath.Join(a, c))
		if got != "linux both" && got != "new on linux" {
			return fmt.Errorf("conflict copy %s = %q", c, got)
		}
	}
	// Clean up the copies so that the notices of this folder are gone for the next scenarios.
	for _, c := range copies {
		os.Remove(filepath.Join(a, c))
	}
	_, err := until("no notices left", 90*time.Second, func() (bool, error) {
		e, _, err := m.d.summary("m4a")
		ok, terr := m.sameTrees("m4a", 7)
		return e == "" && err == nil && ok, fmt.Errorf("errors %q %v %v", e, err, terr)
	})
	return err
}

func (m *m4) restartUpload() error {
	d := m.d
	blocks := m4RestartSize / proto.BlockSize
	c0, err := d.blobCount()
	if err != nil {
		return err
	}
	data := randomBytes(m4RestartSize)
	m.put(m.lroot("m4b"), "restart.bin", data)
	if _, err := d.sh("while [ $(find %s/var/blobs -type f | wc -l) -lt %d ]; do :; done", dataDir, c0+blocks/4); err != nil {
		return err
	}
	if _, err := d.sh("/etc/init.d/kaze-sync restart"); err != nil {
		return err
	}
	if err := d.waitHealthz(60 * time.Second); err != nil {
		return err
	}
	stored, _ := d.blobCount()
	rx0 := d.rxBytes()
	if stored-c0 >= blocks {
		return fmt.Errorf("the upload finished before the restart (%d of %d blocks); inconclusive", stored-c0, blocks)
	}
	if err := d.login(); err != nil {
		return err
	}
	if err := d.waitFile(m.ids["m4b"], "", "restart.bin", "24.0 MiB", 180*time.Second); err != nil {
		return err
	}
	c1, _ := d.blobCount()
	if c1-c0 != blocks {
		return fmt.Errorf("%d blocks stored, want %d", c1-c0, blocks)
	}
	rx := d.rxBytes() - rx0
	remaining := blocks - (stored - c0)
	d.metric("M4 4: restart after %d of %d blocks stored; %d more blocks stored, %d bytes received after the restart (%.1f blocks' worth, %d needed)",
		stored-c0, blocks, c1-stored, rx, float64(rx)/proto.BlockSize, remaining)
	if limit := (remaining+8)*proto.BlockSize + 1<<20; rx > limit {
		return fmt.Errorf("received %d bytes after the restart, more than the %d the remaining blocks need", rx, limit)
	}
	if _, err := m.converge("m4b", 1, 180*time.Second); err != nil {
		return err
	}
	return nil
}

func (m *m4) bigFile() error {
	d := m.d
	data := randomBytes(m4BigSize)
	m.put(m.lroot("m4b"), "big.bin", data)
	if _, err := m.converge("m4b", 2, 240*time.Second); err != nil {
		return err
	}
	time.Sleep(15 * time.Second) // let the polls and status reports settle
	copy(data[2<<20:3<<20], randomBytes(1<<20))
	tx0, rx0 := d.txBytes(), d.rxBytes()
	m.put(m.lroot("m4b"), "big.bin", data)
	took, err := until("the rewritten file on Windows", 120*time.Second, func() (bool, error) {
		return fileHash(filepath.Join(m.wroot("m4b"), "big.bin")) == sumOf(data), nil
	})
	if err != nil {
		return err
	}
	time.Sleep(10 * time.Second) // nothing more may follow
	tx, rx := d.txBytes()-tx0, d.rxBytes()-rx0
	d.metric("M4 5: 1 MiB rewritten in a %d MiB file: %d bytes downloaded by Windows (4 blocks = %d), %d bytes uploaded; done after %.0f s",
		m4BigSize>>20, tx, 4*proto.BlockSize, rx, took.Seconds())
	if lo, hi := 4*proto.BlockSize, 5*proto.BlockSize; tx < lo || tx > hi || rx < lo || rx > hi {
		return fmt.Errorf("downloaded %d, uploaded %d bytes; want between %d and %d (4 blocks)", tx, rx, lo, hi)
	}
	return nil
}

func fileHash(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return sumOf(b)
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (m *m4) moveDir() error {
	d := m.d
	rng := rand.New(rand.NewSource(6))
	for i := 0; i < m4MoveFiles; i++ {
		b := make([]byte, m4MoveSize)
		rng.Read(b)
		m.put(m.lroot("m4a"), fmt.Sprintf("proj/d%02d/f%03d.bin", i%10, i), b)
	}
	if _, err := m.converge("m4a", m4MoveFiles, 300*time.Second); err != nil {
		return err
	}
	time.Sleep(15 * time.Second)
	blobs0, _ := d.blobCount()
	tx0, rx0 := d.txBytes(), d.rxBytes()
	if err := os.Rename(filepath.Join(m.lroot("m4a"), "proj"), filepath.Join(m.lroot("m4a"), "moved")); err != nil {
		return err
	}
	took, err := until("the directory to move on Windows", 240*time.Second, func() (bool, error) {
		_, err1 := os.Stat(filepath.Join(m.wroot("m4a"), "moved", "d09", "f999.bin"))
		_, err2 := os.Stat(filepath.Join(m.wroot("m4a"), "proj"))
		return err1 == nil && err2 != nil, fmt.Errorf("%v %v", err1, err2)
	})
	if err != nil {
		return err
	}
	if _, err := m.converge("m4a", m4MoveFiles, 120*time.Second); err != nil {
		return err
	}
	time.Sleep(10 * time.Second)
	blobs1, _ := d.blobCount()
	tx, rx := d.txBytes()-tx0, d.rxBytes()-rx0
	d.metric("M4 6: moving %d files of %d KiB (%d MB of content): %d bytes downloaded, %d bytes uploaded, %d new blocks; done after %.0f s",
		m4MoveFiles, m4MoveSize>>10, m4MoveFiles*m4MoveSize>>20, tx, rx, blobs1-blobs0, took.Seconds())
	if blobs1 != blobs0 {
		return fmt.Errorf("the move stored %d new blocks", blobs1-blobs0)
	}
	// Moving the content would be 16 MB each way; the bookkeeping of 2000 changes is far below that.
	if limit := 4 << 20; tx > limit || rx > limit {
		return fmt.Errorf("the move transferred %d bytes down and %d up, over %d: blocks were sent", tx, rx, limit)
	}
	return nil
}

func (m *m4) thirdMapping() error {
	d := m.d
	id, err := d.web.newFolder("m4c")
	if err != nil {
		return err
	}
	m.ids["m4c"] = id
	if err := d.web.mapFolder(m.linID, id, m.lroot("m4c"), true); err != nil {
		return err
	}
	took, err := until("the Linux client to set up the third folder", 20*time.Second, func() (bool, error) {
		_, err := os.Stat(filepath.Join(m.lroot("m4c"), ".syncroot"))
		return err == nil, err
	})
	if err != nil {
		return err
	}
	d.metric("M4 7: the third mapping reached the client %.1f s after saving it", took.Seconds())
	_, err = until("the overview to show the configuration as up to date", 40*time.Second, func() (bool, error) {
		_, b, err := d.web.get("/")
		row := regexp.MustCompile(`(?s)<tr>\s*<td><a href="/devices/` + m.linID + `">.*?</tr>`).FindString(b)
		return strings.Contains(row, "最新") && strings.Contains(row, "m4c: "), fmt.Errorf("row %q %v", firstLine(row), err)
	})
	return err
}

func (m *m4) firewall() error {
	d := m.d
	drop := fmt.Sprintf(`nft add table inet kzdrop && nft add chain inet kzdrop in '{ type filter hook input priority -300; policy accept; }' && nft add rule inet kzdrop in tcp dport %s drop`, d.port)
	if _, err := d.sh("%s", drop); err != nil {
		return err
	}
	restored := false
	defer func() {
		if !restored {
			d.vm.SSH("nft delete table inet kzdrop")
		}
	}()
	time.Sleep(3 * time.Second)
	m.put(m.lroot("m4a"), "during-drop.txt", []byte("written while the port is closed"))
	time.Sleep(57 * time.Second)
	if readOr(filepath.Join(m.wroot("m4a"), "during-drop.txt")) == "written while the port is closed" {
		return fmt.Errorf("the file crossed a closed port")
	}
	if _, err := d.sh("nft delete table inet kzdrop"); err != nil {
		return err
	}
	restored = true
	took, err := m.converge("m4a", 5, 180*time.Second)
	if err != nil {
		return err
	}
	d.metric("M4 8: both clients converged %.0f s after the port was restored", took.Seconds())
	return nil
}

func (m *m4) migration() error {
	d := m.d
	id, err := d.web.newFolder("m4d")
	if err != nil {
		return err
	}
	m.ids["m4d"] = id
	rng := func() *rand.Rand { return rand.New(rand.NewSource(11)) }
	for _, root := range []string{m.lroot("m4d"), m.wroot("m4d")} { // the same files, written at different times
		r := rng()
		for i := 0; i < 60; i++ {
			b := make([]byte, 4096)
			r.Read(b)
			m.put(root, fmt.Sprintf("set/f%02d.bin", i), b)
		}
		big := make([]byte, 4<<20)
		r.Read(big)
		m.put(root, "set/big.bin", big)
	}
	if err := d.web.mapFolder(m.linID, id, m.lroot("m4d"), true); err != nil {
		return err
	}
	if err := d.waitFile(id, "set", "big.bin", "4.0 MiB", 120*time.Second); err != nil {
		return err
	}
	if err := d.waitFile(id, "set", "f59.bin", "4.0 KiB", 120*time.Second); err != nil {
		return err
	}
	time.Sleep(10 * time.Second)
	blobs0, _ := d.blobCount()
	tx0, rx0 := d.txBytes(), d.rxBytes()
	if err := d.web.mapFolder(m.winID, id, m.winWin+`\m4d`, true); err != nil {
		return err
	}
	if _, err := until("the Windows client to adopt the folder", 120*time.Second, func() (bool, error) {
		_, b, err := d.web.get("/")
		row := regexp.MustCompile(`(?s)<tr>\s*<td><a href="/devices/` + m.winID + `">.*?</tr>`).FindString(b)
		return regexp.MustCompile(`m4d: idle, 同步于 \d`).MatchString(row), fmt.Errorf("row %q %v", firstLine(row), err)
	}); err != nil {
		return err
	}
	time.Sleep(5 * time.Second)
	blobs1, _ := d.blobCount()
	tx, rx := d.txBytes()-tx0, d.rxBytes()-rx0
	d.metric("M4 migration: the second client adopted %d MB of identical files: %d bytes downloaded, %d bytes uploaded, %d new blocks", (4<<20+60*4096)>>20, tx, rx, blobs1-blobs0)
	if blobs1 != blobs0 {
		return fmt.Errorf("%d new blocks stored", blobs1-blobs0)
	}
	if limit := 1 << 20; tx > limit || rx > limit {
		return fmt.Errorf("adopting moved %d bytes down and %d up, over %d: blocks were transferred", tx, rx, limit)
	}
	_, err = m.converge("m4d", 61, 30*time.Second)
	return err
}

func (m *m4) idle() error {
	d := m.d
	pid, _ := d.serverPID()
	if pid == "" || m.lin.cmd == nil {
		return fmt.Errorf("server pid %q", pid)
	}
	cpid := m.lin.cmd.Process.Pid
	time.Sleep(20 * time.Second)
	srv0, _ := d.sh("cat /proc/%s/stat", pid)
	cb0, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cpid))
	rx0, tx0 := d.rxBytes(), d.txBytes()
	time.Sleep(time.Minute)
	srv1, _ := d.sh("cat /proc/%s/stat", pid)
	cb1, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cpid))
	rx1, tx1 := d.rxBytes(), d.txBytes()
	mem, err := d.sh("grep -E 'VmRSS|VmHWM' /proc/%s/status", pid)
	if err != nil {
		return err
	}
	cmem, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", cpid))
	d.metric("M4 idle minute with 2 clients and 5 folders: server CPU %d ticks, Linux client CPU %d ticks, %d bytes received and %d sent by the server",
		cpuTicks(srv1)-cpuTicks(srv0), cpuTicks(string(cb1))-cpuTicks(string(cb0)), rx1-rx0, tx1-tx0)
	d.metric("M4 server memory: %s", strings.ReplaceAll(strings.TrimSpace(mem), "\n", "; "))
	for _, l := range strings.Split(string(cmem), "\n") {
		if strings.HasPrefix(l, "VmRSS") || strings.HasPrefix(l, "VmHWM") {
			d.metric("M4 Linux client memory: %s", l)
		}
	}
	if out, err := exec.Command("tasklist.exe", "/FI", "IMAGENAME eq kaze-syncc-m4.exe", "/FO", "CSV", "/NH").Output(); err == nil {
		d.metric("M4 Windows client (tasklist): %s", strings.TrimSpace(string(out)))
	}
	if ticks := cpuTicks(srv1) - cpuTicks(srv0); ticks > 50 || (rx1-rx0)+(tx1-tx0) > 300_000 {
		return fmt.Errorf("an idle minute cost the server %d CPU ticks and %d bytes: something spins", ticks, (rx1-rx0)+(tx1-tx0))
	}
	log.Print("idle measured")
	return nil
}
