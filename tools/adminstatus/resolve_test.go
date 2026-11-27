package main

import (
	"bytes"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const bellHTML = `<div id="notif" popover class="pop notif" data-live="notif"><div class="notif-h"><h3>Notifications</h3><span class="cnt err"><svg class="i"><use href="/static/icons.svg#i-err"/></svg>2 errors</span></div>
<div class="notif-b">
<div class="grp-h" id="ng-1"><a href="/folders/1">projects</a><span class="cnt err"><svg class="i"><use href="/static/icons.svg#i-err"/></svg>1</span></div>
<div class="nitem"><span class="ic err"><svg class="i"><use href="/static/icons.svg#i-err"/></svg></span>
<span class="msg">win has 1 change waiting for your decision: proj-x</span><a class="path" href="/folders/1?held=2">Handle…</a>
<span class="meta">win · <time datetime="2026-11-27T18:42:21Z">2026-11-27 18:42:21</time></span>
<span class="adv">In the dialog choose</span>
</div><details class="nitem fold"><summary><span class="ic warn"><svg class="i"><use href="/static/icons.svg#i-warn"/></svg></span><span class="msg">win: 2 symbolic links not created</span><span class="meta">since <time>2026-11-27 18:42:21</time></span><span class="expand">Expand</span></summary>
<div class="nitem"><span class="ic warn"><svg class="i"></svg></span>
<span class="msg">cannot create symbolic link</span>
<a class="path" href="/folders/1?dir=a#a%2Fl1">a/l1</a>
<span class="meta">win · <time>2026-11-27 18:42:21</time></span>
<span class="adv">exclude it</span></div><div class="nitem"><span class="ic warn"><svg class="i"></svg></span>
<span class="msg">cannot create symbolic link</span>
<a class="path" href="/folders/1?dir=a#a%2Fl2">a/l2</a>
<span class="meta">win · <time>2026-11-27 18:42:21</time></span>
<span class="adv">exclude it</span></div></details>
<div class="grp-h" id="ng-dev">Devices</div>
<div class="nitem"><span class="ic warn"><svg class="i"></svg></span><span class="msg"><a href="/devices/4">old &amp; box</a> has not connected for 4 days</span><span class="meta">Last connected <time>2026-11-24 09:00</time></span><span class="adv">Bring the device online, or revoke it</span></div>
<div class="grp-h" id="ng-srv">Server</div>
<div class="nitem"><span class="ic err"><svg class="i"></svg></span>
<span class="msg">The server's daily cleanup (GC) failed</span><span class="adv">Check the server log</span></div>
</div>
<div class="notif-f">Notifications are computed from the current state</div></div>`

func TestParseNotices(t *testing.T) {
	ns := parseNotices("<html>" + bellHTML + "</html>")
	if len(ns) != 4 {
		t.Fatalf("%d notices: %+v", len(ns), ns)
	}
	h := ns[0]
	if h.Level != "error" || h.Group != "projects" || h.FolderID != "1" || h.Link != "/folders/1?held=2" || !strings.HasPrefix(h.Text, "win has 1 change") || h.Meta != "win · 2026-11-27 18:42:21" {
		t.Errorf("held notice: %+v", h)
	}
	f := ns[1]
	if f.Level != "warning" || f.Count != 2 || len(f.Items) != 2 || f.Items[1].LinkText != "a/l2" || f.Link != "" {
		t.Errorf("fold: %+v", f)
	}
	if d := ns[2]; d.Group != "Devices" || d.Text != "old & box has not connected for 4 days" || d.FolderID != "" {
		t.Errorf("device notice: %+v", d)
	}
	if s := ns[3]; s.Group != "Server" || s.Level != "error" || s.Advice != "Check the server log" {
		t.Errorf("server notice: %+v", s)
	}
	var b bytes.Buffer
	printNotices(&b, ns)
	if !strings.HasPrefix(b.String(), "bell: 2 error(s), 3 warning(s)\n") {
		t.Errorf("summary: %s", b.String())
	}
	if parseNotices("<html>no bell</html>") != nil {
		t.Error("notices out of a page without a bell")
	}
}

// fakeRow is a held change of the fake server.
type fakeRow struct {
	path     string
	state    string // open, waiting or gone
	noUpload bool
}

// fakeServer plays the folder page, the resolve form and a device that carries decisions out after actAfter reads.
type fakeServer struct {
	mu       sync.Mutex
	rows     []*fakeRow
	folded   bool
	posts    []url.Values
	reads    int
	actAfter int // reads of the folder page after a post before the device acts; -1 never
}

func (f *fakeServer) page() string {
	var open, waiting int
	for _, r := range f.rows {
		switch r.state {
		case "open":
			open++
		case "waiting":
			waiting++
		}
	}
	if open+waiting == 0 {
		return "<html><body>nothing held</body></html>"
	}
	var b strings.Builder
	b.WriteString(`<dialog id="dlg-held"><div class="dlg-b"><form method="post" action="/folders/1/resolve" class="held-grp">
<button class="sr" tabindex="-1" aria-hidden="true"></button><input type="hidden" name="device" value="2"><input type="hidden" name="dir" value=""><input type="hidden" name="scope" value="d:PROJ-X"><input type="hidden" name="seen" value="775117">
<div class="held-h"><span class="mono">/proj-x</span><span class="muted">` + fmt.Sprint(open+waiting) + ` changes · exclusive to wsl-projects</span></div>
`)
	if open == 0 {
		b.WriteString(`<p class="note" data-waiting><span>Decision sent.</span></p>`)
	}
	b.WriteString(`<div class="held-acts"><button class="btn sm" name="action" value="discard">Discard all</button></div>
<details class="disc"><summary>Choose one by one</summary><table class="held-rows"><thead><tr><th scope="col" class="sel"></th></tr></thead><tbody>
`)
	for _, r := range f.rows {
		if r.state == "gone" {
			continue
		}
		if f.folded {
			fmt.Fprintf(&b, `<tr class="dir"><td class="sel"></td><td class="mono"><span class="dn"><svg></svg>%s/</span></td><td>2 changes</td><td><input type="hidden" name="dr" value="%s"><select name="da"></select></td></tr>`+"\n", r.path, r.path)
			continue
		}
		fmt.Fprintf(&b, `<tr><td class="sel"><input type="checkbox" data-sel></td>
<td class="mono">%s</td><td>modified</td>
<td>`, html.EscapeString(r.path))
		if r.state == "waiting" {
			b.WriteString(`<span class="muted">Waiting for win</span></td></tr>` + "\n")
			continue
		}
		dis := ""
		if r.noUpload {
			dis = " disabled"
		}
		fmt.Fprintf(&b, `<input type="hidden" name="r" value="%s"><select name="a"><option value="">-</option><option value="upload"%s>Upload</option><option value="overwrite">Overwrite</option><option value="discard">Discard</option></select></td></tr>`+"\n", html.EscapeString("7\t10\t99\t"+r.path), dis)
	}
	b.WriteString(`</tbody></table>`)
	if f.folded {
		b.WriteString(`<p class="muted small">4 changes in all, folded by directory</p>`)
	}
	b.WriteString(`</details></form></div></dialog>`)
	return b.String()
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		open := 0
		for _, r := range f.rows {
			if r.state != "gone" {
				open++
			}
		}
		if open > 0 {
			w.Write([]byte(bellHTML))
		} else {
			w.Write([]byte(`<div id="notif"><div class="notif-b"><p class="empty">No notifications</p></div><div class="notif-f">x</div></div>`))
		}
	})
	mux.HandleFunc("GET /folders/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Query().Get("held") != "2" {
			http.NotFound(w, r)
			return
		}
		if len(f.posts) > 0 {
			f.reads++
			if f.actAfter >= 0 && f.reads > f.actAfter {
				for _, r := range f.rows {
					if r.state == "waiting" {
						r.state = "gone"
					}
				}
			}
		}
		w.Write([]byte(f.page()))
	})
	mux.HandleFunc("POST /folders/1/resolve", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()
		f.posts = append(f.posts, r.PostForm)
		if act := r.FormValue("action"); act != "" {
			for _, row := range f.rows {
				if row.state == "open" {
					row.state = "waiting"
				}
			}
		}
		for i, v := range r.PostForm["r"] {
			if r.PostForm["a"][i] == "" {
				continue
			}
			p := strings.SplitN(v, "\t", 4)[3]
			for _, row := range f.rows {
				if row.path == p && row.state == "open" {
					row.state = "waiting"
				}
			}
		}
		http.Redirect(w, r, "/folders/1", http.StatusSeeOther)
	})
	return mux
}

func runFake(t *testing.T, f *fakeServer, o resolveOpts) (int, string) {
	t.Helper()
	srv := httptest.NewTLSServer(f.handler())
	defer srv.Close()
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	rep := &report{Devices: []device{{ID: "2", Name: "win", Online: true}}, Notices: parseNotices(bellHTML)}
	var b bytes.Buffer
	code := runResolve(c, srv.URL, rep, o, &b)
	return code, b.String()
}

func newFake(paths ...string) *fakeServer {
	f := &fakeServer{}
	for _, p := range paths {
		f.rows = append(f.rows, &fakeRow{path: p, state: "open"})
	}
	return f
}

func opts() resolveOpts {
	resolvePoll = time.Millisecond
	return resolveOpts{On: true, Folder: "projects", Device: "win", Scope: "d:PROJ-X", Action: "discard", Save: true, Wait: time.Second}
}

func TestResolveDryRun(t *testing.T) {
	f := newFake("proj-x/index")
	code, out := runFake(t, f, opts())
	if code != 0 || len(f.posts) != 0 || !strings.Contains(out, "dry run") || !strings.Contains(out, "- proj-x/index") || !strings.Contains(out, "exclusive to wsl-projects") {
		t.Errorf("code %d posts %v\n%s", code, f.posts, out)
	}
}

func TestResolveScope(t *testing.T) {
	f := newFake("proj-x/a", "proj-x/b")
	f.actAfter = 1
	o := opts()
	o.Yes = true
	o.Scope = "/proj-x" // the path as the dialog shows it
	code, out := runFake(t, f, o)
	if code != 0 || len(f.posts) != 1 {
		t.Fatalf("code %d posts %v\n%s", code, f.posts, out)
	}
	p := f.posts[0]
	if p.Get("action") != "discard" || p.Get("save") != "1" || p.Get("scope") != "d:PROJ-X" || p.Get("seen") != "775117" || p.Get("device") != "2" || len(p["r"]) != 0 {
		t.Errorf("form %v", p)
	}
	if !strings.Contains(out, "gone from the dialog") || !strings.Contains(out, "bell: 0 error(s), 0 warning(s)") {
		t.Errorf("output:\n%s", out)
	}
	// -save=false sends no save field; overwrite is a whole-scope action too
	f = newFake("proj-x/a")
	o.Save, o.Action = false, "overwrite"
	if code, out := runFake(t, f, o); code != 0 || f.posts[0].Get("save") != "" || f.posts[0].Get("action") != "overwrite" {
		t.Errorf("code %d form %v\n%s", code, f.posts, out)
	}
}

func TestResolveRows(t *testing.T) {
	f := newFake("proj-x/a", "proj-x/dir/b", "proj-x/dir2/c")
	o := opts()
	o.Yes, o.Scope, o.Path = true, "", "proj-x/dir"
	o.Wait = 20 * time.Millisecond // the device never acts: the other rows stay open and the chosen one waits
	f.actAfter = -1
	code, out := runFake(t, f, o)
	if code != 1 || len(f.posts) != 1 {
		t.Fatalf("code %d posts %v\n%s", code, f.posts, out)
	}
	p := f.posts[0]
	if len(p["r"]) != 1 || !strings.HasSuffix(p["r"][0], "\tproj-x/dir/b") || p["a"][0] != "discard" || p.Get("action") != "" {
		t.Errorf("form %v (dir2 must not match dir)", p)
	}
	if !strings.Contains(out, "decision accepted, the device has not carried it out yet") {
		t.Errorf("output:\n%s", out)
	}
	if f.rows[0].state != "open" || f.rows[2].state != "open" || f.rows[1].state != "waiting" {
		t.Errorf("states %s %s %s", f.rows[0].state, f.rows[1].state, f.rows[2].state)
	}
}

func TestResolveRefusals(t *testing.T) {
	cases := []struct {
		name string
		edit func(*resolveOpts, *fakeServer)
		want string
		code int
	}{
		{"no action", func(o *resolveOpts, _ *fakeServer) { o.Action = "" }, "needs -folder, -device and -action", 2},
		{"no scope or path", func(o *resolveOpts, _ *fakeServer) { o.Scope = "" }, "never decides everything", 2},
		{"root path", func(o *resolveOpts, _ *fakeServer) { o.Scope, o.Path = "", "/" }, "would select every held change", 2},
		{"bad action", func(o *resolveOpts, _ *fakeServer) { o.Action = "delete" }, "must be discard, overwrite or upload", 2},
		{"upload of a scope", func(o *resolveOpts, _ *fakeServer) { o.Action = "upload" }, "row by row", 2},
		{"unknown folder", func(o *resolveOpts, _ *fakeServer) { o.Folder = "nope" }, `no folder named "nope"`, 2},
		{"unknown device", func(o *resolveOpts, _ *fakeServer) { o.Device = "ghost" }, `no device "ghost"`, 2},
		{"other scope", func(o *resolveOpts, _ *fakeServer) { o.Scope = "d:OTHER" }, "nothing matches", 1},
		{"upload not possible", func(o *resolveOpts, f *fakeServer) {
			o.Scope, o.Path, o.Action = "", "proj-x/a", "upload"
			f.rows[0].noUpload = true
		}, "upload is not possible for proj-x/a", 2},
		{"folded group by path", func(o *resolveOpts, f *fakeServer) {
			o.Scope, o.Path = "", "proj-x/a"
			f.folded = true
		}, "use -scope for the whole scope", 2},
	}
	for _, tc := range cases {
		f := newFake("proj-x/a")
		o := opts()
		o.Yes = true
		tc.edit(&o, f)
		code, out := runFake(t, f, o)
		if code != tc.code || !strings.Contains(out, tc.want) || len(f.posts) != 0 {
			t.Errorf("%s: code %d posts %v\n%s", tc.name, code, f.posts, out)
		}
	}
}

func TestResolveFoldedScope(t *testing.T) {
	f := newFake("proj-x/a")
	f.folded = true
	f.actAfter = 0
	o := opts()
	o.Yes = true
	if code, out := runFake(t, f, o); code != 0 || f.posts[0].Get("action") != "discard" {
		t.Errorf("code %d form %v\n%s", code, f.posts, out)
	}
}
