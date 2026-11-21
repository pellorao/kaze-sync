package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/installer"
	"kaze-sync/internal/proto"
)

var testReg = installer.Registration{Server: "https://10.0.0.3:8443", Fingerprint: "sha256:" + strings.Repeat("0a", 32), Code: "ABCD-EFGH-JKLM", Name: "laptop"}

type installRig struct {
	t      *testing.T
	env    installEnv
	events []string
	state  string
	target string // the directory the fake autostart record points to; empty: no record
	def    string
	out    bytes.Buffer
}

func newRig(t *testing.T, goos string) *installRig {
	t.Helper()
	r := &installRig{t: t, state: t.TempDir(), def: filepath.Join(t.TempDir(), "default")}
	log := func(format string, a ...any) { r.events = append(r.events, fmt.Sprintf(format, a...)) }
	r.env = installEnv{
		GOOS: goos, GOARCH: "amd64", StateDir: r.state,
		AutostartDir: func() (string, error) { return r.target, nil },
		DefaultDir:   func() (string, error) { return r.def, nil },
		StopAgent:    func() error { log("stop"); return nil },
		Autostart: func(exe string) error {
			log("autostart %s", filepath.Base(filepath.Dir(exe))+"/"+filepath.Base(exe))
			return nil
		},
		Enroll: func(_ context.Context, server, code, fp string) error {
			log("enroll %s %s %s", server, code, fp)
			return SaveDevice(r.state, Device{Server: server, Token: "t", Fingerprint: fp})
		},
		OldVersion:  func(string) string { return "0.1.0" },
		ServerAgent: func(context.Context, Device) (int, error) { return proto.AgentVersion, nil },
		Linger:      func() (bool, bool) { return true, true },
		Sleep:       func(time.Duration) {},
	}
	return r
}

func (r *installRig) enrolled() {
	r.t.Helper()
	if err := SaveDevice(r.state, Device{Server: "https://s", Token: "tok"}); err != nil {
		r.t.Fatal(err)
	}
}

// pkg writes an installer file with the given binaries (contents "new") and returns its path.
func (r *installRig) pkg(goos string, agent int, reg *installer.Registration, names ...string) string {
	r.t.Helper()
	var files []installer.File
	for _, n := range names {
		files = append(files, installer.File{Name: n, Data: []byte("new " + n)})
	}
	b, err := installer.Build([]byte("MZ"), files, installer.Manifest{Version: "0.2.0", AgentVersion: agent, OS: goos, Arch: "amd64"}, reg)
	if err != nil {
		r.t.Fatal(err)
	}
	p := filepath.Join(r.t.TempDir(), "setup")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *installRig) run(o InstallOptions) error {
	o.Out = &r.out
	return install(context.Background(), o, r.env)
}

func (r *installRig) seq() string { return strings.Join(r.events, " | ") }

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallFirstTimeEnrollsFromTheInstallersBlock(t *testing.T) {
	t.Parallel()
	r := newRig(t, "linux")
	err := r.run(InstallOptions{From: r.pkg("linux", 6, &testReg, "kaze-syncc")})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(r.def, "kaze-syncc")); got != "new kaze-syncc" {
		t.Fatalf("installed %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(r.def, "kaze-syncc")); fi.Mode().Perm()&0o100 == 0 {
		t.Fatal("not executable")
	}
	want := "stop | enroll https://10.0.0.3:8443 ABCD-EFGH-JKLM " + testReg.Fingerprint + " | autostart default/kaze-syncc"
	if r.seq() != want {
		t.Fatalf("events %s", r.seq())
	}
	if !strings.Contains(r.out.String(), "0.1.0 -> 0.2.0") {
		t.Fatalf("output %q", r.out.String())
	}
	if ents, _ := os.ReadDir(r.def); len(ents) != 1 {
		t.Fatalf("leftovers in %s: %v", r.def, ents)
	}
}

func TestInstallUpgradeKeepsRegistrationAndUsesTheAutostartDirectory(t *testing.T) {
	t.Parallel()
	r := newRig(t, "windows")
	r.target = t.TempDir()
	r.enrolled()
	before := read(t, filepath.Join(r.state, "device.json"))
	for _, n := range []string{"kaze-syncc.exe", "kaze-syncc-agent.exe"} {
		os.WriteFile(filepath.Join(r.target, n), []byte("old"), 0o755)
	}
	// An old installer lying around still carries its (long expired) block: it must not re-enroll.
	err := r.run(InstallOptions{From: r.pkg("windows", 6, &testReg, "kaze-syncc.exe", "kaze-syncc-agent.exe")})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"kaze-syncc.exe", "kaze-syncc-agent.exe"} {
		if got := read(t, filepath.Join(r.target, n)); got != "new "+n {
			t.Fatalf("%s: %q", n, got)
		}
	}
	if r.seq() != "stop | autostart "+filepath.Base(r.target)+"/kaze-syncc.exe" {
		t.Fatalf("events %s", r.seq())
	}
	if read(t, filepath.Join(r.state, "device.json")) != before {
		t.Fatal("device.json changed")
	}
	if ents, _ := os.ReadDir(r.target); len(ents) != 2 {
		t.Fatalf("leftovers: %v", ents)
	}
}

func TestInstallExplicitArgumentsBeatTheBlockAndEnrolledMachines(t *testing.T) {
	t.Parallel()
	r := newRig(t, "linux")
	r.enrolled()
	err := r.run(InstallOptions{From: r.pkg("linux", 6, &testReg, "kaze-syncc"), Server: "https://other:1", Code: "ZZZZ-1111", Fingerprint: testReg.Fingerprint})
	if err != nil || !strings.Contains(r.seq(), "enroll https://other:1 ZZZZ-1111") {
		t.Fatalf("%v: %s", err, r.seq())
	}
}

func TestInstallRefusesAServerOlderThanThePackage(t *testing.T) {
	t.Parallel()
	r := newRig(t, "linux")
	r.target = t.TempDir()
	r.enrolled()
	os.WriteFile(filepath.Join(r.target, "kaze-syncc"), []byte("old"), 0o755)
	r.env.ServerAgent = func(context.Context, Device) (int, error) { return 5, nil }
	p := r.pkg("linux", 6, nil, "kaze-syncc")
	err := r.run(InstallOptions{From: p})
	if err == nil || !strings.Contains(err.Error(), "upgrade the server first") {
		t.Fatalf("err %v", err)
	}
	if r.seq() != "" || read(t, filepath.Join(r.target, "kaze-syncc")) != "old" {
		t.Fatalf("changed something: %s", r.seq())
	}
	if err := r.run(InstallOptions{From: p, Force: true}); err != nil {
		t.Fatalf("--force: %v", err)
	}
	// A server that cannot be asked only warns.
	r.env.ServerAgent = func(context.Context, Device) (int, error) { return 0, errors.New("offline") }
	if err := r.run(InstallOptions{From: p}); err != nil || !strings.Contains(r.out.String(), "warning") {
		t.Fatalf("offline: %v %q", err, r.out.String())
	}
}

func TestInstallFailureInTheMiddleRestoresTheOldVersion(t *testing.T) {
	t.Parallel()
	r := newRig(t, "windows")
	r.target = t.TempDir()
	r.enrolled()
	for _, n := range []string{"kaze-syncc.exe", "kaze-syncc-agent.exe"} {
		os.WriteFile(filepath.Join(r.target, n), []byte("old "+n), 0o755)
	}
	// A directory in the way of the second old file's resting place makes the second move fail.
	block := filepath.Join(r.target, "kaze-syncc-agent.exe"+oldSuffix)
	os.MkdirAll(filepath.Join(block, "x"), 0o755)
	err := r.run(InstallOptions{From: r.pkg("windows", 6, nil, "kaze-syncc.exe", "kaze-syncc-agent.exe")})
	if err == nil {
		t.Fatal("no error")
	}
	for _, n := range []string{"kaze-syncc.exe", "kaze-syncc-agent.exe"} {
		if got := read(t, filepath.Join(r.target, n)); got != "old "+n {
			t.Fatalf("%s: %q", n, got)
		}
	}
	if r.seq() != "stop | autostart "+filepath.Base(r.target)+"/kaze-syncc.exe" { // the old agent is started again
		t.Fatalf("events %s", r.seq())
	}
	ents, _ := os.ReadDir(r.target)
	if len(ents) != 3 { // two programs and the blocking directory, no .new or .old
		t.Fatalf("leftovers: %v", ents)
	}
}

func TestInstallRejectsBadInstallersBeforeTouchingAnything(t *testing.T) {
	t.Parallel()
	r := newRig(t, "linux")
	r.target = t.TempDir()
	os.WriteFile(filepath.Join(r.target, "kaze-syncc"), []byte("old"), 0o755)
	good := r.pkg("linux", 6, nil, "kaze-syncc")
	raw, _ := os.ReadFile(good)
	trunc := filepath.Join(t.TempDir(), "t")
	os.WriteFile(trunc, raw[:len(raw)-30], 0o644)
	for name, o := range map[string]InstallOptions{
		"wrong os":        {From: r.pkg("windows", 6, nil, "kaze-syncc.exe", "kaze-syncc-agent.exe")},
		"missing binary":  {From: r.pkg("linux", 6, nil, "other")},
		"truncated":       {From: trunc},
		"not an archive":  {From: os.Args[0]},
		"no from":         {},
		"nonexistent":     {From: filepath.Join(t.TempDir(), "none")},
		"duplicate entry": {From: r.pkg("linux", 6, nil, "kaze-syncc", "kaze-syncc")},
	} {
		if err := r.run(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if r.seq() != "" || read(t, filepath.Join(r.target, "kaze-syncc")) != "old" {
		t.Fatalf("something changed: %s", r.seq())
	}
	if ents, _ := os.ReadDir(r.target); len(ents) != 1 {
		t.Fatalf("leftovers: %v", ents)
	}
}

func TestInstallRegistrationProblemsStillInstallTheFiles(t *testing.T) {
	t.Parallel()
	r := newRig(t, "linux")
	r.env.Enroll = func(context.Context, string, string, string) error { return errors.New("code expired") }
	err := r.run(InstallOptions{From: r.pkg("linux", 6, &testReg, "kaze-syncc")})
	if err == nil || !strings.Contains(err.Error(), "code expired") || !strings.Contains(r.out.String(), "add the device again") {
		t.Fatalf("%v %q", err, r.out.String())
	}
	if read(t, filepath.Join(r.def, "kaze-syncc")) != "new kaze-syncc" || strings.Contains(r.seq(), "autostart") {
		t.Fatalf("events %s", r.seq())
	}
	// A tampered block is not used at all.
	r2 := newRig(t, "linux")
	bad := testReg
	bad.Code = "x; rm -rf /"
	if err := r2.run(InstallOptions{From: r2.pkg("linux", 6, &bad, "kaze-syncc")}); err == nil || strings.Contains(r2.seq(), "enroll") {
		t.Fatalf("%v: %s", err, r2.seq())
	}
}

func TestInstallWithoutRegistrationHintsOrPrompts(t *testing.T) {
	t.Parallel()
	r := newRig(t, "linux")
	if err := r.run(InstallOptions{From: r.pkg("linux", 6, nil, "kaze-syncc")}); err != nil || r.seq() != "stop" || !strings.Contains(r.out.String(), "not registered yet") {
		t.Fatalf("%v: %s %q", err, r.seq(), r.out.String())
	}
	r = newRig(t, "linux")
	o := InstallOptions{From: r.pkg("linux", 6, nil, "kaze-syncc"), Prompt: func() (string, string, string, bool) { return "https://s:1", "ABCD-1234", "", true }}
	if err := r.run(o); err != nil || r.seq() != "stop | enroll https://s:1 ABCD-1234  | autostart default/kaze-syncc" {
		t.Fatalf("%v: %s", err, r.seq())
	}
}

func TestInstallSaysWhenTheAgentWouldStopAtLogout(t *testing.T) {
	t.Parallel()
	r := newRig(t, "linux")
	r.env.Linger = func() (bool, bool) { return false, true }
	if err := r.run(InstallOptions{From: r.pkg("linux", 6, &testReg, "kaze-syncc")}); err != nil || !strings.Contains(r.out.String(), "loginctl enable-linger") {
		t.Fatalf("%v %q", err, r.out.String())
	}
}

func TestParseInstallArgs(t *testing.T) {
	t.Parallel()
	o, err := ParseInstallArgs([]string{"https://s:1", "ABCD", "--fingerprint", "sha256:aa", "--from", "/x", "--dir", "/d", "--force"}, "/self")
	if err != nil || o.From != "/x" || o.Dir != "/d" || !o.Force || o.Server != "https://s:1" || o.Code != "ABCD" || o.Fingerprint != "sha256:aa" {
		t.Fatalf("%+v %v", o, err)
	}
	if o, err := ParseInstallArgs(nil, "/self"); err != nil || o.From != "/self" || o.Server != "" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, bad := range [][]string{{"https://s"}, {"--fingerprint", "x"}, {"--bogus"}, {"a", "b", "c"}} {
		if _, err := ParseInstallArgs(bad, ""); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestParseEnrollCommand(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]string{
		"kaze-syncc enroll https://10.0.0.3:8443 ABCD-EFGH --fingerprint sha256:ab":      "https://10.0.0.3:8443 ABCD-EFGH sha256:ab",
		"  .\\kaze-syncc.exe enroll 'https://h:1' \"ABCD\" --fingerprint=sha256:cd \r\n": "https://h:1 ABCD sha256:cd",
		"C:\\Program Files\\kaze-syncc.exe enroll https://h ABCD":                        "https://h ABCD ",
	} {
		s, c, fp, err := ParseEnrollCommand(line)
		if got := s + " " + c + " " + fp; err != nil || got != want {
			t.Errorf("%q: %q %v", line, got, err)
		}
	}
	for _, bad := range []string{"", "https://h ABCD", "kaze-syncc enroll https://h", "kaze-syncc enroll https://h ABCD --x"} {
		if _, _, _, err := ParseEnrollCommand(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAutostartRecordDirectories(t *testing.T) {
	t.Parallel()
	if got := unitExecDir("[Service]\nExecStart=\"/opt/kaze sync/kaze-syncc\" run\nRestart=on-failure\n"); got != "/opt/kaze sync" {
		t.Fatalf("unit dir %q", got)
	}
	if got := unitExecDir("[Service]\nExecStart=/usr/bin/x run\n"); got != "/usr/bin" || unitExecDir("[Unit]\n") != "" {
		t.Fatalf("unit dir %q", got)
	}
	if got := quotedCommandPath(`"C:\Users\a b\kaze-syncc-agent.exe" run`); got != `C:\Users\a b\kaze-syncc-agent.exe` {
		t.Fatalf("run value %q", got)
	}
}

func TestServerAgentVersionFromTheHeader(t *testing.T) {
	t.Parallel()
	header := ""
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/api/v1/client/package" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "bad", 400)
			return
		}
		if header != "" {
			w.Header().Set(proto.ServerAgentVersionHeader, header)
		}
	}))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	dev := Device{Server: srv.URL, Token: "tok", Fingerprint: fmt.Sprintf("sha256:%x", sum)}
	if v, err := serverAgentVersion(context.Background(), dev); err != nil || v != proto.LastAgentVersionWithoutHeader {
		t.Fatalf("no header: %d %v", v, err)
	}
	header = "9"
	if v, err := serverAgentVersion(context.Background(), dev); err != nil || v != 9 {
		t.Fatalf("header: %d %v", v, err)
	}
	dev.Token = "wrong"
	if _, err := serverAgentVersion(context.Background(), dev); err == nil {
		t.Fatal("bad token accepted")
	}
}
