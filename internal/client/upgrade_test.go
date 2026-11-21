package client

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kaze-sync/internal/installer"
	"kaze-sync/internal/proto"
)

// upgradeRig is an installRig whose device is enrolled at a real TLS server that serves a client package.
type upgradeRig struct {
	*installRig
	body     []byte
	header   string // the Kaze-Package-Version header; empty: none
	status   int
	requests []string
	handoff  []string
}

func newUpgradeRig(t *testing.T, goos string) *upgradeRig {
	t.Helper()
	r := &upgradeRig{installRig: newRig(t, goos), status: 200}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.requests = append(r.requests, req.Method+" "+req.URL.RequestURI()+" "+req.Header.Get("Authorization"))
		if req.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		if r.status != 200 {
			http.NotFound(w, req)
			return
		}
		if r.header != "" {
			w.Header().Set(proto.PackageVersionHeader, r.header)
		}
		w.Write(r.body)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	if err := SaveDevice(r.state, Device{Server: srv.URL, Token: "tok", Fingerprint: fmt.Sprintf("sha256:%x", sum)}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *upgradeRig) serve(version string, agent int, goos string, names ...string) {
	var files []installer.File
	for _, n := range names {
		files = append(files, installer.File{Name: n, Data: []byte("new " + n)})
	}
	b, err := installer.Build([]byte("MZ"), files, installer.Manifest{Version: version, AgentVersion: agent, OS: goos, Arch: "amd64"}, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	r.body, r.header = b, version
}

func (r *upgradeRig) upgrade(current string, force bool) error {
	up := upgradeEnv{Current: current, Handoff: func(file string, force bool) error {
		r.handoff = append(r.handoff, fmt.Sprintf("%s force=%v", filepath.Base(file), force))
		return nil
	}}
	return upgrade(context.Background(), UpgradeOptions{Force: force, Out: &r.out}, r.env, up)
}

func (r *upgradeRig) leftovers() []string {
	m, _ := filepath.Glob(filepath.Join(r.state, "upgrade-*"))
	return m
}

func TestUpgradeFetchesVerifiesAndInstalls(t *testing.T) {
	t.Parallel()
	r := newUpgradeRig(t, "linux")
	r.target = t.TempDir()
	os.WriteFile(filepath.Join(r.target, "kaze-syncc"), []byte("old"), 0o755)
	r.serve("0.2.0", 6, "linux", "kaze-syncc")
	before := read(t, filepath.Join(r.state, "device.json"))
	if err := r.upgrade("0.1.0", false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(r.target, "kaze-syncc")); got != "new kaze-syncc" {
		t.Fatalf("installed %q", got)
	}
	if want := "GET /api/v1/client/package?os=linux Bearer tok"; len(r.requests) != 1 || r.requests[0] != want {
		t.Fatalf("requests %v", r.requests)
	}
	if r.seq() != "stop | autostart "+filepath.Base(r.target)+"/kaze-syncc" {
		t.Fatalf("events %s", r.seq())
	}
	out := r.out.String()
	if !strings.Contains(out, "0.1.0 -> 0.2.0") || strings.Contains(out, "you can delete") {
		t.Fatalf("output %q", out)
	}
	if read(t, filepath.Join(r.state, "device.json")) != before || len(r.leftovers()) != 0 {
		t.Fatalf("device.json changed or a download is left: %v", r.leftovers())
	}
}

func TestUpgradeStopsWhenTheServerHasNothingNewer(t *testing.T) {
	t.Parallel()
	for _, offered := range []string{"0.1.0", "0.0.9"} {
		r := newUpgradeRig(t, "linux")
		r.serve(offered, 6, "linux", "kaze-syncc")
		if err := r.upgrade("0.1.0", false); err != nil {
			t.Fatal(err)
		}
		if r.seq() != "" || !strings.Contains(r.out.String(), "already up to date") || len(r.leftovers()) != 0 {
			t.Fatalf("%s: events %q, output %q", offered, r.seq(), r.out.String())
		}
	}
	// A lying header must not decide: the verified file's own version does.
	r := newUpgradeRig(t, "linux")
	r.serve("0.1.0", 6, "linux", "kaze-syncc")
	r.header = "9.9.9"
	if err := r.upgrade("0.1.0", false); err != nil || r.seq() != "" || len(r.leftovers()) != 0 {
		t.Fatalf("%v: %q %v", err, r.seq(), r.leftovers())
	}
}

func TestUpgradeForceReinstallsTheSameVersion(t *testing.T) {
	t.Parallel()
	r := newUpgradeRig(t, "linux")
	r.target = t.TempDir()
	r.serve("0.1.0", 6, "linux", "kaze-syncc")
	if err := r.upgrade("0.1.0", true); err != nil {
		t.Fatal(err)
	}
	if r.seq() == "" || read(t, filepath.Join(r.target, "kaze-syncc")) != "new kaze-syncc" {
		t.Fatalf("events %q", r.seq())
	}
}

func TestUpgradeRefusesWhatIsNotAnInstalledGoodPackage(t *testing.T) {
	t.Parallel()
	r := newUpgradeRig(t, "linux")
	r.serve("0.2.0", 6, "linux", "kaze-syncc")
	r.body[len(r.body)-40] ^= 1 // inside the manifest, or the payload, either is caught
	if err := r.upgrade("0.1.0", false); err == nil || r.seq() != "" || len(r.leftovers()) != 0 {
		t.Fatalf("damaged package: %v %q %v", err, r.seq(), r.leftovers())
	}
	r.serve("0.2.0", 6, "windows", "kaze-syncc.exe", "kaze-syncc-agent.exe")
	if err := r.upgrade("0.1.0", false); err == nil || !strings.Contains(err.Error(), "windows/amd64") || r.seq() != "" || len(r.leftovers()) != 0 {
		t.Fatalf("package of another system: %v %q", err, r.seq())
	}
	r.status = 404
	if err := r.upgrade("0.1.0", false); err == nil || !strings.Contains(err.Error(), "client-pkg") {
		t.Fatalf("no package: %v", err)
	}
	r.status = 200
	r.body = []byte("<html>not an installer</html>")
	if err := r.upgrade("0.1.0", false); err == nil || r.seq() != "" || len(r.leftovers()) != 0 {
		t.Fatalf("junk: %v", err)
	}
}

func TestUpgradeNeedsAnEnrolledDevice(t *testing.T) {
	t.Parallel()
	r := newRig(t, "linux")
	err := upgrade(context.Background(), UpgradeOptions{}, r.env, upgradeEnv{Current: "0.1.0"})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("%v", err)
	}
}

func TestUpgradeOnWindowsHandsTheInstallerOver(t *testing.T) {
	t.Parallel()
	r := newUpgradeRig(t, "windows")
	r.target = t.TempDir()
	r.serve("0.2.0", 6, "windows", "kaze-syncc.exe", "kaze-syncc-agent.exe")
	if err := r.upgrade("0.1.0", true); err != nil {
		t.Fatal(err)
	}
	if len(r.handoff) != 1 || r.handoff[0] != "kaze-sync-setup.exe force=true" || r.seq() != "" {
		t.Fatalf("handoff %v, events %q", r.handoff, r.seq())
	}
	left := r.leftovers()
	if len(left) != 1 {
		t.Fatalf("the setup must stay for the new process: %v", left)
	}
	if _, err := os.Stat(filepath.Join(left[0], "kaze-sync-setup.exe")); err != nil {
		t.Fatal(err)
	}
	// The next upgrade clears what this one left.
	if err := r.upgrade("0.2.0", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left[0]); err != nil {
		t.Fatalf("nothing newer was offered, the old download is not touched: %v", err)
	}
	r.serve("0.3.0", 6, "windows", "kaze-syncc.exe", "kaze-syncc-agent.exe")
	if err := r.upgrade("0.2.0", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left[0]); !os.IsNotExist(err) || len(r.leftovers()) != 1 {
		t.Fatalf("stale download %v: %v", err, r.leftovers())
	}
}

func TestParseUpgradeArgs(t *testing.T) {
	t.Parallel()
	if o, err := ParseUpgradeArgs(nil); err != nil || o.Force {
		t.Fatalf("%v %v", o, err)
	}
	if o, err := ParseUpgradeArgs([]string{"--force"}); err != nil || !o.Force {
		t.Fatalf("%v %v", o, err)
	}
	for _, bad := range [][]string{{"--bogus"}, {"x"}, {"-x", "--force"}} {
		if _, err := ParseUpgradeArgs(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// The real server's route, with a device that enrolled through the real Web UI and Enroll.
func TestUpgradeFromTheRealServer(t *testing.T) {
	t.Parallel()
	e := newCEnv(t)
	r := newRig(t, "linux")
	r.target = t.TempDir()
	if err := Enroll(context.Background(), r.state, e.ts.URL, e.code(), e.fp); err != nil {
		t.Fatal(err)
	}
	b, err := installer.Build([]byte("#!/bin/sh\n"), []installer.File{{Name: "kaze-syncc", Data: []byte("from the server")}},
		installer.Manifest{Version: "9.0.0", AgentVersion: proto.AgentVersion, OS: "linux", Arch: "amd64"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.data, "dl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.data, "dl", "client-linux-amd64.sh"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	up := upgradeEnv{Current: "0.1.0"}
	if err := upgrade(context.Background(), UpgradeOptions{Out: &r.out}, r.env, up); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(r.target, "kaze-syncc")); got != "from the server" || !strings.Contains(r.out.String(), "0.1.0 -> 9.0.0") {
		t.Fatalf("installed %q, output %q", got, r.out.String())
	}
	r.out.Reset()
	if err := upgrade(context.Background(), UpgradeOptions{Out: &r.out}, r.env, upgradeEnv{Current: "9.0.0"}); err != nil || !strings.Contains(r.out.String(), "already up to date") {
		t.Fatalf("%v %q", err, r.out.String())
	}
	if err := os.Remove(filepath.Join(e.data, "dl", "client-linux-amd64.sh")); err != nil {
		t.Fatal(err)
	}
	if err := upgrade(context.Background(), UpgradeOptions{}, r.env, up); err == nil || !strings.Contains(err.Error(), "no client package") {
		t.Fatalf("server without a package: %v", err)
	}
}
