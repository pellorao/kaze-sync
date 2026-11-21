package server

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"html"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
	"kaze-sync/internal/version"
)

// enrollDialog opens the enrollment dialog as an administrator and returns the page text and the code shown in it.
func enrollDialog(t *testing.T, e *env, name string) (page, code string) {
	t.Helper()
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	st, body := e.post(c, "/devices/add", url.Values{"name": {name}})
	if st != 200 || !strings.Contains(body, `id="dlg-enroll"`) {
		t.Fatalf("add device: %d", st)
	}
	i := strings.Index(body, `class="code-big">`) + len(`class="code-big">`)
	return body, body[i : i+strings.Index(body[i:], "<")]
}

func TestEnrollDialogOffersDownloadsAndCommands(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(filepath.Dir(e.s.varDir), downloadDir)
	linux, _ := packageFixture(t, "linux", version.Version, proto.AgentVersion)
	windows, _ := packageFixture(t, "windows", "0.0.1", proto.AgentVersion)
	putFixture(t, dir, clientPackageFilename("linux"), linux)
	putFixture(t, dir, clientPackageFilename("windows"), windows)
	e.s.Fingerprint = "sha256:test"
	cert := e.ts.TLS.Certificates[0]
	e.s.serverCertificate = func() *tls.Certificate { return &cert }

	page, code := enrollDialog(t, e, "phone")
	host := strings.TrimPrefix(e.ts.URL, "https://")
	link := "https://" + host + "/dl/" + code
	pin := sha256.Sum256(e.ts.Certificate().RawSubjectPublicKeyInfo)
	pinned := "sha256//" + base64.StdEncoding.EncodeToString(pin[:])
	meta := e.s.enrollmentDownloads(httptest.NewRequest("POST", "https://"+host+"/devices/add", nil), code, "phone")
	text := html.UnescapeString(page)
	for _, want := range []string{
		`<a href="` + link + `/windows" download>` + link + `/windows</a>`,
		`<a href="` + link + `/linux" download>` + link + `/linux</a>`,
		"curl -fsSk --pinnedpubkey '" + pinned + "' '" + link + "/linux' | sh",
		"wget -q --no-check-certificate -O kaze-sync-client.sh '" + link + "/linux' && echo '" + meta.Linux.SHA256 +
			" *kaze-sync-client.sh' | sha256sum -c - && sh kaze-sync-client.sh",
		"0.0.1", // the Windows package version
		`data-copy="dl-win"`, `data-copy="dl-linux"`, `data-copy="cmd-curl"`, `data-copy="cmd-wget"`,
		"kaze-syncc enroll https://" + host + " " + code + " --fingerprint sha256:test", // the manual commands stay
	} {
		if !strings.Contains(text, want) {
			t.Errorf("dialog lacks %q", want)
		}
	}
	if meta.Linux.SHA256 == "" || !strings.Contains(text, `class="note warn"`) {
		t.Error("a Windows package of another version than the server shows no warning")
	}
}

func TestEnrollDialogTrustedCertificateDropsPin(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(filepath.Dir(e.s.varDir), downloadDir)
	linux, _ := packageFixture(t, "linux", version.Version, proto.AgentVersion)
	windows, _ := packageFixture(t, "windows", version.Version, proto.AgentVersion)
	putFixture(t, dir, clientPackageFilename("linux"), linux)
	putFixture(t, dir, clientPackageFilename("windows"), windows)

	page, code := enrollDialog(t, e, "phone") // no fingerprint: the system roots can verify the certificate
	text := html.UnescapeString(page)
	host := strings.TrimPrefix(e.ts.URL, "https://")
	if !strings.Contains(text, "curl -fsS 'https://"+host+"/dl/"+code+"/linux' | sh") ||
		strings.Contains(text, "pinnedpubkey") || strings.Contains(text, "--no-check-certificate") {
		t.Errorf("trusted-certificate commands are wrong:\n%s", text)
	}
	if strings.Contains(text, `class="note warn"`) {
		t.Error("packages of the server's version show a version warning")
	}
}

func TestEnrollDialogWithoutPackages(t *testing.T) {
	e := newEnv(t)
	page, code := enrollDialog(t, e, "phone")
	for _, bad := range []string{"/dl/", "curl ", "wget ", "data-copy=\"dl-"} {
		if strings.Contains(page, bad) {
			t.Errorf("dialog without packages contains %q", bad)
		}
	}
	for _, want := range []string{"client-pkg", `data-copy="cmd-linux"`, `data-copy="cmd-win"`, code} {
		if !strings.Contains(page, want) {
			t.Errorf("dialog without packages lacks %q", want)
		}
	}
	// One package only: the other platform keeps its place with a sentence, not a link.
	dir := filepath.Join(filepath.Dir(e.s.varDir), downloadDir)
	windows, _ := packageFixture(t, "windows", version.Version, proto.AgentVersion)
	putFixture(t, dir, clientPackageFilename("windows"), windows)
	page, code = enrollDialog(t, e, "tablet")
	if !strings.Contains(page, "/dl/"+code+"/windows") || strings.Contains(page, "/dl/"+code+"/linux") ||
		strings.Contains(page, "client-pkg") {
		t.Error("a server with only the Windows package shows the wrong links")
	}
}
