package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"kaze-sync/e2e/vmctl"
	"kaze-sync/internal/proto"
)

// writeOld writes a file whose mtime is far enough in the past to count as stable.
func writeOld(p string, data []byte) {
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		panic(err)
	}
	old := time.Now().Add(-time.Minute)
	os.Chtimes(p, old, old)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// waitFile waits until the folder page (directory dir) lists name with the given size text.
func (d *driver) waitFile(folder, dir, name, size string, timeout time.Duration) error {
	return waitFor(name+" in the folder page", timeout, func() (bool, error) {
		page, err := d.web.folderPage(folder, "dir="+url.QueryEscape(dir))
		if err != nil {
			return false, err
		}
		row := fileRow(page, name)
		return strings.Contains(row, `<td class="size">`+size+"</td>"), fmt.Errorf("row %q", row)
	})
}

func (d *driver) absent(folder, dir, name string) error {
	page, err := d.web.folderPage(folder, "dir="+url.QueryEscape(dir)+"&deleted=1")
	if err != nil {
		return err
	}
	if fileRow(page, name) != "" {
		return fmt.Errorf("%s is on the server", name)
	}
	return nil
}

// syncScenarios are the M2 upload scenarios with the Linux client (and the Windows client when configured).
func (d *driver) syncScenarios() {
	lin := d.newAgent("wsl", filepath.Join(d.tmp, "wsl-state"))
	dirs := map[string]string{}
	ids := map[string]string{}
	if !d.step("register a Linux device and map 4 folders", func() error {
		code, err := d.web.addDevice("wsl")
		if err != nil {
			return err
		}
		if err := lin.enroll(d.base, code, d.fp); err != nil {
			return err
		}
		did, err := d.web.idOf("devices", "wsl")
		if err != nil {
			return err
		}
		for _, n := range []string{"docs", "media", "data", "existing"} {
			if ids[n], err = d.web.newFolder(n); err != nil {
				return err
			}
			dirs[n] = filepath.Join(d.tmp, "wsl-files", n)
			os.MkdirAll(dirs[n], 0o755)
		}
		// Files exist before the agent starts.
		writeOld(filepath.Join(dirs["docs"], "a.txt"), []byte("hello"))
		writeOld(filepath.Join(dirs["docs"], "sub", "n.txt"), []byte("nested"))
		writeOld(filepath.Join(dirs["docs"], "app.db"), append([]byte("SQLite format 3\x00"), randomBytes(984)...))
		writeOld(filepath.Join(dirs["docs"], "app.db-wal"), randomBytes(100))
		writeOld(filepath.Join(dirs["media"], "m1.bin"), randomBytes(3<<20))
		writeOld(filepath.Join(dirs["data"], "small.txt"), []byte("small"))
		writeOld(filepath.Join(dirs["existing"], "mine.txt"), []byte("not yours"))
		for _, n := range []string{"docs", "media", "data"} {
			if err := d.web.mapFolder(did, ids[n], dirs[n], true); err != nil {
				return err
			}
		}
		return d.web.mapFolder(did, ids["existing"], dirs["existing"], false) // not adopted
	}) {
		return
	}
	d.step("3 folders upload independently within one scan; SQLite in use and the non-adopted directory stay untouched", func() error {
		if err := lin.start(); err != nil {
			return err
		}
		for _, f := range []struct{ folder, dir, name, size string }{
			{"docs", "", "a.txt", "5 B"}, {"docs", "sub", "n.txt", "6 B"}, {"media", "", "m1.bin", "3.0 MiB"}, {"data", "", "small.txt", "5 B"},
		} {
			if err := d.waitFile(ids[f.folder], f.dir, f.name, f.size, 90*time.Second); err != nil {
				return err
			}
		}
		for _, n := range []string{"app.db", "app.db-wal"} {
			if err := d.absent(ids["docs"], "", n); err != nil {
				return err
			}
		}
		if err := d.absent(ids["existing"], "", "mine.txt"); err != nil {
			return err
		}
		// Without the -wal the database goes up.
		os.Remove(filepath.Join(dirs["docs"], "app.db-wal"))
		if err := lin.restart(); err != nil {
			return err
		}
		return d.waitFile(ids["docs"], "", "app.db", "1000 B", 90*time.Second)
	})
	d.step("modify, delete and add files; versions are listed and a restore works", func() error {
		writeOld(filepath.Join(dirs["docs"], "a.txt"), []byte("hello world"))
		os.Remove(filepath.Join(dirs["docs"], "sub", "n.txt"))
		writeOld(filepath.Join(dirs["docs"], "b.txt"), []byte("bee"))
		if err := lin.restart(); err != nil {
			return err
		}
		if err := d.waitFile(ids["docs"], "", "a.txt", "11 B", 90*time.Second); err != nil {
			return err
		}
		if err := d.waitFile(ids["docs"], "", "b.txt", "3 B", 90*time.Second); err != nil {
			return err
		}
		if err := waitFor("n.txt deleted", 60*time.Second, func() (bool, error) {
			page, err := d.web.folderPage(ids["docs"], "dir=sub&deleted=1")
			return strings.Contains(fileRow(page, "n.txt"), "已删除"), err
		}); err != nil {
			return err
		}
		page, err := d.web.folderPage(ids["docs"], "file=a.txt")
		if err != nil {
			return err
		}
		m := regexp.MustCompile(`name="version" value="(\d+)"`).FindStringSubmatch(page)
		if m == nil || !strings.Contains(page, "当前") {
			return fmt.Errorf("no old version listed for a.txt")
		}
		if _, err := d.web.ok("/folders/"+ids["docs"]+"/restore", url.Values{"version": {m[1]}}); err != nil {
			return err
		}
		return d.waitFile(ids["docs"], "", "a.txt", "5 B", 20*time.Second)
	})
	d.step("rewriting 1 MiB of an 8 MiB file uploads exactly 4 blocks of 256 KiB", func() error {
		p := filepath.Join(dirs["media"], "big.bin")
		data := randomBytes(8 << 20)
		writeOld(p, data)
		c0, err := d.blobCount()
		if err != nil {
			return err
		}
		rx0 := d.rxBytes()
		if err := lin.restart(); err != nil {
			return err
		}
		if err := d.waitFile(ids["media"], "", "big.bin", "8.0 MiB", 120*time.Second); err != nil {
			return err
		}
		c1, err := d.blobCount()
		if err != nil {
			return err
		}
		if c1-c0 != 8<<20/proto.BlockSize {
			return fmt.Errorf("first upload stored %d blocks, want %d", c1-c0, 8<<20/proto.BlockSize)
		}
		time.Sleep(5 * time.Second)
		rx1 := d.rxBytes()
		d.metric("first upload of an 8 MiB file: %d bytes received by the server", rx1-rx0)
		copy(data[2<<20:3<<20], randomBytes(1<<20))
		writeOld(p, data)
		if err := lin.restart(); err != nil {
			return err
		}
		if err := waitFor("four new blocks", 60*time.Second, func() (bool, error) {
			c, _ := d.blobCount()
			return c == c1+4, fmt.Errorf("%d blocks, want %d", c, c1+4)
		}); err != nil {
			return err
		}
		time.Sleep(10 * time.Second) // nothing more may follow
		c2, _ := d.blobCount()
		d.metric("rewriting 1 MiB of it: %d bytes received by the server (includes the Web UI polling of the driver)", d.rxBytes()-rx1)
		return eq("blocks after the second upload", fmt.Sprint(c2), fmt.Sprint(c1+4))
	})
	d.step("killing the client mid-upload; the restart completes the file", func() error {
		const blocks = 48 << 20 / proto.BlockSize
		c0, err := d.blobCount()
		if err != nil {
			return err
		}
		writeOld(filepath.Join(dirs["data"], "huge.bin"), randomBytes(48<<20))
		lin.stop(false)
		if err := lin.start(); err != nil {
			return err
		}
		// One guest-side loop, so the kill follows the first blocks closely.
		if _, err := d.sh("while [ $(find %s/var/blobs -type f | wc -l) -lt %d ]; do :; done", dataDir, c0+20); err != nil {
			return err
		}
		lin.stop(true)
		c1, _ := d.blobCount()
		if c1 >= c0+blocks {
			return fmt.Errorf("the upload finished before the kill (%d blocks); inconclusive", c1-c0)
		}
		log.Printf("killed after %d of %d blocks", c1-c0, blocks)
		if err := lin.start(); err != nil {
			return err
		}
		if err := d.waitFile(ids["data"], "", "huge.bin", "48.0 MiB", 180*time.Second); err != nil {
			return err
		}
		c2, _ := d.blobCount()
		return eq("blocks in total", fmt.Sprint(c2-c0), fmt.Sprint(blocks))
	})
	d.step("one database per folder: var/folders/<id>.db for every folder, plus var/meta.db", func() error {
		var want []string
		for _, id := range ids {
			want = append(want, id+".db")
		}
		sort.Strings(want)
		out, err := d.sh("ls %s/var/folders | grep -v -e -wal -e -shm | sort; test -f %s/var/meta.db", dataDir, dataDir)
		if err != nil {
			return err
		}
		return eq("folder databases", strings.Join(strings.Fields(out), " "), strings.Join(want, " "))
	})
	d.idleAndMemory(lin)
	d.windowsScenario()
}

// rxBytes is the number of bytes the guest NIC has received so far (0 when unreadable).
func (d *driver) rxBytes() int {
	out, _ := d.vm.SSH("cat /sys/class/net/eth0/statistics/rx_bytes")
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// cpuTicks is utime+stime, in clock ticks (100 per second), from stat contents.
func cpuTicks(stat string) int {
	f := strings.Fields(stat[strings.LastIndex(stat, ")")+1:])
	if len(f) < 13 {
		return -1
	}
	u, _ := strconv.Atoi(f[11])
	k, _ := strconv.Atoi(f[12])
	return u + k
}

// idleAndMemory measures resident memory (VmRSS, peak VmHWM) of the server and the Linux client and the CPU time
// both use during one idle minute (primary goal: low CPU and memory).
func (d *driver) idleAndMemory(lin *agent) {
	d.step("measure resident memory and the CPU time of one idle minute", func() error {
		pid, _ := d.serverPID()
		if pid == "" || lin.cmd == nil {
			return fmt.Errorf("server pid %q, client running %v", pid, lin.cmd != nil)
		}
		cpid := lin.cmd.Process.Pid
		time.Sleep(20 * time.Second) // let the last scan settle
		srv0, _ := d.sh("cat /proc/%s/stat", pid)
		cb0, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cpid))
		rx0 := d.rxBytes()
		time.Sleep(time.Minute)
		srv1, _ := d.sh("cat /proc/%s/stat", pid)
		cb1, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cpid))
		rx1 := d.rxBytes()
		mem, err := d.sh("grep -E 'VmRSS|VmHWM' /proc/%s/status; free -m | sed -n '1,3p'", pid)
		if err != nil {
			return err
		}
		cmem, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", cpid))
		d.metric("idle minute: server CPU %d ticks (%.2f s), client CPU %d ticks (%.2f s), %d bytes received by the server",
			cpuTicks(srv1)-cpuTicks(srv0), float64(cpuTicks(srv1)-cpuTicks(srv0))/100,
			cpuTicks(string(cb1))-cpuTicks(string(cb0)), float64(cpuTicks(string(cb1))-cpuTicks(string(cb0)))/100, rx1-rx0)
		d.metric("server memory after the sync scenarios:\n%s", mem)
		for _, l := range strings.Split(string(cmem), "\n") {
			if strings.HasPrefix(l, "VmRSS") || strings.HasPrefix(l, "VmHWM") {
				d.metric("client memory: %s", l)
			}
		}
		return nil
	})
}

func (d *driver) windowsScenario() {
	if d.cfg.WindowsWorkDir == "" {
		return
	}
	d.step("Windows client: enroll, upload from NTFS", func() error {
		work, err := vmctl.WSLPath(d.cfg.WindowsWorkDir)
		if err != nil {
			return err
		}
		os.RemoveAll(work)
		if err := os.MkdirAll(work, 0o755); err != nil {
			return err
		}
		exe := filepath.Join(work, "kaze-syncc.exe")
		b, err := os.ReadFile(d.bins.windows)
		if err != nil {
			return err
		}
		if err := os.WriteFile(exe, b, 0o755); err != nil {
			return err
		}
		a := &agent{name: "windows", exe: exe, windows: true, logs: d.logs, state: filepath.Join(work, "state"), stateWin: d.cfg.WindowsWorkDir + `\state`}
		os.MkdirAll(a.state, 0o755)
		d.agents = append(d.agents, a)
		code, err := d.web.addDevice("host")
		if err != nil {
			return err
		}
		if err := a.enroll(d.base, code, d.fp); err != nil {
			return err
		}
		did, err := d.web.idOf("devices", "host")
		if err != nil {
			return err
		}
		fid, err := d.web.newFolder("winfolder")
		if err != nil {
			return err
		}
		root := filepath.Join(work, "root")
		writeOld(filepath.Join(root, "w.txt"), []byte("from windows"))
		writeOld(filepath.Join(root, "dir", "big.bin"), randomBytes(2<<20))
		if err := d.web.mapFolder(did, fid, d.cfg.WindowsWorkDir+`\root`, true); err != nil {
			return err
		}
		if err := a.start(); err != nil {
			return err
		}
		if err := d.waitFile(fid, "", "w.txt", "12 B", 90*time.Second); err != nil {
			return err
		}
		return d.waitFile(fid, "dir", "big.bin", "2.0 MiB", 90*time.Second)
	})
}

func (d *driver) security() error {
	// A POST with a foreign Origin is refused even with a valid session.
	st, _, _, err := d.web.do("POST", "/folders/new", url.Values{"name": {"evil"}}, "https://evil.example")
	if err != nil {
		return err
	}
	if st != 403 {
		return fmt.Errorf("foreign Origin: status %d, want 403", st)
	}
	w := newWeb(d.base)
	for i := 1; i <= 11; i++ {
		st, err := w.login("wrong-password")
		if err != nil {
			return err
		}
		if want := map[bool]int{true: 429, false: 401}[i == 11]; st != want {
			return fmt.Errorf("failed login %d: status %d, want %d", i, st, want)
		}
	}
	old := d.web
	if err := d.adminPassword(); err != nil { // restart: sessions and locks are gone
		return err
	}
	if st, _, err := old.get("/"); err != nil || st != 303 {
		return fmt.Errorf("old session after admin-password: %d %v", st, err)
	}
	if st, _, err := d.web.get("/"); err != nil || st != 200 {
		return fmt.Errorf("new password session: %d %v", st, err)
	}
	return nil
}

func (d *driver) sysupgrade() error {
	for _, a := range d.agents {
		a.stop(false)
	}
	blobs0, err := d.blobCount()
	if err != nil {
		return err
	}
	pid0, _ := d.serverPID()
	_ = pid0
	img, err := vmctl.Image(d.cfg)
	if err != nil {
		return err
	}
	if err := d.vm.Put(img, "/tmp/sysupgrade.img.gz", 0o644); err != nil {
		return err
	}
	if err := d.reboot("(sysupgrade -v /tmp/sysupgrade.img.gz > /tmp/sysupgrade.log 2>&1 &)"); err != nil {
		return err
	}
	if err := d.vm.WaitSSH(bootTimeout); err != nil {
		return err
	}
	out, err := d.sh(`id kaze-sync; ls -l /etc/init.d/kaze-sync /etc/rc.d/S99kaze-sync; grep kaze /etc/sysupgrade.conf; uci show firewall.kaze_sync; mount | grep ' /mnt/data '`)
	if err != nil {
		return err
	}
	log.Printf("after sysupgrade:\n%s", out)
	if _, err := d.waitService(""); err != nil {
		return err
	}
	d.setBase()
	if err := d.waitHealthz(60 * time.Second); err != nil {
		return err
	}
	blobs1, err := d.blobCount()
	if err != nil {
		return err
	}
	if err := eq("blocks after sysupgrade", fmt.Sprint(blobs1), fmt.Sprint(blobs0)); err != nil {
		return err
	}
	if err := d.login(); err != nil {
		return err
	}
	// The data is complete and the service keeps working: the clients resume and upload a new file.
	for _, a := range d.agents {
		if err := a.start(); err != nil {
			return err
		}
	}
	id, err := d.web.idOf("folders", "docs")
	if err != nil {
		return err
	}
	if err := d.waitFile(id, "", "a.txt", "5 B", 30*time.Second); err != nil {
		return err
	}
	writeOld(filepath.Join(d.tmp, "wsl-files", "docs", "after.txt"), []byte("after"))
	for _, a := range d.agents {
		if a.name == "wsl" {
			a.restart()
		}
	}
	return d.waitFile(id, "", "after.txt", "5 B", 90*time.Second)
}
