package main

import (
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// M5 scenarios (docs/plan.md): two-layer exclusion rules with the Linux and the Windows client of the M4 setup.

// rulesText returns the text of the rule box of a page (the overview for the global rules, a folder page for its own).
func (d *driver) rulesText(page string) string {
	m := regexp.MustCompile(`(?s)<textarea\b[^>]*\sname="rules"[^>]*>(.*?)</textarea>`).FindStringSubmatch(page)
	if m == nil {
		return ""
	}
	return strings.TrimLeft(html.UnescapeString(m[1]), "\n")
}

func (d *driver) setRules(path, text string) error {
	_, err := d.web.ok(path, url.Values{"rules": {text}})
	return err
}

// bothCurrent waits until the overview shows the configuration as up to date on both clients.
func (m *m4) bothCurrent() (time.Duration, error) {
	return until("both clients to hold the latest configuration", 30*time.Second, func() (bool, error) {
		_, b, err := m.d.web.get("/")
		for _, id := range []string{m.linID, m.winID} {
			row := overviewRow(b, "devices", id)
			if !strings.Contains(row, "最新") {
				return false, fmt.Errorf("device %s: %q %v", id, firstLine(row), err)
			}
		}
		return true, nil
	})
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// waitHave waits until the files exist (Windows and Linux paths alike).
func waitHave(what string, paths ...string) error {
	_, err := until(what, 60*time.Second, func() (bool, error) {
		for _, p := range paths {
			if !exists(p) {
				return false, fmt.Errorf("%s is missing", p)
			}
		}
		return true, nil
	})
	return err
}

func missing(paths ...string) error {
	for _, p := range paths {
		if exists(p) {
			return fmt.Errorf("%s exists but its path is excluded", p)
		}
	}
	return nil
}

// m5Global: a change of the global rules reaches every folder of every device within a long poll, and
// the case mode follows each device's system.
func (m *m4) m5Global() error {
	d := m.d
	_, page, _ := d.web.get("/")
	base := d.rulesText(page)
	if !strings.Contains(base, "Thumbs.db") {
		return fmt.Errorf("the global rules do not start as the defaults: %q", base)
	}
	if err := d.setRules("/rules", base+"\n*.zzz\nbuild/\n"); err != nil {
		return err
	}
	saved := time.Now()
	if _, err := m.bothCurrent(); err != nil {
		return err
	}
	d.metric("M5 1: both clients held the new global rules %.1f s after saving", time.Since(saved).Seconds())
	for _, f := range []string{"m5a", "m5b"} {
		m.put(m.lroot(f), "x.zzz", []byte("excluded"))
		m.put(m.lroot(f), "x.txt", []byte("kept"))
	}
	m.put(m.lroot("m5a"), "L.ZZZ", []byte("upper case on linux")) // Linux is case sensitive: not excluded there
	m.put(m.lroot("m5a"), "l-control.txt", []byte("c"))
	m.put(m.wroot("m5a"), "W.ZZZ", []byte("upper case on windows")) // Windows ignores case: excluded there
	m.put(m.wroot("m5a"), "w-control.txt", []byte("c"))
	if err := waitHave("the control files on the other side", m.wroot("m5a")+"/x.txt", m.wroot("m5b")+"/x.txt", m.wroot("m5a")+"/l-control.txt", m.lroot("m5a")+"/w-control.txt"); err != nil {
		return err
	}
	if err := missing(m.wroot("m5a")+"/x.zzz", m.wroot("m5b")+"/x.zzz", m.wroot("m5a")+"/L.ZZZ", m.lroot("m5a")+"/W.ZZZ"); err != nil {
		return err
	}
	for _, f := range []string{"m5a", "m5b"} {
		if err := d.absent(m.ids[f], "", "x.zzz"); err != nil {
			return err
		}
	}
	// L.ZZZ was uploaded by the Linux client, so the server holds it although the Windows client skips it.
	return d.waitFile(m.ids["m5a"], "", "L.ZZZ", "19 B", 30*time.Second)
}

// m5Folder: the design 3.2 example: the folder rules bring one file back from an excluded directory
// in one folder only.
func (m *m4) m5Folder() error {
	d := m.d
	if err := d.setRules("/folders/"+m.ids["m5a"]+"/rules", "!/build/\n/build/*\n!/build/keep.txt"); err != nil {
		return err
	}
	if _, err := m.bothCurrent(); err != nil {
		return err
	}
	for _, f := range []string{"m5a", "m5b"} {
		m.put(m.lroot(f), "build/keep.txt", []byte("keep"))
		m.put(m.lroot(f), "build/other.txt", []byte("other"))
		m.put(m.lroot(f), "build/sub/deep.txt", []byte("deep"))
		m.put(m.lroot(f), "build-control.txt", []byte("c"))
	}
	if err := waitHave("build/keep.txt and the controls on Windows", m.wroot("m5a")+"/build/keep.txt", m.wroot("m5a")+"/build-control.txt", m.wroot("m5b")+"/build-control.txt"); err != nil {
		return err
	}
	if err := missing(m.wroot("m5a")+"/build/other.txt", m.wroot("m5a")+"/build/sub", m.wroot("m5b")+"/build"); err != nil {
		return err
	}
	if err := d.waitFile(m.ids["m5a"], "build", "keep.txt", "4 B", 30*time.Second); err != nil {
		return err
	}
	if err := d.absent(m.ids["m5a"], "build", "other.txt"); err != nil {
		return err
	}
	return d.absent(m.ids["m5b"], "build", "keep.txt")
}

// m5Release: a new exclusion leaves local and server files alone; removing it brings both sides together again.
func (m *m4) m5Release() error {
	d := m.d
	a, w := m.lroot("m5a"), m.wroot("m5a")
	m.put(a, "late/a.txt", []byte("original"))
	if err := waitHave("late/a.txt on Windows", w+"/late/a.txt"); err != nil {
		return err
	}
	if err := d.waitFile(m.ids["m5a"], "late", "a.txt", "8 B", 30*time.Second); err != nil {
		return err
	}
	_, page, _ := d.web.get("/folders/" + m.ids["m5a"])
	folderRules := d.rulesText(page)
	if err := d.setRules("/folders/"+m.ids["m5a"]+"/rules", folderRules+"\n/late/"); err != nil {
		return err
	}
	if _, err := m.bothCurrent(); err != nil {
		return err
	}
	m.put(a, "late/a.txt", []byte("edited on linux"))
	m.put(w, "late/b.txt", []byte("win-only-b"))
	m.put(a, "late-control.txt", []byte("c"))
	if err := waitHave("the control file on Windows", w+"/late-control.txt"); err != nil {
		return err
	}
	time.Sleep(5 * time.Second) // a little longer than the scan debounce
	if readOr(w+"/late/a.txt") != "original" || readOr(a+"/late/a.txt") != "edited on linux" || exists(a+"/late/b.txt") {
		return fmt.Errorf("local files changed while excluded: windows %q, linux %q", readOr(w+"/late/a.txt"), readOr(a+"/late/a.txt"))
	}
	if err := d.waitFile(m.ids["m5a"], "late", "a.txt", "8 B", 5*time.Second); err != nil {
		return fmt.Errorf("the server's file changed: %w", err)
	}
	if err := d.absent(m.ids["m5a"], "late", "b.txt"); err != nil {
		return err
	}
	// Release it.
	if err := d.setRules("/folders/"+m.ids["m5a"]+"/rules", folderRules); err != nil {
		return err
	}
	_, err := until("both sides to hold the same files below late/ with both edits", 120*time.Second, func() (bool, error) {
		la, err := tree(a + "/late")
		if err != nil {
			return false, err
		}
		wa, err := tree(w + "/late")
		if err != nil {
			return false, err
		}
		if len(la) != len(wa) {
			return false, fmt.Errorf("%d files on Linux, %d on Windows", len(la), len(wa))
		}
		for p, h := range la {
			if wa[p] != h {
				return false, fmt.Errorf("%s differs", p)
			}
		}
		for _, c := range []string{"edited on linux", "win-only-b"} {
			found := false
			for p := range la {
				found = found || readOr(filepath.Join(a, "late", p)) == c
			}
			if !found {
				return false, fmt.Errorf("%q is on no file yet", c)
			}
		}
		return true, nil
	})
	return err
}

func (m *m4) m5Scenarios() {
	d := m.d
	for _, n := range []string{"m5a", "m5b"} {
		if !d.step("M5 setup: folder "+n, func() error {
			id, err := d.web.newFolder(n)
			if err != nil {
				return err
			}
			m.ids[n] = id
			os.MkdirAll(m.lroot(n), 0o755)
			os.MkdirAll(m.wroot(n), 0o755)
			if err := d.web.mapFolder(m.linID, id, m.lroot(n), true); err != nil {
				return err
			}
			if err := d.web.mapFolder(m.winID, id, m.winWin+`\`+n, true); err != nil {
				return err
			}
			return waitHave("the markers", m.lroot(n)+"/.syncroot", m.wroot(n)+"/.syncroot")
		}) {
			return
		}
	}
	d.step("M5 1: global rules reach all folders on all devices within a long poll; case mode follows the system", m.m5Global)
	d.step("M5 2: folder rules re-include one file below an excluded directory; other folders still exclude it", m.m5Folder)
	d.step("M5 3: a new exclusion leaves files alone; removing it converges per the conflict table", m.m5Release)
}
