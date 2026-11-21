package server

import (
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"kaze-sync/internal/blobstore"
	"kaze-sync/internal/version"
)

// Installer installs and maintains the server on OpenWrt, systemd and OpenRC systems. Its fields are the
// seams that tests replace; New fills in the real ones.
type Installer struct {
	Root             string                                  // "/" on a router; the prefix of /etc paths
	Self             string                                  // path of the running binary
	Out              io.Writer                               // messages for the administrator
	Run              func(name string, args ...string) error // runs a command
	InstalledVersion func(path string) (string, error)
	SameDevice       func(path string) (bool, error)             // is path on the root file system?
	Lookup           func(name string) (uid, gid int, err error) // the service user's ids
	Chown            func(path string, uid, gid int) error       // must not follow symbolic links: the service user can plant them below var/
	Groups           func(name string) []int                     // the user's supplementary group ids; nil means none
}

type InstallOptions struct {
	Data, Listen, Cert, Key string
	CA                      bool   // local CA mode (design 2.5)
	SAN                     string // extra names and addresses for the CA-signed certificate, comma separated; kept when empty
	WAN                     bool
	NewEpoch                bool // after restoring a backup: make the clients read everything again (design 9.4)
}

// Layout of the installation (design 2.3, 9.1): everything that depends on
// where files live is named here.
const (
	serviceName = "kaze-sync" // service user, init script, firewall rule
	binName     = "kaze-syncd"
	VarDir      = "var" // everything the service writes; <data> and the binary stay root's (0755)
	DBFile      = "meta.db"
	TLSDir      = "tls"
	initPath    = "/etc/init.d/" + serviceName // procd and OpenRC script
	rcLink      = "/etc/rc.d/S99" + serviceName
	fwSection   = "firewall.kaze_sync"
)

func NewInstaller() *Installer {
	self, _ := os.Executable()
	return &Installer{
		Root: "/", Self: self, Out: os.Stdout, SameDevice: SameDevice, Chown: os.Lchown,
		InstalledVersion: readInstalledVersion,
		Run: func(name string, args ...string) error {
			out, err := exec.Command(name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		Lookup: func(name string) (int, int, error) {
			u, err := user.Lookup(name)
			if err != nil {
				return 0, 0, err
			}
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			return uid, gid, nil
		},
		Groups: func(name string) (gids []int) {
			if u, err := user.Lookup(name); err == nil {
				ids, _ := u.GroupIds()
				for _, id := range ids {
					if n, err := strconv.Atoi(id); err == nil {
						gids = append(gids, n)
					}
				}
			}
			return gids
		},
	}
}

func (in *Installer) say(format string, a ...any) { fmt.Fprintf(in.Out, format+"\n", a...) }

var safePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)

// service describes how one service manager runs kaze-syncd (design 9.1): the
// probe file that identifies it, the definition file with its text (%[1]s is
// the data directory, %[2]s the systemd ProtectHome value), and the commands that enable and restart the service.
type service struct {
	probe, file, text string
	dataLine          *regexp.Regexp
	enable, restart   [][]string
}

var (
	dataArg = `(/[A-Za-z0-9._/-]*)`
	procd   = service{probe: "/etc/openwrt_release", file: initPath,
		dataLine: regexp.MustCompile(`(?m)^\s*procd_set_param command [^\s]+/kaze-syncd run --data ` + dataArg + `[ \t]*$`),
		text: `#!/bin/sh /etc/rc.common
START=99
USE_PROCD=1
start_service() {
	procd_open_instance
	procd_set_param command %[1]s/` + binName + ` run --data %[1]s
	procd_set_param user kaze-sync
	procd_set_param respawn
	procd_set_param stderr 1
	procd_close_instance
}
`, enable: [][]string{{initPath, "enable"}}, restart: [][]string{{initPath, "restart"}}}

	systemd = service{probe: "/run/systemd/system", file: "/etc/systemd/system/" + serviceName + ".service",
		dataLine: regexp.MustCompile(`(?m)^ExecStart=[^\s]+/kaze-syncd run --data ` + dataArg + `[ \t]*$`),
		text: `[Unit]
Description=kaze-sync server
After=network-online.target
Wants=network-online.target
RequiresMountsFor=%[1]s

[Service]
ExecStart=%[1]s/` + binName + ` run --data %[1]s
User=kaze-sync
Restart=on-failure
RestartSec=5
# Hardening (design 9.1): the service writes only below <data>/var and opens no outgoing connections.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=%[2]s
ReadWritePaths=%[1]s/` + VarDir + `
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RemoveIPC=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
`, enable: [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", serviceName}}, restart: [][]string{{"systemctl", "restart", serviceName}}}

	openrc = service{probe: "/sbin/openrc-run", file: initPath,
		dataLine: regexp.MustCompile(`(?m)^command_args="run --data ` + dataArg + `"[ \t]*$`),
		text: `#!/sbin/openrc-run
description="kaze-sync server"
supervisor=supervise-daemon
command=%[1]s/` + binName + `
command_args="run --data %[1]s"
command_user=kaze-sync
respawn_delay=5
output_logger="logger -t kaze-sync"
error_logger="logger -t kaze-sync"
depend() {
	need net localmount
}
`, enable: [][]string{{"rc-update", "add", serviceName, "default"}}, restart: [][]string{{"rc-service", serviceName, "restart"}}}
)

// detect picks the service manager in the order of design 9.1.
func (in *Installer) detect() (*service, error) {
	for _, s := range []*service{&procd, &systemd, &openrc} {
		if _, err := os.Stat(in.path(s.probe)); err == nil {
			return s, nil
		}
	}
	return nil, errors.New("no supported service manager found (OpenWrt, systemd or OpenRC)")
}

func (in *Installer) installedData(svc *service) (string, error) {
	path := in.path(svc.file)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("no installed kaze-sync service; specify --data for the first install")
	}
	if err != nil {
		return "", err
	}
	matches := svc.dataLine.FindAllSubmatch(b, 2)
	if len(matches) != 1 {
		return "", fmt.Errorf("cannot determine --data from %s; specify --data explicitly", svc.file)
	}
	return string(matches[0][1]), nil
}

func (in *Installer) checkInstalledVersion(data string) error {
	dst := filepath.Join(data, binName)
	if sameFile(in.Self, dst) {
		return nil
	}
	fi, err := os.Lstat(dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", dst)
	}
	old, err := in.InstalledVersion(dst)
	if err != nil {
		return fmt.Errorf("cannot read installed kaze-syncd version: %w", err)
	}
	order, err := version.Compare(version.Version, old)
	if err != nil {
		return err
	}
	if order < 0 {
		return fmt.Errorf("refusing to downgrade kaze-syncd from %s to %s; follow the rollback steps in docs/design.md section 9.3", old, version.Version)
	}
	return nil
}

// Install installs or upgrades the server (design 9.1); it can run repeatedly.
func (in *Installer) Install(o InstallOptions) error {
	svc, err := in.detect()
	if err != nil {
		return err
	}
	useInstalled := o.Data == ""
	if useInstalled {
		o.Data, err = in.installedData(svc)
		if err != nil {
			return err
		}
	}
	data := filepath.Clean(o.Data)
	if !safePath.MatchString(data) {
		return fmt.Errorf("--data must be an absolute path of letters, digits and ._-/ (got %q)", o.Data)
	}
	if (o.Cert == "") != (o.Key == "") {
		return errors.New("--cert and --key go together")
	}
	if o.CA && o.Cert != "" {
		return errors.New("--ca and --cert exclude each other")
	}
	if o.Cert != "" {
		if !filepath.IsAbs(o.Cert) || !filepath.IsAbs(o.Key) {
			return errors.New("--cert and --key must be absolute paths")
		}
		if _, err := tls.LoadX509KeyPair(o.Cert, o.Key); err != nil {
			return fmt.Errorf("certificate: %w", err)
		}
	}
	if svc != &procd && o.WAN {
		return errors.New("--wan only configures the OpenWrt firewall; open the port yourself")
	}
	if svc == &procd {
		if same, err := in.SameDevice(data); err != nil {
			return err
		} else if same {
			return fmt.Errorf("%s is on the root file system; mount the data disk first", data)
		}
	}
	if !useInstalled {
		if err := os.MkdirAll(data, 0o755); err != nil {
			return err
		}
	}
	lock, err := lockInstall(data)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := in.checkInstalledVersion(data); err != nil {
		return err
	}
	state := filepath.Join(data, VarDir)
	if !useInstalled {
		if err := os.MkdirAll(state, 0o700); err != nil {
			return err
		}
	}
	db, err := OpenDB(filepath.Join(state, DBFile), !useInstalled)
	if err != nil {
		return err
	}
	listen, fp, err := in.initState(db, data, o)
	certFiles := []string{MetaString(db, "cert"), MetaString(db, "key")}
	caMode := MetaString(db, "tls") == "ca"
	db.Close()
	if err != nil {
		return err
	}
	if err := in.createUser(svc); err != nil {
		return err
	}
	if err := in.copySelf(data); err != nil {
		return err
	}
	if err := in.chownData(data); err != nil {
		return err
	}
	if err := in.warnCertAccess(svc, certFiles); err != nil {
		return err
	}
	if err := in.writeService(svc, data, certFiles); err != nil {
		return err
	}
	if svc == &procd {
		if err := in.keepAcrossSysupgrade(); err != nil {
			return err
		}
		if !useInstalled || o.WAN {
			if err := in.firewall(listen, o.WAN); err != nil {
				return err
			}
		} else if o.Listen != "" && in.Run("uci", "-q", "get", fwSection) == nil {
			_, port, _ := net.SplitHostPort(listen)
			if err := in.Run("uci", "set", fwSection+".dest_port="+port); err != nil {
				return err
			}
			if err := in.reloadFirewall(); err != nil {
				return err
			}
		}
	}
	if err := in.runAll(append(svc.enable, svc.restart...)); err != nil {
		return err
	}
	in.say("installed in %s, listening on %s", data, listen)
	if svc != &procd {
		in.say("open this TCP port in the cloud security group or the local firewall")
	}
	if fp != "" {
		in.say("certificate fingerprint: %s", fp)
		if caMode {
			in.say("local CA mode: clients pin the CA fingerprint above; get the CA certificate to trust in browsers with: %s/%s ca --data %s --export ca.pem", data, binName, data)
		}
	} else {
		in.say("using the certificate files given; clients enroll without --fingerprint")
	}
	in.say("next: %s/kaze-syncd admin-password", data)
	return nil
}

// copySelf copies the running binary to <data>/<binName> through a rename, so a running service keeps working.
func (in *Installer) copySelf(data string) error {
	dst := filepath.Join(data, binName)
	if sameFile(in.Self, dst) {
		return nil
	}
	src, err := os.Open(in.Self)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(data, ".install-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

// createUser adds the kaze-sync user and group: with OpenWrt's /lib/functions.sh
// helpers there, elsewhere with useradd, or busybox adduser (Alpine) without it.
func (in *Installer) createUser(svc *service) error {
	if svc == &procd {
		return in.Run("sh", "-c", `. /lib/functions.sh
user_exists kaze-sync || user_add kaze-sync
group_exists kaze-sync || group_add kaze-sync "$(id -u kaze-sync)"`)
	}
	return in.Run("sh", "-c", `id -u kaze-sync >/dev/null 2>&1 && exit 0
if command -v useradd >/dev/null; then useradd --system --no-create-home --shell /bin/false kaze-sync
else addgroup -S kaze-sync && adduser -S -D -H -G kaze-sync -s /sbin/nologin kaze-sync; fi`)
}

func (in *Installer) runAll(cmds [][]string) error {
	for _, c := range cmds {
		if err := in.Run(c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// initState migrates the database and decides the listen address and certificate.
func (in *Installer) initState(db *sql.DB, root string, o InstallOptions) (listen, fingerprint string, err error) {
	data := filepath.Join(root, VarDir)
	if err := migrateState(db, data, time.Now()); err != nil {
		return "", "", err
	}
	if o.NewEpoch {
		if err := SetMeta(db, "epoch", randomBase32(13)); err != nil {
			return "", "", err
		}
	}
	listen = o.Listen
	if listen == "" {
		listen = MetaString(db, "listen")
	}
	if listen == "" {
		listen = ":" + strconv.Itoa(20000+rand.IntN(40000))
	}
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return "", "", fmt.Errorf("--listen: %w", err)
	}
	if err := SetMeta(db, "listen", listen); err != nil {
		return "", "", err
	}
	if o.Cert != "" {
		for k, v := range map[string]string{"cert": o.Cert, "key": o.Key} {
			if err := SetMeta(db, k, v); err != nil {
				return "", "", err
			}
		}
	}
	if o.CA || (MetaString(db, "tls") == "ca" && o.Cert == "") { // --ca also leaves certificate-file mode
		c, err := SetupCA(db, root, o.SAN, o.SAN != "")
		if err != nil {
			return "", "", err
		}
		return listen, CAFingerprint(c), nil
	}
	if o.SAN != "" {
		return "", "", errors.New("--san needs --ca")
	}
	if o.Cert != "" {
		if err := SetMeta(db, "tls", ""); err != nil { // --cert leaves CA mode
			return "", "", err
		}
	}
	if MetaString(db, "cert") != "" {
		return listen, "", nil
	}
	cert, err := EnsureSelfSigned(filepath.Join(data, TLSDir))
	if err != nil {
		return "", "", err
	}
	return listen, Fingerprint(cert), nil
}

// chownData makes <data> and the binary root's and hands <data>/var, which
// the service writes to, to the service user, so that the service has no way
// to replace what root executes. It creates the block store directories
// first, which the service could not.
func (in *Installer) chownData(data string) error {
	state := filepath.Join(data, VarDir)
	if _, err := blobstore.Open(state); err != nil {
		return err
	}
	uid, gid, err := in.Lookup(serviceName)
	if err != nil {
		return err
	}
	for _, p := range []string{data, filepath.Join(data, binName)} {
		if err := in.Chown(p, 0, 0); err != nil {
			return err
		}
	}
	if err := os.Chmod(data, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(state, 0o700); err != nil {
		return err
	}
	// The CA (design 2.5) stays root's: the service signs nothing and must not read the key.
	if ca := filepath.Join(data, CADir); dirExists(ca) {
		if err := os.Chmod(ca, 0o700); err != nil {
			return err
		}
		ents, err := os.ReadDir(ca)
		if err != nil {
			return err
		}
		paths := []string{ca}
		for _, e := range ents {
			paths = append(paths, filepath.Join(ca, e.Name()))
		}
		for _, p := range paths {
			if err := in.Chown(p, 0, 0); err != nil {
				return err
			}
		}
	}
	// Everything below var/, blocks included (the service refreshes their mtime with os.Chtimes, which needs the
	// owner): this also repairs a tree that root restored from a backup (design 9.4) or made by another user.
	return filepath.WalkDir(state, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := in.Chown(p, uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func (in *Installer) path(p string) string { return filepath.Join(in.Root, p) }

// protectHome is the systemd ProtectHome value: "yes" hides /home, /root and /run/user, which would hide a data
// directory, a certificate or the binary below them; then "read-only" keeps them visible (the data stays
// writable through ReadWritePaths).
func protectHome(paths ...string) string {
	for _, p := range paths {
		for _, h := range []string{"/home", "/root", "/run/user"} {
			if p == h || strings.HasPrefix(p, h+"/") {
				return "read-only"
			}
		}
	}
	return "yes"
}

func (in *Installer) writeService(svc *service, data string, certFiles []string) error {
	p := in.path(svc.file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644) // a systemd unit; scripts are executable
	if strings.HasPrefix(svc.text, "#!") {
		mode = 0o755
	}
	if err := os.WriteFile(p, []byte(fmt.Sprintf(svc.text, data, protectHome(append(certFiles, data)...))), mode); err != nil {
		return err
	}
	return os.Chmod(p, mode)
}

// keepAcrossSysupgrade lists the init script and its enable link in /etc/sysupgrade.conf.
func (in *Installer) keepAcrossSysupgrade() error {
	p := in.path("/etc/sysupgrade.conf")
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text := string(b)
	for _, l := range []string{initPath, rcLink} {
		if !slices.Contains(strings.Fields(text), l) {
			if text != "" && !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			text += l + "\n"
		}
	}
	if text == string(b) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(text), 0o644)
}

// firewall creates or updates the "kaze-sync" WAN rule, or removes it without --wan.
func (in *Installer) firewall(listen string, wan bool) error {
	if !wan {
		if in.Run("uci", "-q", "delete", fwSection) != nil {
			return nil // there was no rule
		}
		return in.reloadFirewall()
	}
	_, port, _ := net.SplitHostPort(listen)
	for _, args := range [][]string{
		{"-q", "delete", fwSection},
		{"set", fwSection + "=rule"},
		{"set", fwSection + ".name=kaze-sync"},
		{"set", fwSection + ".src=wan"},
		{"set", fwSection + ".proto=tcp"},
		{"set", fwSection + ".dest_port=" + port},
		{"set", fwSection + ".target=ACCEPT"},
	} {
		if err := in.Run("uci", args...); err != nil && args[0] != "-q" {
			return err
		}
	}
	return in.reloadFirewall()
}

func (in *Installer) reloadFirewall() error {
	if err := in.Run("uci", "commit", "firewall"); err != nil {
		return err
	}
	return in.Run("/etc/init.d/firewall", "reload")
}

// AdminPassword replaces the administrator password, prints the new one once
// and restarts the service, which drops all sessions and lockouts.
func (in *Installer) AdminPassword(data string) error {
	db, err := OpenDB(filepath.Join(data, VarDir, DBFile), false)
	if err != nil {
		return err
	}
	pw := group(randomBase32(20), 5)
	h := sha(normalizeSecret(pw))
	err = SetMeta(db, "admin_password_hash", fmt.Sprintf("%x", h))
	db.Close()
	if err != nil {
		return err
	}
	if err := in.chownData(data); err != nil {
		return err
	}
	in.say("new admin password (shown once): %s", pw)
	svc, err := in.detect()
	if err == nil {
		_, err = os.Stat(in.path(svc.file))
	}
	if err != nil {
		in.say("service not installed; start it with: %s/%s run --data %s", data, binName, data)
		return nil
	}
	return in.runAll(svc.restart)
}

// accessProblem is one reason the service user cannot use a certificate file: path is the file or a directory
// above it, missing says it does not exist.
type accessProblem struct {
	path    string
	isDir   bool
	missing bool
	mode    os.FileMode
}

// canUse reports whether an account with the user id uid and the group ids gids may read the file (want 4) or
// enter the directory (want 1) described by fi, by the owner, group and other permission bits as the kernel
// applies them. It does not see ACLs, SELinux or mount options; when the owner cannot be known it says yes.
func canUse(fi os.FileInfo, uid int, gids []int, want os.FileMode) bool {
	owner, group, ok := fileOwner(fi)
	if !ok || uid == 0 {
		return true
	}
	perm := fi.Mode().Perm()
	switch {
	case uid == owner:
		perm >>= 6
	case slices.Contains(gids, group):
		perm >>= 3
	}
	return perm&want != 0
}

// certProblems lists what keeps the account from reading file: itself, and every directory above it, both as
// written and after resolving symbolic links, outermost first.
func certProblems(file string, uid int, gids []int) []accessProblem {
	resolved, err := filepath.EvalSymlinks(file)
	if err != nil {
		return []accessProblem{{path: file, missing: true}}
	}
	var out []accessProblem
	seen := map[string]bool{}
	for _, p := range []string{file, resolved} {
		var dirs []string
		for d := filepath.Dir(p); ; d = filepath.Dir(d) {
			dirs = append([]string{d}, dirs...)
			if d == filepath.Dir(d) {
				break
			}
		}
		for _, d := range dirs {
			if seen[d] {
				continue
			}
			seen[d] = true
			if fi, err := os.Stat(d); err == nil && !canUse(fi, uid, gids, 1) {
				out = append(out, accessProblem{path: d, isDir: true, mode: fi.Mode().Perm()})
			}
		}
	}
	if fi, err := os.Stat(resolved); err != nil {
		out = append(out, accessProblem{path: file, missing: true})
	} else if !canUse(fi, uid, gids, 4) {
		out = append(out, accessProblem{path: file, mode: fi.Mode().Perm()})
	}
	return out
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9._/@%+=:,-]+$`)

func shellQuote(s string) string {
	if plainArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// warnCertAccess warns, without stopping the installation, about certificate and key files (the stored paths, so
// a repeated install checks them too) that the service user cannot use. install runs as root, which can read
// anything, so only this check can tell before the service exits at its first start with "permission denied".
func (in *Installer) warnCertAccess(svc *service, files []string) error {
	if files[0] == "" && files[1] == "" {
		return nil
	}
	uid, gid, err := in.Lookup(serviceName)
	if err != nil {
		return err
	}
	gids := []int{gid}
	if in.Groups != nil {
		gids = append(gids, in.Groups(serviceName)...)
	}
	restart := "restart the service"
	if len(svc.restart) > 0 {
		restart = "restart the service with: " + strings.Join(svc.restart[0], " ")
	}
	for i, f := range files {
		if f == "" {
			continue
		}
		what := [...]string{"certificate file", "private key file"}[i]
		for _, p := range certProblems(f, uid, gids) {
			q := shellQuote(p.path)
			switch {
			case p.missing:
				in.say("warning: the %s %s does not exist; the service cannot start until it does", what, p.path)
			case p.isDir:
				in.say("warning: the service user %s cannot enter the directory %s (mode %04o), which holds the %s %s, so the service would exit at every start with \"permission denied\". Fix: chgrp %s %s && chmod 750 %s, then %s",
					serviceName, p.path, p.mode, what, f, serviceName, q, q, restart)
			default:
				in.say("warning: the service user %s cannot read the %s %s (mode %04o), so the service would exit at every start with \"permission denied\". Fix: chgrp %s %s && chmod 640 %s, then %s",
					serviceName, what, p.path, p.mode, serviceName, q, q, restart)
			}
		}
	}
	return nil
}
