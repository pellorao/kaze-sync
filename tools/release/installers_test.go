package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"kaze-sync/internal/installer"
)

// Stub programs stand in for the real binaries: each logs its command line, one line per call, to $KS_LOG.
const stub = "#!/bin/sh\necho \"$(basename $0) $*\" >> \"$KS_LOG\"\n"

func fakeBinaries() binaries {
	mk := func(string) []byte { return []byte(stub) }
	return binaries{Syncd: mk("kaze-syncd"), Syncc: mk("kaze-syncc"), WinSyncc: []byte("syncc.exe"), WinAgent: []byte("agent.exe"), WinSetup: []byte("MZ setup")}
}

func fakeInstallers(t *testing.T) installers {
	t.Helper()
	files, err := buildInstallers("1.2.3", 7, fakeBinaries())
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func untar(t *testing.T, gz []byte) map[string][]byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name], _ = io.ReadAll(tr)
	}
}

func TestInstallersLayout(t *testing.T) {
	files := fakeInstallers(t)
	var names []string
	for n := range files {
		names = append(names, n)
	}
	if len(names) != 3 {
		t.Fatalf("files %v", names)
	}
	server := files["kaze-sync-server-1.2.3-linux-amd64.sh"]
	client := files["kaze-sync-client-1.2.3-linux-amd64.sh"]
	win := files["kaze-sync-client-1.2.3-windows-amd64-setup.exe"]
	for name, data := range map[string][]byte{"server": server, "client": client, "setup": win} {
		p, err := installer.Open(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Manifest.Version != "1.2.3" || p.Manifest.AgentVersion != 7 || p.Manifest.Arch != "amd64" {
			t.Fatalf("%s manifest %+v", name, p.Manifest)
		}
		if name == "setup" {
			if p.Manifest.OS != "windows" || !bytes.HasPrefix(data, []byte("MZ setup")) || p.PayloadStart != 8 {
				t.Fatalf("setup layout %+v", p)
			}
			continue
		}
		// The shell finds the payload by the header size written in the header: it must equal Open's offset.
		line := strings.SplitN(strings.SplitN(string(data), "KS_HEADER_SIZE=", 2)[1], "\n", 2)[0]
		if n, err := strconv.Atoi(strings.TrimSpace(line)); err != nil || int64(n) != p.PayloadStart {
			t.Fatalf("%s header size %q, payload at %d", name, line, p.PayloadStart)
		}
	}
	if m, err := installer.Open(bytes.NewReader(win), int64(len(win))); err != nil {
		t.Fatal(err)
	} else if got := untar(t, readPayload(t, win, m)); string(got["kaze-syncc-agent.exe"]) != "agent.exe" || string(got["kaze-syncc.exe"]) != "syncc.exe" {
		t.Fatalf("setup payload %v", got)
	}
	m, _ := installer.Open(bytes.NewReader(server), int64(len(server)))
	got := untar(t, readPayload(t, server, m))
	if !bytes.Equal(got[clientLinuxPackage], client) || !bytes.Equal(got[clientWindowsPackage], win) || len(got) != 3 {
		t.Fatalf("server payload must carry the two client installers unchanged, has %d entries", len(got))
	}
}

func readPayload(t *testing.T, data []byte, p *installer.Package) []byte {
	t.Helper()
	return data[p.PayloadStart:p.PayloadEnd]
}

// A downloaded installer has a registration block after the manifest: the shell must not see it.
func TestInstallersRoundTripWithRegistration(t *testing.T) {
	files := fakeInstallers(t)
	client := files["kaze-sync-client-1.2.3-linux-amd64.sh"]
	reg := installer.Registration{Server: "https://10.0.0.3:8443", Code: "ABCD-1234", Fingerprint: "sha256:" + strings.Repeat("ab", 32), Name: "box"}
	rt, err := installer.MarshalTrailer(reg, installer.RegistrationMagic)
	if err != nil {
		t.Fatal(err)
	}
	withReg := append(append([]byte(nil), client...), rt...)
	p, err := installer.Open(bytes.NewReader(withReg), int64(len(withReg)))
	if err != nil || p.Registration == nil || *p.Registration != reg {
		t.Fatalf("open: %+v %v", p, err)
	}
	r := newShellRun(t)
	r.run(t, withReg, "client", "--dir", "/x y")
	if got := r.log(); got != "kaze-syncc install --from "+r.file+" --dir /x y\n" {
		t.Fatalf("log %q", got)
	}
}

func TestInstallersReproducible(t *testing.T) {
	a, b := fakeInstallers(t), fakeInstallers(t)
	for name, data := range a {
		if !bytes.Equal(data, b[name]) {
			t.Fatalf("%s differs between builds", name)
		}
	}
	if _, err := buildInstallers("1.2.3-rc1", 7, fakeBinaries()); err == nil {
		t.Fatal("a pre-release version must be refused: installers carry release versions only")
	}
}

// shellRun runs an installer file with dash, the strictest sh the installers must work in, with stub programs.
type shellRun struct {
	dir, file, root string
	env             []string
}

func newShellRun(t *testing.T) *shellRun {
	t.Helper()
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash is not installed; the sh header is tested with dash")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	bin := filepath.Join(dir, "bin")
	tmp := filepath.Join(dir, "tmp")
	for _, d := range []string{root, bin, tmp} {
		os.Mkdir(d, 0755)
	}
	os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho ${KS_UID:-0}\n"), 0755)
	return &shellRun{dir: dir, root: root, file: filepath.Join(dir, "installer.sh"), env: []string{
		"PATH=" + bin + ":/usr/bin:/bin", "TMPDIR=" + tmp, "KAZE_SYNC_ROOT=" + root, "KS_LOG=" + filepath.Join(dir, "log"), "SHELL_UNDER_TEST=" + dash}}
}

// run writes the installer (the "client" or "server" file of fakeInstallers unless data is given) and returns
// its output and error.
func (r *shellRun) run(t *testing.T, data []byte, kind string, args ...string) (string, error) {
	t.Helper()
	if data == nil {
		data = fakeInstallers(t)["kaze-sync-"+kind+"-1.2.3-linux-amd64.sh"]
	}
	if err := os.WriteFile(r.file, data, 0644); err != nil {
		t.Fatal(err)
	}
	var shell string
	for _, e := range r.env {
		if v, ok := strings.CutPrefix(e, "SHELL_UNDER_TEST="); ok {
			shell = v
		}
	}
	cmd := exec.Command(shell, append([]string{r.file}, args...)...)
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *shellRun) log() string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "log"))
	return string(b)
}

func (r *shellRun) leftovers(t *testing.T) {
	t.Helper()
	if left, _ := os.ReadDir(filepath.Join(r.dir, "tmp")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestShellClientPassesArguments(t *testing.T) {
	r := newShellRun(t)
	out, err := r.run(t, nil, "client", "https://h:8443", "CODE-1", "--fingerprint", "sha256:aa")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := r.log(); got != "kaze-syncc install --from "+r.file+" https://h:8443 CODE-1 --fingerprint sha256:aa\n" {
		t.Fatalf("log %q", got)
	}
	r.leftovers(t)
}

func TestShellClientExitStatus(t *testing.T) {
	b := fakeBinaries()
	b.Syncc = []byte("#!/bin/sh\nexit 3\n")
	files, err := buildInstallers("1.2.3", 7, b)
	if err != nil {
		t.Fatal(err)
	}
	r := newShellRun(t)
	_, err = r.run(t, files["kaze-sync-client-1.2.3-linux-amd64.sh"], "client")
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Fatalf("want the install status 3, got %v", err)
	}
	r.leftovers(t)
}

func TestShellRefusesDamagedFiles(t *testing.T) {
	good := fakeInstallers(t)["kaze-sync-client-1.2.3-linux-amd64.sh"]
	p, _ := installer.Open(bytes.NewReader(good), int64(len(good)))
	flipped := append([]byte(nil), good...)
	flipped[p.PayloadStart+20] ^= 0xff
	truncated := good[:p.PayloadEnd-10]
	for name, data := range map[string][]byte{"flipped": flipped, "truncated": truncated} {
		r := newShellRun(t)
		out, err := r.run(t, data, "client")
		if err == nil || !strings.Contains(out, "SHA-256") || r.log() != "" {
			t.Fatalf("%s: err %v, log %q, output %q", name, err, r.log(), out)
		}
		r.leftovers(t)
	}
}

func TestShellServerFirstInstall(t *testing.T) {
	r := newShellRun(t)
	out, err := r.run(t, nil, "server", "--data", "/srv/ks", "--listen", ":9443")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	x := ""
	for _, f := range strings.Fields(strings.Split(r.log(), "\n")[1]) { // the unpack directory appears in the paths
		if strings.HasSuffix(f, clientLinuxPackage) {
			x = filepath.Dir(f)
		}
	}
	want := "kaze-syncd install --data /srv/ks --listen :9443\n" +
		"kaze-syncd client-pkg --data /srv/ks " + x + "/client-linux-amd64.sh " + x + "/client-windows-amd64-setup.exe\n" +
		"kaze-syncd admin-password --data /srv/ks\n"
	if x == "" || r.log() != want {
		t.Fatalf("log\n%s\nwant\n%s", r.log(), want)
	}
	r.leftovers(t)
}

func TestShellServerUpgradeKeepsSettings(t *testing.T) {
	r := newShellRun(t)
	os.MkdirAll(filepath.Join(r.root, "etc/init.d"), 0755)
	os.WriteFile(filepath.Join(r.root, "etc/init.d/kaze-sync"), nil, 0755)
	if out, err := r.run(t, nil, "server"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(r.log()), "\n")
	if len(lines) != 2 || lines[0] != "kaze-syncd install" || !strings.HasPrefix(lines[1], "kaze-syncd client-pkg /") {
		t.Fatalf("an upgrade runs install and client-pkg with no --data and no admin-password:\n%s", r.log())
	}
}

func TestShellServerRefusals(t *testing.T) {
	cases := []struct {
		name, want string
		setup      func(r *shellRun)
		args       []string
	}{
		{"not root", "root", func(r *shellRun) { r.env = append(r.env, "KS_UID=1000") }, []string{"--data", "/d"}},
		{"first install, no data directory, no terminal", "--data /var/lib/kaze-sync", func(*shellRun) {}, nil},
		{"first install on OpenWrt", "OpenWrt", func(r *shellRun) {
			os.MkdirAll(filepath.Join(r.root, "etc"), 0755)
			os.WriteFile(filepath.Join(r.root, "etc/openwrt_release"), nil, 0644)
		}, nil},
	}
	for _, c := range cases {
		r := newShellRun(t)
		c.setup(r)
		out, err := r.run(t, nil, "server", c.args...)
		if err == nil || !strings.Contains(out, c.want) || r.log() != "" {
			t.Fatalf("%s: err %v, log %q, output %q", c.name, err, r.log(), out)
		}
		r.leftovers(t)
	}
}

func TestShellServerStopsWhenInstallFails(t *testing.T) {
	b := fakeBinaries()
	b.Syncd = []byte("#!/bin/sh\necho \"kaze-syncd $*\" >> \"$KS_LOG\"\n[ \"$1\" = install ] && exit 1\nexit 0\n")
	files, err := buildInstallers("1.2.3", 7, b)
	if err != nil {
		t.Fatal(err)
	}
	r := newShellRun(t)
	out, err := r.run(t, files["kaze-sync-server-1.2.3-linux-amd64.sh"], "server", "--data=/srv/ks")
	if err == nil || r.log() != "kaze-syncd install --data=/srv/ks\n" || !strings.Contains(out, "install failed") {
		t.Fatalf("err %v, log %q, output %q", err, r.log(), out)
	}
	r.leftovers(t)
}

func TestAgentVersion(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "internal/proto"), 0755)
	os.WriteFile(filepath.Join(dir, "internal/proto/wire.go"), []byte("const (\n\tAgentVersion    = 6\n\tMinAgentVersion = 5\n\tLastAgentVersionWithoutHeader = 4\n)\n"), 0644)
	if n, err := agentVersion(dir); err != nil || n != 6 {
		t.Fatalf("%d %v", n, err)
	}
	if n, err := agentVersion("../.."); err != nil || n < 1 { // this repository's own tree
		t.Fatalf("%d %v", n, err)
	}
}

// fakePE writes the smallest PE32+ header debug/pe accepts, with the given subsystem.
func fakePE(t *testing.T, path string, subsystem uint16) {
	t.Helper()
	b := make([]byte, 0x80+4+20+240)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 0x80)
	copy(b[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[0x84:], 0x8664) // machine
	binary.LittleEndian.PutUint16(b[0x84+16:], 240) // size of optional header
	binary.LittleEndian.PutUint16(b[0x98:], 0x20b)  // PE32+
	binary.LittleEndian.PutUint16(b[0x98+68:], subsystem)
	binary.LittleEndian.PutUint32(b[0x98+108:], 16) // number of data directories
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckSubsystem(t *testing.T) {
	dir := t.TempDir()
	gui, console := filepath.Join(dir, "gui.exe"), filepath.Join(dir, "console.exe")
	fakePE(t, gui, subsystemGUI)
	fakePE(t, console, subsystemConsole)
	if err := checkSubsystem(gui, subsystemGUI); err != nil {
		t.Fatal(err)
	}
	if err := checkSubsystem(console, subsystemGUI); err == nil || !strings.Contains(err.Error(), "subsystem 3") {
		t.Fatalf("a console program must not pass as the agent: %v", err)
	}
	if err := checkSubsystem(filepath.Join(dir, "missing.exe"), subsystemGUI); err == nil {
		t.Fatal("a missing file must fail")
	}
}
