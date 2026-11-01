package server

import (
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
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

// Installer installs and maintains the server on OpenWrt. Its fields are the
// seams that tests replace; New fills in the real ones.
type Installer struct {
	Root       string                                      // "/" on a router; the prefix of /etc paths
	Self       string                                      // path of the running binary
	Out        io.Writer                                   // messages for the administrator
	Run        func(name string, args ...string) error     // runs a command
	SameDevice func(path string) (bool, error)             // is path on the root file system?
	Lookup     func(name string) (uid, gid int, err error) // the service user's ids
	Chown      func(path string, uid, gid int) error
}

type InstallOptions struct {
	Data, Listen, Cert, Key string
	WAN                     bool
}

// Layout of the installation (design 2.3, 9.1): everything that depends on
// where files live is named here.
const (
	serviceName = "kaze-sync" // service user, init script, firewall rule
	binName     = "kaze-syncd"
	DBFile      = "meta.db"
	TLSDir      = "tls"
	initPath    = "/etc/init.d/" + serviceName
	rcLink      = "/etc/rc.d/S99" + serviceName
	fwSection   = "firewall.kaze_sync"
)

// serviceOwned lists what the service user must own, relative to the data directory (blobs are created by the service itself).
var serviceOwned = []string{"", "blobs", "tmp", TLSDir, TLSDir + "/cert.pem", TLSDir + "/key.pem", DBFile, DBFile + "-wal", DBFile + "-shm"}

func NewInstaller() *Installer {
	self, _ := os.Executable()
	return &Installer{
		Root: "/", Self: self, Out: os.Stdout, SameDevice: SameDevice, Chown: os.Chown,
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

const initScript = `#!/bin/sh /etc/rc.common
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
`

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
	if same, err := in.SameDevice(data); err != nil {
		return err
	} else if same {
		return fmt.Errorf("%s is on the root file system; mount the data disk first", data)
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	if err := in.copySelf(data); err != nil {
		return err
	}
	if err := in.createUser(); err != nil {
		return err
	}

	db, err := OpenDB(filepath.Join(data, DBFile), true)
	if err != nil {
		return err
	}
	listen, fp, err := in.initState(db, data, o)
	db.Close()
	if err != nil {
		return err
	}
	if err := in.chownData(data); err != nil {
		return err
	}
	if err := in.writeInit(data); err != nil {
		return err
	}
	if err := in.keepAcrossSysupgrade(); err != nil {
		return err
	}
	if err := in.firewall(listen, o.WAN); err != nil {
		return err
	}
	for _, cmd := range []string{"enable", "restart"} {
		if err := in.Run(initPath, cmd); err != nil {
			return err
		}
	}
	in.say("installed in %s, listening on %s", data, listen)
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

// createUser adds the kaze-sync user and group with OpenWrt's /lib/functions.sh helpers.
func (in *Installer) createUser() error {
	return in.Run("sh", "-c", `. /lib/functions.sh
user_exists kaze-sync || user_add kaze-sync
group_exists kaze-sync || group_add kaze-sync "$(id -u kaze-sync)"`)
}

// initState migrates the database and decides the listen address and certificate.
func (in *Installer) initState(db *sql.DB, data string, o InstallOptions) (listen, fingerprint string, err error) {
	if err := Migrate(db); err != nil {
		return "", "", err
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

// chownData hands the files the service writes to the service user. It
// creates the block store directories first, which the service could not.
func (in *Installer) chownData(data string) error {
	if _, err := blobstore.Open(data); err != nil {
		return err
	}
	uid, gid, err := in.Lookup(serviceName)
	if err != nil {
		return err
	}
	for _, p := range serviceOwned {
		if err := in.Chown(filepath.Join(data, p), uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (in *Installer) path(p string) string { return filepath.Join(in.Root, p) }

func (in *Installer) writeInit(data string) error {
	p := in.path(initPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(fmt.Sprintf(initScript, data)), 0o755); err != nil {
		return err
	}
	return os.Chmod(p, 0o755)
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
	db, err := OpenDB(filepath.Join(data, DBFile), false)
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
	if _, err := os.Stat(in.path(initPath)); err != nil {
		in.say("service not installed; start it with: %s/%s run --data %s", data, binName, data)
		return nil
	}
	return in.Run(initPath, "restart")
}
