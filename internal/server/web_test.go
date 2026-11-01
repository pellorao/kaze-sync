package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

func TestLockoutAfterTenFailures(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("secret")
	c := e.webClient()
	for i := 1; i <= 10; i++ {
		if resp := e.login(c, "wrong"); resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	if resp := e.login(c, "wrong"); resp.StatusCode != 429 {
		t.Fatalf("11th attempt: %d", resp.StatusCode)
	}
	// Locked out even with the right password, and for device tokens too.
	if resp := e.login(c, "secret"); resp.StatusCode != 429 {
		t.Fatalf("right password while locked: %d", resp.StatusCode)
	}
	if status, _ := e.api("bad-token", "poll", proto.PollReq{}); status != 429 {
		t.Fatalf("device API while locked: %d", status)
	}
	e.clk.Add(61 * time.Minute)
	if resp := e.login(c, "secret"); resp.StatusCode != 303 {
		t.Fatalf("after the lock expired: %d", resp.StatusCode)
	}
}

func TestLockoutCountsDeviceTokensAndCodes(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.api("nope", "poll", proto.PollReq{})
		e.api("", "enroll", proto.EnrollReq{Code: "WRONG", OS: "linux"})
	}
	if status, _ := e.api("nope", "poll", proto.PollReq{}); status != 429 {
		t.Fatalf("status %d", status)
	}
}

func TestLoginSessionAndHeaders(t *testing.T) {
	e := newEnv(t)
	c := e.webClient()
	if status, _ := e.get(c, "/"); status != 303 {
		t.Fatalf("anonymous overview: %d", status)
	}
	if resp := e.login(c, "anything"); resp.StatusCode != 401 {
		t.Fatalf("no password set: %d", resp.StatusCode)
	}
	e.setAdminPassword("ABCDE-FGHIJ-KLMNO-PQRST")
	resp := e.login(c, "abcde fghij klmno pqrst") // case and separators do not matter
	if resp.StatusCode != 303 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	ck := resp.Header.Get("Set-Cookie")
	for _, want := range []string{"__Host-kaze-sync=", "Secure", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(ck, want) {
			t.Errorf("cookie %q lacks %s", ck, want)
		}
	}
	r, err := c.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("Content-Security-Policy") != "default-src 'self'" ||
		r.Header.Get("X-Frame-Options") != "DENY" || r.Header.Get("Referrer-Policy") != "no-referrer" || r.Header.Get("Strict-Transport-Security") != "" {
		t.Fatalf("overview: %d %v", r.StatusCode, r.Header)
	}
	// The session expires after 7 days, without sliding.
	e.clk.Add(6 * 24 * time.Hour)
	if status, _ := e.get(c, "/"); status != 200 {
		t.Fatalf("day 6: %d", status)
	}
	e.clk.Add(25 * time.Hour)
	if status, _ := e.get(c, "/"); status != 303 {
		t.Fatalf("day 7+: %d", status)
	}
}

func TestLogoutEndsSession(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	e.post(c, "/logout", nil)
	if status, _ := e.get(c, "/"); status != 303 {
		t.Fatalf("after logout: %d", status)
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	post := func(origin string) int {
		req, _ := http.NewRequest("POST", e.ts.URL+"/folders/new", strings.NewReader(url.Values{"name": {"x"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("https://evil.example"); code != 403 {
		t.Fatalf("foreign origin: %d", code)
	}
	if e.count(`SELECT count(*) FROM folders`) != 0 {
		t.Fatal("foreign POST took effect")
	}
	if code := post(e.ts.URL); code != 303 {
		t.Fatalf("own origin: %d", code)
	}
}

// adminFlow drives the Web UI from login to a mapped, enrolled device.
func TestEnrollmentAndMappingFlow(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	e.s.Fingerprint = "sha256:abcd"

	if status, body := e.post(c, "/devices/add", url.Values{"name": {"bad name!"}}); status != 400 || !strings.Contains(body, "设备名称") {
		t.Fatalf("bad name: %d", status)
	}
	status, body := e.post(c, "/devices/add", url.Values{"name": {"laptop"}})
	if status != 200 {
		t.Fatalf("add device: %d", status)
	}
	m := regexp.MustCompile(`kaze-syncc enroll (https://\S+) ([A-Z2-7]{4}-[A-Z2-7]{4}-[A-Z2-7]{4}) --fingerprint sha256:abcd`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no command in page:\n%s", body)
	}
	if status, _ := e.post(c, "/devices/add", url.Values{"name": {"laptop"}}); status != 409 {
		t.Fatalf("duplicate pending name: %d", status)
	}
	if status, _ := e.post(c, "/folders/new", url.Values{"name": {"Docs"}}); status != 303 {
		t.Fatal("new folder")
	}
	if status, _ := e.post(c, "/folders/new", url.Values{"name": {"Docs"}}); status != 409 {
		t.Fatal("duplicate folder")
	}

	// Enroll with the code (lower case, no dashes); a code works once.
	code := strings.ToLower(strings.ReplaceAll(m[2], "-", ""))
	st, out := e.api("", "enroll", proto.EnrollReq{Code: code, OS: "linux", Hostname: "lap"})
	if st != 200 {
		t.Fatalf("enroll: %d %s", st, out)
	}
	var er proto.EnrollResp
	json.Unmarshal(out, &er)
	if st, _ := e.api("", "enroll", proto.EnrollReq{Code: code, OS: "linux"}); st != 401 {
		t.Fatalf("second use of the code: %d", st)
	}

	// Mapping validation.
	dev := "/devices/" + itoa(er.DeviceID) + "/mapping"
	folder := "1"
	for _, bad := range []string{"relative/path", `C:\Users`} {
		if st, _ := e.post(c, dev, url.Values{"folder": {folder}, "path": {bad}, "action": {"save"}}); st != 400 {
			t.Errorf("path %q accepted: %d", bad, st)
		}
	}
	if st, _ := e.post(c, dev, url.Values{"folder": {folder}, "path": {"/home/u/Docs/"}, "adopt": {"1"}, "action": {"save"}}); st != 303 {
		t.Fatalf("save mapping: %d", st)
	}

	// The device sees the configuration with the default global rules.
	var pr proto.PollResp
	st, out = e.api(er.Token, "poll", proto.PollReq{ConfigVersion: 1})
	json.Unmarshal(out, &pr)
	if st != 200 || pr.Config == nil || pr.Config.Version != 2 || len(pr.Config.Folders) != 1 {
		t.Fatalf("poll: %d %s", st, out)
	}
	fc := pr.Config.Folders[0]
	if fc.Path != "/home/u/Docs" || !fc.Adopt || fc.Name != "Docs" || fc.Rules[0] != "~$*" {
		t.Fatalf("folder config %+v", fc)
	}
	// Up to date: the poll hangs until the timeout and carries no config.
	start := time.Now()
	_, out = e.api(er.Token, "poll", proto.PollReq{ConfigVersion: 2})
	pr = proto.PollResp{}
	json.Unmarshal(out, &pr)
	if pr.Config != nil || time.Since(start) < e.s.PollTimeout {
		t.Fatalf("idle poll returned early or with config: %s", out)
	}
}

func TestPollWakesOnConfigChange(t *testing.T) {
	e := newEnv(t)
	e.s.PollTimeout = 10 * time.Second
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	dev, tok := e.enrollDevice("d1", "windows")
	e.post(c, "/folders/new", url.Values{"name": {"F"}})

	done := make(chan proto.PollResp)
	go func() {
		_, out := e.api(tok, "poll", proto.PollReq{ConfigVersion: 1})
		var pr proto.PollResp
		json.Unmarshal(out, &pr)
		done <- pr
	}()
	time.Sleep(100 * time.Millisecond)
	if st, body := e.post(c, "/devices/"+itoa(dev)+"/mapping", url.Values{"folder": {"1"}, "path": {`D:/Docs/`}, "action": {"save"}}); st != 303 {
		t.Fatalf("save: %d %s", st, body)
	}
	select {
	case pr := <-done:
		if pr.Config == nil || pr.Config.Folders[0].Path != `D:\Docs` {
			t.Fatalf("poll result %+v", pr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not wake")
	}
	// Saving the same mapping again changes nothing; deleting it bumps the version.
	v := metaInt(e.s.db, "config_version")
	e.post(c, "/devices/"+itoa(dev)+"/mapping", url.Values{"folder": {"1"}, "path": {`D:\Docs`}, "action": {"save"}})
	if metaInt(e.s.db, "config_version") != v {
		t.Fatal("no-op save bumped the version")
	}
	e.post(c, "/devices/"+itoa(dev)+"/mapping", url.Values{"folder": {"1"}, "action": {"delete"}})
	if metaInt(e.s.db, "config_version") != v+1 || e.count(`SELECT count(*) FROM mappings`) != 0 {
		t.Fatal("delete")
	}
}

func TestCleanMapping(t *testing.T) {
	for _, tc := range []struct {
		os, path string
		others   []string
		want     string // "" = error
	}{
		{"linux", "/home/u/Docs/", nil, "/home/u/Docs"},
		{"linux", "/a/../b", nil, "/b"},
		{"linux", "docs", nil, ""},
		{"linux", "/home/u", []string{"/home/u/Docs"}, ""},
		{"linux", "/home/u/Docs/sub", []string{"/home/u/Docs"}, ""},
		{"linux", "/home/u/Docs2", []string{"/home/u/Docs"}, "/home/u/Docs2"},
		{"linux", "/home/u/docs", []string{"/home/u/Docs"}, "/home/u/docs"}, // case matters on Linux
		{"windows", `D:\Docs`, nil, `D:\Docs`},
		{"windows", "D:/Docs/", nil, `D:\Docs`},
		{"windows", "/home", nil, ""},
		{"windows", `D:Docs`, nil, ""},
		{"windows", `d:\docs\sub`, []string{`D:\Docs`}, ""},
		{"windows", `D:\Docs`, []string{`D:\Docs\Sub`}, ""},
		{"windows", `E:\Docs`, []string{`D:\Docs`}, `E:\Docs`},
		{"windows", `D:\Docs2`, []string{`D:\Docs`}, `D:\Docs2`},
		{"windows", `D:\`, []string{`D:\Docs`}, ""},
	} {
		got, err := cleanMapping(tc.os, tc.path, tc.others)
		if (err != nil) != (tc.want == "") || got != tc.want {
			t.Errorf("%s %q %v: got %q, %v; want %q", tc.os, tc.path, tc.others, got, err, tc.want)
		}
	}
}

func TestRevokeStopsDevice(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	dev, tok := e.enrollDevice("d1", "linux")
	f := e.folder("F")
	e.mapFolder(dev, f, "/x")
	if st, _ := e.api(tok, "commit", proto.CommitReq{Folder: f}); st != 200 {
		t.Fatalf("commit: %d", st)
	}
	if st, _ := e.api(tok, "commit", proto.CommitReq{Folder: f + 1}); st != 403 {
		t.Fatalf("unmapped folder: %d", st)
	}
	e.post(c, "/devices/"+itoa(dev)+"/revoke", nil)
	if st, _ := e.api(tok, "commit", proto.CommitReq{Folder: f}); st != 401 {
		t.Fatalf("after revoke: %d", st)
	}
	if e.count(`SELECT count(*) FROM mappings`) != 0 {
		t.Fatal("mappings remain")
	}
}

func TestBrowseVersionsAndRestore(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	e.commit(f, e.op("a.txt", 0, "v1"), e.op("sub/b.txt", 0, "bee"), e.op("sub/deep/c.txt", 0, "cee"))
	e.commit(f, e.op("a.txt", 1, "version two"))
	e.commit(f, proto.Op{Path: "sub/b.txt", BaseSeq: 2})

	_, root := e.get(c, "/folders/"+itoa(f))
	if !strings.Contains(root, "sub/") || !strings.Contains(root, "a.txt") || strings.Contains(root, "b.txt") {
		t.Fatalf("root listing:\n%s", root)
	}
	_, sub := e.get(c, "/folders/"+itoa(f)+"?dir=sub")
	if strings.Contains(sub, "b.txt") || !strings.Contains(sub, "deep/") {
		t.Fatalf("sub listing hides deleted files:\n%s", sub)
	}
	_, subDel := e.get(c, "/folders/"+itoa(f)+"?dir=sub&deleted=1")
	if !strings.Contains(subDel, "b.txt") || !strings.Contains(subDel, "已删除") {
		t.Fatalf("sub listing with deleted:\n%s", subDel)
	}

	// Restore the first version of a.txt; the current content becomes a version.
	_, page := e.get(c, "/folders/"+itoa(f)+"?file=a.txt")
	m := regexp.MustCompile(`name="version" value="(\d+)"`).FindAllStringSubmatch(page, -1)
	if len(m) != 1 {
		t.Fatalf("version list:\n%s", page)
	}
	if st, body := e.post(c, "/folders/"+itoa(f)+"/restore", url.Values{"version": {m[0][1]}}); st != 303 {
		t.Fatalf("restore: %d %s", st, body)
	}
	var size int64
	var dev string
	e.s.db.QueryRow(`SELECT size, device FROM files WHERE folder_id=? AND path='a.txt'`, f).Scan(&size, &dev)
	if size != 2 || dev != "web" || e.count(`SELECT count(*) FROM versions WHERE path='a.txt'`) != 2 {
		t.Fatalf("after restore: size %d device %s", size, dev)
	}
	// Restore a deleted file's last content.
	var vid int64
	e.s.db.QueryRow(`SELECT id FROM versions WHERE path='sub/b.txt'`).Scan(&vid)
	if st, _ := e.post(c, "/folders/"+itoa(f)+"/restore", url.Values{"version": {itoa(vid)}}); st != 303 {
		t.Fatal("restore deleted")
	}
	if e.count(`SELECT count(*) FROM files WHERE path='sub/b.txt' AND hash IS NOT NULL`) != 1 {
		t.Fatal("deleted file not restored")
	}
}

func TestOverviewAndKeepDays(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	e.commit(f, e.op("a", 0, "12345"), e.op("b", 0, "123"))
	_, body := e.get(c, "/")
	if !strings.Contains(body, "Docs") || !strings.Contains(body, ">2<") || !strings.Contains(body, "8 B") || !strings.Contains(body, `value="30"`) {
		t.Fatalf("overview:\n%s", body)
	}
	if st, _ := e.post(c, "/settings", url.Values{"keep_days": {"0"}}); st != 400 {
		t.Fatal("keep_days 0 accepted")
	}
	e.post(c, "/settings", url.Values{"keep_days": {"7"}})
	if metaInt(e.s.db, "keep_days") != 7 {
		t.Fatal("keep_days not saved")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
