package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

// The pages must work under `default-src 'self'` (design 4.1): no inline scripts, style attributes or event handlers.
func TestTemplatesAreCSPSafe(t *testing.T) {
	scripts := regexp.MustCompile(`(?is)<script\b([^>]*)>`)
	styleAttr := regexp.MustCompile(`(?i)\sstyle\s*=`)
	styleTag := regexp.MustCompile(`(?i)<style\b`)
	handler := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	jsLink := regexp.MustCompile(`(?i)(href|src|action)\s*=\s*["']?\s*javascript:`)
	n := 0
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return err
		}
		n++
		b, _ := fs.ReadFile(webFS, p)
		s := string(b)
		for _, m := range scripts.FindAllStringSubmatch(s, -1) {
			if !strings.Contains(strings.ToLower(m[1]), "src=") {
				t.Errorf("%s: inline <script>", p)
			}
		}
		for name, re := range map[string]*regexp.Regexp{"style attribute": styleAttr, "<style>": styleTag, "event handler attribute": handler, "javascript: link": jsLink} {
			if re.MatchString(s) {
				t.Errorf("%s: %s", p, name)
			}
		}
		return nil
	})
	if err != nil || n < 8 {
		t.Fatalf("walked %d templates: %v", n, err)
	}
	// The script file builds no HTML with inline handlers either.
	js, _ := fs.ReadFile(webFS, "web/app.js")
	if handler.Match(js) && regexp.MustCompile(`(?i)\bon(click|load|error|submit)\s*=\s*["']`).Match(js) {
		t.Error("app.js writes an inline handler attribute")
	}
	// And the response says what the pages rely on.
	e := newEnv(t)
	resp, err := e.webClient().Get(e.ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("CSP %q", csp)
	}
}

// A form that fails validation shows the same page with the dialog open, the input kept and the error under it.
func TestFormErrorsRedisplayTheDialog(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	st, body := e.post(c, "/devices/add", url.Values{"name": {"bad name!"}})
	if st != 400 || !strings.Contains(body, `<dialog id="dlg-add-device" class="dlg" data-autoopen`) || !strings.Contains(body, `value="bad name!"`) || !strings.Contains(body, `class="err-msg"`) {
		t.Fatalf("add device: %d\n%s", st, body)
	}
	if e.count(`SELECT count(*) FROM devices`) != 0 {
		t.Fatal("a device was created")
	}
	st, body = e.post(c, "/folders/new", url.Values{"name": {""}})
	if st != 400 || !strings.Contains(body, `<dialog id="dlg-new-folder" class="dlg" data-autoopen`) || !strings.Contains(body, `class="err-msg"`) {
		t.Fatalf("new folder: %d\n%s", st, body)
	}
	// A folder rules form with a bad line keeps the text and shows the line.
	f := e.folder("F")
	st, body = e.post(c, "/folders/"+itoa(f)+"/rules", url.Values{"rules": {"*.o\r\nbad\\"}})
	if st != 400 || !strings.Contains(body, `<dialog id="dlg-rules" class="dlg wide" data-autoopen`) || !strings.Contains(body, "bad\\</textarea>") || !strings.Contains(body, "第 2 行") {
		t.Fatalf("rules: %d\n%s", st, body)
	}
	// A time that cannot be read keeps the restore dialog open.
	st, body = e.post(c, "/folders/"+itoa(f)+"/restore-dir", url.Values{"path": {""}, "time": {"yesterday"}})
	if st != 400 || !strings.Contains(body, `<dialog id="dlg-restore" class="dlg" data-folder-name="F" data-keep-days="30" data-autoopen`) || !strings.Contains(body, `value="yesterday"`) {
		t.Fatalf("restore: %d\n%s", st, body)
	}
	// The dialog is a time box with two axes, one for the day and one for the time of day (hidden until the script shows
	// them), not a range input.
	for _, w := range []string{`data-axes hidden`, `data-axis="day"`, `data-axis="tod"`, `role="slider"`, `data-rel`, `data-step="-1"`, `data-step="1"`, `class="preview fixed" data-preview hidden`, `data-note2`} {
		if !strings.Contains(body, w) {
			t.Errorf("the restore dialog lacks %s", w)
		}
	}
	if strings.Contains(body, `type="range"`) || strings.Contains(body, "data-slider") {
		t.Error("the restore dialog still has the slider")
	}
}

func TestRulesValidateUsesTheServerParser(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	if st, _ := e.post(c, "/rules/validate", nil); st != http.StatusSeeOther {
		t.Fatalf("without a session: %d", st)
	}
	e.login(c, "pw")
	check := func(text string) []struct {
		Line int    `json:"line"`
		Msg  string `json:"msg"`
	} {
		t.Helper()
		resp, err := c.Post(e.ts.URL+"/rules/validate", "text/plain", strings.NewReader(text))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Errors []struct {
				Line int    `json:"line"`
				Msg  string `json:"msg"`
			} `json:"errors"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 || out.Errors == nil {
			t.Fatalf("%d %v %+v", resp.StatusCode, err, out)
		}
		return out.Errors
	}
	if errs := check("# comment\n*.o\r\n!/keep/\n"); len(errs) != 0 {
		t.Errorf("valid rules: %+v", errs)
	}
	errs := check("*.o\n[bad\n# fine\nbackup\\\n")
	if len(errs) != 2 || errs[0].Line != 2 || errs[1].Line != 4 || errs[0].Msg == "" {
		t.Errorf("invalid rules: %+v", errs)
	}
	// Whatever it accepts the save handler accepts, and the reverse.
	for _, text := range []string{"*.o\nbuild/", "[bad"} {
		want := len(check(text)) == 0
		st, _ := e.post(c, "/rules", url.Values{"rules": {text}})
		if (st == http.StatusSeeOther) != want {
			t.Errorf("%q: validate says ok=%v, save answered %d", text, want, st)
		}
	}
}

func TestTimeZoneIsStoredOnTheServer(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	if _, body := e.get(c, "/"); !strings.Contains(body, `data-tz=""`) {
		t.Fatal("the default is the browser's zone (empty)")
	}
	if st, _ := e.post(c, "/settings/timezone", url.Values{"tz_mode": {"fixed"}, "tz": {"Asia/Shanghai"}}); st != 303 {
		t.Fatalf("save: %d", st)
	}
	if _, body := e.get(c, "/"); !strings.Contains(body, `data-tz="Asia/Shanghai"`) {
		t.Fatal("the zone is not given to the pages")
	}
	for _, bad := range []string{"", "Not A Zone", "../etc/passwd", strings.Repeat("a", 80)} {
		if st, body := e.post(c, "/settings/timezone", url.Values{"tz_mode": {"fixed"}, "tz": {bad}}); st != 400 || !strings.Contains(body, "时区名称无效") {
			t.Errorf("%q: %d", bad, st)
		}
	}
	if MetaString(e.s.db, "time_zone") != "Asia/Shanghai" {
		t.Fatal("a rejected zone changed the setting")
	}
	e.post(c, "/settings/timezone", url.Values{"tz_mode": {"browser"}})
	if MetaString(e.s.db, "time_zone") != "" {
		t.Fatal("back to the browser's zone")
	}
	// Times travel as UTC and are formatted in the browser; the markup carries the UTC time.
	f := e.folder("F")
	dev, tok := e.enrollDevice("pc", "linux")
	e.mapFolder(dev, f, "/x")
	e.report(tok, f, proto.FileError{Path: "x", Kind: proto.KindIO, Message: "m", Since: e.clk.Now().Unix()})
	_, body := e.get(c, "/")
	if !regexp.MustCompile(`<time datetime="2026-11-01T12:00:00Z">2026-11-01 12:00:00</time>`).MatchString(body) {
		t.Errorf("no <time> element in UTC:\n%s", body)
	}
}

// readEvents returns the lines "data: ..." of an /events stream with the time each arrived.
func readEvents(t *testing.T, e *env, c *http.Client) (<-chan [2]any, func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+"/events", nil)
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %v %v", resp, err)
	}
	out := make(chan [2]any, 32)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				out <- [2]any{time.Now(), strings.TrimPrefix(sc.Text(), "data: ")}
			}
		}
	}()
	return out, func() { resp.Body.Close() }
}

func TestEventsPushOnChangeNoMoreOftenThanTheGap(t *testing.T) {
	e := newEnv(t)
	e.s.EventGap = 400 * time.Millisecond
	e.setAdminPassword("pw")
	c := e.webClient()
	if resp, err := c.Get(e.ts.URL + "/events"); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a session: %v %v", resp, err)
	}
	e.login(c, "pw")
	f := e.folder("F")
	ev, stop := readEvents(t, e, c)
	defer stop()
	type payload struct {
		Gen int64            `json:"gen"`
		Seq map[string]int64 `json:"seq"`
	}
	next := func(wait time.Duration) (time.Time, payload, bool) {
		select {
		case m, ok := <-ev:
			var p payload
			if !ok || json.Unmarshal([]byte(m[1].(string)), &p) != nil {
				t.Fatalf("stream ended or bad data: %v", m)
			}
			return m[0].(time.Time), p, true
		case <-time.After(wait):
			return time.Time{}, payload{}, false
		}
	}
	t0, first, ok := next(2 * time.Second)
	if !ok {
		t.Fatal("no event when the stream opens")
	}
	if _, _, ok := next(900 * time.Millisecond); ok {
		t.Fatal("an event without a change")
	}
	// The first change after a quiet time goes out at once; the next ones, made at once, wait for the gap and go out together.
	e.commit(f, e.op("a", 0, "1"))
	t1, second, ok := next(2 * time.Second)
	if !ok || second.Gen <= first.Gen || second.Seq[itoa(f)] != 1 {
		t.Fatalf("after the first commit: %+v %v", second, ok)
	}
	e.commit(f, e.op("b", 0, "2"))
	e.commit(f, e.op("c", 0, "3"))
	t1b, secondb, ok := next(2 * time.Second)
	if !ok || secondb.Seq[itoa(f)] != 3 || t1b.Sub(t1) < e.s.EventGap-20*time.Millisecond {
		t.Fatalf("after the next two: %+v %v %v", secondb, ok, t1b.Sub(t1))
	}
	if _, _, ok := next(900 * time.Millisecond); ok {
		t.Fatal("a further event for the same changes")
	}
	// A change right after a push waits for the gap.
	e.commit(f, e.op("d", 0, "4"))
	t2, third, ok := next(2 * time.Second)
	if !ok || third.Seq[itoa(f)] != 4 {
		t.Fatalf("third: %+v %v", third, ok)
	}
	e.commit(f, e.op("e", 0, "5"))
	t3, _, ok := next(2 * time.Second)
	if !ok || t3.Sub(t2) < e.s.EventGap-20*time.Millisecond {
		t.Fatalf("pushes %v apart, want at least %v", t3.Sub(t2), e.s.EventGap)
	}
	_ = t0
	// Reports, enrolment and settings count as changes too.
	dev, tok := e.enrollDevice("d", "linux")
	e.mapFolder(dev, f, "/x")
	before := e.s.uiGen
	e.report(tok, f, proto.FileError{Path: "x", Kind: proto.KindIO, Message: "m", Since: e.clk.Now().Unix()})
	e.report(tok, f) // the report changes: the error is gone
	e.post(c, "/settings", url.Values{"keep_days": {"9"}})
	e.post(c, "/settings/timezone", url.Values{"tz_mode": {"browser"}})
	if e.s.uiGen < before+2 {
		t.Errorf("gen %d after the reports, was %d", e.s.uiGen, before)
	}
	// An unchanged report is not a change.
	g := e.s.uiGen
	e.report(tok, f)
	if e.s.uiGen != g {
		t.Error("an identical report bumped the version")
	}
}

func TestEventsEndWhenTheServerStops(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	ev, stop := readEvents(t, e, c)
	defer stop()
	<-ev
	e.s.StopEvents()
	select {
	case _, ok := <-ev:
		if ok {
			t.Fatal("an event after the stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream stays open")
	}
}

// The preview lists what the restore then does: same query, same files.
func TestRestorePreviewMatchesTheRestore(t *testing.T) {
	count := func(page string) int {
		m := regexp.MustCompile(`data-count="(\d+)"`).FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("no count in the preview:\n%s", page)
		}
		var n int
		fmt.Sscan(m[1], &n)
		return n
	}
	for _, tc := range []struct {
		path string
		n    int
		want []string
		not  []string
	}{
		{"proj", 3, []string{"a", "b", "sub/c"}, []string{"d", "other"}},
		{"proj/b", 1, []string{"b"}, nil},
		{"", 4, []string{"proj/a", "other"}, []string{"proj/d"}},
		{"nothing", 0, nil, nil},
	} {
		t.Run("path="+tc.path, func(t *testing.T) {
			e := newEnv(t)
			e.setAdminPassword("pw")
			c := e.webClient()
			e.login(c, "pw")
			f := e.folder("Docs")
			base := "/folders/" + itoa(f)
			e.commit(f, e.op("proj/a", 0, "a1"), e.op("proj/b", 0, "b1"), e.op("proj/sub/c", 0, "c1"), e.op("other", 0, "o1"))
			e.clk.Add(time.Hour)
			at := e.clk.Now().Format("2006-01-02 15:04:05")
			e.clk.Add(time.Hour)
			e.commit(f, e.op("proj/a", 1, "a2"), proto.Op{Path: "proj/b", BaseSeq: 2}, e.op("proj/d", 0, "d1"), e.op("other", 4, "o2"), e.op("proj/sub/c", 3, "c2"))
			st, page := e.get(c, base+"/restore-preview?"+url.Values{"path": {tc.path}, "time": {at}}.Encode())
			if st != 200 || count(page) != tc.n {
				t.Fatalf("%d, %d files, want %d\n%s", st, count(page), tc.n, page)
			}
			if tc.path == "proj" { // the second column is short: b was deleted, a changed
				if !strings.Contains(page, `<span class="s">已删除</span>`) || !strings.Contains(page, `<span class="s">已改变</span>`) || strings.Contains(page, "现在内容不同") || strings.Contains(page, "现在已删除") {
					t.Errorf("the status column:\n%s", page)
				}
			}
			for _, w := range tc.want {
				if !strings.Contains(page, ">"+w+"<") {
					t.Errorf("the preview lacks %q", w)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(page, ">"+w+"<") {
					t.Errorf("the preview lists %q", w)
				}
			}
			before := e.nextSeq(f)
			e.post(c, base+"/restore-dir", url.Values{"path": {tc.path}, "time": {at}})
			if got := int(e.nextSeq(f) - before); got != tc.n {
				t.Errorf("the preview says %d, the restore changed %d", tc.n, got)
			}
			if st, _ := e.get(c, base+"/restore-preview?time=yesterday"); st != 400 {
				t.Errorf("bad time: %d", st)
			}
		})
	}
}

// Many changed files: the preview shows the first ones and the number of the rest.
func TestRestorePreviewIsLimited(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	var ops, del []proto.Op
	for i := 0; i < previewLimit+7; i++ {
		ops = append(ops, e.op(fmt.Sprintf("d/f%03d", i), 0, "x"))
	}
	for i, r := range e.commit(f, ops...) {
		del = append(del, proto.Op{Path: ops[i].Path, BaseSeq: r.Seq})
	}
	e.clk.Add(time.Hour)
	at := e.clk.Now().Format("2006-01-02 15:04:05")
	e.clk.Add(time.Hour)
	e.commit(f, del...)
	_, page := e.get(c, "/folders/"+itoa(f)+"/restore-preview?"+url.Values{"path": {"d"}, "time": {at}}.Encode())
	if n := strings.Count(page, `<li><span class="p">`); n != previewLimit || !strings.Contains(page, `data-count="27"`) || !strings.Contains(page, "还有 7 个") {
		t.Fatalf("%d items listed:\n%s", n, page)
	}
}

// Many notices of one kind and device fold into one line; the list can be opened.
func TestNoticesFoldAboveFive(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	dev, tok := e.enrollDevice("pc", "windows")
	e.mapFolder(dev, f, "/docs")
	var errs []proto.FileError
	for i := 0; i < foldAbove+3; i++ {
		errs = append(errs, proto.FileError{Path: fmt.Sprintf("links/l%d", i), Kind: proto.KindSkipped, Message: "symbolic link", Since: e.clk.Now().Unix()})
	}
	errs = append(errs, proto.FileError{Path: "one.db", Kind: proto.KindInUse, Message: "in use", Since: e.clk.Now().Unix()})
	e.report(tok, f, errs...)
	_, page := e.get(c, "/")
	if !strings.Contains(page, fmt.Sprintf("pc: %d 个条目被跳过", foldAbove+3)) {
		t.Fatalf("no folded line:\n%s", page)
	}
	if !strings.Contains(page, "links/l7") || !strings.Contains(page, "one.db") {
		t.Error("the folded paths or the single notice are missing from the markup")
	}
	if n := strings.Count(page, `<details class="nitem fold"`); n != 1 {
		t.Errorf("%d folds, want 1 (the in-use file is alone)", n)
	}
	// At the limit nothing folds.
	e.report(tok, f, errs[:foldAbove]...)
	_, page = e.get(c, "/")
	if strings.Contains(page, "个条目被跳过") || strings.Count(page, "symbolic link") < foldAbove {
		t.Errorf("%d notices fold:\n%s", foldAbove, page)
	}
}

// A device's report of a symbolic link shows as a row of the directory that holds it, and as a tag on the original of a
// conflict copy.
func TestFolderPageShowsSkippedAndConflictRows(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	dev, tok := e.enrollDevice("pc", "windows")
	e.mapFolder(dev, f, "/docs")
	e.commit(f, e.op("d/report.docx", 0, "one"), e.op("d/report.conflict-pc-20261101-120000.docx", 0, "two"))
	e.report(tok, f, proto.FileError{Path: "d/latest", Kind: proto.KindSkipped, Message: "symbolic link", Since: e.clk.Now().Unix()},
		proto.FileError{Path: "other/x", Kind: proto.KindSkipped, Message: "symbolic link", Since: e.clk.Now().Unix()})
	_, page := e.get(c, "/folders/"+itoa(f)+"?dir=d")
	for _, want := range []string{`data-kind="unsynced" data-path="d/latest"`, "未同步: 符号链接 · pc", `data-kind="conflict" data-path="d/report.conflict-pc-20261101-120000.docx"`,
		"冲突副本", "有冲突", `data-orig="d/report.docx"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(page, `data-path="other/x"`) {
		t.Error("a row of another directory is listed")
	}
	// The list shows no source device, only the version history does.
	if strings.Contains(page, "来源设备</th>") {
		t.Error("the list has a source column")
	}
	_, vers := e.get(c, "/folders/"+itoa(f)+"?dir=d&file=d%2Freport.docx")
	if !strings.Contains(vers, `id="dlg-versions"`) || !strings.Contains(vers, "来源设备") {
		t.Errorf("no version dialog with the source device:\n%s", vers)
	}
}

// The folder statistics are counted again only when the folder changed and the last count is old.
func TestFolderStatsAreCached(t *testing.T) {
	e := newEnv(t)
	f := e.folder("Docs")
	e.commit(f, e.op("a", 0, "12"))
	files, bytes, err := e.s.folderStats(f)
	if err != nil || files != 1 || bytes != 2 {
		t.Fatalf("%d %d %v", files, bytes, err)
	}
	e.commit(f, e.op("b", 0, "123"))
	if files, _, _ = e.s.folderStats(f); files != 1 {
		t.Error("recounted although the last count is fresh")
	}
	e.clk.Add(statsMaxAge + time.Second)
	if files, bytes, _ = e.s.folderStats(f); files != 2 || bytes != 5 {
		t.Errorf("after the age: %d %d", files, bytes)
	}
	// Old but unchanged: the cached numbers stay without a scan (the seq is the same).
	e.clk.Add(time.Hour)
	e.s.mu.Lock()
	e.s.stats[f] = folderStat{seq: e.s.stats[f].seq, files: 99, bytes: 99, at: e.s.stats[f].at}
	e.s.mu.Unlock()
	if files, _, _ = e.s.folderStats(f); files != 99 {
		t.Error("an unchanged folder was counted again")
	}
}

func TestStaticFilesAreServedWithETag(t *testing.T) {
	e := newEnv(t)
	c := e.webClient()
	resp, err := c.Get(e.ts.URL + "/static/app.js")
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("ETag") == "" {
		t.Fatalf("%v %v", resp, err)
	}
	resp.Body.Close()
	req, _ := http.NewRequest("GET", e.ts.URL+"/static/app.js", nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	r2, err := c.Do(req)
	if err != nil || r2.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation: %v %v", r2, err)
	}
	r2.Body.Close()
	if r3, err := c.Get(e.ts.URL + "/static/icons.svg"); err != nil || r3.StatusCode != 200 || !strings.Contains(r3.Header.Get("Content-Type"), "svg") {
		t.Fatalf("icons: %v %v", r3, err)
	}
}

// A time before the first record of the path is taken as the first record, and the preview says so: "no
// difference" for a time that has no history was wrong (user 2026-11-05).
func TestRestoreBeforeTheFirstRecordUsesTheFirstRecord(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	base := "/folders/" + itoa(f)
	e.commit(f, e.op("a", 0, "a1"))
	first := e.clk.Now()
	e.clk.Add(30 * time.Minute)
	e.commit(f, e.op("b", 0, "b1"))
	b1 := e.clk.Now()
	e.clk.Add(30 * time.Minute)
	e.commit(f, e.op("a", 1, "a2"), e.op("b", 2, "b2"))
	long := first.Add(-7 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	preview := func(path, at string) string {
		_, page := e.get(c, base+"/restore-preview?"+url.Values{"path": {path}, "time": {at}}.Encode())
		return page
	}
	// The whole folder: the state at the end of its initial import, which holds a only.
	page := preview("", long)
	if !strings.Contains(page, `data-count="1"`) || !strings.Contains(page, ">a<") || strings.Contains(page, ">b<") ||
		!strings.Contains(page, "首次导入完成于") || !strings.Contains(page, `datetime="`+first.Format("2006-01-02T15:04:05Z")+`"`) {
		t.Fatalf("folder:\n%s", page)
	}
	// One file: its own first record, not the folder's.
	page = preview("b", long)
	if !strings.Contains(page, `data-count="1"`) || !strings.Contains(page, "最早记录为") || !strings.Contains(page, `datetime="`+b1.Format("2006-01-02T15:04:05Z")+`"`) {
		t.Fatalf("file b:\n%s", page)
	}
	// A time inside the history carries no note.
	if page = preview("", first.Add(10*time.Minute).Format("2006-01-02 15:04:05")); strings.Contains(page, "最早记录为") || strings.Contains(page, "首次导入") || !strings.Contains(page, `data-count="1"`) {
		t.Fatalf("inside the history:\n%s", page)
	}
	// The restore does what the preview says.
	before := e.nextSeq(f)
	e.post(c, base+"/restore-dir", url.Values{"path": {""}, "time": {long}})
	if got := e.nextSeq(f) - before; got != 1 {
		t.Fatalf("the restore changed %d files, want 1", got)
	}
	h := e.block([]byte("a1"))
	if got := e.fcount(f, `SELECT count(*) FROM files WHERE path='a' AND hash=?`, h[:]); got != 1 {
		t.Fatal("a is not back to its first content")
	}
	// Nothing recorded at all.
	g := e.folder("Empty")
	if _, page = e.get(c, "/folders/"+itoa(g)+"/restore-preview?"+url.Values{"time": {long}}.Encode()); !strings.Contains(page, "没有任何历史记录") || !strings.Contains(page, `data-count="0"`) {
		t.Fatalf("empty folder:\n%s", page)
	}
}

// The "content updated" hint is for changes the listing shows: direct children of the shown directory, and
// subdirectories that appear or vanish. A change deeper down, or in another directory, is not one.
func TestListingChangedOnlyForWhatThePageShows(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	e.commit(f, e.op("top", 0, "t"), e.op("sub/a", 0, "a"), e.op("sub/deep/b", 0, "b"), e.op("other/c", 0, "c"))
	cur := func() int64 { return e.nextSeq(f) - 1 }
	type answer struct {
		Changed bool  `json:"changed"`
		Seq     int64 `json:"seq"`
	}
	ask := func(since int64, dir string, shown ...string) answer {
		t.Helper()
		q := url.Values{"since": {itoa(since)}, "dir": {dir}, "d": shown}
		st, body := e.get(c, "/folders/"+itoa(f)+"/changed?"+q.Encode())
		var a answer
		if st != 200 || json.Unmarshal([]byte(body), &a) != nil {
			t.Fatalf("%d %s", st, body)
		}
		return a
	}
	for _, bad := range []string{"abc", "-1", ""} { // a since that is not a number is a bad request, not "since 0"
		if st, _ := e.get(c, "/folders/"+itoa(f)+"/changed?since="+bad); st != http.StatusBadRequest {
			t.Errorf("since=%q: %d", bad, st)
		}
	}
	since := cur()
	if a := ask(since, "", "sub", "other"); a.Changed || a.Seq != since {
		t.Fatalf("nothing happened: %+v", a)
	}
	seqOf := func(p string) int64 {
		var s int64
		db, _ := e.s.folderDB(f)
		db.QueryRow(`SELECT seq FROM files WHERE path=?`, p).Scan(&s)
		return s
	}
	// Inside a subdirectory, or in a directory that is not shown: the root listing is the same.
	e.commit(f, e.op("sub/deep/b", seqOf("sub/deep/b"), "b2"), e.op("sub/new", 0, "n"), e.op("other/c", seqOf("other/c"), "c2"))
	if a := ask(since, "", "sub", "other"); a.Changed || a.Seq != cur() {
		t.Fatalf("changes below shown subdirectories: %+v", a)
	}
	// The same changes seen from inside "sub": a direct child changed.
	if a := ask(since, "sub", "deep"); !a.Changed {
		t.Fatalf("a child of sub changed: %+v", a)
	}
	// ...and from "other/", which they did not touch below its own entries? other/c is a child of other.
	if a := ask(since, "other"); !a.Changed {
		t.Fatalf("other/c changed: %+v", a)
	}
	since = cur()
	// A direct file of the directory.
	e.commit(f, e.op("top", seqOf("top"), "t2"))
	if a := ask(since, "", "sub", "other"); !a.Changed {
		t.Fatalf("a file of the root changed: %+v", a)
	}
	// A subdirectory that appears; one that disappears.
	since = cur()
	e.commit(f, e.op("fresh/x", 0, "x"))
	if a := ask(since, "", "sub", "other"); !a.Changed {
		t.Fatalf("a new subdirectory: %+v", a)
	}
	if a := ask(since, "", "sub", "other", "fresh"); a.Changed {
		t.Fatalf("a subdirectory the page already shows: %+v", a)
	}
	since = cur()
	e.commit(f, proto.Op{Path: "fresh/x", BaseSeq: seqOf("fresh/x")})
	if a := ask(since, "", "sub", "other", "fresh"); !a.Changed {
		t.Fatalf("a subdirectory that is empty now: %+v", a)
	}
	if a := ask(since, "", "sub", "other", "fresh"); !a.Changed {
		t.Fatal("asked again with the same answer")
	}
	// With the deleted files shown, the subdirectory stays (it holds a tombstone).
	q := url.Values{"since": {itoa(since)}, "deleted": {"1"}, "d": {"sub", "other", "fresh"}}
	if _, body := e.get(c, "/folders/"+itoa(f)+"/changed?"+q.Encode()); strings.Contains(body, `"changed":true`) {
		t.Fatalf("deleted files are shown: %s", body)
	}
}

// The first upload of a folder takes many commits. A time before the folder's history is taken as the end of that
// upload, not as its first commit (which held one file), and a file created afterwards is not part of that state
// (user 2026-11-05: projects at 7 days ago said "no difference").
func TestRestoreBeforeTheHistoryUsesTheEndOfTheInitialImport(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	base := "/folders/" + itoa(f)
	names := []string{"a", "b", "c", "d", "dir/e", "dir/f"}
	for _, n := range names { // the import: one commit every 10 s
		e.commit(f, e.op(n, 0, n+"1"))
		e.clk.Add(10 * time.Second)
	}
	end := e.clk.Now().Add(-10 * time.Second)
	e.clk.Add(30 * time.Minute) // quiet: the import is over
	e.commit(f, e.op("late", 0, "l1"))
	e.clk.Add(time.Minute)
	e.commit(f, e.op("late", 7, "l2"), e.op("a", 1, "a2"), e.op("dir/e", 5, "e2"))
	e.clk.Add(time.Minute)
	e.commit(f, proto.Op{Path: "d", BaseSeq: 4}) // deleted
	long := end.Add(-7 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	preview := func(path string) string {
		_, page := e.get(c, base+"/restore-preview?"+url.Values{"path": {path}, "time": {long}}.Encode())
		return page
	}
	// The whole folder: a and dir/e changed, d is gone; "late" did not exist at the import end, c, b and dir/f are as they were.
	page := preview("")
	if !strings.Contains(page, `data-count="3"`) || !strings.Contains(page, "首次导入完成于") || !strings.Contains(page, `datetime="`+end.Format("2006-01-02T15:04:05Z")+`"`) ||
		strings.Contains(page, ">late<") || strings.Contains(page, ">b<") {
		t.Fatalf("folder:\n%s", page)
	}
	// A directory of the import, the same state.
	if page = preview("dir"); !strings.Contains(page, `data-count="1"`) || !strings.Contains(page, "首次导入完成于") {
		t.Fatalf("dir:\n%s", page)
	}
	// A file created after the import starts at its own first record.
	late := end.Add(30*time.Minute + 10*time.Second) // the clock after the quiet time; the file is created then
	if page = preview("late"); !strings.Contains(page, `data-count="1"`) || !strings.Contains(page, "最早记录为") || strings.Contains(page, "首次导入") || !strings.Contains(page, `datetime="`+late.Format("2006-01-02T15:04:05Z")+`"`) {
		t.Fatalf("late:\n%s", page)
	}
	// The restore does what the preview says and touches nothing else.
	before := e.nextSeq(f)
	e.post(c, base+"/restore-dir", url.Values{"path": {""}, "time": {long}})
	if got := e.nextSeq(f) - before; got != 3 {
		t.Fatalf("the restore changed %d files, want 3", got)
	}
	h := e.block([]byte("l2"))
	if got := e.fcount(f, `SELECT count(*) FROM files WHERE path='late' AND hash=?`, h[:]); got != 1 {
		t.Fatal("late was touched")
	}
}

// An import that has not gone quiet yet is not finished: the time falls back to the first record.
func TestRestoreDuringTheInitialImportUsesTheFirstRecord(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	first := e.clk.Now()
	for _, n := range []string{"a", "b", "c"} {
		e.commit(f, e.op(n, 0, n+"1"))
		e.clk.Add(10 * time.Second)
	}
	e.commit(f, e.op("a", 1, "a2"))
	_, page := e.get(c, "/folders/"+itoa(f)+"/restore-preview?"+url.Values{"time": {first.Add(-time.Hour).Format("2006-01-02 15:04:05")}}.Encode())
	if !strings.Contains(page, "最早记录为") || strings.Contains(page, "首次导入") || !strings.Contains(page, `data-count="1"`) {
		t.Fatalf("page:\n%s", page)
	}
}
