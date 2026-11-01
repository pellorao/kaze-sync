// Package vmctl drives the OpenWrt test VM: vmrun (through WSL interop) and
// SSH with a pinned host key.
package vmctl

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"kaze-sync/e2e/config"
)

type VM struct {
	Cfg        *config.Config
	HostKey    string // "ssh-ed25519 AAAA..."; empty only while the builder discovers it
	IP         string
	knownHosts string
}

func New(c *config.Config) *VM { return &VM{Cfg: c, HostKey: c.SSH.HostKey} }

// Name is the VM's file name without extension; it doubles as HostKeyAlias.
func (v *VM) Name() string {
	w := v.Cfg.Vmx
	return strings.TrimSuffix(w[strings.LastIndexAny(w, `\/`)+1:], ".vmx")
}

// Vmrun runs vmrun for this VM; the command goes first, then the .vmx, then extra arguments.
func (v *VM) Vmrun(cmd string, extra ...string) (string, error) {
	args := append([]string{"-T", "ws", cmd}, extra...)
	if cmd != "list" {
		args = append([]string{"-T", "ws", cmd, v.Cfg.Vmx}, extra...)
	}
	var out bytes.Buffer
	c := exec.Command(v.Cfg.Vmrun, args...)
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return strings.TrimSpace(out.String()), err
}

func (v *VM) Running() bool {
	out, _ := v.Vmrun("list")
	return strings.Contains(strings.ToLower(out), strings.ToLower(v.Cfg.Vmx))
}

// WaitIP sets the VM's address: it is fixed by the image (the VMware Tools
// of OpenWrt cannot answer vmrun getGuestIPAddress).
func (v *VM) WaitIP(time.Duration) error {
	v.IP = v.Cfg.IP
	return nil
}

// WaitSSH retries a trivial command until SSH answers with the pinned key.
func (v *VM) WaitSSH(timeout time.Duration) error {
	for deadline := time.Now().Add(timeout); ; time.Sleep(3 * time.Second) {
		_, err := v.SSH("true")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ssh not ready within %v: %w", timeout, err)
		}
	}
}

func (v *VM) sshArgs() []string {
	if v.knownHosts == "" {
		f, _ := os.CreateTemp("", "kaze-known-hosts-")
		fmt.Fprintf(f, "%s %s\n", v.Name(), v.HostKey)
		f.Close()
		v.knownHosts = f.Name()
	}
	return []string{"-i", v.Cfg.SSH.IdentityFile, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + v.knownHosts, "-o", "HostKeyAlias=" + v.Name(),
		"-o", "ConnectTimeout=5", "-p", fmt.Sprint(v.Cfg.SSH.Port), v.Cfg.SSH.User + "@" + v.IP}
}

// Close removes the temporary known_hosts file.
func (v *VM) Close() {
	if v.knownHosts != "" {
		os.Remove(v.knownHosts)
	}
}

// SSH runs a shell command in the guest and returns its combined output.
func (v *VM) SSH(command string) (string, error) {
	var out bytes.Buffer
	c := exec.Command("ssh", append(v.sshArgs(), command)...)
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return strings.TrimSpace(out.String()), err
}

// Put copies a local file to the guest through "cat >" (dropbear has no sftp).
func (v *VM) Put(local, remote string, mode os.FileMode) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	var out bytes.Buffer
	c := exec.Command("ssh", append(v.sshArgs(), fmt.Sprintf("cat > %[1]s && chmod %[2]o %[1]s", remote, mode))...)
	c.Stdin, c.Stdout, c.Stderr = f, &out, &out
	if err := c.Run(); err != nil {
		return fmt.Errorf("put %s: %w: %s", path.Base(remote), err, out.String())
	}
	return nil
}

// Scan reads the guest's ed25519 host key (the builder pins it after the first boot).
func (v *VM) Scan() (string, error) {
	out, err := exec.Command("ssh-keyscan", "-t", "ed25519", "-p", fmt.Sprint(v.Cfg.SSH.Port), v.IP).Output()
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) == 3 && !strings.HasPrefix(l, "#") {
			return f[1] + " " + f[2], nil
		}
	}
	return "", fmt.Errorf("no host key from %s", v.IP)
}

// Stop powers the VM off hard, which is how the clean baseline is protected.
func (v *VM) Stop() { v.Vmrun("stop", "hard") }

// Revert restores the clean snapshot.
func (v *VM) Revert() error {
	_, err := v.Vmrun("revertToSnapshot", v.Cfg.Snapshot)
	return err
}

// WSLPath converts a Windows path.
func WSLPath(win string) (string, error) {
	out, err := exec.Command("wslpath", "-u", win).Output()
	return filepath.Clean(strings.TrimSpace(string(out))), err
}
