package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func caData(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	if err := EnsureCA(filepath.Join(data, CADir)); err != nil {
		t.Fatal(err)
	}
	return data
}

func leafOf(t *testing.T, data string) (*x509.Certificate, *tls.Certificate) {
	t.Helper()
	c, err := loadChain(serverPEMPath(data))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf, c
}

func TestCAFilesAndPermissions(t *testing.T) {
	data := caData(t)
	dir := filepath.Join(data, CADir)
	for name, mode := range map[string]os.FileMode{caKeyFile: 0o600, caCertFile: 0o644, ".": 0o700} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v", name, fi, err)
		}
	}
	before, _ := os.ReadFile(filepath.Join(dir, caCertFile))
	if err := EnsureCA(dir); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, caCertFile)); string(after) != string(before) {
		t.Fatal("an existing CA was replaced")
	}
	if _, err := EnsureServerCert(data, []string{"localhost"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(serverPEMPath(data)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("server.pem mode %v", fi.Mode())
	}
}

func TestServerCertCoversNamesAndChainsToCA(t *testing.T) {
	data := caData(t)
	sans := ServerSANs("192.168.1.1:4000", "nas.example.org, 10.0.0.5")
	for _, want := range []string{"localhost", "127.0.0.1", "::1", "192.168.1.1", "nas.example.org", "10.0.0.5"} {
		if !slices.Contains(sans, want) {
			t.Fatalf("SANs %v lack %s", sans, want)
		}
	}
	if h, _ := os.Hostname(); !slices.Contains(sans, strings.ToLower(h)) {
		t.Fatalf("SANs %v lack the host name", sans)
	}
	if got := ServerSANs("0.0.0.0:1", ""); slices.Contains(got, "0.0.0.0") {
		t.Fatalf("wildcard address in %v", got)
	}
	if _, err := EnsureServerCert(data, sans, time.Now()); err != nil {
		t.Fatal(err)
	}
	leaf, chain := leafOf(t, data)
	ca, _ := x509.ParseCertificate(chain.Certificate[1])
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, name := range []string{"localhost", "127.0.0.1", "::1", "nas.example.org", "10.0.0.5"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "evil.example.org"}); err == nil {
		t.Error("an uncovered name verified")
	}
	if leaf.IsCA || ca.MaxPathLen != 0 || !ca.IsCA {
		t.Error("CA flags")
	}
	if !ValidSAN("a.example.org") || ValidSAN("a b") || ValidSAN("") || ValidSAN("x:1") {
		t.Error("ValidSAN")
	}
}

func TestServerCertRenewal(t *testing.T) {
	data := caData(t)
	now := time.Now()
	sans := []string{"localhost"}
	if renewed, err := EnsureServerCert(data, sans, now); err != nil || !renewed {
		t.Fatalf("first: %v %v", renewed, err)
	}
	first, _ := leafOf(t, data)
	if renewed, _ := EnsureServerCert(data, sans, now.Add(24*time.Hour)); renewed {
		t.Fatal("a fresh certificate was renewed")
	}
	if renewed, _ := EnsureServerCert(data, []string{"localhost", "x.example.org"}, now); !renewed {
		t.Fatal("changed names did not renew")
	}
	second, _ := leafOf(t, data)
	if second.SerialNumber.Cmp(first.SerialNumber) == 0 || !slices.Contains(second.DNSNames, "x.example.org") {
		t.Fatal("not replaced")
	}
	// Close to expiry: renewed, and the new one is valid then.
	late := now.Add(leafLife - renewBefore + time.Hour)
	if renewed, _ := EnsureServerCert(data, []string{"localhost", "x.example.org"}, late); !renewed {
		t.Fatal("expiring certificate not renewed")
	}
	third, _ := leafOf(t, data)
	if !third.NotAfter.After(late.Add(renewBefore)) {
		t.Fatalf("renewed one expires %v", third.NotAfter)
	}
	// A certificate of another CA is replaced.
	other := caData(t)
	b, _ := os.ReadFile(filepath.Join(other, CADir, caCertFile))
	k, _ := os.ReadFile(filepath.Join(other, CADir, caKeyFile))
	os.WriteFile(filepath.Join(data, CADir, caCertFile), b, 0o644)
	os.WriteFile(filepath.Join(data, CADir, caKeyFile), k, 0o600)
	if renewed, _ := EnsureServerCert(data, []string{"localhost", "x.example.org"}, now); !renewed {
		t.Fatal("certificate of the former CA kept")
	}
}

func TestCheckServerCertWithoutCAKey(t *testing.T) {
	data := caData(t)
	db := installDB(t, data)
	now := time.Now()
	if _, err := EnsureServerCert(data, ServerSANs("", ""), now); err != nil {
		t.Fatal(err)
	}
	if renewed, err := CheckServerCert(db, data, now.Add(leafLife-time.Hour)); err != nil || !renewed {
		t.Fatalf("with the key: %v %v", renewed, err)
	}
	// The service user cannot read the key: nothing is renewed, only a warning is logged.
	os.Remove(filepath.Join(data, CADir, caKeyFile))
	if renewed, err := CheckServerCert(db, data, now.Add(2*leafLife)); err != nil || renewed {
		t.Fatalf("without the key: %v %v", renewed, err)
	}
}

func installDB(t *testing.T, data string) *sql.DB {
	t.Helper()
	os.MkdirAll(filepath.Join(data, VarDir), 0o700)
	db, err := OpenDB(filepath.Join(data, VarDir, DBFile), true)
	if err != nil || Migrate(db) != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCertSourceReloadsAReplacedFile(t *testing.T) {
	data := caData(t)
	if _, err := EnsureServerCert(data, []string{"localhost"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	cs, err := NewCertSource(serverPEMPath(data))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	cs.now = func() time.Time { return clock }
	old, _ := cs.GetCertificate(nil)
	// Renewal by another process (root, while the service runs).
	if _, err := EnsureServerCert(data, []string{"localhost", "b.example.org"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(serverPEMPath(data), time.Now(), time.Now().Add(time.Second))
	if got, _ := cs.GetCertificate(nil); got != old {
		t.Fatal("looked at the file again within a minute")
	}
	clock = clock.Add(2 * time.Minute)
	got, _ := cs.GetCertificate(nil)
	if got == old {
		t.Fatal("renewed file not picked up")
	}
	// A broken file keeps the working certificate.
	os.WriteFile(serverPEMPath(data), []byte("junk"), 0o600)
	os.Chtimes(serverPEMPath(data), time.Now(), time.Now().Add(2*time.Second))
	clock = clock.Add(2 * time.Minute)
	if again, _ := cs.GetCertificate(nil); again != got {
		t.Fatal("a broken file replaced the certificate")
	}
}

func TestInstallCAMode(t *testing.T) {
	f := newFakeSystem(t)
	cert, key := writeCertPair(t, t.TempDir())
	if err := f.install(InstallOptions{CA: true, Cert: cert, Key: key}); err == nil {
		t.Fatal("--ca with --cert accepted")
	}
	if err := f.install(InstallOptions{SAN: "a.example.org"}); err == nil {
		t.Fatal("--san without --ca accepted")
	}
	f.out.Reset()
	if err := f.install(InstallOptions{Listen: "10.1.2.3:4000", CA: true, SAN: "nas.local"}); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`certificate fingerprint: (sha256:[0-9a-f]{64})`).FindStringSubmatch(f.out.String())
	if m == nil || !strings.Contains(f.out.String(), "ca --data") {
		t.Fatalf("output:\n%s", f.out.String())
	}
	chain, err := loadChain(serverPEMPath(f.data))
	if err != nil || CAFingerprint(chain) != m[1] {
		t.Fatalf("printed %s, chain %v", m[1], err)
	}
	leaf, _ := x509.ParseCertificate(chain.Certificate[0])
	for _, n := range []string{"nas.local", "10.1.2.3", "localhost"} {
		if !slices.Contains(sanSet(leaf), n) {
			t.Fatalf("SANs %v lack %s", sanSet(leaf), n)
		}
	}
	ca := filepath.Join(f.data, CADir)
	if fi, _ := os.Stat(filepath.Join(ca, caKeyFile)); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Fatal("CA key missing or open")
	}
	for _, p := range []string{ca, filepath.Join(ca, caKeyFile), filepath.Join(ca, caCertFile)} {
		if uid, ok := f.owners[p]; !ok || uid != 0 {
			t.Fatalf("%s not handed to root: %v %v", p, uid, ok)
		}
	}
	if uid := f.owners[serverPEMPath(f.data)]; uid != 32768 {
		t.Fatalf("server.pem owner %d, the service user must read it", uid)
	}
	if _, err := os.Stat(filepath.Join(f.data, "var", "tls", "cert.pem")); err == nil {
		t.Fatal("self-signed certificate created in CA mode")
	}
	// The service tree must not contain the CA key.
	filepath.WalkDir(filepath.Join(f.data, "var"), func(p string, d os.DirEntry, err error) error {
		if d != nil && d.Name() == caKeyFile {
			t.Errorf("CA key below var: %s", p)
		}
		return nil
	})
	// Later installs keep mode, certificate and fingerprint.
	f.out.Reset()
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), m[1]) {
		t.Fatalf("fingerprint changed:\n%s", f.out.String())
	}
	// A new name renews the certificate, not the CA; --cert leaves the mode.
	if err := f.install(InstallOptions{SAN: "other.local"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := loadChain(serverPEMPath(f.data)); CAFingerprint(again) != m[1] {
		t.Fatal("CA changed")
	}
	if err := f.install(InstallOptions{Cert: cert, Key: key}); err != nil {
		t.Fatal(err)
	}
	db, _ := OpenDB(filepath.Join(f.data, "var", "meta.db"), false)
	defer db.Close()
	if MetaString(db, "tls") == "ca" || MetaString(db, "cert") != cert {
		t.Fatal("--cert did not leave CA mode")
	}
	// And back: --ca replaces the certificate files.
	if err := f.install(InstallOptions{CA: true}); err != nil {
		t.Fatal(err)
	}
	if MetaString(db, "tls") != "ca" || MetaString(db, "cert") != "" {
		t.Fatal("--ca did not replace certificate mode")
	}
}

func TestCACommand(t *testing.T) {
	f := newFakeSystem(t)
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.in.CA(CAOptions{Data: f.data}); err == nil {
		t.Fatal("ca switched a self-signed installation without --enable")
	}
	f.out.Reset()
	exp := filepath.Join(t.TempDir(), "ca.pem")
	if err := f.in.CA(CAOptions{Data: f.data, Enable: true, SAN: "x.local", Export: exp}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(exp)
	blk, _ := pem.Decode(b)
	if blk == nil || strings.Contains(string(b), "PRIVATE") {
		t.Fatalf("export:\n%s", b)
	}
	c, _ := x509.ParseCertificate(blk.Bytes)
	if !c.IsCA {
		t.Fatal("exported certificate is not the CA")
	}
	sum := sha256.Sum256(blk.Bytes)
	if !strings.Contains(f.out.String(), "sha256:"+hex.EncodeToString(sum[:])) {
		t.Fatalf("output:\n%s", f.out.String())
	}
	// Repeating needs no --enable, keeps the CA, and "-" prints only the certificate.
	f.out.Reset()
	if err := f.in.CA(CAOptions{Data: f.data, Export: "-"}); err != nil {
		t.Fatal(err)
	}
	if f.out.String() != string(b) {
		t.Fatalf("stdout export:\n%s", f.out.String())
	}
	if err := f.in.CA(CAOptions{Data: f.data, SAN: "bad name"}); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestRunServesCAMode(t *testing.T) {
	f := newFakeSystem(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	if err := f.install(InstallOptions{Listen: addr, CA: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, f.data) }()
	defer func() { cancel(); <-done }()
	chain, _ := loadChain(serverPEMPath(f.data))
	ca, _ := x509.ParseCertificate(chain.Certificate[1])
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	var conn *tls.Conn
	var err error
	for i := 0; i < 50; i++ {
		conn, err = tls.Dial("tcp", addr, &tls.Config{RootCAs: roots, ServerName: "localhost"})
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

// The first certificate check runs right after start: its alert must reach ntfy, so the notifier has to be set
// before the loop starts.
func TestRunFirstCertAlertIsPushed(t *testing.T) {
	f := newFakeSystem(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	if err := f.install(InstallOptions{Listen: addr, CA: true}); err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		select {
		case got <- struct{}{}:
		default:
		}
	}))
	defer ts.Close()
	os.WriteFile(filepath.Join(f.data, "ntfy.json"), []byte(`{"url":"`+ts.URL+`","topic":"t"}`), 0o600)
	os.WriteFile(filepath.Join(f.data, CADir, caCertFile), []byte("broken"), 0o644) // the check fails at once
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, f.data) }()
	defer func() { cancel(); <-done }()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the alert of the first certificate check was lost")
	}
}
