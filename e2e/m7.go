package main

import (
	"crypto/sha256"
	"fmt"
	"html"
	"io/fs"
	"log"
	"math/rand"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kaze-sync/internal/proto"
)

// M7 scenarios (docs/plan.md): retention, GC, directory restore, export, backup and upgrade.
const (
	m7Files     = 1000    // the directory that is deleted and restored
	m7Size      = 4 << 10 // small files: the scenario is about bookkeeping, not volume
	m7Big       = 600 << 10
	upgradeFrom = "2ec5ecf" // the last revision before M4 (folder schema 1); its server and client are an old pair
	jump        = 48 * time.Hour
)

// fileInfo is what a tree comparison checks: content, executable bit, modification time in seconds.
type fileInfo struct {
	hash  string
	exec  bool
	mtime int64
}

func treeInfo(root string) (map[string]fileInfo, error) {
	out := map[string]fileInfo{}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		if n := e.Name(); n == ".syncroot" || strings.HasPrefix(n, ".syncpart-") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = fileInfo{sumOf(b), fi.Mode().Perm()&0o100 != 0, fi.ModTime().Unix()}
		return nil
	})
	return out, err
}

// contentOnly drops the executable bit and the time, which differ between the platforms.
func contentOnly(t map[string]fileInfo) map[string]fileInfo {
	out := map[string]fileInfo{}
	for p, f := range t {
		out[p] = fileInfo{hash: f.hash}
	}
	return out
}

// diffInfo describes the first differences of two trees ("" when identical).
func diffInfo(got, want map[string]fileInfo) string {
	var diffs []string
	for p, w := range want {
		if g, ok := got[p]; !ok {
			diffs = append(diffs, "missing "+p)
		} else if g != w {
			diffs = append(diffs, fmt.Sprintf("%s: %+v, want %+v", p, g, w))
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			diffs = append(diffs, "extra "+p)
		}
	}
	if len(diffs) > 5 {
		diffs = append(diffs[:5], fmt.Sprintf("... %d differences", len(diffs)))
	}
	return strings.Join(diffs, "; ")
}

func (d *driver) vmNow() (time.Time, error) {
	out, err := d.sh("date -u +%%s")
	n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return time.Unix(n, 0).UTC(), err
}

func (d *driver) setVMClock(t time.Time) error {
	_, err := d.sh("date -u -s '%s'", t.UTC().Format("2006-01-02 15:04:05"))
	return err
}

// exportDir runs "kaze-syncd export" in the VM with args and returns a new local directory holding the result.
func (d *driver) exportDir(args string) (string, error) {
	if _, err := d.sh("rm -rf /tmp/ex && %s export /tmp/ex --data %s %s", remoteBin, dataDir, args); err != nil {
		return "", err
	}
	local, err := os.MkdirTemp(d.tmp, "export-")
	if err != nil {
		return "", err
	}
	if err := d.vm.TarGet("/tmp/ex", local); err != nil {
		os.RemoveAll(local)
		return "", err
	}
	return local, nil
}

// exportTree is exportDir followed by treeInfo of the subdirectory sub.
func (d *driver) exportTree(sub, args string) (map[string]fileInfo, error) {
	local, err := d.exportDir(args)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(local)
	return treeInfo(filepath.Join(local, sub))
}

func (m *m4) putExec(root, rel string, data []byte) {
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, data, 0o755)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(p, old, old)
}

// restoreDir: one side deletes a directory of 1000 files by mistake; the Web UI restores it to the time
// before; both sides hold every file again and a file created after the deletion is untouched.
func (m *m4) restoreDir() error {
	d, id := m.d, m.ids["m7a"]
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < m7Files; i++ {
		b := make([]byte, m7Size)
		rng.Read(b)
		m.put(m.lroot("m7a"), fmt.Sprintf("proj/d%02d/f%04d.bin", i%20, i), b)
	}
	m.put(m.lroot("m7a"), "keep.txt", []byte("outside the directory"))
	if _, err := m.converge("m7a", m7Files+1, 300*time.Second); err != nil {
		return err
	}
	orig, err := treeInfo(m.lroot("m7a"))
	if err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	at, err := d.vmNow()
	if err != nil {
		return err
	}
	time.Sleep(3 * time.Second) // the deletion must fall into a later second than the time asked for
	if err := os.RemoveAll(filepath.Join(m.lroot("m7a"), "proj")); err != nil {
		return err
	}
	if _, err := until("the deletion on Windows", 300*time.Second, func() (bool, error) { return m.sameTrees("m7a", 1) }); err != nil {
		return err
	}
	if t, _ := tree(m.wroot("m7a")); len(t) != 1 {
		return fmt.Errorf("Windows still holds %d files", len(t))
	}
	m.put(m.wroot("m7a"), "later.txt", []byte("created after the deletion"))
	if _, err := m.converge("m7a", 2, 60*time.Second); err != nil {
		return err
	}
	time.Sleep(5 * time.Second)
	blobs0, _ := d.blobCount()
	tx0, rx0 := d.txBytes(), d.rxBytes()
	start := time.Now()
	res, err := d.web.restoreDir(id, "proj", at.Format("2006-01-02 15:04:05"))
	if err != nil {
		return err
	}
	if res != fmt.Sprintf("restored=%d&failed=0", m7Files) {
		return fmt.Errorf("restore result %q", res)
	}
	orig["later.txt"] = fileInfo{hash: sumOf([]byte("created after the deletion"))}
	if _, err := m.converge("m7a", m7Files+2, 300*time.Second); err != nil {
		return err
	}
	took := time.Since(start)
	want := contentOnly(orig)
	for _, root := range []string{m.lroot("m7a"), m.wroot("m7a")} {
		got, err := treeInfo(root)
		if err != nil {
			return err
		}
		if s := diffInfo(contentOnly(got), want); s != "" {
			return fmt.Errorf("%s after the restore: %s", root, s)
		}
	}
	time.Sleep(5 * time.Second)
	blobs1, _ := d.blobCount()
	d.metric("M7 restore dir: %d files back on both clients %.0f s after the click; the VM sent %d and received %d bytes; %d new blocks (the restored content is still stored)",
		m7Files, took.Seconds(), d.txBytes()-tx0, d.rxBytes()-rx0, blobs1-blobs0)
	return nil
}

// exportAt: export --at gives the tree the clients held at that time, byte for byte (content, executable
// bit and modification time); without --at it gives the current tree.
func (m *m4) exportAt() error {
	d := m.d
	root := m.lroot("m7a")
	time.Sleep(3 * time.Second)
	before, err := treeInfo(root)
	if err != nil {
		return err
	}
	at, err := d.vmNow()
	if err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	rng := rand.New(rand.NewSource(8))
	for i := 0; i < 5; i++ {
		m.put(root, fmt.Sprintf("proj/d00/f%04d.bin", i*20), randomBytesFrom(rng, m7Size+i)) // overwritten
		os.Remove(filepath.Join(root, fmt.Sprintf("proj/d01/f%04d.bin", i*20+1)))            // deleted
	}
	m.put(root, "new/multi-block.bin", randomBytesFrom(rng, m7Big))
	m.put(root, "new/empty", nil)
	m.put(root, "new/é-unicode.txt", []byte("unicode name"))
	m.putExec(root, "new/run.sh", []byte("#!/bin/sh\necho hi\n"))
	if _, err := m.converge("m7a", 1, 120*time.Second); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	now, err := treeInfo(root)
	if err != nil {
		return err
	}
	if diffInfo(now, before) == "" {
		return fmt.Errorf("the changes did not take effect")
	}
	// --folder writes into the directory itself, without a subdirectory.
	got, err := d.exportTree("", fmt.Sprintf("--folder m7a --at %q", at.Format("2006-01-02 15:04:05")))
	if err != nil {
		return err
	}
	if s := diffInfo(got, before); s != "" {
		return fmt.Errorf("export --at differs from the tree at that time: %s", s)
	}
	if got, err = d.exportTree("", "--folder m7a"); err != nil {
		return err
	}
	if s := diffInfo(got, now); s != "" {
		return fmt.Errorf("export differs from the current tree: %s", s)
	}
	d.metric("M7 export: --at %s reproduced %d files byte for byte (content, executable bit, mtime); the current tree %d files", at.Format("15:04:05"), len(before), len(now))
	return nil
}

func randomBytesFrom(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

type folderLink struct{ id, name string }

// listLinks returns the distinct (id, name) pairs of the overview's links of a kind.
func (d *driver) listLinks(kind string) ([]folderLink, error) {
	st, b, err := d.web.get("/")
	if err != nil {
		return nil, err
	}
	if st != 200 {
		return nil, fmt.Errorf("overview: %d %s", st, firstLine(b))
	}
	var out []folderLink
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`<a\b[^>]*\shref="/`+regexp.QuoteMeta(kind)+`/(\d+)"[^>]*>([^<]*)</a>`).FindAllStringSubmatch(b, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, folderLink{m[1], html.UnescapeString(m[2])})
		}
	}
	return out, nil
}

// distinctBlocks counts the distinct blocks of the files below the roots.
func distinctBlocks(roots ...string) (int, error) {
	set := map[[32]byte]bool{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || e.Name() == ".syncroot" || strings.HasPrefix(e.Name(), ".syncpart-") {
				return err
			}
			b, err := os.ReadFile(p)
			for off := 0; off < len(b); off += proto.BlockSize {
				set[sha256.Sum256(b[off:min(off+proto.BlockSize, len(b))])] = true
			}
			return err
		})
		if err != nil {
			return 0, err
		}
	}
	return len(set), nil
}

// retention: the other folders are deleted in the Web UI (only unmapped ones can be); then, two days later
// in the VM's clock, saving keep_days = 1 clears every version at once and then every block that no current
// file needs, with the service running all the time.
func (m *m4) retention() error {
	d := m.d
	folders, err := d.listLinks("folders")
	if err != nil {
		return err
	}
	devices, err := d.listLinks("devices")
	if err != nil {
		return err
	}
	// Versions to expire: a three-block file written twice.
	rng := rand.New(rand.NewSource(9))
	m.put(m.lroot("m7a"), "gc.bin", randomBytesFrom(rng, m7Big))
	if _, err := m.converge("m7a", 1, 120*time.Second); err != nil {
		return err
	}
	m.put(m.lroot("m7a"), "gc.bin", randomBytesFrom(rng, m7Big))
	if _, err := m.converge("m7a", 1, 120*time.Second); err != nil {
		return err
	}
	if err := d.waitFile(m.ids["m7a"], "", "gc.bin", "600.0 KiB", 60*time.Second); err != nil {
		return err
	}
	page, _ := d.web.folderPage(m.ids["m7a"], "file=gc.bin")
	if n := strings.Count(page, `name="version"`); n != 1 {
		return fmt.Errorf("gc.bin has %d restorable versions before the GC, want 1", n)
	}
	// A mapped folder cannot be deleted.
	if st, _, err := d.web.post("/folders/"+m.ids["m7a"]+"/delete", url.Values{}); err != nil || st != 409 {
		return fmt.Errorf("deleting a mapped folder: status %d %v", st, err)
	}
	keep, hasDocs := "1", false // docs exists only in a full run
	for _, f := range folders {
		if f.name == "docs" {
			hasDocs, keep = true, "2"
		}
		if f.id == m.ids["m7a"] || f.name == "docs" { // the sysupgrade step still uses docs
			continue
		}
		for _, dev := range devices {
			if _, err := d.web.ok("/devices/"+dev.id+"/mapping", url.Values{"folder": {f.id}, "action": {"delete"}}); err != nil {
				return err
			}
		}
		if _, err := d.web.ok("/folders/"+f.id+"/delete", url.Values{}); err != nil {
			return err
		}
		if st, _, _ := d.web.get("/folders/" + f.id); st != 404 {
			return fmt.Errorf("folder %s still has a page: %d", f.name, st)
		}
	}
	if out, err := d.sh("ls %s/var/folders | grep -c '\\.db$'", dataDir); err != nil || strings.TrimSpace(out) != keep {
		return fmt.Errorf("folder databases left: %q %v", out, err)
	}
	blobs0, _ := d.blobCount()
	roots := []string{m.lroot("m7a")}
	if hasDocs {
		docs, err := d.exportDir("--folder docs") // the server's state of docs, whatever the client left out
		if err != nil {
			return err
		}
		defer os.RemoveAll(docs)
		roots = append(roots, docs)
	}
	want, err := distinctBlocks(roots...)
	if err != nil {
		return err
	}
	pid, _ := d.serverPID()
	vm0, err := d.vmNow()
	if err != nil {
		return err
	}
	host0 := time.Now()
	if err := d.setVMClock(vm0.Add(jump)); err != nil {
		return err
	}
	defer func() { d.setVMClock(vm0.Add(time.Since(host0))) }()
	if _, err := d.web.ok("/settings", url.Values{"keep_days": {"1"}}); err != nil {
		return err
	}
	took, err := until("the blocks to be collected", 120*time.Second, func() (bool, error) {
		n, _ := d.blobCount()
		return n == want, fmt.Errorf("%d blocks stored, %d wanted", n, want)
	})
	if err != nil {
		return err
	}
	page, _ = d.web.folderPage(m.ids["m7a"], "file=gc.bin")
	if n := strings.Count(page, `name="version"`); n != 0 {
		return fmt.Errorf("gc.bin still has %d versions", n)
	}
	if p, _ := d.serverPID(); p != pid {
		return fmt.Errorf("the service restarted (pid %s, was %s)", p, pid)
	}
	out, _ := d.sh("logread | grep 'msg=gc ' | tail -1; grep -E 'VmHWM|VmRSS' /proc/%s/status", pid)
	d.metric("M7 retention: %d blocks before, %d after (%d distinct blocks in the current files), collected %.1f s after saving keep_days; server log and memory: %s",
		blobs0, want, want, took.Seconds(), strings.ReplaceAll(out, "\n", "; "))
	// What GC left is exactly what the current files need: export proves it, byte for byte.
	now, err := treeInfo(m.lroot("m7a"))
	if err != nil {
		return err
	}
	got, err := d.exportTree("", "--folder m7a")
	if err != nil {
		return err
	}
	if s := diffInfo(got, now); s != "" {
		return fmt.Errorf("after the GC the export differs from the clients' tree: %s", s)
	}
	if _, err := d.web.ok("/settings", url.Values{"keep_days": {"30"}}); err != nil {
		return err
	}
	return nil
}

// m7State is what the clean-VM restore and the upgrade check afterwards.
type m7State struct {
	backup      string // local copy of var/ as design 9.4 lists it
	final       map[string]fileInfo
	fp, port    string
	admin       string
	lin         *agent
	lroot, name string
}

// backup copies meta.db, folders/ and tls/ with the service stopped and blobs/ afterwards (design 9.4).
func (m *m4) backup() error {
	d := m.d
	dir := filepath.Join(d.tmp, "backup")
	os.RemoveAll(dir)
	if _, err := d.sh("/etc/init.d/kaze-sync stop"); err != nil {
		return err
	}
	err := d.vm.TarGet(dataDir+"/var", dir, "meta.db", "folders", "tls")
	if _, err2 := d.sh("/etc/init.d/kaze-sync start"); err == nil {
		err = err2
	}
	if err != nil {
		return err
	}
	if err := d.vm.TarGet(dataDir+"/var", dir, "blobs"); err != nil {
		return err
	}
	if _, err := d.waitService(""); err != nil {
		return err
	}
	if err := d.waitHealthz(60 * time.Second); err != nil {
		return err
	}
	if err := d.login(); err != nil {
		return err
	}
	final, err := treeInfo(m.lroot("m7a"))
	if err != nil {
		return err
	}
	d.m7 = &m7State{backup: dir, final: final, fp: d.fp, port: d.port, admin: d.admin, lin: m.lin, lroot: m.lroot("m7a"), name: "m7a"}
	size, _ := exec.Command("du", "-sk", dir).Output()
	d.metric("M7 backup of the state after the retention scenario: %s KiB locally (%d files in the folder)", strings.Fields(string(size))[0], len(final))
	return nil
}

func (m *m4) m7Scenarios() {
	d := m.d
	if !d.step("M7 setup: folder m7a on both clients", func() error {
		id, err := d.web.newFolder("m7a")
		if err != nil {
			return err
		}
		m.ids["m7a"] = id
		os.MkdirAll(m.lroot("m7a"), 0o755)
		os.MkdirAll(m.wroot("m7a"), 0o755)
		if err := d.web.mapFolder(m.linID, id, m.lroot("m7a"), true); err != nil {
			return err
		}
		if err := d.web.mapFolder(m.winID, id, m.winWin+`\m7a`, true); err != nil {
			return err
		}
		return waitHave("the markers", m.lroot("m7a")+"/.syncroot", m.wroot("m7a")+"/.syncroot")
	}) {
		return
	}
	d.step("M7 1: a directory of 1000 files deleted by mistake is restored to the time before, on both sides", m.restoreDir)
	d.step("M7 2: export --at reproduces the clients' tree of that time byte for byte", m.exportAt)
	d.step("M7 3: unmapped folders are deleted; keep_days = 1 clears versions and blocks at once", m.retention)
	d.step("M7 4: backup as in design 9.4", m.backup)
	d.step("M7 resident memory and one idle minute with two clients", m.idle)
}

// restorePhase: on a clean VM the backup is restored, exported, and installed over; the service takes over
// with the same certificate, admin password and devices.
func (d *driver) restorePhase() {
	if d.m7 == nil {
		return
	}
	defer d.endPhase()
	s := d.m7
	if !d.step("M7 clean VM: restore the backup, export gives the same tree", func() error {
		if err := d.setupServer(); err != nil {
			return err
		}
		if err := d.vm.TarPut(s.backup, dataDir+"/var"); err != nil {
			return err
		}
		got, err := d.exportTree(s.name, "")
		if err != nil {
			return err
		}
		if diff := diffInfo(got, s.final); diff != "" {
			return fmt.Errorf("export of the restored backup differs: %s", diff)
		}
		d.metric("M7 restore on a clean VM: export of the restored backup is identical to the clients' tree (%d files)", len(got))
		return nil
	}) {
		return
	}
	if !d.step("M7 install over the restored data: service, certificate, admin password and ownership", func() error {
		_, err := d.install()
		if err != nil {
			return err
		}
		d.admin = s.admin // the password of the backup, not the later one
		if d.fp != s.fp || d.port != s.port {
			return fmt.Errorf("fingerprint %q port %s after the restore, were %q %s", d.fp, d.port, s.fp, s.port)
		}
		if _, err := d.waitService(""); err != nil {
			return err
		}
		if err := d.waitHealthz(60 * time.Second); err != nil {
			return err
		}
		return d.login()
	}) {
		return
	}
	d.step("M7 the Linux client carries on against the restored server", func() error {
		if err := os.WriteFile(filepath.Join(s.lroot, "after-restore.txt"), []byte("after the restore"), 0o644); err != nil {
			return err
		}
		old := time.Now().Add(-time.Minute)
		os.Chtimes(filepath.Join(s.lroot, "after-restore.txt"), old, old)
		d.agents = append(d.agents, s.lin)
		if err := s.lin.start(); err != nil {
			return err
		}
		id, err := d.web.idOf("folders", s.name)
		if err != nil {
			return err
		}
		return d.waitFile(id, "", "after-restore.txt", "17 B", 90*time.Second)
	})
}

// buildOld builds the server and the Linux client of an old revision from git, without touching the working tree.
func (d *driver) buildOld(rev string) (syncd, syncc string, err error) {
	src := filepath.Join(d.tmp, "old-src")
	os.MkdirAll(src, 0o755)
	if out, err := exec.Command("sh", "-c", fmt.Sprintf("git archive %s | tar -x -C %s", rev, src)).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("git archive %s: %w: %s", rev, err, out)
	}
	for _, b := range []struct{ out, pkg string }{{"kaze-syncd-old", "./cmd/kaze-syncd"}, {"kaze-syncc-old", "./cmd/kaze-syncc"}} {
		c := exec.Command("go", "build", "-o", filepath.Join(d.tmp, b.out), b.pkg)
		c.Dir = src
		c.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
		if out, err := c.CombinedOutput(); err != nil {
			return "", "", fmt.Errorf("go build %s at %s: %w: %s", b.pkg, rev, err, out)
		}
	}
	return filepath.Join(d.tmp, "kaze-syncd-old"), filepath.Join(d.tmp, "kaze-syncc-old"), nil
}

// upgradePhase: an old server and client sync; the new binary is installed over the old one
// (design 9.3); the data is migrated and intact, and a client of the new version carries on.
func (d *driver) upgradePhase() {
	defer d.endPhase()
	var oldD, oldC string
	if !d.step("M7 upgrade: build the old server and client from "+upgradeFrom, func() (err error) {
		oldD, oldC, err = d.buildOld(upgradeFrom)
		return
	}) {
		return
	}
	root := filepath.Join(d.tmp, "upg-files")
	var oldAgent *agent
	if !d.step("M7 upgrade: the old pair syncs", func() error {
		newBin := d.bins.syncd
		d.bins.syncd = oldD
		err := d.setupServer()
		d.bins.syncd = newBin
		if err != nil {
			return err
		}
		if _, err := d.install(); err != nil {
			return err
		}
		if _, err := d.waitService(""); err != nil {
			return err
		}
		if err := d.adminPassword(); err != nil {
			return err
		}
		code, err := d.web.addDevice("olddev")
		if err != nil {
			return err
		}
		a := &agent{name: "upg-old", exe: oldC, state: filepath.Join(d.tmp, "upg-old-state"), logs: d.logs}
		os.MkdirAll(a.state, 0o755)
		d.agents = append(d.agents, a)
		oldAgent = a
		if err := a.enroll(d.base, code, d.fp); err != nil {
			return err
		}
		fid, err := d.web.newFolder("upg")
		if err != nil {
			return err
		}
		did, err := d.web.idOf("devices", "olddev")
		if err != nil {
			return err
		}
		writeOld(filepath.Join(root, "a.txt"), []byte("before the upgrade"))
		writeOld(filepath.Join(root, "sub", "big.bin"), randomBytes(700<<10))
		if err := d.web.mapFolder(did, fid, root, true); err != nil {
			return err
		}
		if err := a.start(); err != nil {
			return err
		}
		for name, dir := range map[string]string{"a.txt": "", "big.bin": "sub"} {
			if err := d.waitFile(fid, dir, name, map[string]string{"a.txt": "18 B", "big.bin": "700.0 KiB"}[name], 90*time.Second); err != nil {
				return err
			}
		}
		return nil
	}) {
		return
	}
	d.step("M7 upgrade: install the new binary over it; data migrated, service back, same settings", func() error {
		port, fp := d.port, d.fp
		if err := d.vm.Put(d.bins.syncd, "/root/kaze-syncd-new", 0o755); err != nil {
			return err
		}
		pid, _ := d.serverPID()
		out, err := d.sh("/root/kaze-syncd-new install --data %s", dataDir)
		if err != nil {
			return err
		}
		if !strings.Contains(out, "listening on :"+port) || !strings.Contains(out, fp) {
			return fmt.Errorf("the upgrade changed port or fingerprint: %s", out)
		}
		if _, err := d.waitService(pid); err != nil {
			return err
		}
		if err := d.waitHealthz(60 * time.Second); err != nil {
			return err
		}
		if err := d.login(); err != nil {
			return err
		}
		fid, err := d.web.idOf("folders", "upg")
		if err != nil {
			return err
		}
		// Browsing a folder opens its database: it works only after the migration (index files_seq).
		if err := d.waitFile(fid, "sub", "big.bin", "700.0 KiB", 30*time.Second); err != nil {
			return err
		}
		return d.waitFile(fid, "", "a.txt", "18 B", 30*time.Second)
	})
	d.step("M7 upgrade: after the client is replaced too, sync carries on in both directions", func() error {
		// The old client against the new server is outside the support rule (server first, then the clients), but it must
		// be told: it cannot send the version header (design 9.3), so the server takes it for version 1 and answers every
		// request with 426; the old client writes the message into its log (it logs the body of a failed poll), the
		// server does not take its upload, and the overview marks the device. Then the new client takes over its state.
		fid, err := d.web.idOf("folders", "upg")
		if err != nil {
			return err
		}
		writeOld(filepath.Join(root, "during.txt"), []byte("old client, new server"))
		if _, err := until("the old client to log the too-old error", 120*time.Second, func() (bool, error) {
			log := oldAgent.stateLog()
			return strings.Contains(log, "client version 1 is too old") && strings.Contains(log, "upgrade kaze-syncc"), fmt.Errorf("old client log:\n%s", log)
		}); err != nil {
			return err
		}
		_, page, err := d.web.get("/")
		if err != nil {
			return err
		}
		if !strings.Contains(page, "1 (过旧, 请升级客户端)") {
			return fmt.Errorf("the overview does not mark the old client:\n%s", page)
		}
		time.Sleep(5 * time.Second) // past the debounce of the new file, so the refused upload has been tried
		if err := d.absent(fid, "", "during.txt"); err != nil {
			return fmt.Errorf("the refused old client still synced: %w", err)
		}
		oldAgent.stop(false)
		d.dropAgent(oldAgent)
		a := &agent{name: "upg-new", exe: d.bins.linux, state: oldAgent.state, logs: d.logs}
		d.agents = append(d.agents, a)
		if err := a.start(); err != nil {
			return err
		}
		writeOld(filepath.Join(root, "after.txt"), []byte("after the upgrade"))
		if err := d.waitFile(fid, "", "after.txt", "17 B", 90*time.Second); err != nil {
			return err
		}
		if _, page, err := d.web.get("/"); err != nil || strings.Contains(page, "过旧") {
			return fmt.Errorf("the overview still marks the replaced client (%v)", err)
		}
		// A second client of the new version receives everything the old pair uploaded.
		code, err := d.web.addDevice("newdev")
		if err != nil {
			return err
		}
		b := d.newAgent("upg-new2", filepath.Join(d.tmp, "upg-new2-state"))
		if err := b.enroll(d.base, code, d.fp); err != nil {
			return err
		}
		did, err := d.web.idOf("devices", "newdev")
		if err != nil {
			return err
		}
		root2 := filepath.Join(d.tmp, "upg-files2")
		if err := d.web.mapFolder(did, fid, root2, true); err != nil {
			return err
		}
		if err := b.start(); err != nil {
			return err
		}
		want, err := treeInfo(root)
		if err != nil {
			return err
		}
		want = contentOnly(want)
		var diff string
		_, err = until("the second client to hold the same files", 120*time.Second, func() (bool, error) {
			got, err := treeInfo(root2)
			if err != nil {
				return false, err
			}
			w := want
			diff = diffInfo(contentOnly(got), w)
			return diff == "", fmt.Errorf("%s", diff)
		})
		if err == nil {
			log.Printf("upgrade: %d files on both clients after the upgrade", len(want))
		}
		return err
	})
}
