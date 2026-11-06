// Command e2e runs the end-to-end scenarios against the OpenWrt test VM
// (docs/plan.md, M3): it restores the clean snapshot, installs the real
// server with procd, drives it through the Web UI and syncs with real
// clients, then powers the VM off hard and restores the snapshot.
//
// Usage: go run ./e2e -config ~/.kaze-sync/config/e2e.json
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"kaze-sync/e2e/config"
	"kaze-sync/e2e/vmctl"
)

// Tunables.
const (
	dataDir     = "/mnt/data/kaze-sync"
	remoteBin   = "/root/kaze-syncd"
	bootTimeout = 3 * time.Minute
)

type driver struct {
	cfg     *config.Config
	vm      *vmctl.VM
	tmp     string // local scratch
	logs    string // collected logs
	bins    struct{ syncd, linux, windows, winAgent string }
	port    string
	fp      string
	base    string
	admin   string
	web     *webClient
	agents  []*agent
	fails   []string
	current string
	only    string
	metrics []string
	m7      *m7State
}

func main() {
	cfgPath := flag.String("config", filepath.Join(os.Getenv("HOME"), ".kaze-sync/config/e2e.json"), "e2e config file")
	logs := flag.String("logs", "", "directory for collected logs (default: a new temp directory)")
	only := flag.String("only", "", "stop after a stage while debugging: setup (up to the admin login), sync (before security, sysupgrade and the certificate), m4 (only the M4 and M5 scenarios), m5 (only the M5 scenarios), m6, m7 (the M7 scenarios, then the backup restore), upgrade (only the upgrade phase)")
	flag.Parse()
	log.SetFlags(log.Ltime)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	d := &driver{cfg: cfg, vm: vmctl.New(cfg)}
	if d.cfg.SSH.HostKey == "" {
		log.Fatal("ssh.hostKey is empty; run ./e2e/vm first")
	}
	// Scratch data stays in the cache directory (a tmpfs) so that it does not wear the SSD.
	os.MkdirAll(cfg.Cache, 0o755)
	d.tmp, _ = os.MkdirTemp(cfg.Cache, "run-")
	defer os.RemoveAll(d.tmp)
	d.logs = *logs
	if d.logs == "" {
		d.logs = filepath.Join(cfg.Cache, "logs")
	}
	d.only = *only
	os.MkdirAll(d.logs, 0o755)
	// Whatever happens, the VM ends powered off and reverted.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; d.cleanup(); os.Exit(130) }()
	start := time.Now()
	d.run()
	d.cleanup()
	for _, m := range d.metrics {
		log.Print("METRIC ", m)
	}
	log.Printf("done in %v; logs in %s", time.Since(start).Round(time.Second), d.logs)
	if len(d.fails) > 0 {
		log.Printf("FAILED (%d):\n  %s", len(d.fails), strings.Join(d.fails, "\n  "))
		os.Exit(1)
	}
	log.Print("ALL PASSED")
}

func (d *driver) run() {
	if !d.step("build binaries", d.build) {
		return
	}
	if d.only != "upgrade" {
		d.mainPhase()
		d.restorePhase()
	}
	if d.only == "" || d.only == "upgrade" {
		d.upgradePhase()
	}
}

// metric records a measurement for the final report.
func (d *driver) metric(format string, a ...any) {
	d.metrics = append(d.metrics, fmt.Sprintf(format, a...))
}

// step runs one named check and records a failure; it reports success.
func (d *driver) step(name string, f func() error) bool {
	d.current = name
	t := time.Now()
	if err := f(); err != nil {
		log.Printf("FAIL %s: %v", name, err)
		d.fails = append(d.fails, name+": "+err.Error())
		return false
	}
	log.Printf("PASS %s (%v)", name, time.Since(t).Round(time.Second))
	return true
}

// cleanup stops every process the driver started and restores the snapshot.
func (d *driver) cleanup() {
	for _, a := range d.agents {
		a.stop(true)
	}
	d.agents = nil
	d.collectServerLog()
	d.vm.Stop()
	if err := d.vm.Revert(); err != nil {
		log.Print("revert failed: ", err)
	}
	d.vm.Close()
	log.Print("VM powered off and reverted to ", d.cfg.Snapshot)
}

func (d *driver) collectServerLog() {
	if d.vm.IP == "" || !d.vm.Running() {
		return
	}
	if out, err := d.vm.SSH("logread"); err == nil {
		os.WriteFile(filepath.Join(d.logs, fmt.Sprintf("server-%d.log", time.Now().Unix())), []byte(out), 0o644)
	}
}

func (d *driver) build() error {
	d.bins.syncd = filepath.Join(d.tmp, "kaze-syncd")
	d.bins.linux = filepath.Join(d.tmp, "kaze-syncc")
	d.bins.windows = filepath.Join(d.tmp, "kaze-syncc.exe")
	d.bins.winAgent = filepath.Join(d.tmp, "kaze-syncc-agent.exe")
	for _, b := range []struct{ goos, out, pkg, ldflags string }{
		{"linux", d.bins.syncd, "./cmd/kaze-syncd", ""}, {"linux", d.bins.linux, "./cmd/kaze-syncc", ""}, {"windows", d.bins.windows, "./cmd/kaze-syncc", ""},
		{"windows", d.bins.winAgent, "./cmd/kaze-syncc", "-H=windowsgui"}, // the windowless agent of the Run key (design 2.1)
	} {
		c := exec.Command("go", "build", "-ldflags", b.ldflags, "-o", b.out, b.pkg)
		c.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+b.goos, "GOARCH=amd64")
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("go build %s: %w: %s", b.pkg, err, out)
		}
	}
	return checkGUI(d.bins.winAgent)
}

// checkGUI verifies that the PE file is a GUI-subsystem program (no console window when started by the Run key).
func checkGUI(exe string) error {
	b, err := os.ReadFile(exe)
	if err != nil || len(b) < 0x40 {
		return fmt.Errorf("read %s: %v", exe, err)
	}
	pe := int(binary.LittleEndian.Uint32(b[0x3c:]))
	if sub := binary.LittleEndian.Uint16(b[pe+24+68:]); sub != 2 { // IMAGE_SUBSYSTEM_WINDOWS_GUI
		return fmt.Errorf("%s has subsystem %d, want 2 (GUI)", exe, sub)
	}
	return nil
}

// boot restores the snapshot, starts the VM and waits for SSH.
func (d *driver) boot() error {
	d.vm.Stop()
	if err := d.vm.Revert(); err != nil {
		return err
	}
	if _, err := d.vm.Vmrun("start", "nogui"); err != nil {
		return err
	}
	return d.waitBack(true)
}

func (d *driver) waitBack(first bool) error {
	if err := d.vm.WaitIP(bootTimeout); err != nil {
		return err
	}
	return d.vm.WaitSSH(bootTimeout)
}

// sh runs a command in the VM and fails with its output.
func (d *driver) sh(format string, a ...any) (string, error) {
	cmd := fmt.Sprintf(format, a...)
	out, err := d.vm.SSH(cmd)
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", cmd, err, out)
	}
	return out, nil
}

func (d *driver) shouldFail(cmd, wantInOutput string) error {
	out, err := d.vm.SSH(cmd)
	if err == nil {
		return fmt.Errorf("%q succeeded but should have failed: %s", cmd, out)
	}
	if wantInOutput != "" && !strings.Contains(out, wantInOutput) {
		return fmt.Errorf("%q failed without %q: %s", cmd, wantInOutput, out)
	}
	return nil
}

func eq(what, got, want string) error {
	if got != want {
		return fmt.Errorf("%s = %q, want %q", what, got, want)
	}
	return nil
}

// waitFor polls until f reports true or the timeout passes.
func waitFor(what string, timeout time.Duration, f func() (bool, error)) error {
	var last error
	for deadline := time.Now().Add(timeout); ; time.Sleep(2 * time.Second) {
		ok, err := f()
		if ok {
			return nil
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s (last: %v)", what, last)
		}
	}
}
