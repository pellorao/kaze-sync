package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// M6 scenarios (docs/plan.md): the Windows client, with the Linux and the Windows client of the M4 setup.

// listDir returns the names of the synced files in a directory, sorted.
func listDir(dir string) []string {
	var out []string
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if n := e.Name(); n != ".syncroot" && !strings.HasPrefix(n, ".syncpart-") {
			out = append(out, n)
		}
	}
	return out
}

// onlyNames waits until the directory holds exactly the names.
func onlyNames(what, dir string, timeout time.Duration, names ...string) error {
	want := strings.Join(names, ",")
	_, err := until(what, timeout, func() (bool, error) {
		got := strings.Join(listDir(dir), ",")
		return got == want, fmt.Errorf("%s holds [%s], want [%s]", dir, got, want)
	})
	return err
}

// invalidName: a name that Windows cannot hold is refused by the server, shown as an error of the folder, and the
// notice goes away when the file is renamed.
func (m *m4) invalidName() error {
	d := m.d
	l, w := m.lroot("m6a"), m.wroot("m6a")
	m.put(l, "a:b.txt", []byte("not for windows"))
	if _, err := until("one error in the overview", 60*time.Second, func() (bool, error) {
		e, _, err := d.summary("m6a")
		return e == "1", fmt.Errorf("errors %q %v", e, err)
	}); err != nil {
		return err
	}
	page, err := d.web.folderPage(m.ids["m6a"], "")
	if err != nil || !strings.Contains(page, "a:b.txt") || !strings.Contains(page, "invalid_name") || !strings.Contains(page, "wsl4") {
		return fmt.Errorf("the folder page lacks the notice (%v)", err)
	}
	if err := d.absent(m.ids["m6a"], "", "a:b.txt"); err != nil {
		return err
	}
	if got := listDir(w); len(got) != 0 {
		return fmt.Errorf("Windows received %v", got)
	}
	if err := os.Rename(filepath.Join(l, "a:b.txt"), filepath.Join(l, "ab.txt")); err != nil {
		return err
	}
	if _, err := until("the notice to disappear", 60*time.Second, func() (bool, error) {
		e, _, err := d.summary("m6a")
		return e == "" && err == nil, fmt.Errorf("errors %q %v", e, err)
	}); err != nil {
		return err
	}
	return onlyNames("ab.txt on Windows", w, 60*time.Second, "ab.txt")
}

// caseRename: a case-only rename on Windows leaves a single file on Linux, and one on Linux a single file on Windows.
func (m *m4) caseRename() error {
	d := m.d
	l, w := m.lroot("m6a"), m.wroot("m6a")
	m.put(w, "a.txt", []byte("case"))
	m.put(l, "b.txt", []byte("case too"))
	if err := onlyNames("a.txt and b.txt on both sides", l, 60*time.Second, "a.txt", "ab.txt", "b.txt"); err != nil {
		return err
	}
	if err := onlyNames("a.txt and b.txt on Windows", w, 60*time.Second, "a.txt", "ab.txt", "b.txt"); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(w, "a.txt"), filepath.Join(w, "A.txt")); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(l, "b.txt"), filepath.Join(l, "B.txt")); err != nil {
		return err
	}
	for _, root := range []string{l, w} {
		if err := onlyNames("the renamed files in "+root, root, 60*time.Second, "A.txt", "B.txt", "ab.txt"); err != nil {
			return err
		}
	}
	if err := d.waitFile(m.ids["m6a"], "", "A.txt", "4 B", 30*time.Second); err != nil {
		return err
	}
	if err := d.waitFile(m.ids["m6a"], "", "B.txt", "8 B", 30*time.Second); err != nil {
		return err
	}
	page, _ := d.web.folderPage(m.ids["m6a"], "")
	if fileRow(page, "a.txt") != "" || fileRow(page, "b.txt") != "" {
		return fmt.Errorf("the server still lists the old names")
	}
	if got := readOr(filepath.Join(l, "A.txt")) + "/" + readOr(filepath.Join(w, "B.txt")); got != "case/case too" {
		return fmt.Errorf("contents %q", got)
	}
	if c := append(conflictCopies(l), conflictCopies(w)...); len(c) != 0 {
		return fmt.Errorf("conflict copies %v", c)
	}
	if e, _, err := d.summary("m6a"); e != "" || err != nil {
		return fmt.Errorf("notices after the renames: errors %q %v", e, err)
	}
	return nil
}

// freeDrive returns a drive letter that is not in use.
func freeDrive() (string, error) {
	for _, l := range "XYZWVUTSRQ" {
		if exec.Command("cmd.exe", "/c", "vol "+string(l)+":").Run() != nil {
			return string(l) + ":", nil
		}
	}
	return "", fmt.Errorf("no free drive letter")
}

// twoDrives: mappings on two drive letters sync at the same time. Without admin rights the second letter
// is a subst of a directory below the work directory.
func (m *m4) twoDrives() error {
	d := m.d
	drive, err := freeDrive()
	if err != nil {
		return err
	}
	target := m.winWin + `-subst`
	targetWSL := m.winRoot + "-subst"
	if err := os.MkdirAll(filepath.Join(targetWSL, "m6x"), 0o755); err != nil {
		return err
	}
	if out, err := exec.Command("cmd.exe", "/c", "subst", drive, target).CombinedOutput(); err != nil {
		return fmt.Errorf("subst %s %s: %v: %s", drive, target, err, out)
	}
	defer exec.Command("cmd.exe", "/c", "subst", drive, "/D").Run()
	m.d.metric("M6 drive letters: the second drive is %s, created without admin rights by `subst %s %s`", drive, drive, target)
	id, err := d.web.newFolder("m6x")
	if err != nil {
		return err
	}
	m.ids["m6x"] = id
	os.MkdirAll(m.lroot("m6x"), 0o755)
	if err := d.web.mapFolder(m.linID, id, m.lroot("m6x"), true); err != nil {
		return err
	}
	if err := d.web.mapFolder(m.winID, id, drive+`\m6x`, true); err != nil {
		return err
	}
	xroot := filepath.Join(targetWSL, "m6x")
	if err := waitHave("the marker on the second drive", xroot+"/.syncroot", m.lroot("m6x")+"/.syncroot"); err != nil {
		return err
	}
	// Both folders at once, in both directions.
	for i := 0; i < 20; i++ {
		data := randomBytes(48 << 10)
		m.put(m.lroot("m6a"), fmt.Sprintf("two/l%02d.bin", i), data)
		m.put(m.lroot("m6x"), fmt.Sprintf("two/l%02d.bin", i), data)
		m.put(m.wroot("m6a"), fmt.Sprintf("two/w%02d.bin", i), data)
		m.put(xroot, fmt.Sprintf("two/w%02d.bin", i), data)
	}
	for _, f := range []string{"m6a", "m6x"} {
		l, w := m.lroot(f), m.wroot(f)
		if f == "m6x" {
			w = xroot
		}
		if _, err := until("both sides of "+f+" to hold 40 files below two/", 120*time.Second, func() (bool, error) {
			a, err := tree(l + "/two")
			if err != nil {
				return false, err
			}
			b, err := tree(w + "/two")
			if err != nil {
				return false, err
			}
			ok := len(a) == 40 && len(b) == 40
			for p, h := range a {
				ok = ok && b[p] == h
			}
			return ok, fmt.Errorf("%d files on Linux, %d on Windows", len(a), len(b))
		}); err != nil {
			return err
		}
	}
	return nil
}

// lockedFile: a file the Windows user holds open (as Word does: others may read and write, not replace or delete)
// is not touched when a remote update arrives; the update shows as an in_use warning, waits for the slow retry
// (30 s) and is applied after the file is closed.
func (m *m4) lockedFile() error {
	d := m.d
	l, w := m.lroot("m6a"), m.wroot("m6a")
	m.put(l, "locked.txt", []byte("version 1"))
	if err := onlyNames("locked.txt on Windows", w, 60*time.Second, "A.txt", "B.txt", "ab.txt", "locked.txt", "two"); err != nil {
		return err
	}
	winPath := m.winWin + `\m6a\locked.txt`
	ready := filepath.Join(m.winWork, "lock-ready")
	os.Remove(ready)
	ps := fmt.Sprintf(`$f=[IO.File]::Open('%s','Open','Read','ReadWrite'); New-Item '%s\lock-ready' | Out-Null; Start-Sleep 25; $f.Close()`, winPath, strings.TrimSuffix(m.winWin, `\root`))
	holder := exec.Command("powershell.exe", "-NoProfile", "-Command", ps)
	if err := holder.Start(); err != nil {
		return err
	}
	m.d.metric("M6 locked file: powershell.exe holder started (WSL pid %d)", holder.Process.Pid)
	defer func() { holder.Process.Kill(); holder.Wait() }()
	if _, err := until("the file to be held", 30*time.Second, func() (bool, error) { return exists(ready), nil }); err != nil {
		return err
	}
	m.put(l, "locked.txt", []byte("version 2"))
	if _, err := until("the in_use warning in the overview", 20*time.Second, func() (bool, error) {
		_, wn, err := d.summary("m6a")
		return wn == "1", fmt.Errorf("warnings %q %v", wn, err)
	}); err != nil {
		return err
	}
	page, _ := d.web.folderPage(m.ids["m6a"], "")
	if !strings.Contains(page, "locked.txt") || !strings.Contains(page, "提醒") {
		return fmt.Errorf("the folder page lacks the in_use notice")
	}
	if got := readOr(filepath.Join(w, "locked.txt")); got != "version 1" {
		return fmt.Errorf("the open file was replaced: %q", got)
	}
	if err := holder.Wait(); err != nil {
		return err
	}
	took, err := until("the update after the file is closed", 90*time.Second, func() (bool, error) {
		return readOr(filepath.Join(w, "locked.txt")) == "version 2", nil
	})
	if err != nil {
		return err
	}
	d.metric("M6 locked file: replaced %.0f s after the holder closed it (the first retry is due 30 s after the first attempt)", took.Seconds())
	_, err = until("the warning to go away", 60*time.Second, func() (bool, error) {
		_, wn, err := d.summary("m6a")
		return wn == "" && err == nil, fmt.Errorf("warnings %q %v", wn, err)
	})
	return err
}

func (m *m4) m6Scenarios() {
	d := m.d
	if !d.step("M6 setup: folder m6a", func() error {
		id, err := d.web.newFolder("m6a")
		if err != nil {
			return err
		}
		m.ids["m6a"] = id
		os.MkdirAll(m.lroot("m6a"), 0o755)
		os.MkdirAll(m.wroot("m6a"), 0o755)
		if err := d.web.mapFolder(m.linID, id, m.lroot("m6a"), true); err != nil {
			return err
		}
		if err := d.web.mapFolder(m.winID, id, m.winWin+`\m6a`, true); err != nil {
			return err
		}
		return waitHave("the markers", m.lroot("m6a")+"/.syncroot", m.wroot("m6a")+"/.syncroot")
	}) {
		return
	}
	d.step("M6 1: a:b.txt from Linux is rejected, shown as an error, gone after the rename", m.invalidName)
	d.step("M6 2: a case-only rename leaves a single file on the other side, both ways", m.caseRename)
	d.step("M6 3: mappings on two drive letters sync at the same time", m.twoDrives)
	d.step("M6 4: a file open on Windows waits for the slow retry and is replaced after it is closed", m.lockedFile)
	if out, err := exec.Command("tasklist.exe", "/FI", "IMAGENAME eq kaze-syncc-agent-m4.exe", "/FO", "CSV", "/NH").Output(); err == nil {
		d.metric("M6 Windows agent with 3 folders (GUI build, tasklist): %s", strings.TrimSpace(string(out)))
	}
}
