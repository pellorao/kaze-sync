// Command vm builds the kaze-sync-alpine test VM from the Alpine NoCloud cloud
// image without console interaction, snapshots it as the clean baseline,
// verifies SSH with the pinned host key and writes the e2e config.
//
// Usage: go run ./e2e/vm -config ~/.config/kaze-sync-e2e/vm.json
package main

import (
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Tunables.
const (
	vmwareDir      = "/mnt/c/Program Files (x86)/VMware/VMware Workstation"
	vmDirWin       = `C:\Users\pello\Documents\Virtual Machines\kaze-sync-alpine`
	vmName         = "kaze-sync-alpine"
	snapshot       = "kaze-sync-clean"
	imageURL       = "https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/cloud/alpine-3.24.3-x86_64-tiny-r0.qcow2"
	repoURL        = "https://dl-cdn.alpinelinux.org/alpine/v3.24"
	diskSize       = "4G"
	toolsDir       = ".local/opt/kaze-sync-tools" // relative to $HOME, from dpkg -x of qemu-utils and xorriso
	identityFile   = ".ssh/kaze-sync-e2e"         // relative to $HOME
	user           = "tester"
	windowsWorkDir = `C:\Users\pello\AppData\Local\Temp\kaze-sync-e2e`
	installTimeout = 10 * time.Minute
	sshTimeout     = 2 * time.Minute
)

var (
	home         = os.Getenv("HOME")
	vmrunExe     = vmwareDir + "/vmrun.exe"
	vdiskmanager = vmwareDir + "/vmware-vdiskmanager.exe"
)

func main() {
	config := flag.String("config", filepath.Join(home, ".config/kaze-sync-e2e/vm.json"), "e2e config file to write")
	flag.Parse()
	log.SetFlags(log.Ltime)

	vmDir := strings.TrimSpace(run("wslpath", "-u", vmDirWin))
	vmx := vmDir + "/" + vmName + ".vmx"
	vmxWin := vmDirWin + `\` + vmName + ".vmx"
	if _, err := os.Stat(vmx); err == nil {
		log.Print("deleting previous VM")
		exec.Command(vmrunExe, "-T", "ws", "stop", vmxWin, "hard").Run() // fails when already off
		run(vmrunExe, "-T", "ws", "deleteVM", vmxWin)
	}
	must(os.RemoveAll(vmDir))
	must(os.MkdirAll(vmDir, 0o755))
	work, err := os.MkdirTemp("", "kaze-sync-vm-")
	must(err)
	defer os.RemoveAll(work)

	log.Print("downloading ", imageURL)
	qcow2 := filepath.Join(work, "image.qcow2")
	download(imageURL, qcow2)
	download(imageURL+".sha512", qcow2+".sha512")
	f, err := os.Open(qcow2)
	must(err)
	h := sha512.New()
	_, err = io.Copy(h, f)
	must(err)
	f.Close()
	want, err := os.ReadFile(qcow2 + ".sha512")
	must(err)
	if got := hex.EncodeToString(h.Sum(nil)); got != strings.Fields(string(want))[0] {
		log.Panicf("image sha512 mismatch: got %s", got)
	}

	log.Print("converting disk")
	qemuImg := filepath.Join(home, toolsDir, "usr/bin/qemu-img")
	run(qemuImg, "resize", "-f", "qcow2", qcow2, diskSize)
	run(qemuImg, "convert", "-f", "qcow2", "-O", "vmdk", qcow2, vmDir+"/tmp.vmdk")
	run(vdiskmanager, "-r", vmDirWin+`\tmp.vmdk`, "-t", "0", vmDirWin+`\`+vmName+".vmdk")
	must(os.Remove(vmDir + "/tmp.vmdk"))

	log.Print("building seed ISO")
	key := filepath.Join(home, identityFile)
	if _, err := os.Stat(key); err != nil {
		run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "kaze-sync-e2e", "-f", key)
	}
	pub, err := os.ReadFile(key + ".pub")
	must(err)
	hostKey := filepath.Join(work, "ssh_host_ed25519_key")
	run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", vmName, "-f", hostKey)
	hostPriv, err := os.ReadFile(hostKey)
	must(err)
	hostPub, err := os.ReadFile(hostKey + ".pub")
	must(err)
	seed := filepath.Join(work, "seed")
	must(os.Mkdir(seed, 0o755))
	must(os.WriteFile(seed+"/meta-data", []byte("instance-id: "+vmName+"-1\nlocal-hostname: "+vmName+"\n"), 0o644))
	must(os.WriteFile(seed+"/user-data", []byte(strings.NewReplacer("{repo}", repoURL, "{name}", vmName, "{user}", user,
		"{pub}", strings.TrimSpace(string(pub)), "{hostKey}", string(hostPriv), "{hostPub}", string(hostPub)).Replace(userData)), 0o644))
	run(filepath.Join(home, toolsDir, "usr/bin/xorriso"), "-as", "mkisofs", "-quiet", "-o", vmDir+"/seed.iso", "-V", "cidata", "-J", "-r", seed)
	must(os.WriteFile(vmx, []byte(fmt.Sprintf(vmxTemplate, vmName, vmName)), 0o644))

	log.Print("first boot: installing (guest powers off when done)")
	run(vmrunExe, "-T", "ws", "start", vmxWin, "nogui")
	deadline := time.Now().Add(installTimeout)
	for strings.Contains(strings.ToLower(run(vmrunExe, "-T", "ws", "list")), strings.ToLower(vmxWin)) {
		if time.Now().After(deadline) {
			exec.Command(vmrunExe, "-T", "ws", "stop", vmxWin, "hard").Run()
			log.Panicf("guest did not power off within %v, see %s/serial.log", installTimeout, vmDir)
		}
		time.Sleep(5 * time.Second)
	}

	data, err := os.ReadFile(vmx)
	must(err)
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "sata0:1.") {
			kept = append(kept, line)
		}
	}
	must(os.WriteFile(vmx, []byte(strings.Join(kept, "\n")), 0o644))
	must(os.Remove(vmDir + "/seed.iso"))
	run(vmrunExe, "-T", "ws", "snapshot", vmxWin, snapshot)

	log.Print("verifying from snapshot")
	knownHosts := filepath.Join(work, "known_hosts")
	must(os.WriteFile(knownHosts, []byte(vmName+" "+string(hostPub)), 0o600))
	run(vmrunExe, "-T", "ws", "start", vmxWin, "nogui")
	defer func() {
		run(vmrunExe, "-T", "ws", "stop", vmxWin, "hard")
		run(vmrunExe, "-T", "ws", "revertToSnapshot", vmxWin, snapshot)
		log.Print("VM powered off and reverted to ", snapshot)
	}()
	for deadline := time.Now().Add(sshTimeout); ; time.Sleep(3 * time.Second) {
		// getGuestIPAddress fails until the guest's tools are up.
		out, err := exec.Command(vmrunExe, "-T", "ws", "getGuestIPAddress", vmxWin, "-wait").CombinedOutput()
		ip := strings.TrimSpace(string(out))
		if err == nil {
			out, err = exec.Command("ssh", "-i", key, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
				"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+knownHosts, "-o", "HostKeyAlias="+vmName,
				"-o", "ConnectTimeout=5", "-p", "22", user+"@"+ip, "doas true && cat /etc/alpine-release").CombinedOutput()
		}
		if err == nil {
			log.Printf("SSH to %s with pinned host key and doas OK, Alpine %s", ip, bytes.TrimSpace(out))
			break
		}
		if time.Now().After(deadline) {
			log.Panicf("verification failed: %v: %s", err, out)
		}
	}

	type sshConfig struct {
		User         string `json:"user"`
		Port         int    `json:"port"`
		IdentityFile string `json:"identityFile"`
		HostKey      string `json:"hostKey"`
	}
	out, err := json.MarshalIndent(struct {
		Vmrun          string    `json:"vmrun"`
		Vmx            string    `json:"vmx"`
		Snapshot       string    `json:"snapshot"`
		SSH            sshConfig `json:"ssh"`
		WindowsWorkDir string    `json:"windowsWorkDir"`
	}{vmrunExe, vmxWin, snapshot, sshConfig{user, 22, key, strings.Join(strings.Fields(string(hostPub))[:2], " ")}, windowsWorkDir}, "", "  ")
	must(err)
	must(os.MkdirAll(filepath.Dir(*config), 0o755))
	must(os.WriteFile(*config, append(out, '\n'), 0o644))
	log.Print("wrote ", *config)
}

// run executes a command and returns its stdout, exiting on failure.
func run(name string, args ...string) string {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if strings.HasPrefix(name, home) {
		cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Join(home, toolsDir, "usr/lib/x86_64-linux-gnu"))
	}
	if err := cmd.Run(); err != nil {
		log.Panicf("%s %s: %v\n%s%s", filepath.Base(name), strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func download(url, path string) {
	resp, err := http.Get(url)
	must(err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Panicf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(path)
	must(err)
	_, err = io.Copy(f, resp.Body)
	must(err)
	must(f.Close())
}

// must, run and download panic rather than exit so that deferred cleanup runs.
func must(err error) {
	if err != nil {
		log.Panic(err)
	}
}

// userData runs once on first boot via Tiny Cloud, logging to the serial console.
const userData = `#!/bin/sh
exec >/dev/ttyS0 2>&1
set -ex
printf '%s\n' {repo}/main {repo}/community >/etc/apk/repositories
apk add doas open-vm-tools open-vm-tools-guestinfo open-vm-tools-vix
adduser -D -s /bin/sh {user}
sed -i 's/^{user}:!/{user}:*/' /etc/shadow
install -d -m 700 -o {user} -g {user} /home/{user}/.ssh
echo '{pub}' >/home/{user}/.ssh/authorized_keys
chown {user}:{user} /home/{user}/.ssh/authorized_keys
chmod 600 /home/{user}/.ssh/authorized_keys
cat >/etc/ssh/ssh_host_ed25519_key <<'EOF'
{hostKey}EOF
chmod 600 /etc/ssh/ssh_host_ed25519_key
cat >/etc/ssh/ssh_host_ed25519_key.pub <<'EOF'
{hostPub}EOF
echo 'permit nopass {user}' >/etc/doas.d/{user}.conf
rc-update add open-vm-tools default
rm -f /etc/localtime
echo UTC >/etc/timezone
echo {name} >/etc/hostname
poweroff
`

const vmxTemplate = `.encoding = "UTF-8"
config.version = "8"
virtualHW.version = "22"
displayName = "%s"
guestOS = "other6xlinux-64"
firmware = "efi"
uefi.secureBoot.enabled = "FALSE"
memsize = "1024"
numvcpus = "2"
pciBridge0.present = "TRUE"
pciBridge4.present = "TRUE"
pciBridge4.virtualDev = "pcieRootPort"
pciBridge4.functions = "8"
sata0.present = "TRUE"
sata0:0.present = "TRUE"
sata0:0.fileName = "%s.vmdk"
sata0:1.present = "TRUE"
sata0:1.deviceType = "cdrom-image"
sata0:1.fileName = "seed.iso"
ethernet0.present = "TRUE"
ethernet0.connectionType = "nat"
ethernet0.virtualDev = "vmxnet3"
ethernet0.addressType = "generated"
serial0.present = "TRUE"
serial0.fileType = "file"
serial0.fileName = "serial.log"
tools.syncTime = "FALSE"
rtc.diffFromUTC = "0"
msg.autoAnswer = "TRUE"
floppy0.present = "FALSE"
`
