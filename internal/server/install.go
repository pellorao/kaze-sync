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

	"kaze-sync/internal/blobstore"
)

// Installer installs and maintains the server on OpenWrt, systemd and OpenRC systems. Its fields are the
// seams that tests replace; New fills in the real ones.
type Installer struct {
	Root       string                                      // "/" on a router; the prefix of /etc paths
	Self       string                                      // path of the running binary
	Out        io.Writer                                   // messages for the administrator
	Run        func(name string, args ...string) error     // runs a command
	SameDevice func(path string) (bool, error)             // is path on the root file system?
	Lookup     func(name string) (uid, gid int, err error) // the service user's ids
	Chown      func(path string, uid, gid int) error       // must not follow symbolic links: the service user can plant them below var/
}

type InstallOptions struct {
	Data, Listen, Cert, Key string
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
	}
}

func (in *Installer) say(format string, a ...any) { fmt.Fprintf(in.Out, format+"\n", a...) }

var safePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)

// service describes how one service manager runs kaze-syncd (design 9.1): the
// probe file that identifies it, the definition file with its text (%[1]s is
// the data directory, %[2]s the systemd ProtectHome value), and the commands that enable and restart the service.
type service struct {
	probe, file, text string
	enable, restart   [][]string
}

var (
	procd = service{probe: "/etc/openwrt_release", file: initPath, text: `#!/bin/sh /etc/rc.common
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

	systemd = service{probe: "/run/systemd/system", file: "/etc/systemd/system/" + serviceName + ".service", text: `[Unit]
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

	openrc = service{probe: "/sbin/openrc-run", file: initPath, text: `#!/sbin/openrc-run
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

// Install installs or upgrades the server (design 9.1); it can run repeatedly.
func (in *Installer) Install(o InstallOptions) error {
	data := filepath.Clean(o.Data)
	if !safePath.MatchString(data) {
		return fmt.Errorf("--data must be an absolute path of letters, digits and ._-/ (got %q)", o.Data)
	}
	if (o.Cert == "") != (o.Key == "") {
		return errors.New("--cert and --key go together")
	}
	if o.Cert != "" {
		if !filepath.IsAbs(o.Cert) || !filepath.IsAbs(o.Key) {
			return errors.New("--cert and --key must be absolute paths")
		}
		if _, err := tls.LoadX509KeyPair(o.Cert, o.Key); err != nil {
			return fmt.Errorf("certificate: %w", err)
		}
	}
	svc, err := in.detect()
	if err != nil {
		return err
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
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	if err := in.copySelf(data); err != nil {
		return err
	}
	if err := in.createUser(svc); err != nil {
		return err
	}
	state := filepath.Join(data, VarDir)
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	db, err := OpenDB(filepath.Join(state, DBFile), true)
	if err != nil {
		return err
	}
	listen, fp, err := in.initState(db, state, o)
	certFiles := []string{MetaString(db, "cert"), MetaString(db, "key")}
	db.Close()
	if err != nil {
		return err
	}
	if err := in.chownData(data); err != nil {
		return err
	}
	if err := in.writeService(svc, data, certFiles); err != nil {
		return err
	}
	if svc == &procd {
		if err := in.keepAcrossSysupgrade(); err != nil {
			return err
		}
		if err := in.firewall(listen, o.WAN); err != nil {
			return err
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

// migrateFolders migrates the database of every folder; a missing one is an error, never recreated.
func migrateFolders(db *sql.DB, varDir string) error {
	ids, err := queryAll(db, func(rows *sql.Rows, id *int64) error { return rows.Scan(id) }, `SELECT id FROM folders ORDER BY id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		fdb, err := OpenDB(FolderDBPath(varDir, id), false)
		if err == nil {
			err = MigrateFolder(fdb)
			fdb.Close()
		}
		if err != nil {
			return fmt.Errorf("folder %d: %w", id, err)
		}
	}
	return nil
}

// initState migrates the database and decides the listen address and certificate.
func (in *Installer) initState(db *sql.DB, data string, o InstallOptions) (listen, fingerprint string, err error) {
	if err := Migrate(db); err != nil {
		return "", "", err
	}
	if err := migrateFolders(db, data); err != nil {
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
	// Everything below var/ but the blocks themselves (read-only, world-readable): this also repairs a
	// tree that root restored from a backup (design 9.4).
	return filepath.WalkDir(state, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(p, filepath.Join(state, "blobs")+"/") {
			return nil
		}
		if err := in.Chown(p, uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
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
