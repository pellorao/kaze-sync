// Command vm builds the OpenWrt x86-64 test VM from the official image,
// configures it through files written into its root filesystem and SSH, snapshots it as the clean
// baseline, verifies SSH with the pinned host key and stores the host key in
// the e2e config. Everything large is built in the cache directory, which
// should be a tmpfs (/dev/shm), so that only the final disks reach the SSD.
//
// Usage: go run ./e2e/vm -config ~/.kaze-sync/config/e2e.json
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"kaze-sync/e2e/config"
	"kaze-sync/e2e/vmctl"
)

// Tunables.
const (
	memoryMiB   = 512    // the server is meant for a 512 MB VPS
	dataDisk    = "2G"   // sparse: only what is written is stored
	swapDisk    = "512M" // a block device, because swap files may not have holes and filling one would write 512 MB
	bootTimeout = 3 * time.Minute
)

func main() {
	cfgPath := flag.String("config", filepath.Join(os.Getenv("HOME"), ".kaze-sync/config/e2e.json"), "e2e config file")
	flag.Parse()
	log.SetFlags(log.Ltime)
	if err := build(*cfgPath); err != nil {
		log.Fatal(err) // deferred cleanups ran inside build
	}
}

func build(cfgPath string) (err error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	vm := vmctl.New(cfg)
	defer vm.Close()
	vmDirWin := cfg.Vmx[:strings.LastIndex(cfg.Vmx, `\`)]
	vmDir, err := vmctl.WSLPath(vmDirWin)
	if err != nil {
		return err
	}
	vmx := filepath.Join(vmDir, vm.Name()+".vmx")
	if _, err := os.Stat(vmx); err == nil {
		log.Print("deleting previous VM")
		vm.Stop()
		if _, err := vm.Vmrun("deleteVM"); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(vmDir); err != nil {
		return err
	}
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Cache, 0o755); err != nil {
		return err
	}
	key, err := os.ReadFile(cfg.SSH.IdentityFile + ".pub")
	if err != nil {
		return err
	}

	log.Print("fetching the official image ", vmctl.ImageFile)
	gz, err := vmctl.Image(cfg)
	if err != nil {
		return err
	}
	raw := filepath.Join(cfg.Cache, "system.img")
	data := filepath.Join(cfg.Cache, "data.img")
	defer os.Remove(raw)
	defer os.Remove(data)
	qemuEnv := append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Join(cfg.Tools, "usr/lib/x86_64-linux-gnu"))
	sh := func(name string, args ...string) error {
		c := exec.Command(name, args...)
		c.Env = qemuEnv
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
		}
		return nil
	}
	log.Print("converting the disks")
	if err := sh("sh", "-c", fmt.Sprintf("gzip -dc %q > %q; true", gz, raw)); err != nil { // gzip warns about trailing zeros
		return err
	}
	if err := inject(raw, cfg.IP, strings.TrimSpace(string(key)), cfg.Cache); err != nil {
		return err
	}
	if err := sh("sh", "-c", fmt.Sprintf("truncate -s %s %q && mkfs.ext4 -q -F -L kazedata %q", dataDisk, data, data)); err != nil {
		return err
	}
	swap := filepath.Join(cfg.Cache, "swap.img")
	defer os.Remove(swap)
	if err := sh("sh", "-c", fmt.Sprintf("truncate -s %s %q && mkswap %q >/dev/null", swapDisk, swap, swap)); err != nil {
		return err
	}
	qemuImg := filepath.Join(cfg.Tools, "usr/bin/qemu-img")
	for img, name := range map[string]string{raw: vm.Name(), data: vm.Name() + "-data", swap: vm.Name() + "-swap"} {
		if err := sh(qemuImg, "convert", "-f", "raw", "-O", "vmdk", "-o", "subformat=monolithicSparse", img, filepath.Join(vmDir, name+".vmdk")); err != nil {
			return err
		}
	}
	if err := os.WriteFile(vmx, []byte(fmt.Sprintf(vmxTemplate, vm.Name(), memoryMiB, vm.Name(), vm.Name(), vm.Name())), 0o644); err != nil {
		return err
	}

	log.Print("first boot")
	if _, err := vm.Vmrun("start", "nogui"); err != nil {
		return err
	}
	defer func() { // the VM must not stay powered on, on any outcome
		vm.Stop()
		if err == nil {
			err = vm.Revert()
		}
		log.Print("VM powered off and reverted to ", cfg.Snapshot)
	}()
	vm.IP = cfg.IP
	if vm.HostKey, err = waitScan(vm); err != nil {
		return err
	}
	cfg.SSH.HostKey = vm.HostKey
	if err := vm.WaitSSH(bootTimeout); err != nil {
		return err
	}

	log.Print("configuring over SSH")
	if out, err := vm.SSH(setup); err != nil {
		return fmt.Errorf("setup: %w: %s", err, out)
	} else {
		log.Print(out)
	}
	vm.SSH("poweroff")
	for deadline := time.Now().Add(time.Minute); vm.Running() && time.Now().Before(deadline); time.Sleep(2 * time.Second) {
	}
	vm.Stop()
	if _, err := vm.Vmrun("snapshot", cfg.Snapshot); err != nil {
		return err
	}

	log.Print("verifying from the snapshot")
	if err := vm.Revert(); err != nil {
		return err
	}
	if _, err := vm.Vmrun("start", "nogui"); err != nil {
		return err
	}
	if err := vm.WaitSSH(bootTimeout); err != nil {
		return err
	}
	out, err := vm.SSH(`mount | grep ' /mnt/data '; free -m | sed -n '1,3p'; date +%Z`)
	if err != nil {
		return err
	}
	log.Printf("SSH to %s with the pinned host key OK:\n%s", vm.IP, out)
	return cfg.Save(cfgPath)
}

func waitScan(vm *vmctl.VM) (string, error) {
	var err error
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		var k string
		if k, err = vm.Scan(); err == nil {
			return k, nil
		}
	}
	return "", err
}

// setup runs once over SSH. The data disk (/dev/sdb, formatted on the host)
// and the swap disk (/dev/sdc) are mounted from rc.local, which runs before the
// kaze-sync init script and which sysupgrade keeps.
const setup = `set -e
uci set system.@system[0].hostname='kaze-sync-openwrt'
uci set system.@system[0].timezone='UTC'
uci set dhcp.lan.ignore='1'
uci set dropbear.@dropbear[0].PasswordAuth='off'
uci set dropbear.@dropbear[0].RootPasswordAuth='off'
uci commit
cat > /etc/rc.local <<'EOF'
mkdir -p /mnt/data
grep -q ' /mnt/data ' /proc/mounts || mount -t ext4 /dev/sdb /mnt/data
swapon /dev/sdc 2>/dev/null
exit 0
EOF
chmod +x /etc/rc.local
sh /etc/rc.local
mount | grep ' /mnt/data '
free -m | sed -n '1,3p'
. /etc/openwrt_release; echo $DISTRIB_DESCRIPTION
`

const vmxTemplate = `.encoding = "UTF-8"
config.version = "8"
virtualHW.version = "22"
displayName = "%s"
guestOS = "other6xlinux-64"
firmware = "efi"
uefi.secureBoot.enabled = "FALSE"
memsize = "%d"
mainMem.useNamedFile = "FALSE"
numvcpus = "2"
sata0.present = "TRUE"
sata0:0.present = "TRUE"
sata0:0.fileName = "%s.vmdk"
sata0:1.present = "TRUE"
sata0:1.fileName = "%s-data.vmdk"
sata0:2.present = "TRUE"
sata0:2.fileName = "%s-swap.vmdk"
ethernet0.present = "TRUE"
ethernet0.connectionType = "nat"
ethernet0.virtualDev = "e1000"
ethernet0.addressType = "generated"
tools.syncTime = "FALSE"
rtc.diffFromUTC = "0"
msg.autoAnswer = "TRUE"
floppy0.present = "FALSE"
`

// inject writes the first-boot network settings and the SSH key into the root
// filesystem (second partition, ext4) of the raw image with debugfs, so no
// console or root privileges are needed.
func inject(img, ip, pubkey, tmp string) error {
	const offset = 33280 * 512 // start of the rootfs partition in the official x86-64 image
	gw := ip[:strings.LastIndex(ip, ".")] + ".2"
	defaults := "uci -q delete network.lan.ipaddr\nuci set network.lan.proto=static\nuci set network.lan.ipaddr=" + ip +
		"\nuci set network.lan.netmask=255.255.255.0\nuci set network.lan.gateway=" + gw +
		"\nuci add_list network.lan.dns=" + gw + "\nuci commit network\nexit 0\n"
	files := map[string]string{"99-kaze": defaults, "authorized_keys": pubkey + "\n"}
	var cmds []string
	for name, body := range files {
		path := filepath.Join(tmp, "inject-"+name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return err
		}
		defer os.Remove(path)
		dst, mode := "etc/uci-defaults/"+name, "0100755"
		if name == "authorized_keys" {
			dst, mode = "etc/dropbear/"+name, "0100600"
		}
		cmds = append(cmds, "write "+path+" "+dst, "sif "+dst+" mode "+mode)
	}
	c := exec.Command("debugfs", "-w", "-f", "-", fmt.Sprintf("%s?offset=%d", img, offset))
	c.Stdin = strings.NewReader(strings.Join(cmds, "\n") + "\n")
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("debugfs: %w\n%s", err, out)
	}
	return nil
}
