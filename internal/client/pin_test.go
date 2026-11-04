package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kaze-sync/internal/server"
)

type pinCert struct {
	der  []byte
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// mkCert makes a certificate signed by parent (nil: self-signed).
func mkCert(t *testing.T, cn string, ca bool, parent *pinCert, notAfter time.Time) *pinCert {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, BasicConstraintsValid: true, IsCA: ca,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{cn},
	}
	if ca {
		tmpl.KeyUsage = x509.KeyUsageCertSign
	}
	signer, signKey := tmpl, key
	if parent != nil {
		signer, signKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &pinCert{der, c, key}
}

func pinOf(c *pinCert) []byte { s := sha256.Sum256(c.der); return s[:] }

func TestCheckPin(t *testing.T) {
	soon := time.Now().Add(time.Hour)
	ca := mkCert(t, "ca", true, nil, soon)
	leaf := mkCert(t, "leaf", false, ca, soon)
	otherCA := mkCert(t, "ca2", true, nil, soon)
	forged := mkCert(t, "leaf", false, otherCA, soon)
	selfSigned := mkCert(t, "self", false, nil, soon)
	expired := mkCert(t, "leaf", false, ca, time.Now().Add(-time.Minute))

	if err := checkPin([][]byte{selfSigned.der}, pinOf(selfSigned)); err != nil {
		t.Errorf("pinned self-signed certificate: %v", err)
	}
	if err := checkPin([][]byte{leaf.der, ca.der}, pinOf(ca)); err != nil {
		t.Errorf("leaf under the pinned CA: %v", err)
	}
	if err := checkPin([][]byte{leaf.der, ca.der}, pinOf(leaf)); err != nil {
		t.Errorf("pinned leaf with a chain: %v", err)
	}
	for name, raw := range map[string][][]byte{
		"empty":                       nil,
		"leaf alone, CA pinned":       {leaf.der},
		"leaf of another CA":          {forged.der, otherCA.der},
		"forged leaf, pinned CA sent": {forged.der, ca.der},
		"expired leaf":                {expired.der, ca.der},
		"pinned self-signed as CA":    {forged.der, selfSigned.der},
	} {
		want := pinOf(ca)
		if name == "pinned self-signed as CA" {
			want = pinOf(selfSigned)
		}
		if err := checkPin(raw, want); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// A client enrolled with the CA fingerprint talks to a server in local CA mode, also after the server
// certificate was renewed, and a server of another CA is refused.
func TestPinnedCAOverTLS(t *testing.T) {
	data := t.TempDir()
	if err := server.EnsureCA(filepath.Join(data, server.CADir)); err != nil {
		t.Fatal(err)
	}
	pem := filepath.Join(data, server.VarDir, server.TLSDir, "server.pem")
	if _, err := server.EnsureServerCert(data, []string{"localhost", "127.0.0.1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	cs, err := server.NewCertSource(pem)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	// Like the real server: no static Certificates (httptest adds one, which wins when the client sends no name).
	ts.Listener = tls.NewListener(ts.Listener, &tls.Config{GetCertificate: cs.GetCertificate})
	ts.Start()
	defer ts.Close()

	fp := server.CAFingerprint(cs.Current())
	get := func(fp string) error {
		hc, err := NewHTTPClient(fp)
		if err != nil {
			t.Fatal(err)
		}
		hc.Transport.(*http.Transport).DisableKeepAlives = true
		resp, err := hc.Get("https://" + ts.Listener.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(fp); err != nil {
		t.Fatal(err)
	}
	if err := get("sha256:" + hex.EncodeToString(make([]byte, 32))); err == nil {
		t.Fatal("wrong fingerprint accepted")
	}
	// Another server certificate (a renewal) under the same CA is accepted without re-enrolling.
	if _, err := server.EnsureServerCert(data, []string{"localhost", "127.0.0.1", "new.example.org"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(pem, time.Now(), time.Now().Add(time.Second))
	time.Sleep(10 * time.Millisecond)
	if err := get(fp); err != nil {
		t.Fatal(err)
	}
}
