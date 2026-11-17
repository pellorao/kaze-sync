package main

// The write-control scenario group (design 2.8.12): real clients against a running server, every scenario in a
// folder of its own. It never touches real data: it makes its own devices and folders (prefix "wct") and removes
// them at the end.
//
//	KAZE_E2E_PASSWORD=... go run ./e2e -wc https://host:port [-wc-fp sha256:...] [-wc-win-dir 'C:\x\wct']
//	go run ./e2e -wc-local        # a throw-away server on 127.0.0.1 and two Linux clients, for trying the driver
//
// Side B is a Linux client unless -wc-win-dir names a directory on the Windows disk.

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"kaze-sync/e2e/vmctl"
	"kaze-sync/internal/server"
)

// Tunables.
const (
	wcWindowMin = 1                // N of the single-writer time window, in minutes
	wcSync      = 60 * time.Second // how long a change may take to reach the other side or the page
	wcPrefix    = "wct"
)

// wcSide is one client: its agent, its device id on the server, and where it keeps a folder.
type wcSide struct {
	name, id string
	ag       *agent
	root     string // below this, one directory per folder (a path of the machine running the driver)
	spell    string // the same directory as the client sees it (differs for Windows)
	sep      string
}

func (s *wcSide) dir(folder string) string { return filepath.Join(s.root, folder) }
func (s *wcSide) path(folder, rel string) string {
	return filepath.Join(s.dir(folder), filepath.FromSlash(rel))
}

type wcGroup struct {
	d       *driver
	a, b    *wcSide
	folders map[string]string // name -> id
	clicks  int
	window  string // the window setting to restore
	winWork string // the Windows-side directory made by setup (a path of the machine running the driver)
	push    *fakePush
}

// fakePush is a push service the local server sends its Web Push to (the driver registers one subscription at it), so
// the driver sees when a push arrives; it needs the in-process server (-wc-local), as a remote one would not trust it.
type fakePush struct {
	ts   *httptest.Server
	mu   sync.Mutex
	msgs []time.Time
}

func startFakePush() *fakePush {
	f := &fakePush{}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.msgs = append(f.msgs, time.Now())
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	server.SetPushClient(f.ts.Client())
	return f
}

func (f *fakePush) stop() { f.ts.Close() }

// subscribe registers the push service as the browser "kaze-e2e" of the admin session.
func (f *fakePush) subscribe(w *webClient) error {
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	b64 := base64.RawURLEncoding.EncodeToString
	body, _ := json.Marshal(map[string]any{"endpoint": f.ts.URL + "/e2e", "name": "kaze-e2e", "keys": map[string]string{"p256dh": b64(k.PublicKey().Bytes()), "auth": b64(auth)}})
	req, _ := http.NewRequest("POST", w.base+"/push/subscribe", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", w.base)
	resp, err := w.hc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("push subscribe: %d", resp.StatusCode)
	}
	return nil
}

// first returns the first push received after t0, or false.
func (f *fakePush) first(t0 time.Time) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, at := range f.msgs {
		if at.After(t0) {
			return at, true
		}
	}
	return time.Time{}, false
}

// pushLatency waits a little for the Web Push of a problem raised after t0 and describes how long it took. A held
// change is pushed after the 2 minute wait (design 4.3), so the wait here is only a short look, not a pass criterion.
func (g *wcGroup) pushLatency(t0 time.Time) string {
	if g.push == nil {
		return "web push not set up"
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if at, ok := g.push.first(t0); ok {
			return fmt.Sprintf("web push %.1f s after the write", at.Sub(t0).Seconds())
		}
		if time.Now().After(deadline) {
			return "web push not received within 15 s (it waits 2 min for a held change)"
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// runWC is the entry point of the group; it reports success.
func runWC(base, fp, winDir string, local bool, only string) (success bool) {
	d := &driver{tmp: "", logs: "", base: base, fp: fp, admin: os.Getenv("KAZE_E2E_PASSWORD")}
	var err error
	if d.tmp, err = os.MkdirTemp("", "wc-run-"); err != nil {
		log.Fatal(err)
	}
	defer func() {
		if len(d.fails) == 0 {
			os.RemoveAll(d.tmp)
		}
	}()
	d.logs = filepath.Join(d.tmp, "logs")
	os.MkdirAll(d.logs, 0o755)
	if local {
		var stop func()
		if d.base, d.fp, d.admin, stop = localServer(d.tmp); stop != nil {
			defer stop()
		}
	}
	g := &wcGroup{d: d, folders: map[string]string{}}
	if local {
		g.push = startFakePush()
		defer g.push.stop()
	}
	defer func() {
		g.cleanup()
		success = success && len(d.fails) == 0
		g.removeWinWork()
		for _, m := range d.metrics {
			log.Print("METRIC ", m)
		}
		log.Printf("logs in %s", d.logs)
	}()
	if !d.step("build binaries", d.build) || !d.step("login", d.login) || !d.step("setup: two clients", func() error { return g.setup(winDir) }) {
		return false
	}
	for _, s := range []struct {
		name string
		f    func() error
	}{
		{"new-folder prompt: link opens the root dialog, saving or keeping the default ends it", g.prompt},
		{"hand-over: after N no freeze, within N the second writer is held; upload", g.handover},
		{"free: the same file on both sides holds only that file; discard and keep a copy", g.freeSameFile},
		{"single root, offline edit of an unrelated file: the folder is frozen; stale decision, then overwrite", g.offlineUnrelated},
		{"config change while frozen sends the held change", g.configWhileFrozen},
		{"Web restore, then a write within N", g.webRestore},
		{"exclusive misuse is held; discard without saving", g.exclusive},
		{"git commit on A, git status on B within N (informational)", g.git},
	} {
		if !strings.Contains(s.name, only) {
			continue
		}
		if !d.step("wc: "+s.name, s.f) {
			g.snapshot()
		}
	}
	return len(d.fails) == 0
}

// localServer starts a throw-away server in this process (for trying the driver; the clients are real processes).
func localServer(tmp string) (base, fp, pw string, stop func()) {
	data := filepath.Join(tmp, "server")
	os.MkdirAll(filepath.Join(data, server.VarDir), 0o755)
	db, err := server.OpenDB(filepath.Join(data, server.VarDir, "meta.db"), true)
	if err != nil || server.Migrate(db) != nil {
		log.Fatal(err, " (local server)")
	}
	h := sha256.Sum256([]byte("PW")) // passwords are compared upper-cased
	server.SetMeta(db, "admin_password_hash", hex.EncodeToString(h[:]))
	db.Close()
	s, err := server.New(data, nil)
	if err != nil {
		log.Fatal(err)
	}
	ts := httptest.NewTLSServer(s.Handler())
	sum := sha256.Sum256(ts.Certificate().Raw)
	return ts.URL, "sha256:" + hex.EncodeToString(sum[:]), "PW", func() { ts.Close(); s.Close() }
}

func (g *wcGroup) setup(winDir string) error {
	d := g.d
	g.a = &wcSide{name: wcPrefix + "-a", root: filepath.Join(d.tmp, "a"), sep: "/"}
	g.a.spell = g.a.root
	g.a.ag = d.newAgent(g.a.name, filepath.Join(d.tmp, "a-state"))
	g.b = &wcSide{name: wcPrefix + "-b", root: filepath.Join(d.tmp, "b"), sep: "/"}
	g.b.spell = g.b.root
	g.b.ag = d.newAgent(g.b.name, filepath.Join(d.tmp, "b-state"))
	if winDir != "" {
		work, err := vmctl.WSLPath(winDir)
		if err != nil {
			return err
		}
		if err := requireEmptyDir(work); err != nil {
			return err
		}
		g.winWork = work // set only after the check: removeWinWork must never touch a directory setup did not create empty
		if err := os.MkdirAll(filepath.Join(work, "state"), 0o755); err != nil {
			return err
		}
		exe, runExe := filepath.Join(work, "wct.exe"), filepath.Join(work, "wct-agent.exe")
		for from, to := range map[string]string{d.bins.windows: exe, d.bins.winAgent: runExe} {
			b, err := os.ReadFile(from)
			if err != nil {
				return err
			}
			if err := os.WriteFile(to, b, 0o755); err != nil {
				return err
			}
		}
		d.dropAgent(g.b.ag)
		g.b.ag = &agent{name: g.b.name, exe: exe, runExe: runExe, windows: true, logs: d.logs, state: filepath.Join(work, "state"), stateWin: winDir + `\state`}
		d.agents = append(d.agents, g.b.ag)
		g.b.root, g.b.spell, g.b.sep = filepath.Join(work, "root"), winDir+`\root`, `\`
	}
	for _, s := range []*wcSide{g.a, g.b} {
		code, err := d.web.addDevice(s.name)
		if err != nil {
			return err
		}
		if err := s.ag.enroll(d.base, code, d.fp); err != nil {
			return err
		}
		if s.id, err = d.web.idOf("devices", s.name); err != nil {
			return err
		}
		if err := s.ag.start(); err != nil {
			return err
		}
	}
	if g.push != nil {
		if err := g.push.subscribe(d.web); err != nil {
			return err
		}
	}
	// N is 1 minute for the group; the old value comes back at the end.
	_, set, err := d.web.get("/settings")
	if err != nil {
		return err
	}
	if m := regexp.MustCompile(`name="window" value="(\d+)"`).FindStringSubmatch(set); m != nil {
		g.window = m[1]
	}
	return g.setWindow(fmt.Sprint(wcWindowMin))
}

func (g *wcGroup) setWindow(min string) error {
	if min == "" {
		return nil
	}
	_, err := g.d.web.ok("/settings/window", url.Values{"window": {min}})
	return err
}

// folder makes a folder mapped to both sides (adopting the empty directories), with the root mode given.
func (g *wcGroup) folder(name, rootMode string) (string, error) {
	d, name := g.d, wcPrefix+"-"+name
	id, err := d.web.newFolder(name)
	if err != nil {
		return "", err
	}
	g.folders[name] = id
	for _, s := range []*wcSide{g.a, g.b} {
		os.MkdirAll(s.dir(name), 0o755)
		if err := d.web.mapFolder(s.id, id, s.spell+s.sep+name, true); err != nil {
			return "", err
		}
	}
	if rootMode != "" {
		if err := g.mode(id, "", rootMode, ""); err != nil {
			return "", err
		}
	}
	for _, s := range []*wcSide{g.a, g.b} {
		if _, err := until("the marker in "+s.dir(name), wcSync, func() (bool, error) {
			_, err := os.Stat(filepath.Join(s.dir(name), ".syncroot"))
			return err == nil, err
		}); err != nil {
			return "", err
		}
	}
	return id, nil
}

func (g *wcGroup) mode(folder, path, mode, dev string) error {
	_, err := g.d.web.ok("/folders/"+folder+"/writectl", url.Values{"path": {path}, "mode": {mode}, "device": {dev}})
	return err
}

// put writes a file the way a user would (an old mtime: stable at once).
func (g *wcGroup) put(s *wcSide, folder, rel, content string) {
	writeOld(s.path(folder, rel), []byte(content))
}

func (g *wcGroup) read(s *wcSide, folder, rel string) string {
	return readOr(s.path(folder, rel))
}

// has waits until the file has the content on the side and returns how long it took.
func (g *wcGroup) has(s *wcSide, folder, rel, content string) (time.Duration, error) {
	return until(rel+" on "+s.name, wcSync, func() (bool, error) { return g.read(s, folder, rel) == content, nil })
}

// held returns the paths the side holds in the folder, from the decision dialog of the Web UI.
func (g *wcGroup) held(s *wcSide, folder string) (page string, paths []string, err error) {
	page, err = g.d.web.folderPage(folder, "held="+s.id)
	for _, m := range regexp.MustCompile(`<td class="mono">([^<]*)</td>`).FindAllStringSubmatch(page, -1) {
		paths = append(paths, html.UnescapeString(m[1]))
	}
	return
}

// waitHeld waits until the held list of the side is exactly want (sorted by the page) and returns the latency.
func (g *wcGroup) waitHeld(s *wcSide, folder string, want ...string) (time.Duration, error) {
	return until(fmt.Sprintf("%s to hold %v", s.name, want), wcSync, func() (bool, error) {
		page, got, err := g.held(s, folder)
		var why []string
		for _, m := range regexp.MustCompile(`<span class="muted">([^<]*)</span></div>`).FindAllStringSubmatch(page, -1) {
			why = append(why, html.UnescapeString(m[1]))
		}
		return err == nil && strings.Join(got, ",") == strings.Join(want, ","), fmt.Errorf("held %v (%v) %v", got, why, err)
	})
}

// resolve takes the decisions: all = "" decides row by row ("path=action" pairs), otherwise it is a batch action
// on every scope. Each submit is a click, and so is opening the dialog.
func (g *wcGroup) resolve(s *wcSide, folder, all string, save bool, rows ...string) error {
	page, _, err := g.held(s, folder)
	if err != nil {
		return err
	}
	form := url.Values{"device": {s.id}}
	if save {
		form.Set("save", "1")
	}
	if all != "" {
		for _, sc := range regexp.MustCompile(`name="scope" value="([^"]*)"`).FindAllStringSubmatch(page, -1) {
			f := url.Values{"device": form["device"], "save": form["save"], "scope": {html.UnescapeString(sc[1])}, "action": {all}, "seen": {"999999999"}}
			if _, err := g.d.web.ok("/folders/"+folder+"/resolve", f); err != nil {
				return err
			}
			g.clicks++
		}
	} else {
		for _, r := range rows {
			p, act, _ := strings.Cut(r, "=")
			for _, m := range regexp.MustCompile(`name="r" value="([^"]*)"`).FindAllStringSubmatch(page, -1) {
				if v := html.UnescapeString(m[1]); strings.HasSuffix(v, "\t"+p) {
					form.Add("r", v)
					form.Add("a", act)
				}
			}
		}
		if _, err := g.d.web.ok("/folders/"+folder+"/resolve", form); err != nil {
			return err
		}
		g.clicks++
	}
	g.clicks++ // the dialog was opened from the notification
	return nil
}

// same checks that both sides hold the same files in the folder (the hash of every file).
func (g *wcGroup) same(folder string) error {
	var hashes string
	_, err := until("equal trees of "+folder, wcSync, func() (bool, error) {
		a, err := tree(g.a.dir(folder))
		if err != nil {
			return false, err
		}
		b, err := tree(g.b.dir(folder))
		if err != nil {
			return false, err
		}
		for _, t := range []map[string]string{a, b} {
			for p := range t {
				if strings.HasPrefix(p, ".syncsaved/") { // what a discard set aside stays on one side
					delete(t, p)
				}
			}
		}
		if len(a) != len(b) {
			return false, fmt.Errorf("%d vs %d files", len(a), len(b))
		}
		for p, h := range a {
			if b[p] != h {
				return false, fmt.Errorf("%s differs", p)
			}
		}
		var hs []string
		for p, h := range a {
			hs = append(hs, p+" "+h[:8])
		}
		sort.Strings(hs)
		hashes = strings.Join(hs, ", ")
		return true, nil
	})
	if err == nil {
		g.d.metric("WC hashes %s: A = B (sha256 prefix): %s; the server's own hash is not readable from the Web UI or API", folder, hashes)
	}
	return err
}

// report records a scenario's latency to the held page and, once decided, the clicks it took (call decided after).
func (g *wcGroup) report(what string, clicks0 int, took time.Duration) func() {
	return func() {
		g.d.metric("WC %s: held page %.1f s after the write, %s, %d clicks to decide", what, took.Seconds(), g.pushLatency(time.Now().Add(-took-time.Second)), g.clicks-clicks0)
	}
}

func (g *wcGroup) prompt() error {
	d := g.d
	for _, keep := range []bool{false, true} {
		name := wcPrefix + "-prompt"
		id, err := d.web.newFolder(name + fmt.Sprint(keep))
		if err != nil {
			return err
		}
		g.folders[name+fmt.Sprint(keep)] = id
		if _, page, _ := d.web.get("/"); !strings.Contains(page, "/folders/"+id+"?wc=") {
			return fmt.Errorf("no new-folder prompt for folder %s", id)
		}
		if page, err := d.web.folderPage(id, "wc="); err != nil || !strings.Contains(page, `id="dlg-wc"`) || !strings.Contains(page, "data-autoopen") {
			return fmt.Errorf("the link does not open the root dialog (%v)", err)
		}
		if keep {
			_, err = d.web.ok("/folders/"+id+"/wc-ack", url.Values{})
		} else {
			err = g.mode(id, "", "single", "")
		}
		if err != nil {
			return err
		}
		if _, page, _ := d.web.get("/"); strings.Contains(page, "/folders/"+id+"?wc=") {
			return fmt.Errorf("the prompt of folder %s stays", id)
		}
	}
	return nil
}

func (g *wcGroup) handover() error {
	id, err := g.folder("handover", "single")
	if err != nil {
		return err
	}
	f := wcPrefix + "-handover"
	g.put(g.a, f, "h.txt", "a1")
	if _, err := g.has(g.b, f, "h.txt", "a1"); err != nil {
		return err
	}
	// B writes right after A: within N the second writer is held, and uploading makes its version the server's.
	t0, c0 := time.Now(), g.clicks
	g.put(g.b, f, "h.txt", "b within N!")
	if _, err := g.waitHeld(g.b, id, "h.txt"); err != nil {
		return err
	}
	done := g.report("hand-over within N", c0, time.Since(t0))
	if err := g.resolve(g.b, id, "", false, "h.txt=upload"); err != nil {
		return err
	}
	done()
	if _, err := g.has(g.a, f, "h.txt", "b within N!"); err != nil {
		return err
	}
	// After N the writer changes hands without a freeze, in both directions.
	time.Sleep(time.Duration(wcWindowMin)*time.Minute + 10*time.Second)
	g.put(g.a, f, "h.txt", "a after N")
	if _, err := g.has(g.b, f, "h.txt", "a after N"); err != nil {
		return fmt.Errorf("a write after N was not accepted: %w", err)
	}
	if _, p, _ := g.held(g.a, id); len(p) != 0 {
		return fmt.Errorf("A held after N: %v", p)
	}
	return g.same(f)
}

// offline lets side B settle, stops it, lets both sides write (A's write must be on the server before B writes and
// starts again, so B is the stale one). The first file of a must change its size.
func (g *wcGroup) offline(folder string, a, b map[string]string) error {
	time.Sleep(5 * time.Second) // B has recorded what it pulled
	g.b.ag.stop(false)
	for p, c := range a {
		g.put(g.a, folder, p, c)
	}
	k := firstKey(a)
	re := regexp.MustCompile(`(?s)data-path="` + regexp.QuoteMeta(html.EscapeString(k)) + `".*?<td class="size">([^<]*)</td>`)
	if _, err := until("A's write of "+k+" on the server", wcSync, func() (bool, error) {
		page, err := g.d.web.folderPage(g.folders[folder], "")
		m := re.FindStringSubmatch(page)
		return err == nil && m != nil && m[1] == fmt.Sprintf("%d B", len(a[k])), err
	}); err != nil {
		return err
	}
	for p, c := range b {
		g.put(g.b, folder, p, c)
	}
	return g.b.ag.start()
}

func firstKey(m map[string]string) string {
	for k := range m {
		return k
	}
	return ""
}

func (g *wcGroup) freeSameFile() error {
	id, err := g.folder("free", "free")
	if err != nil {
		return err
	}
	f := wcPrefix + "-free"
	g.put(g.a, f, "same.txt", "base")
	g.put(g.a, f, "own.txt", "base")
	if _, err := g.has(g.b, f, "own.txt", "base"); err != nil {
		return err
	}
	time.Sleep(time.Duration(wcWindowMin)*time.Minute + 10*time.Second) // even in a free folder a file just written by the other side is held
	t0, c0 := time.Now(), g.clicks
	if err := g.offline(f, map[string]string{"same.txt": "from A"}, map[string]string{"same.txt": "from B!", "own.txt": "B only"}); err != nil {
		return err
	}
	if _, err := g.waitHeld(g.b, id, "same.txt"); err != nil {
		return err
	}
	done := g.report("free, same file", c0, time.Since(t0))
	if _, err := g.has(g.a, f, "own.txt", "B only"); err != nil {
		return fmt.Errorf("an unrelated file was frozen too: %w", err)
	}
	if err := g.resolve(g.b, id, "discard", true); err != nil {
		return err
	}
	done()
	if _, err := g.has(g.b, f, "same.txt", "from A"); err != nil {
		return err
	}
	if m, _ := filepath.Glob(filepath.Join(g.b.dir(f), ".syncsaved", "*", "same.txt")); len(m) != 1 {
		return fmt.Errorf("saved copies: %v", m)
	}
	return g.same(f)
}

func (g *wcGroup) offlineUnrelated() error {
	id, err := g.folder("single", "single")
	if err != nil {
		return err
	}
	f := wcPrefix + "-single"
	g.put(g.a, f, "a.txt", "base")
	g.put(g.a, f, "b.txt", "base")
	if _, err := g.has(g.b, f, "b.txt", "base"); err != nil {
		return err
	}
	time.Sleep(time.Duration(wcWindowMin)*time.Minute + 10*time.Second) // the setup writes are older than N
	t0, c0 := time.Now(), g.clicks
	if err := g.offline(f, map[string]string{"a.txt": "A edit"}, map[string]string{"b.txt": "B edit"}); err != nil {
		return err
	}
	if _, err := g.waitHeld(g.b, id, "b.txt"); err != nil {
		return fmt.Errorf("no freeze (D9): %w", err)
	}
	done := g.report("single root, unrelated file", c0, time.Since(t0))
	// A decision made on a file that has changed since is not carried out and says so.
	page, _, _ := g.held(g.b, id)
	stale := regexp.MustCompile(`name="r" value="([^"]*)"`).FindStringSubmatch(page)
	g.put(g.b, f, "b.txt", "B edited again, longer")
	time.Sleep(3 * time.Second)
	if stale == nil {
		return fmt.Errorf("the held page has no decision form (name=\"r\"): the stale-decision check cannot run")
	}
	if _, err := g.d.web.ok("/folders/"+id+"/resolve", url.Values{"device": {g.b.id}, "r": {html.UnescapeString(stale[1])}, "a": {"overwrite"}}); err != nil {
		return err
	}
	g.clicks++
	if _, err := until("the stale decision to be refused", wcSync, func() (bool, error) {
		p, _, err := g.held(g.b, id)
		return strings.Contains(p, "本机又改过"), err
	}); err != nil {
		return err
	}
	if err := g.resolve(g.b, id, "overwrite", false); err != nil {
		return err
	}
	done()
	if _, err := g.has(g.a, f, "b.txt", "B edited again, longer"); err != nil {
		return err
	}
	return g.same(f)
}

func (g *wcGroup) configWhileFrozen() error {
	id, err := g.folder("config", "single")
	if err != nil {
		return err
	}
	f := wcPrefix + "-config"
	g.put(g.a, f, "a.txt", "base")
	g.put(g.a, f, "b.txt", "base")
	if _, err := g.has(g.b, f, "b.txt", "base"); err != nil {
		return err
	}
	time.Sleep(time.Duration(wcWindowMin)*time.Minute + 10*time.Second)
	if err := g.offline(f, map[string]string{"a.txt": "A edit"}, map[string]string{"b.txt": "B edit"}); err != nil {
		return err
	}
	if _, err := g.waitHeld(g.b, id, "b.txt"); err != nil {
		return err
	}
	if err := g.mode(id, "", "free", ""); err != nil {
		return err
	}
	if _, err := g.has(g.a, f, "b.txt", "B edit"); err != nil {
		return fmt.Errorf("the held change was not sent after the configuration change: %w", err)
	}
	if _, err := g.waitHeld(g.b, id); err != nil {
		return err
	}
	return g.same(f)
}

func (g *wcGroup) webRestore() error {
	id, err := g.folder("restore", "single")
	if err != nil {
		return err
	}
	f := wcPrefix + "-restore"
	g.put(g.a, f, "r.txt", "v1")
	if _, err := g.has(g.b, f, "r.txt", "v1"); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	at := time.Now().UTC().Format("2006-01-02 15:04:05")
	time.Sleep(3 * time.Second)
	time.Sleep(time.Duration(wcWindowMin)*time.Minute + 10*time.Second)
	g.put(g.a, f, "r.txt", "v2 longer")
	if _, err := g.has(g.b, f, "r.txt", "v2 longer"); err != nil {
		return err
	}
	time.Sleep(time.Duration(wcWindowMin)*time.Minute + 10*time.Second)
	if _, err := g.d.web.restoreDir(id, "", at); err != nil {
		return err
	}
	if _, err := g.has(g.b, f, "r.txt", "v1"); err != nil {
		return err
	}
	g.put(g.b, f, "r2.txt", "B right after the restore")
	t0 := time.Now()
	if _, err := g.waitHeld(g.b, id, "r2.txt"); err != nil {
		return err
	}
	g.d.metric("WC web restore: the write of the other side was held after %.1f s, %s", time.Since(t0).Seconds(), g.pushLatency(t0.Add(-time.Second)))
	return g.resolve(g.b, id, "discard", false)
}

func (g *wcGroup) exclusive() error {
	id, err := g.folder("excl", "free")
	if err != nil {
		return err
	}
	f := wcPrefix + "-excl"
	if err := g.mode(id, "locked", "exclusive", g.a.id); err != nil {
		return err
	}
	g.put(g.a, f, "locked/own.txt", "A owns this")
	if _, err := g.has(g.b, f, "locked/own.txt", "A owns this"); err != nil {
		return err
	}
	t0, c0 := time.Now(), g.clicks
	g.put(g.b, f, "locked/mine.txt", "B must not")
	if _, err := g.waitHeld(g.b, id, "locked/mine.txt"); err != nil {
		return err
	}
	done := g.report("exclusive misuse", c0, time.Since(t0))
	if err := g.resolve(g.b, id, "discard", false); err != nil {
		return err
	}
	done()
	if _, err := until("the file to go from B", wcSync, func() (bool, error) { _, err := os.Stat(g.b.path(f, "locked/mine.txt")); return err != nil, nil }); err != nil {
		return err
	}
	if m, _ := filepath.Glob(filepath.Join(g.b.dir(f), ".syncsaved", "*")); len(m) != 0 {
		return fmt.Errorf("a direct discard kept a copy: %v", m)
	}
	return g.same(f)
}

// git is informational: what git does to the index of a synced repository depends on the version, so the outcome is
// recorded, not asserted.
func (g *wcGroup) git() error {
	// A is always local. A Windows B uses git.exe (WSL interop); a local B uses git.
	gitB := "git"
	if g.b.sep != "/" {
		gitB = "git.exe"
	}
	_, errA := exec.LookPath("git")
	_, errB := exec.LookPath(gitB)
	if errA != nil || errB != nil {
		g.d.metric("WC git: skipped (%s not found here)", gitB)
		return nil
	}
	id, err := g.folder("git", "single")
	if err != nil {
		return err
	}
	f := wcPrefix + "-git"
	run := func(dir string, args ...string) error {
		bin := "git"
		if dir == g.b.dir(f) {
			bin = gitB
		}
		c := exec.Command(bin, append([]string{"-c", "user.name=wct", "-c", "user.email=wct@example.invalid"}, args...)...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %v: %v: %s", args, err, out)
		}
		return nil
	}
	if err := run(g.a.dir(f), "init", "-q"); err != nil {
		return err
	}
	g.put(g.a, f, "x.txt", "code")
	if err := run(g.a.dir(f), "add", "."); err != nil {
		return err
	}
	if err := run(g.a.dir(f), "commit", "-q", "-m", "one"); err != nil {
		return err
	}
	if _, err := until("the repository on B", wcSync, func() (bool, error) {
		_, err := os.Stat(g.b.path(f, ".git/HEAD"))
		_, err2 := os.Stat(g.b.path(f, "x.txt"))
		return err == nil && err2 == nil, nil
	}); err != nil {
		return err
	}
	time.Sleep(10 * time.Second) // let B finish applying the objects
	if err := run(g.b.dir(f), "status"); err != nil {
		return err
	}
	time.Sleep(15 * time.Second)
	_, p, _ := g.held(g.b, id)
	g.d.metric("WC git status on B within N: %d held paths %v", len(p), p)
	if len(p) > 0 {
		return g.resolve(g.b, id, "discard", true)
	}
	return nil
}

// snapshot saves what is needed to find out why a scenario failed: the held dialog of every side and the client logs.
func (g *wcGroup) snapshot() {
	for name, id := range g.folders {
		for _, s := range []*wcSide{g.a, g.b} {
			if page, err := g.d.web.folderPage(id, "held="+s.id); err == nil {
				os.WriteFile(filepath.Join(g.d.logs, fmt.Sprintf("held-%s-%s.html", name, s.name)), []byte(page), 0o644)
			}
		}
	}
	for _, s := range []*wcSide{g.a, g.b} {
		os.WriteFile(filepath.Join(g.d.logs, s.name+".state.log"), []byte(s.ag.stateLog()), 0o644)
	}
}

// cleanup also discovers resources whose creation succeeded before setup or a scenario failed to record the id.
func (g *wcGroup) cleanup() {
	d := g.d
	for _, s := range []*wcSide{g.a, g.b} {
		if s != nil && s.ag != nil {
			s.ag.stop(false)
		}
	}
	if d.web == nil {
		return
	}
	report := func(err error) {
		if err != nil {
			log.Printf("FAIL wc cleanup: %v", err)
			d.fails = append(d.fails, "wc cleanup: "+err.Error())
		}
	}
	report(g.setWindow(g.window))
	devices, folders := map[string]bool{}, map[string]bool{}
	for _, s := range []*wcSide{g.a, g.b} {
		if s != nil && s.id != "" {
			devices[s.id] = true
		}
	}
	for _, id := range g.folders {
		folders[id] = true
	}
	for _, item := range []struct {
		kind string
		ids  map[string]bool
	}{{"devices", devices}, {"folders", folders}} {
		links, err := d.listLinks(item.kind)
		report(err)
		for _, link := range links {
			if strings.HasPrefix(link.name, wcPrefix+"-") {
				item.ids[link.id] = true
			}
		}
	}
	// Revocation removes all device mappings, so even a partially mapped folder can be deleted.
	for id := range devices {
		_, err := d.web.ok("/devices/"+id+"/revoke", url.Values{})
		report(err)
	}
	for id := range folders {
		_, err := d.web.ok("/folders/"+id+"/delete", url.Values{"confirm": {"1"}})
		report(err)
	}
}

// removeWinWork deletes what setup put in the Windows-side directory (only those entries), then the directory if empty.
func (g *wcGroup) removeWinWork() {
	if g.winWork == "" {
		return
	}
	for _, n := range []string{"wct.exe", "wct-agent.exe", "state", "root"} {
		if err := os.RemoveAll(filepath.Join(g.winWork, n)); err != nil {
			log.Printf("remove %s: %v", filepath.Join(g.winWork, n), err)
		}
	}
	os.Remove(g.winWork)
}

// requireEmptyDir accepts a missing or empty directory and refuses anything else, so that the
// cleanup (removeWinWork) only ever deletes what setup itself put there.
func requireEmptyDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(ents) > 0 {
		return fmt.Errorf("%s is not empty (%d entries): refusing to use it as the scratch directory", dir, len(ents))
	}
	return nil
}
