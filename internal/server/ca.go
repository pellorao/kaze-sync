package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Local CA mode (design 2.5): the CA certificate and key live in <data>/ca (root's, 0700); the server
// certificate it signs, with the CA certificate behind it and its own key, is one file in var/tls, which
// the service reads. The service cannot read the CA key unless it runs as the user who owns it.
const (
	CADir      = "ca"
	caCertFile = "ca.pem"
	caKeyFile  = "ca-key.pem"
	serverFile = "server.pem"

	caLife       = 20 * 365 * 24 * time.Hour
	leafLife     = 730 * 24 * time.Hour
	renewBefore  = 90 * 24 * time.Hour // the service renews itself, when it may, this long before expiry
	warnBefore   = 30 * 24 * time.Hour // and complains when it may not
	certCheckGap = 24 * time.Hour
)

func serverPEMPath(data string) string { return filepath.Join(data, VarDir, TLSDir, serverFile) }

// EnsureCA creates the CA in dir when absent (certificate 0644, key 0600, directory 0700).
func EnsureCA(dir string) error {
	certPath, keyPath := filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
	if _, err := os.Stat(keyPath); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "kaze-sync local CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(caLife),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true,
		MaxPathLen: 0, MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	// The key first: a certificate without its key is never created, and the next run starts over.
	if err := writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	return n
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ServerSANs lists the names the server certificate must cover: loopback, the host name, the host in the
// listen address and the extra names (comma separated, from --san).
func ServerSANs(listen, extra string) []string {
	set := []string{"localhost", "127.0.0.1", "::1"}
	if h, err := os.Hostname(); err == nil {
		set = append(set, strings.ToLower(h))
	}
	if host, _, err := net.SplitHostPort(listen); err == nil {
		set = append(set, host)
	}
	for _, e := range strings.Split(extra, ",") {
		set = append(set, strings.TrimSpace(e))
	}
	var out []string
	for _, s := range set {
		if ip := net.ParseIP(s); ip != nil {
			if ip.IsUnspecified() {
				continue
			}
			s = ip.String()
		}
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// ValidSAN reports whether s can go into a certificate as a name or an address.
func ValidSAN(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	return s != "" && !strings.ContainsAny(s, " \t/:@,") && len(s) <= 253
}

func sanSet(c *x509.Certificate) []string {
	out := slices.Clone(c.DNSNames)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	slices.Sort(out)
	return out
}

// EnsureServerCert makes the server certificate in <data>/var/tls/server.pem valid for sans, signed by the
// CA in caDir, and valid for more than renewBefore. It reports whether it wrote a new one.
func EnsureServerCert(data string, sans []string, now time.Time) (bool, error) {
	caDir := filepath.Join(data, CADir)
	caCert, caKey, err := loadCA(caDir)
	if err != nil {
		return false, err
	}
	path := serverPEMPath(data)
	if old, err := loadChain(path); err == nil {
		leaf, err := x509.ParseCertificate(old.Certificate[0])
		if err == nil && leaf.NotAfter.Sub(now) > renewBefore && leaf.CheckSignatureFrom(caCert) == nil &&
			slices.Equal(sanSet(leaf), sans) {
			return false, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "kaze-sync"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(leafLife),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, s)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return false, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return false, err
	}
	var buf bytes.Buffer
	pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	pem.Encode(&buf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	// One file, replaced by one rename: a reader never sees a certificate with the wrong key.
	return true, writeAtomic(path, buf.Bytes(), 0o600)
}

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, nil, err
	}
	cblk, _ := pem.Decode(cb)
	kblk, _ := pem.Decode(kb)
	if cblk == nil || kblk == nil {
		return nil, nil, errors.New("CA files are not PEM")
	}
	cert, err := x509.ParseCertificate(cblk.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kblk.Bytes)
	return cert, key, err
}

// loadChain reads a server.pem: the server certificate, the CA certificate, the key.
func loadChain(path string) (*tls.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var certs, key []byte
	for rest := b; ; {
		var blk *pem.Block
		if blk, rest = pem.Decode(rest); blk == nil {
			break
		}
		if blk.Type == "CERTIFICATE" {
			certs = append(certs, pem.EncodeToMemory(blk)...)
		} else {
			key = pem.EncodeToMemory(blk)
		}
	}
	c, err := tls.X509KeyPair(certs, key)
	if err != nil {
		return nil, err
	}
	if len(c.Certificate) < 2 {
		return nil, errors.New(path + " has no CA certificate behind the server certificate")
	}
	return &c, nil
}

// CAFingerprint is "sha256:<hex>" of the CA certificate behind the server certificate, the form clients pin.
func CAFingerprint(c *tls.Certificate) string {
	sum := sha256.Sum256(c.Certificate[len(c.Certificate)-1])
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CACertPEM returns the CA certificate of the chain, the only part of the CA that is exported.
func CACertPEM(c *tls.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[len(c.Certificate)-1]})
}

// CertSource gives the TLS server the certificate in a server.pem and picks up a replaced file (a renewal
// by root while the service runs) at most once a minute.
type CertSource struct {
	path string
	gap  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate
	mod     time.Time
	checked time.Time
}

func NewCertSource(path string) (*CertSource, error) {
	cs := &CertSource{path: path, gap: time.Minute, now: time.Now}
	c, err := loadChain(path)
	if err != nil {
		return nil, err
	}
	cs.cert = c
	if fi, err := os.Stat(path); err == nil {
		cs.mod = fi.ModTime()
	}
	cs.checked = cs.now()
	return cs, nil
}

func (cs *CertSource) Current() *tls.Certificate {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.cert
}

func (cs *CertSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if now := cs.now(); now.Sub(cs.checked) >= cs.gap {
		cs.checked = now
		if fi, err := os.Stat(cs.path); err == nil && !fi.ModTime().Equal(cs.mod) {
			if c, err := loadChain(cs.path); err != nil {
				slog.Warn("reading the renewed server certificate failed, keeping the old one", "err", err)
			} else {
				cs.cert, cs.mod = c, fi.ModTime()
				slog.Info("server certificate reloaded")
			}
		}
	}
	return cs.cert, nil
}

// CheckServerCert reports an error (logged and pushed by certLoop) when the server certificate is about to expire. The service runs as the unprivileged
// kaze-sync user and cannot read the CA key, so it never renews: root does, with `install` or `kaze-syncd ca`.
func CheckServerCert(data string, now time.Time) error {
	c, err := loadChain(serverPEMPath(data))
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return err
	}
	if left := leaf.NotAfter.Sub(now); left < warnBefore {
		return fmt.Errorf("server certificate expires on %s and this service cannot renew it: run `kaze-syncd ca --data %s` as root",
			leaf.NotAfter.Format(time.DateOnly), data)
	}
	return nil
}

// SetupCA turns on CA mode for the installation in data: it stores the mode and the extra names, creates the
// CA and the server certificate, and returns the chain the service will serve.
func SetupCA(db *sql.DB, data string, extraSAN string, setSAN bool) (*tls.Certificate, error) {
	for _, s := range strings.Split(extraSAN, ",") {
		if s = strings.TrimSpace(s); s != "" && !ValidSAN(s) {
			return nil, fmt.Errorf("--san: %q is not a host name or an IP address", s)
		}
	}
	if err := EnsureCA(filepath.Join(data, CADir)); err != nil {
		return nil, err
	}
	if setSAN {
		if err := SetMeta(db, "san", extraSAN); err != nil {
			return nil, err
		}
	}
	if _, err := EnsureServerCert(data, ServerSANs(MetaString(db, "listen"), MetaString(db, "san")), time.Now()); err != nil {
		return nil, err
	}
	for k, v := range map[string]string{"tls": "ca", "cert": "", "key": ""} { // the mode replaces certificate files
		if err := SetMeta(db, k, v); err != nil {
			return nil, err
		}
	}
	return loadChain(serverPEMPath(data))
}

// CAOptions are the arguments of `kaze-syncd ca`.
type CAOptions struct {
	Data   string
	SAN    string // extra names, comma separated; kept when empty
	Export string // write the CA certificate here ("-" is the output)
}

// CA is `kaze-syncd ca`: it checks and renews the server certificate of an installation in CA mode (run by
// root), and exports the CA certificate.
func (in *Installer) CA(o CAOptions) error {
	db, err := OpenDB(filepath.Join(o.Data, VarDir, DBFile), false)
	if err != nil {
		return err
	}
	defer db.Close()
	if MetaString(db, "tls") != "ca" {
		return errors.New("this installation is not in CA mode: run `install --ca` as root (clients must pin the new fingerprint)")
	}
	chain, err := SetupCA(db, o.Data, o.SAN, o.SAN != "")
	if err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := in.chownData(o.Data); err != nil { // the certificate must stay readable for the service user
			return err
		}
	}
	if o.Export != "-" {
		in.say("CA fingerprint: %s", CAFingerprint(chain))
	}
	switch o.Export {
	case "":
	case "-":
		_, err = in.Out.Write(CACertPEM(chain))
	default:
		err = os.WriteFile(o.Export, CACertPEM(chain), 0o644)
	}
	return err
}
