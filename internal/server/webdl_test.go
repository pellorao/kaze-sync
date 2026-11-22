package server

import (
	"bytes"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

// adminRequest sends one request straight to the handler, with the session cookie when session is set.
func adminRequest(h http.Handler, method, path string, session *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "https://sync.example"+path, nil)
	req.Header.Set("Range", "bytes=0-1")
	if session != nil {
		req.AddCookie(session)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// adminSession signs in and returns the session cookie.
func adminSession(t *testing.T, e *env) *http.Cookie {
	t.Helper()
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	u, _ := url.Parse(e.ts.URL)
	for _, k := range c.Jar.Cookies(u) {
		if k.Name == sessionCookie {
			return k
		}
	}
	t.Fatal("no session cookie")
	return nil
}

// A signed-in admin downloads the client installers as they are, without a registration block, under the release
// file names, so that a device that is already enrolled can be upgraded from the browser (design 9.5).
func TestAdminClientDownload(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dir := filepath.Join(filepath.Dir(e.s.varDir), downloadDir)
	linux, _ := packageFixture(t, "linux", "1.0.0", proto.AgentVersion)
	windows, _ := packageFixture(t, "windows", "1.0.1", proto.AgentVersion)
	putFixture(t, dir, clientPackageFilename("linux"), linux)
	putFixture(t, dir, clientPackageFilename("windows"), windows)
	session := adminSession(t, e)
	h := e.s.Handler()

	for _, c := range []struct {
		platform, file string
		body           []byte
	}{
		{"windows", "kaze-sync-client-1.0.1-windows-amd64-setup.exe", windows},
		{"linux", "kaze-sync-client-1.0.0-linux-amd64.sh", linux},
	} {
		w := adminRequest(h, http.MethodGet, "/dl/"+c.platform, session)
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), c.body) {
			t.Fatalf("%s: %d, body changed: %v", c.platform, w.Code, !bytes.Equal(w.Body.Bytes(), c.body))
		}
		hd := w.Header()
		if hd.Get("Content-Disposition") != "attachment; filename="+c.file || hd.Get("Cache-Control") != "no-store" ||
			hd.Get("Content-Length") != strconv.Itoa(len(c.body)) || hd.Get("Content-Range") != "" {
			t.Errorf("%s headers: %v", c.platform, hd)
		}
		head := adminRequest(h, http.MethodHead, "/dl/"+c.platform, session)
		if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != hd.Get("Content-Length") {
			t.Errorf("%s HEAD: %d %v", c.platform, head.Code, head.Header())
		}
	}
	if w := adminRequest(h, http.MethodGet, "/dl/macos", session); w.Code != http.StatusNotFound {
		t.Errorf("unknown system: %d", w.Code)
	}
	os.Remove(filepath.Join(dir, clientPackageFilename("windows")))
	if w := adminRequest(h, http.MethodGet, "/dl/windows", session); w.Code != http.StatusNotFound {
		t.Errorf("missing package: %d", w.Code)
	}
	e.s.mu.Lock()
	fails := len(e.s.fails)
	e.s.mu.Unlock()
	if fails != 0 {
		t.Errorf("requests with a session counted %d failures", fails)
	}
}

// Without a session the download is refused like a wrong enrollment code: it counts towards the IP lockout shared
// with the login and the codes, and a locked IP gets 429. A session keeps working, as on the other admin pages.
func TestAdminClientDownloadSharesIPLockout(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	payload, _ := packageFixture(t, "linux", "1.0.0", proto.AgentVersion)
	putFixture(t, filepath.Join(filepath.Dir(e.s.varDir), downloadDir), clientPackageFilename("linux"), payload)
	session := adminSession(t, e)
	h := e.s.Handler()
	stale := &http.Cookie{Name: sessionCookie, Value: "expired"}
	for i := 0; i < lockFailures; i++ {
		cookie := stale
		if i%2 == 0 {
			cookie = nil
		}
		w := adminRequest(h, http.MethodGet, "/dl/linux", cookie)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" || w.Body.Len() > 100 {
			t.Fatalf("attempt %d: %d %v", i, w.Code, w.Header())
		}
	}
	if w := adminRequest(h, http.MethodGet, "/dl/linux", nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("locked IP: %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/login", strings.NewReader("password=pw"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("login bypassed the download lockout: %d", w.Code)
	}
	if w := adminRequest(h, http.MethodGet, "/dl/linux", session); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), payload) {
		t.Fatalf("session refused on a locked IP: %d", w.Code)
	}
}

// overviewWith renders the overview in the base language with the given packages in dl/.
func overviewWith(t *testing.T, e *env, pkgs map[string]string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(e.s.varDir), downloadDir)
	for _, platform := range []string{"linux", "windows"} {
		os.Remove(filepath.Join(dir, clientPackageFilename(platform)))
		if v := pkgs[platform]; v != "" {
			b, _ := packageFixture(t, platform, v, proto.AgentVersion)
			putFixture(t, dir, clientPackageFilename(platform), b)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "zh-CN")
	w := httptest.NewRecorder()
	if err := e.s.overview(w, r); err != nil {
		t.Fatal(err)
	}
	return w.Body.String()
}

// The overview's device table has a "download client" button at any time; its dialog links the packages of dl/ and
// says that running one on an enrolled device upgrades it (design 4.2).
func TestOverviewClientDownloadDialog(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	d := dictOf("zh-CN")
	both := overviewWith(t, e, map[string]string{"linux": "1.0.0", "windows": "1.0.1"})
	for _, want := range []string{
		`data-dialog="dlg-client"`, `<dialog id="dlg-client"`, d["dl.title"],
		`<a class="btn" href="/dl/windows" download>`, `<a class="btn" href="/dl/linux" download>`,
		"kaze-sync-client-1.0.1-windows-amd64-setup.exe", "kaze-sync-client-1.0.0-linux-amd64.sh", template.HTMLEscapeString(d["dl.upgradeNote"]),
	} {
		if !strings.Contains(both, want) {
			t.Errorf("with both packages: missing %q", want)
		}
	}

	only := overviewWith(t, e, map[string]string{"linux": "1.0.0"})
	if strings.Contains(only, `href="/dl/windows"`) || !strings.Contains(only, `href="/dl/linux"`) ||
		!strings.Contains(only, strings.ReplaceAll(d["en.noPkg"], "{os}", "Windows")) {
		t.Error("with only the Linux package: the Windows place should say there is none")
	}

	none := overviewWith(t, e, nil)
	if !strings.Contains(none, `data-dialog="dlg-client"`) || strings.Contains(none, `href="/dl/`) ||
		!strings.Contains(none, "kaze-syncd client-pkg") {
		t.Error("without packages: the dialog should only say how to add them")
	}
}

// A device whose reported release is older than the package of its system in dl/ gets an "upgrade available" button
// that opens the download dialog; nothing is shown when the device reported no x.y.z release or dl/ has no package of its system.
func TestDeviceTableUpgradeHint(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, d := range []struct{ name, os, release string }{
		{"win-old", "windows", "0.6.0"},
		{"lin-same", "linux", "0.7.0"},
		{"lin-newer", "linux", "0.10.0"},
		{"lin-old", "linux", "0.6.9"},
		{"lin-silent", "linux", ""},
		{"lin-odd", "linux", "0.6.0-dev"},
	} {
		id, _ := e.enrollDevice(d.name, d.os)
		e.s.pollStart(id, proto.PollReq{AgentVersion: proto.AgentVersion, ClientVersion: d.release, ConfigVersion: -1})
	}
	hinted := func(page, name string) bool {
		for _, row := range strings.Split(page, "<tr>") {
			if strings.Contains(row, ">"+name+"</a>") {
				return strings.Contains(row, `class="tag upg" data-dialog="dlg-client"`)
			}
		}
		t.Fatalf("no row for %s", name)
		return false
	}
	page := overviewWith(t, e, map[string]string{"linux": "0.7.0", "windows": "0.7.0"})
	for name, want := range map[string]bool{"win-old": true, "lin-same": false, "lin-newer": false, "lin-old": true, "lin-silent": false, "lin-odd": false} {
		if got := hinted(page, name); got != want {
			t.Errorf("%s: hint %v, want %v", name, got, want)
		}
	}
	if !strings.Contains(page, dictOf("zh-CN")["dl.upgradable"]) {
		t.Error("the hint lacks its text")
	}
	page = overviewWith(t, e, map[string]string{"linux": "0.7.0"})
	if hinted(page, "win-old") || !hinted(page, "lin-old") {
		t.Error("a device is compared with the package of its own system only")
	}
}
