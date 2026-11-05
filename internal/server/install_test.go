package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type fakeSystem struct {
	t       *testing.T
	in      *Installer
	root    string // fake "/"
	data    string
	out     bytes.Buffer
	cmds    []string
	overlay bool
	chowned []string
	owners  map[string]int // path -> last uid given
	failCmd string
}

func newFakeSystem(t *testing.T) *fakeSystem {
	t.Helper()
	dir := t.TempDir()
	f := &fakeSystem{t: t, owners: map[string]int{}, root: filepath.Join(dir, "root"), data: filepath.Join(dir, "mnt", "kaze-sync")}
	self := filepath.Join(dir, "self-binary")
	os.WriteFile(self, []byte("binary v1"), 0o755)
	f.useManager(&procd)
	f.in = &Installer{
		Root: f.root, Self: self, Out: &f.out,
		Run: func(name string, args ...string) error {
			line := name + " " + strings.Join(args, " ")
			f.cmds = append(f.cmds, line)
			if f.failCmd != "" && strings.Contains(line, f.failCmd) {
				return os.ErrInvalid
			}
			return nil
		},
		SameDevice: func(string) (bool, error) { return f.overlay, nil },
		Lookup:     func(string) (int, int, error) { return 32768, 32768, nil },
		Chown:      func(p string, uid, gid int) error { f.chowned = append(f.chowned, p); f.owners[p] = uid; return nil },
	}
	return f
}

// useManager makes the fake root look like a system run by svc only.
func (f *fakeSystem) useManager(svc *service) {
	for _, s := range []*service{&procd, &systemd, &openrc} {
		os.RemoveAll(filepath.Join(f.root, s.probe))
	}
	p := filepath.Join(f.root, svc.probe)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, nil, 0o755)
}

func (f *fakeSystem) install(o InstallOptions) error {
	o.Data = f.data
	return f.in.Install(o)
}

func (f *fakeSystem) hasCmd(sub string) bool {
	for _, c := range f.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func TestInstallFirstTime(t *testing.T) {
	f := newFakeSystem(t)
	f.failCmd = "uci -q delete" // no rule to delete on a fresh router
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.data, "kaze-syncd")); string(b) != "binary v1" {
		t.Fatal("binary not copied")
	}
	if fi, _ := os.Stat(filepath.Join(f.data, "kaze-syncd")); fi.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode %v", fi.Mode())
	}
	// Database, port in range, self-signed certificate with a printed fingerprint.
	db, err := OpenDB(filepath.Join(f.data, "var", "meta.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, _ := schemaVersion(db); v != SchemaVersion {
		t.Fatalf("schema %d", v)
	}
	m := regexp.MustCompile(`^:(\d+)$`).FindStringSubmatch(MetaString(db, "listen"))
	if m == nil || m[1] < "20000" || m[1] > "59999" {
		t.Fatalf("listen %q", MetaString(db, "listen"))
	}
	if !regexp.MustCompile(`fingerprint: sha256:[0-9a-f]{64}`).MatchString(f.out.String()) {
		t.Fatalf("output:\n%s", f.out.String())
	}
	// Init script.
	script, _ := os.ReadFile(filepath.Join(f.root, "etc/init.d/kaze-sync"))
	for _, want := range []string{"START=99", "USE_PROCD=1", "procd_set_param command " + f.data + "/kaze-syncd run --data " + f.data,
		"procd_set_param user kaze-sync", "procd_set_param respawn", "procd_set_param stderr 1"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("init script lacks %q", want)
		}
	}
	if fi, _ := os.Stat(filepath.Join(f.root, "etc/init.d/kaze-sync")); fi.Mode().Perm() != 0o755 {
		t.Fatal("init script not executable")
	}
	// sysupgrade.conf, user creation, service start, no firewall rule left behind.
	conf, _ := os.ReadFile(filepath.Join(f.root, "etc/sysupgrade.conf"))
	if string(conf) != "/etc/init.d/kaze-sync\n/etc/rc.d/S99kaze-sync\n" {
		t.Fatalf("sysupgrade.conf = %q", conf)
	}
	if !f.hasCmd("user_add kaze-sync") || !f.hasCmd("group_add kaze-sync") {
		t.Fatalf("user creation missing: %v", f.cmds)
	}
	last := f.cmds[len(f.cmds)-2:]
	if last[0] != "/etc/init.d/kaze-sync enable" || last[1] != "/etc/init.d/kaze-sync restart" {
		t.Fatalf("service commands %v", last)
	}
	if f.hasCmd("uci set") || f.hasCmd("firewall reload") {
		t.Fatalf("firewall touched without --wan: %v", f.cmds)
	}
	// The service user owns what it writes.
	owned := strings.Join(f.chowned, " ")
	for _, want := range []string{f.data + "/var", "var/meta.db", "var/blobs", "var/tmp", "key.pem"} {
		if !strings.Contains(owned, want) {
			t.Errorf("%s not chowned: %s", want, owned)
		}
	}
	// <data> and the binary are root's and 0755; the service user owns only <data>/var (0700).
	bin := filepath.Join(f.data, "kaze-syncd")
	if f.owners[f.data] != 0 || f.owners[bin] != 0 || f.owners[filepath.Join(f.data, "var")] != 32768 {
		t.Errorf("owners %v", f.owners)
	}
	for p, mode := range map[string]os.FileMode{f.data: 0o755, bin: 0o755, filepath.Join(f.data, "var"): 0o700} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s mode %v, want %v", p, fi, mode)
		}
	}
}

func TestInstallRefusesRootFilesystem(t *testing.T) {
	f := newFakeSystem(t)
	f.overlay = true
	if err := f.install(InstallOptions{}); err == nil || !strings.Contains(err.Error(), "root file system") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(f.data); err == nil {
		t.Fatal("data directory created on the overlay")
	}
	if len(f.cmds) != 0 {
		t.Fatalf("commands ran: %v", f.cmds)
	}
}

func TestInstallOnOtherLinux(t *testing.T) {
	for _, c := range []struct {
		svc      *service
		file     string
		mode     os.FileMode
		wantText []string
		wantCmds []string
	}{
		{&systemd, "etc/systemd/system/kaze-sync.service", 0o644,
			[]string{"ExecStart=%[1]s/kaze-syncd run --data %[1]s", "RequiresMountsFor=%[1]s", "User=kaze-sync", "Restart=on-failure",
				"NoNewPrivileges=yes", "ProtectSystem=strict", "ProtectHome=yes", "ReadWritePaths=%[1]s/var", "PrivateTmp=yes"},
			[]string{"systemctl daemon-reload", "systemctl enable kaze-sync", "systemctl restart kaze-sync"}},
		{&openrc, "etc/init.d/kaze-sync", 0o755,
			[]string{"supervisor=supervise-daemon", "command=%[1]s/kaze-syncd", `command_args="run --data %[1]s"`, "command_user=kaze-sync", "respawn_delay=5"},
			[]string{"rc-update add kaze-sync default", "rc-service kaze-sync restart"}},
	} {
		f := newFakeSystem(t)
		f.useManager(c.svc)
		f.overlay = true // not checked outside OpenWrt: the data directory may be on the root file system
		if err := f.install(InstallOptions{}); err != nil {
			t.Fatal(err)
		}
		text, _ := os.ReadFile(filepath.Join(f.root, c.file))
		for _, w := range c.wantText {
			if w = strings.ReplaceAll(w, "%[1]s", f.data); !strings.Contains(string(text), w) {
				t.Errorf("%s lacks %q", c.file, w)
			}
		}
		if fi, _ := os.Stat(filepath.Join(f.root, c.file)); fi.Mode().Perm() != c.mode {
			t.Errorf("%s mode %v", c.file, fi.Mode())
		}
		if !f.hasCmd("useradd --system") || !f.hasCmd("adduser -S -D -H -G kaze-sync") {
			t.Errorf("user creation missing: %v", f.cmds)
		}
		if f.hasCmd("user_add") || f.hasCmd("uci") {
			t.Errorf("OpenWrt steps ran: %v", f.cmds)
		}
		if _, err := os.Stat(filepath.Join(f.root, "etc/sysupgrade.conf")); err == nil {
			t.Error("sysupgrade.conf written")
		}
		if got := strings.Join(f.cmds[len(f.cmds)-len(c.wantCmds):], "|"); got != strings.Join(c.wantCmds, "|") {
			t.Errorf("commands %v, want %v", f.cmds, c.wantCmds)
		}
		if !strings.Contains(f.out.String(), "open this TCP port") {
			t.Errorf("no firewall hint:\n%s", f.out.String())
		}
		f.cmds = nil
		if err := f.in.AdminPassword(f.data); err != nil {
			t.Fatal(err)
		}
		if len(f.cmds) != 1 || f.cmds[0] != c.wantCmds[len(c.wantCmds)-1] {
			t.Errorf("admin-password commands %v", f.cmds)
		}
	}
}

// The unit lets the service write only below <data>/var; a data directory or certificate under /home would
// vanish under ProtectHome=yes.
func TestSystemdUnitProtectsHome(t *testing.T) {
	for _, c := range []struct {
		data  string
		certs []string
		want  string
	}{
		{"/var/lib/kaze-sync", nil, "yes"},
		{"/var/lib/kaze-sync", []string{"/etc/ssl/c.pem", "/etc/ssl/k.pem"}, "yes"},
		{"/home/u/kaze", nil, "read-only"},
		{"/root/kaze", nil, "read-only"},
		{"/var/lib/kaze-sync", []string{"/home/u/c.pem", "/home/u/k.pem"}, "read-only"},
		{"/var/lib/kaze-sync", []string{"/run/user/1000/c.pem"}, "read-only"},
		{"/homestead/kaze", nil, "yes"},
	} {
		f := newFakeSystem(t)
		if err := f.in.writeService(&systemd, c.data, c.certs); err != nil {
			t.Fatal(err)
		}
		text, _ := os.ReadFile(filepath.Join(f.root, systemd.file))
		for _, w := range []string{"ProtectHome=" + c.want + "\n", "ReadWritePaths=" + c.data + "/var\n", "ExecStart=" + c.data + "/kaze-syncd run"} {
			if !strings.Contains(string(text), w) {
				t.Errorf("data %s, certs %v: unit lacks %q:\n%s", c.data, c.certs, w, text)
			}
		}
	}
}

func TestInstallRefusesWANAndUnknownManager(t *testing.T) {
	f := newFakeSystem(t)
	f.useManager(&systemd)
	if err := f.install(InstallOptions{WAN: true}); err == nil || !strings.Contains(err.Error(), "--wan") {
		t.Fatalf("--wan on systemd: %v", err)
	}
	for _, s := range []*service{&procd, &systemd, &openrc} {
		os.RemoveAll(filepath.Join(f.root, s.probe))
	}
	if err := f.install(InstallOptions{}); err == nil || !strings.Contains(err.Error(), "no supported service manager") {
		t.Fatalf("unknown manager: %v", err)
	}
	if _, err := os.Stat(f.data); err == nil || len(f.cmds) != 0 {
		t.Fatalf("something was installed: %v", f.cmds)
	}
}

func TestInstallIsRepeatable(t *testing.T) {
	f := newFakeSystem(t)
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	db, _ := OpenDB(filepath.Join(f.data, "var", "meta.db"), false)
	listen := MetaString(db, "listen")
	SetMeta(db, "admin_password_hash", "keep")
	db.Close()
	cert1, _ := os.ReadFile(filepath.Join(f.data, "var", "tls", "cert.pem"))
	os.WriteFile(f.in.Self, []byte("binary v2"), 0o755)
	f.out.Reset()
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	db, _ = OpenDB(filepath.Join(f.data, "var", "meta.db"), false)
	defer db.Close()
	if MetaString(db, "listen") != listen || MetaString(db, "admin_password_hash") != "keep" {
		t.Fatal("upgrade changed port or data")
	}
	if cert2, _ := os.ReadFile(filepath.Join(f.data, "var", "tls", "cert.pem")); !bytes.Equal(cert1, cert2) {
		t.Fatal("certificate regenerated")
	}
	if b, _ := os.ReadFile(filepath.Join(f.data, "kaze-syncd")); string(b) != "binary v2" {
		t.Fatal("binary not upgraded")
	}
	conf, _ := os.ReadFile(filepath.Join(f.root, "etc/sysupgrade.conf"))
	if strings.Count(string(conf), "kaze-sync") != 2 {
		t.Fatalf("sysupgrade.conf duplicated lines: %q", conf)
	}
	// --listen changes the port.
	if err := f.install(InstallOptions{Listen: ":41873"}); err != nil {
		t.Fatal(err)
	}
	db2, _ := OpenDB(filepath.Join(f.data, "var", "meta.db"), false)
	defer db2.Close()
	if MetaString(db2, "listen") != ":41873" {
		t.Fatal("--listen ignored")
	}
}

func TestInstallKeepsExistingSysupgradeConf(t *testing.T) {
	f := newFakeSystem(t)
	os.MkdirAll(filepath.Join(f.root, "etc"), 0o755)
	os.WriteFile(filepath.Join(f.root, "etc/sysupgrade.conf"), []byte("# mine\n/etc/foo"), 0o644)
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	conf, _ := os.ReadFile(filepath.Join(f.root, "etc/sysupgrade.conf"))
	if string(conf) != "# mine\n/etc/foo\n/etc/init.d/kaze-sync\n/etc/rc.d/S99kaze-sync\n" {
		t.Fatalf("sysupgrade.conf = %q", conf)
	}
}

func TestInstallFirewallRule(t *testing.T) {
	f := newFakeSystem(t)
	if err := f.install(InstallOptions{WAN: true, Listen: ":41873"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"uci set firewall.kaze_sync=rule", "uci set firewall.kaze_sync.name=kaze-sync", "uci set firewall.kaze_sync.src=wan",
		"uci set firewall.kaze_sync.proto=tcp", "uci set firewall.kaze_sync.dest_port=41873", "uci set firewall.kaze_sync.target=ACCEPT",
		"uci commit firewall", "/etc/init.d/firewall reload"} {
		if !f.hasCmd(want) {
			t.Errorf("missing command %q in %v", want, f.cmds)
		}
	}
	// Without --wan the rule is deleted (delete succeeding means it existed: commit and reload).
	f.cmds = nil
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if !f.hasCmd("uci -q delete firewall.kaze_sync") || !f.hasCmd("uci commit firewall") || !f.hasCmd("firewall reload") {
		t.Fatalf("rule not removed: %v", f.cmds)
	}
	// When there was no rule (delete fails) nothing is reloaded.
	f.cmds, f.failCmd = nil, "uci -q delete"
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.hasCmd("firewall reload") {
		t.Fatal("firewall reloaded needlessly")
	}
}

func writeCertPair(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kder, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return
}

func TestInstallCertificateFiles(t *testing.T) {
	f := newFakeSystem(t)
	cert, key := writeCertPair(t, t.TempDir())
	if err := f.install(InstallOptions{Cert: cert}); err == nil {
		t.Fatal("--cert without --key accepted")
	}
	if err := f.install(InstallOptions{Cert: cert, Key: "/nonexistent.pem"}); err == nil {
		t.Fatal("unreadable key accepted")
	}
	if err := f.install(InstallOptions{Cert: cert, Key: key}); err != nil {
		t.Fatal(err)
	}
	db, _ := OpenDB(filepath.Join(f.data, "var", "meta.db"), false)
	defer db.Close()
	if MetaString(db, "cert") != cert || MetaString(db, "key") != key {
		t.Fatal("cert paths not stored")
	}
	if strings.Contains(f.out.String(), "fingerprint:") || !strings.Contains(f.out.String(), "without --fingerprint") {
		t.Fatalf("output:\n%s", f.out.String())
	}
	if _, err := os.Stat(filepath.Join(f.data, "var", "tls")); err == nil {
		t.Fatal("self-signed certificate created in certificate mode")
	}
	// A later install without flags keeps the mode.
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if MetaString(db, "cert") != cert {
		t.Fatal("mode lost")
	}
}

func TestInstallRejectsUnsafeDataPath(t *testing.T) {
	f := newFakeSystem(t)
	for _, p := range []string{"relative/dir", "/mnt/with space", "/mnt/$(x)"} {
		if err := f.in.Install(InstallOptions{Data: p}); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestAdminPassword(t *testing.T) {
	f := newFakeSystem(t)
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	f.cmds, f.out = nil, bytes.Buffer{}
	if err := f.in.AdminPassword(f.data); err != nil {
		t.Fatal(err)
	}
	pw := regexp.MustCompile(`: ([A-Z2-7]{5}(?:-[A-Z2-7]{5}){3})\n`).FindStringSubmatch(f.out.String())
	if pw == nil {
		t.Fatalf("no password in output:\n%s", f.out.String())
	}
	if len(f.cmds) != 1 || f.cmds[0] != "/etc/init.d/kaze-sync restart" {
		t.Fatalf("commands %v", f.cmds)
	}
	// The server accepts it; a second call replaces it.
	s, err := New(f.data, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.checkAdminPassword(pw[1]) || s.checkAdminPassword("AAAAA-AAAAA-AAAAA-AAAAA") {
		t.Fatal("password check")
	}
	f.out.Reset()
	f.in.AdminPassword(f.data)
	if s.checkAdminPassword(pw[1]) {
		t.Fatal("old password still valid")
	}
}

func TestRunFailsWithoutDatabase(t *testing.T) {
	if _, err := New(t.TempDir(), nil); err == nil {
		t.Fatal("New succeeded on an empty directory")
	}
	if err := Run(t.Context(), t.TempDir()); err == nil {
		t.Fatal("Run succeeded on an empty directory")
	}
}

func TestRunServesTLSAndStops(t *testing.T) {
	f := newFakeSystem(t)
	if err := f.install(InstallOptions{Listen: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	// Port 0 is only for this test; a real install stores a concrete port.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, f.data) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// Installing again over data from an older version migrates every database and hands what root
// restored from a backup (design 9.4) to the service user: databases and block directories, not block files.
func TestInstallUpgradesDatabasesAndFixesOwnership(t *testing.T) {
	f := newFakeSystem(t)
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(f.data, "var")
	// A folder database of the previous schema, as a backup holds it, and a block below two directories.
	db, _ := OpenDB(filepath.Join(state, "meta.db"), false)
	db.Exec(`INSERT INTO folders(id, name, rules) VALUES(1, 'docs', '')`)
	db.Close()
	os.MkdirAll(filepath.Join(state, "folders"), 0o700)
	old, _ := OpenDB(FolderDBPath(state, 1), true)
	if _, err := old.Exec(folderMigrations[0]); err != nil {
		t.Fatal(err)
	}
	SetMeta(old, "schema_version", 1)
	old.Close()
	blob := filepath.Join(state, "blobs", "ab", "cd", strings.Repeat("ab", 32))
	os.MkdirAll(filepath.Dir(blob), 0o755)
	os.WriteFile(blob, []byte("x"), 0o444)

	f.chowned = nil
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	fdb, _ := OpenDB(FolderDBPath(state, 1), false)
	defer fdb.Close()
	if v, _ := schemaVersion(fdb); v != FolderSchemaVersion {
		t.Fatalf("folder schema %d after install, want %d", v, FolderSchemaVersion)
	}
	if b := backupsOf(state, "folder-1"); len(b) != 1 || !strings.Contains(b[0], "folder-1-v1-") {
		t.Fatalf("install did not back up the old folder database: %v", b)
	}
	owned := map[string]bool{}
	for _, p := range f.chowned {
		owned[p] = true
	}
	for _, p := range []string{filepath.Join(state, "folders"), FolderDBPath(state, 1), filepath.Dir(blob), filepath.Join(state, "blobs", "ab")} {
		if !owned[p] {
			t.Errorf("%s not handed to the service user", p)
		}
	}
	if owned[blob] {
		t.Error("block files are chowned one by one")
	}
}

// root hands var/ to the service user, which can plant symbolic links there: the real Chown must not follow
// them (a dangling link would fail with os.Chown), and a link in place of blobs/ stops the install.
func TestInstallDoesNotFollowSymbolicLinks(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	os.Symlink(filepath.Join(dir, "nowhere"), link)
	if err := NewInstaller().Chown(link, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("Chown follows the link: %v", err)
	}

	f := newFakeSystem(t)
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	os.Mkdir(victim, 0o700)
	blobs := filepath.Join(f.data, "var", "blobs")
	os.RemoveAll(blobs)
	os.Symlink(victim, blobs)
	if err := f.install(InstallOptions{}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("install over a linked blobs directory: %v", err)
	}
	if entries, _ := os.ReadDir(victim); len(entries) != 0 {
		t.Fatalf("the link target was written to: %v", entries)
	}
}
