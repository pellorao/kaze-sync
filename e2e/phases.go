package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// setupServer restores the snapshot and installs the real server.
func (d *driver) setupServer(installArgs ...string) error {
	if err := d.boot(); err != nil {
		return err
	}
	// The server must run in plain 512 MB: no swap, also not after reboots and sysupgrade (the snapshot's rc.local enables it).
	// The snapshot's rc.local also mounts the data disk as /dev/sdb, but the kernel names the 512 MiB swap disk and the
	// 2 GiB data disk in either order; pick the data disk by size so that reboots do not lose it.
	const rcLocal = `mkdir -p /mnt/data\nfor d in sdb sdc; do [ "$(cat /sys/block/$d/size)" = 4194304 ] && { grep -q " /mnt/data " /proc/mounts || mount -t ext4 /dev/$d /mnt/data; }; done\nexit 0\n`
	if _, err := d.sh("printf '" + rcLocal + "' > /etc/rc.local; sh /etc/rc.local; swapoff -a; grep -q ' /mnt/data ' /proc/mounts && test $(awk 'NR>1' /proc/swaps | wc -l) -eq 0"); err != nil {
		return err
	}
	return d.vm.Put(d.bins.syncd, remoteBin, 0o755)
}

var (
	reListen = regexp.MustCompile(`listening on :(\d+)`)
	reFP     = regexp.MustCompile(`fingerprint: (sha256:[0-9a-f]{64})`)
	rePass   = regexp.MustCompile(`: ([A-Z2-7]{5}(?:-[A-Z2-7]{5}){3})`)
)

// install runs "kaze-syncd install" and records the port and fingerprint.
func (d *driver) install(args ...string) (string, error) {
	out, err := d.sh("b=%s; [ -x $b ] || b=%s/kaze-syncd; $b install --data %s %s", remoteBin, dataDir, dataDir, strings.Join(args, " "))
	if err != nil {
		return out, err
	}
	m := reListen.FindStringSubmatch(out)
	if m == nil {
		return out, fmt.Errorf("no port in install output: %s", out)
	}
	d.port = m[1]
	d.fp = ""
	if f := reFP.FindStringSubmatch(out); f != nil {
		d.fp = f[1]
	}
	d.setBase()
	return out, nil
}

func (d *driver) setBase() {
	d.base = "https://" + d.vm.IP + ":" + d.port
	d.web = newWeb(d.base)
}

// adminPassword sets a new admin password and logs in.
func (d *driver) adminPassword() error {
	out, err := d.sh("%s/kaze-syncd admin-password", dataDir)
	if err != nil {
		return err
	}
	m := rePass.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("no password in: %s", out)
	}
	d.admin = m[1]
	if err := d.waitHealthz(60 * time.Second); err != nil {
		return err
	}
	return d.login()
}

func (d *driver) login() error {
	d.web = newWeb(d.base)
	st, err := d.web.login(d.admin)
	if err != nil {
		return err
	}
	if st != 303 {
		return fmt.Errorf("login: status %d", st)
	}
	return nil
}

func (d *driver) waitHealthz(timeout time.Duration) error {
	return waitFor("healthz", timeout, func() (bool, error) {
		st, b, err := newWeb(d.base).get("/healthz")
		if err != nil {
			return false, err
		}
		return st == 200 && b == "ok\n", fmt.Errorf("status %d", st)
	})
}

// serverPID returns the pid and user of the running service, "" when it is not running.
func (d *driver) serverPID() (pid, user string) {
	out, _ := d.vm.SSH(`p=$(ps w | grep '[k]aze-syncd run' | awk '{print $1}' | head -1); [ -n "$p" ] && echo $p $(ls -ld /proc/$p | awk "{print \$3}")`) // ps cuts user names at 8 characters
	if f := strings.Fields(out); len(f) == 2 {
		return f[0], f[1]
	}
	return "", ""
}

func (d *driver) waitService(not string) (string, error) {
	var pid string
	err := waitFor("service", 90*time.Second, func() (bool, error) {
		p, u := d.serverPID()
		if p == "" || p == not {
			return false, fmt.Errorf("pid %q", p)
		}
		if u != "kaze-sync" {
			return false, fmt.Errorf("runs as %q", u)
		}
		pid = p
		return true, nil
	})
	return pid, err
}

func (d *driver) bootID() string {
	out, _ := d.vm.SSH("cat /proc/sys/kernel/random/boot_id")
	return out
}

// reboot runs command in the guest and waits for the next boot to answer SSH.
func (d *driver) reboot(command string) error {
	old := d.bootID()
	d.vm.SSH(command)
	time.Sleep(15 * time.Second)
	return waitFor("new boot", 6*time.Minute, func() (bool, error) {
		d.vm.SetIP()
		id := d.bootID()
		return id != "" && id != old, fmt.Errorf("boot id %q", id)
	})
}

// waitBlobsCmd is a guest-side loop that returns once the blob directory holds at least n files, and fails after
// limitSec seconds: one loop in the guest, so a kill follows the first blocks closely, but never an endless one
// that keeps the VM's CPUs busy when the client died.
func waitBlobsCmd(dir string, n, limitSec int) string {
	return fmt.Sprintf("t0=$(date +%%s); while [ $(find %s/var/blobs -type f | wc -l) -lt %d ]; do "+
		"[ $(( $(date +%%s) - t0 )) -gt %d ] && { echo 'timeout waiting for blobs' >&2; exit 1; }; done", dir, n, limitSec)
}

func (d *driver) blobCount() (int, error) {
	out, err := d.sh("find %s/var/blobs -type f | wc -l", dataDir)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func (d *driver) endPhase() {
	for _, a := range d.agents {
		a.stop(true)
	}
	d.agents = nil
	d.collectServerLog()
	d.vm.Stop()
	d.vm.Revert()
}

func (d *driver) mainPhase() {
	defer d.endPhase()
	if !d.step("restore snapshot, boot, upload binary", func() error {
		if err := d.setupServer(); err != nil {
			return err
		}
		out, err := d.sh(`. /etc/openwrt_release; echo $DISTRIB_DESCRIPTION; mount | grep ' /mnt/data type ext4'; id kaze-sync 2>&1; true`)
		if err != nil {
			return err
		}
		if !strings.Contains(out, "/mnt/data type ext4") || !strings.Contains(out, "unknown user") {
			return fmt.Errorf("unexpected baseline: %s", out)
		}
		log.Printf("baseline: %s", strings.Split(out, "\n")[0])
		return nil
	}) {
		return
	}
	d.step("install refuses a data directory on the overlay", func() error {
		if err := d.shouldFail(remoteBin+" install --data /root/kd", "root file system"); err != nil {
			return err
		}
		_, err := d.sh("test ! -e /root/kd")
		return err
	})
	if !d.step("install: procd service as kaze-sync", func() error {
		if _, err := d.install(); err != nil {
			return err
		}
		if _, err := d.waitService(""); err != nil {
			return err
		}
		out, err := d.sh(`grep ^kaze-sync /etc/passwd /etc/group; cat /etc/sysupgrade.conf; ls -l /etc/rc.d/S99kaze-sync; /etc/init.d/kaze-sync enabled && echo enabled`)
		if err != nil {
			return err
		}
		for _, want := range []string{"/etc/passwd:kaze-sync:", "/etc/group:kaze-sync:", "/etc/init.d/kaze-sync", "/etc/rc.d/S99kaze-sync", "enabled"} {
			if !strings.Contains(out, want) {
				return fmt.Errorf("missing %q in:\n%s", want, out)
			}
		}
		return nil
	}) {
		return
	}
	d.step("layout: root owns <data> and the binary, the service user cannot write them", func() error {
		out, err := d.sh(`ls -ldn %[1]s %[1]s/kaze-syncd %[1]s/var %[1]s/var/meta.db %[1]s/var/blobs %[1]s/var/tls/key.pem | awk '{print $1, $3, $4, $NF}'; id -u kaze-sync; id -g kaze-sync`, dataDir)
		if err != nil {
			return err
		}
		f := strings.Split(strings.TrimSpace(out), "\n")
		uid, gid := f[len(f)-2], f[len(f)-1]
		svc := " " + uid + " " + gid + " "
		for _, want := range []string{"drwxr-xr-x 0 0 " + dataDir, "-rwxr-xr-x 0 0 " + dataDir + "/kaze-syncd", "drwx------" + svc + dataDir + "/var",
			"-rw-r--r--" + svc + dataDir + "/var/meta.db", "drwxr-xr-x" + svc + dataDir + "/var/blobs", "-rw-------" + svc + dataDir + "/var/tls/key.pem"} {
			if !strings.Contains(out, want+"\n") {
				return fmt.Errorf("want %q in:\n%s", want, out)
			}
		}
		// OpenWrt has no su: root ownership with mode 0755 on <data> and the binary (checked above) is what keeps the service user out.
		return nil
	})
	d.step("uci -q delete exits non-zero for a missing rule; no rule without --wan", func() error {
		if err := d.shouldFail("uci -q delete firewall.kaze_sync", ""); err != nil {
			return err
		}
		return d.shouldFail("uci -q get firewall.kaze_sync", "")
	})
	d.step("/healthz from WSL and the Windows host", func() error {
		if err := d.waitHealthz(30 * time.Second); err != nil {
			return err
		}
		out, err := exec.Command("curl.exe", "-sk", "--max-time", "10", d.base+"/healthz").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "ok" {
			return fmt.Errorf("from Windows: %v %q", err, out)
		}
		return nil
	})
	if !d.step("admin-password and Web UI login", d.adminPassword) {
		return
	}
	d.step("procd restarts the service after kill -9", func() error {
		pid, _ := d.serverPID()
		if _, err := d.sh("kill -9 %s", pid); err != nil {
			return err
		}
		_, err := d.waitService(pid)
		if err != nil {
			return err
		}
		return d.waitHealthz(30 * time.Second)
	})
	d.step("service recovers after a VM reboot", func() error {
		if err := d.reboot("reboot"); err != nil {
			return err
		}
		if err := d.vm.WaitSSH(bootTimeout); err != nil {
			return err
		}
		if _, err := d.waitService(""); err != nil {
			return err
		}
		d.setBase()
		if err := d.waitHealthz(60 * time.Second); err != nil {
			return err
		}
		return d.login() // sessions are in memory
	})
	d.step("--wan creates the firewall rule, re-install without it removes it", func() error {
		if _, err := d.install("--wan"); err != nil { // same port: it is kept in meta
			return err
		}
		out, err := d.sh("uci show firewall.kaze_sync; nft list ruleset | grep -c 'dport %s'", d.port)
		if err != nil {
			return err
		}
		for _, want := range []string{"firewall.kaze_sync.src='wan'", "firewall.kaze_sync.dest_port='" + d.port + "'", "firewall.kaze_sync.target='ACCEPT'"} {
			if !strings.Contains(out, want) {
				return fmt.Errorf("want %s in:\n%s", want, out)
			}
		}
		if _, err := d.install(); err != nil {
			return err
		}
		if err := d.shouldFail("uci -q get firewall.kaze_sync", ""); err != nil {
			return err
		}
		if out, _ := d.vm.SSH(fmt.Sprintf("nft list ruleset | grep -c 'dport %s'", d.port)); strings.TrimSpace(out) != "0" {
			return fmt.Errorf("rule still loaded: %s", out)
		}
		_, err = d.install("--wan") // leave it on for the sysupgrade check
		if err != nil {
			return err
		}
		if _, err := d.waitService(""); err != nil {
			return err
		}
		return d.login()
	})
	d.step("data disk not mounted: install refused, run fails and creates no database", func() error {
		if _, err := d.sh("/etc/init.d/kaze-sync stop; for i in 1 2 3 4 5 6 7 8 9 10; do umount /mnt/data && exit 0; sleep 1; done; exit 1"); err != nil {
			return err
		}
		defer d.sh("sh /etc/rc.local; /etc/init.d/kaze-sync start")
		if err := d.shouldFail(remoteBin+" install --data "+dataDir, "root file system"); err != nil {
			return err
		}
		if err := d.shouldFail(remoteBin+" run --data "+dataDir, "meta.db"); err != nil {
			return err
		}
		_, err := d.sh("test ! -e %s", dataDir)
		return err
	})
	if _, err := d.waitService(""); err == nil && d.waitHealthz(60*time.Second) == nil {
		d.login()
	}
	if d.only == "setup" {
		return
	}
	if d.only == "m4" || d.only == "m5" || d.only == "m6" || d.only == "m7" {
		d.m4Scenarios(d.only != "m4")
		return
	}
	d.syncScenarios()
	d.m4Scenarios(false)
	if d.only == "sync" {
		return
	}
	d.step("security: foreign Origin, lockout, admin-password invalidates sessions", d.security)
	d.step("sysupgrade keeps user, init script, link, firewall rule, service and data", d.sysupgrade)
	d.certMode()
}

// certMode re-installs the service with a certificate from a test CA (design 9.3) and enrolls a client without a fingerprint.
func (d *driver) certMode() {
	if !d.step("cert mode: re-install with a test CA certificate", func() error {
		caPEM, certPEM, keyPEM, err := testCA(d.vm.IP)
		if err != nil {
			return err
		}
		for name, b := range map[string][]byte{"ca.pem": caPEM, "cert.pem": certPEM, "key.pem": keyPEM} {
			os.WriteFile(filepath.Join(d.tmp, name), b, 0o600)
		}
		if _, err := d.sh("mkdir -p /etc/kaze-certs"); err != nil {
			return err
		}
		for _, f := range []string{"cert.pem", "key.pem"} {
			if err := d.vm.Put(filepath.Join(d.tmp, f), "/etc/kaze-certs/"+f, 0o644); err != nil { // the service user must read them
				return err
			}
		}
		out, err := d.install("--cert /etc/kaze-certs/cert.pem --key /etc/kaze-certs/key.pem")
		if err != nil {
			return err
		}
		if d.fp != "" || !strings.Contains(out, "enroll without --fingerprint") {
			return fmt.Errorf("unexpected install output: %s", out)
		}
		if _, err := d.waitService(""); err != nil {
			return err
		}
		if err := d.waitHealthz(60 * time.Second); err != nil {
			return err
		}
		return d.login()
	}) {
		return
	}
	d.step("cert mode: a client enrolls without fingerprint and uploads", func() error {
		code, err := d.web.addDevice("certdev")
		if err != nil {
			return err
		}
		root := filepath.Join(d.tmp, "cert-root")
		a := d.newAgent("certdev", root)
		a.env = []string{"SSL_CERT_FILE=" + filepath.Join(d.tmp, "ca.pem")}
		// Without the CA the certificate is unknown, so enrollment must fail.
		bad := d.newAgent("certdev-noca", filepath.Join(d.tmp, "cert-root-noca"))
		bad.env = []string{"SSL_CERT_FILE=/dev/null"}
		if err := bad.enroll(d.base, code, ""); err == nil {
			return fmt.Errorf("enrollment succeeded without trusting the CA")
		}
		if err := a.enroll(d.base, code, ""); err != nil {
			return err
		}
		fid, err := d.web.newFolder("certfolder")
		if err != nil {
			return err
		}
		did, err := d.web.idOf("devices", "certdev")
		if err != nil {
			return err
		}
		dir := filepath.Join(d.tmp, "cert-files")
		if err := d.web.mapFolder(did, fid, dir, true); err != nil {
			return err
		}
		writeOld(filepath.Join(dir, "hello.txt"), []byte("hello"))
		if err := a.start(); err != nil {
			return err
		}
		return d.waitFile(fid, "", "hello.txt", "5 B", 90*time.Second)
	})
}

// testCA makes a CA and a server certificate for ip.
func testCA(ip string) (caPEM, certPEM, keyPEM []byte, err error) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kaze-sync e2e CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "kaze-sync"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IPAddresses: []net.IP{net.ParseIP(ip)},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	enc := func(t string, b []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: t, Bytes: b}) }
	return enc("CERTIFICATE", caDER), enc("CERTIFICATE", der), enc("EC PRIVATE KEY", keyDER), nil
}

func (d *driver) newAgent(name, state string) *agent {
	os.MkdirAll(state, 0o755)
	a := &agent{name: name, exe: d.bins.linux, state: state, logs: d.logs}
	d.agents = append(d.agents, a)
	return a
}
