package installer

import (
	"bytes"
	"strings"
	"testing"
)

func fixture(t *testing.T, reg *Registration) []byte {
	t.Helper()
	// The prefix holds a decoy gzip header: the digest, not the marker, selects the payload.
	prefix := append([]byte("MZ\x00"), 0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0, 'x')
	b, err := Build(prefix, []File{{Name: "kaze-syncc", Data: []byte("bin")}},
		Manifest{Version: "1.2.3", AgentVersion: 6, OS: "linux", Arch: "amd64"}, reg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOpenWithAndWithoutRegistration(t *testing.T) {
	t.Parallel()
	plain := fixture(t, nil)
	p, err := Open(bytes.NewReader(plain), int64(len(plain)))
	if err != nil || p.Registration != nil || p.Manifest.Version != "1.2.3" || p.PayloadStart != 12 {
		t.Fatalf("plain: %+v %v", p, err)
	}
	reg := &Registration{Server: "https://10.0.0.3:8443", Fingerprint: "sha256:" + strings.Repeat("ab", 32), Code: "ABCD-EFGH", Name: "laptop"}
	full := fixture(t, reg)
	p, err = Open(bytes.NewReader(full), int64(len(full)))
	if err != nil || p.Registration == nil || *p.Registration != *reg {
		t.Fatalf("registered: %+v %v", p, err)
	}
	if err := p.Registration.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsDamagedFiles(t *testing.T) {
	t.Parallel()
	good := fixture(t, nil)
	flipped := append([]byte(nil), good...)
	flipped[len(good)/2] ^= 1
	for name, b := range map[string][]byte{
		"truncated": good[:len(good)-40], "flipped payload": flipped, "empty": nil, "plain exe": []byte("MZ plain program"),
	} {
		if _, err := Open(bytes.NewReader(b), int64(len(b))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRegistrationValidateRejects(t *testing.T) {
	t.Parallel()
	ok := Registration{Server: "https://host:8443", Code: "ABCD-EFGH", Name: "a_b-1"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Registration){
		"http":        func(g *Registration) { g.Server = "http://host" },
		"userinfo":    func(g *Registration) { g.Server = "https://u:p@host" },
		"path":        func(g *Registration) { g.Server = "https://host/x" },
		"no host":     func(g *Registration) { g.Server = "https://" },
		"query":       func(g *Registration) { g.Server = "https://host?a=b" },
		"code space":  func(g *Registration) { g.Code = "AB CD; rm" },
		"short code":  func(g *Registration) { g.Code = "A" },
		"fingerprint": func(g *Registration) { g.Fingerprint = "sha256:zz" },
		"name":        func(g *Registration) { g.Name = "a\nb" },
	} {
		g := ok
		mut(&g)
		if g.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
