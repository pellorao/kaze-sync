package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/installer"
	"kaze-sync/internal/proto"
)

func packageFixture(t *testing.T, platform, release string, agent int) ([]byte, int) {
	t.Helper()
	var payload bytes.Buffer
	gz := gzip.NewWriter(&payload)
	tarw := tar.NewWriter(gz)
	name := "kaze-syncc"
	if platform == "windows" {
		name += ".exe"
	}
	if err := tarw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: 3}); err != nil {
		t.Fatal(err)
	}
	tarw.Write([]byte("foo"))
	if err := tarw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := installer.MarshalTrailer(installer.Manifest{
		Version: release, AgentVersion: agent, OS: platform, Arch: "amd64",
		PayloadSHA256: fmt.Sprintf("%x", sha256.Sum256(payload.Bytes())),
	}, installer.ManifestMagic)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("MZ")
	if platform == "linux" {
		prefix = []byte("#!/bin/sh\nprintf 'installed %s\\n' \"$*\"\nexit 0\n")
	}
	return append(append(prefix, payload.Bytes()...), m...), len(prefix) + payload.Len() - 1
}

func putFixture(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClientPackagesPublishesValidatedFiles(t *testing.T) {
	f := newFakeSystem(t)
	if err := f.install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	input := t.TempDir()
	linux, _ := packageFixture(t, "linux", "1.0.0", proto.AgentVersion)
	windows, _ := packageFixture(t, "windows", "1.0.0", proto.AgentVersion)
	sh := putFixture(t, input, "release.sh", linux)
	exe := putFixture(t, input, "setup.exe", windows)
	f.cmds = nil
	if err := f.in.ClientPackages("", []string{sh, exe}, false); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(f.data, downloadDir, clientPackageFilename("linux"))
	old, err := os.Open(installed)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	fi, err := old.Stat()
	if err != nil || fi.Mode().Perm() != 0o644 || len(f.cmds) != 0 {
		t.Fatalf("published mode %v, stat %v, commands %v", fi, err, f.cmds)
	}

	newLinux, _ := packageFixture(t, "linux", "1.1.0", proto.AgentVersion)
	badWindows, corruptAt := packageFixture(t, "windows", "1.1.0", proto.AgentVersion)
	badWindows[corruptAt] ^= 1
	putFixture(t, input, "release.sh", newLinux)
	putFixture(t, input, "setup.exe", badWindows)
	if err := f.in.ClientPackages(f.data, []string{sh, exe}, false); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("corrupt package: %v", err)
	}
	got, err := os.ReadFile(installed)
	if err != nil || !bytes.Equal(got, linux) {
		t.Fatal("invalid batch changed the installed Linux package")
	}

	future, _ := packageFixture(t, "linux", "1.2.0", proto.AgentVersion+1)
	putFixture(t, input, "release.sh", future)
	if err := f.in.ClientPackages(f.data, []string{sh}, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("future agent protocol: %v", err)
	}
	if err := f.in.ClientPackages(f.data, []string{sh}, true); err != nil {
		t.Fatal(err)
	}
	original, err := io.ReadAll(old)
	if err != nil || !bytes.Equal(original, linux) {
		t.Fatal("replacing a package changed an open download")
	}
	got, err = os.ReadFile(installed)
	if err != nil || !bytes.Equal(got, future) {
		t.Fatal("replacement not published")
	}
}

func TestEnrollmentDownloads(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(filepath.Dir(e.s.varDir), downloadDir)
	linux, _ := packageFixture(t, "linux", "1.0.0", proto.AgentVersion)
	windows, _ := packageFixture(t, "windows", "1.0.1", proto.AgentVersion)
	putFixture(t, dir, clientPackageFilename("linux"), linux)
	putFixture(t, dir, clientPackageFilename("windows"), windows)
	e.s.Fingerprint = "sha256:test"
	cert := e.ts.TLS.Certificates[0]
	e.s.serverCertificate = func() *tls.Certificate { return &cert }

	code := "ABCD-EFGH-IJKL"
	e.s.codes[sha(normalizeSecret(code))] = pendingCode{name: "test-device", expires: e.clk.Now().Add(codeLife)}
	handler := e.s.Handler()
	request := func(method, platform string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://sync.example/dl/"+code+"/"+platform, nil)
		req.Header.Set("Range", "bytes=0-1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	w := request(http.MethodGet, "windows")
	if w.Code != http.StatusOK || !bytes.HasPrefix(w.Body.Bytes(), windows) ||
		w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) ||
		w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Range") != "" ||
		!strings.HasSuffix(w.Header().Get("Content-Disposition"), ".exe") {
		t.Fatalf("Windows response: %d %v", w.Code, w.Header())
	}
	var reg installer.Registration
	tail := w.Body.Bytes()[len(windows):]
	if len(tail) < 16 || string(tail[len(tail)-8:]) != installer.RegistrationMagic ||
		binary.LittleEndian.Uint64(tail[len(tail)-16:]) != uint64(len(tail)-16) ||
		json.Unmarshal(tail[:len(tail)-16], &reg) != nil ||
		reg != (installer.Registration{Server: "https://sync.example", Fingerprint: "sha256:test", Code: code, Name: "test-device"}) {
		t.Fatalf("registration trailer: %+v", reg)
	}

	l := request(http.MethodGet, "linux")
	if l.Code != http.StatusOK || l.Header().Get("Content-Length") != strconv.Itoa(l.Body.Len()) {
		t.Fatalf("Linux response: %d %v", l.Code, l.Header())
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(l.Body.String(), pipeStart), pipeEnd)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !bytes.Equal(decoded, append(linux, tail...)) {
		t.Fatalf("Linux stream changed installer bytes or registration trailer: %v", err)
	}
	meta := e.s.enrollmentDownloads(httptest.NewRequest("POST", "https://sync.example/devices", nil), code, reg.Name)
	pin := sha256.Sum256(e.ts.Certificate().RawSubjectPublicKeyInfo)
	if meta.Linux.URL != "https://sync.example/dl/"+code+"/linux" ||
		meta.Linux.SHA256 != fmt.Sprintf("%x", sha256.Sum256(l.Body.Bytes())) ||
		meta.PublicKeyPin != "sha256//"+base64.StdEncoding.EncodeToString(pin[:]) || meta.Windows.Version != "1.0.1" {
		t.Fatalf("dialog metadata: %+v", meta)
	}
	if cached := e.s.codes[sha(normalizeSecret(code))].linuxHash; cached == nil || cached.sum != meta.Linux.SHA256 {
		t.Fatal("did not cache the Linux response checksum with the code")
	}
	head := request(http.MethodHead, "linux")
	if head.Code != http.StatusOK || head.Body.Len() != 0 ||
		head.Header().Get("Content-Length") != l.Header().Get("Content-Length") {
		t.Fatalf("HEAD response: %d, %v", head.Code, head.Header())
	}
	if status, body := e.api("", "enroll", proto.EnrollReq{OS: "linux", Code: code}); status != http.StatusOK {
		t.Fatalf("downloads consumed code: %d %s", status, body)
	}
	if got := request(http.MethodGet, "windows").Code; got != http.StatusNotFound {
		t.Fatalf("used code returned %d", got)
	}
}

func TestEnrollmentDownloadSharesIPLockout(t *testing.T) {
	e := newEnv(t)
	h := e.s.Handler()
	code := "ABCD-EFGH-IJKL"
	e.s.codes[sha(normalizeSecret(code))] = pendingCode{name: "test-device", expires: e.clk.Now().Add(-time.Minute)}
	for i := 0; i < lockFailures; i++ {
		platform := "linux"
		if i > 0 {
			platform = "unknown"
		}
		if i == 1 {
			e.s.codes[sha(normalizeSecret(code))] = pendingCode{name: "test-device", expires: e.clk.Now().Add(codeLife)}
		}
		req := httptest.NewRequest("GET", "/dl/"+code+"/"+platform, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), code) {
			t.Fatalf("download attempt %d: %d %s", i, w.Code, w.Body)
		}
	}
	req := httptest.NewRequest("POST", "/api/v1/enroll", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("enroll bypassed download lockout: %d", w.Code)
	}
	req = httptest.NewRequest("GET", "/dl/"+code+"/linux", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("valid code bypassed download lockout: %d", w.Code)
	}
}

func TestDevicePackageAllowsOutdatedClients(t *testing.T) {
	e := newEnv(t)
	_, token := e.enrollDevice("device", "linux")
	payload, _ := packageFixture(t, "linux", "1.0.0", proto.AgentVersion)
	putFixture(t, filepath.Join(filepath.Dir(e.s.varDir), downloadDir), clientPackageFilename("linux"), payload)
	h := e.s.Handler()
	for _, c := range []struct {
		method, endpoint, token string
		status                  int
		wantBody, wantInfo      bool
	}{
		{"GET", "client/package?os=linux", "", 401, false, false},
		{"GET", "client/package?os=linux", token, 200, true, true},
		{"HEAD", "client/package?os=linux", token, 200, false, true},
		{"HEAD", "client/package", token, 200, false, false},
		{"GET", "client/package?os=windows", token, 404, false, false},
		{"POST", "poll", token, 426, false, false},
	} {
		req := httptest.NewRequest(c.method, "/api/v1/"+c.endpoint, nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set(proto.VersionHeader, "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		agent := w.Header().Get(proto.ServerAgentVersionHeader)
		if w.Code != c.status || (c.token == "" && agent != "") ||
			(c.token != "" && agent != strconv.Itoa(proto.AgentVersion)) {
			t.Errorf("%s %s: %d %v", c.method, c.endpoint, w.Code, w.Header())
		}
		if c.wantInfo && (w.Header().Get(proto.PackageVersionHeader) != "1.0.0" ||
			w.Header().Get(proto.PackageAgentVersionHeader) != agent) {
			t.Errorf("package headers: %v", w.Header())
		}
		if c.wantBody && !bytes.Equal(w.Body.Bytes(), payload) {
			t.Error("authenticated package was modified")
		}
		if c.method == "HEAD" && w.Body.Len() != 0 {
			t.Error("compatibility probe unexpectedly sent a body")
		}
	}
}

func TestPipeInstallerWaitsForFinalCall(t *testing.T) {
	dir := t.TempDir()
	b, _ := packageFixture(t, "linux", "1.0.0", proto.AgentVersion)
	f, err := openClientPackage(putFixture(t, dir, "client.sh", b))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var script bytes.Buffer
	if err := writeEnrollmentDownload(&script, f, nil); err != nil {
		t.Fatal(err)
	}
	lastCall := bytes.LastIndex(script.Bytes(), []byte("\nkaze_install"))
	for _, c := range []struct {
		body []byte
		want string
	}{
		{script.Bytes()[:len(pipeStart)+1], ""},
		{script.Bytes()[:lastCall+1], ""},
		{script.Bytes(), "installed arg\n"},
	} {
		cmd := exec.Command("sh", "-s", "--", "arg")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir}
		cmd.Dir = dir
		cmd.Stdin = bytes.NewReader(c.body)
		out, err := cmd.Output()
		if string(out) != c.want || (c.want != "" && err != nil) {
			t.Fatalf("piped installer: output %q, error %v, wanted %q", out, err, c.want)
		}
	}
}
