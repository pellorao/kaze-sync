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
	failCmd string
}

func newFakeSystem(t *testing.T) *fakeSystem {
	t.Helper()
	dir := t.TempDir()
	f := &fakeSystem{t: t, root: filepath.Join(dir, "root"), data: filepath.Join(dir, "mnt", "kaze-sync")}
	self := filepath.Join(dir, "self-binary")
	os.WriteFile(self, []byte("binary v1"), 0o755)
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
		Chown:      func(p string, uid, gid int) error { f.chowned = append(f.chowned, p); return nil },
	}
	return f
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
	db, err := OpenDB(filepath.Join(f.data, "meta.db"), false)
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
	for _, want := range []string{f.data, "meta.db", "blobs", "tmp", "key.pem"} {
		if !strings.Contains(owned, want) {
			t.Errorf("%s not chowned: %s", want, owned)
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

func TestInstallIsRepeatable(t *testing.T) {
	f := newFakeSystem(t)
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	db, _ := OpenDB(filepath.Join(f.data, "meta.db"), false)
	listen := MetaString(db, "listen")
	SetMeta(db, "admin_password_hash", "keep")
	db.Close()
	cert1, _ := os.ReadFile(filepath.Join(f.data, "tls", "cert.pem"))
	os.WriteFile(f.in.Self, []byte("binary v2"), 0o755)
	f.out.Reset()
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	db, _ = OpenDB(filepath.Join(f.data, "meta.db"), false)
	defer db.Close()
	if MetaString(db, "listen") != listen || MetaString(db, "admin_password_hash") != "keep" {
		t.Fatal("upgrade changed port or data")
	}
	if cert2, _ := os.ReadFile(filepath.Join(f.data, "tls", "cert.pem")); !bytes.Equal(cert1, cert2) {
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
	db2, _ := OpenDB(filepath.Join(f.data, "meta.db"), false)
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
	db, _ := OpenDB(filepath.Join(f.data, "meta.db"), false)
	defer db.Close()
	if MetaString(db, "cert") != cert || MetaString(db, "key") != key {
		t.Fatal("cert paths not stored")
	}
	if strings.Contains(f.out.String(), "fingerprint:") || !strings.Contains(f.out.String(), "without --fingerprint") {
		t.Fatalf("output:\n%s", f.out.String())
	}
	if _, err := os.Stat(filepath.Join(f.data, "tls")); err == nil {
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
