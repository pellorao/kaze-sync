package main

import (
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// resolveOpts is what -resolve was asked to decide.
type resolveOpts struct {
	On                                  bool
	Folder, Device, Scope, Path, Action string
	Save, Yes                           bool
	Wait                                time.Duration
}

// resolvePoll is how often the dialog is read again while waiting for the device to carry a decision out.
var resolvePoll = 2 * time.Second

// heldRow is a row of the decision dialog: one held change, or a folded directory of them.
type heldRow struct {
	Path      string
	Field     string // the value of the row's r field; "" when the row has no choice (a decision waits for the device)
	Dir       bool
	CanUpload bool
	Undecided bool
}

// heldGroup is one scope of the decision dialog (one form of the page).
type heldGroup struct {
	Scope     string
	Path      string // as the dialog shows it, with the leading slash
	Why       string
	Hidden    url.Values // the form's hidden fields: device, dir, scope, seen
	Rows      []heldRow
	Truncated bool // the dialog draws only some of the rows (folded by directory, or the first N)
	Waiting   bool // every change of the scope has a decision waiting for the device
}

var (
	heldFormRe  = `(?s)<form method="post" action="/folders/%s/resolve" class="held-grp">(.*?)</form>`
	hiddenRe    = regexp.MustCompile(`<input type="hidden" name="(\w+)" value="([^"]*)">`)
	heldHeadRe  = regexp.MustCompile(`(?s)<div class="held-h"><span class="mono">(.*?)</span><span class="muted">(.*?)</span>`)
	heldRowRe   = regexp.MustCompile(`(?s)<tr( class="dir")?><td class="sel">(.*?)</tr>`)
	rowPathRe   = regexp.MustCompile(`(?s)<td class="mono">(.*?)</td>`)
	rowFieldRe  = regexp.MustCompile(`name="r" value="([^"]*)"`)
	rowUpDisRe  = regexp.MustCompile(`<option value="upload" disabled>`)
	rowDirAct   = regexp.MustCompile(`name="dr" value=`)
	smallNoteRe = regexp.MustCompile(`<p class="muted small">`)
)

// parseHeld reads the decision dialog of a folder page.
func parseHeld(page, folder string) []heldGroup {
	var out []heldGroup
	for _, fm := range regexp.MustCompile(fmt.Sprintf(heldFormRe, regexp.QuoteMeta(folder))).FindAllStringSubmatch(page, -1) {
		body := fm[1]
		head, rest := body, ""
		if i := strings.Index(body, `<details class="disc">`); i >= 0 {
			head, rest = body[:i], body[i:]
		}
		g := heldGroup{Hidden: url.Values{}, Waiting: strings.Contains(head, "data-waiting")}
		for _, h := range hiddenRe.FindAllStringSubmatch(head, -1) {
			g.Hidden.Set(h[1], html.UnescapeString(h[2]))
		}
		g.Scope = g.Hidden.Get("scope")
		if m := heldHeadRe.FindStringSubmatch(head); m != nil {
			g.Path, g.Why = plain(m[1]), plain(m[2])
		}
		for _, rm := range heldRowRe.FindAllStringSubmatch(rest, -1) {
			r := heldRow{Dir: rm[1] != ""}
			r.Path = strings.TrimSuffix(plain(firstGroup(rowPathRe, rm[2])), "/")
			r.Field = html.UnescapeString(firstGroup(rowFieldRe, rm[2]))
			r.Undecided = r.Field != "" || rowDirAct.MatchString(rm[2])
			r.CanUpload = !r.Dir && !rowUpDisRe.MatchString(rm[2])
			if r.Dir {
				g.Truncated = true
			}
			g.Rows = append(g.Rows, r)
		}
		if smallNoteRe.MatchString(rest) {
			g.Truncated = true
		}
		out = append(out, g)
	}
	return out
}

func (g heldGroup) matchesScope(want string) bool {
	trim := func(s string) string { return strings.Trim(s, "/") }
	return g.Scope == want || strings.EqualFold(trim(g.Path), trim(want)) && trim(want) != ""
}

// under reports whether path p is dir or lies under it; dir "" is everything.
func under(p, dir string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
}

// plan is one form to submit: the whole scope (rows nil) or the listed rows.
type plan struct {
	g    heldGroup
	rows []heldRow
}

func (o resolveOpts) validate() error {
	switch {
	case o.Folder == "" || o.Device == "" || o.Action == "":
		return errors.New("-resolve needs -folder, -device and -action")
	case o.Scope == "" && o.Path == "":
		return errors.New("-resolve needs -scope or -path (it never decides everything a device holds)")
	case o.Scope == "" && strings.Trim(o.Path, "/") == "":
		return errors.New("-path / would select every held change: give a path below the root, or -scope")
	case o.Action != "discard" && o.Action != "overwrite" && o.Action != "upload":
		return errors.New("-action must be discard, overwrite or upload")
	case o.Action == "upload" && o.Path == "":
		return errors.New("-action upload is decided row by row: give -path (the dialog has no whole-scope upload)")
	}
	return nil
}

func isNumber(s string) bool {
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

// lookup finds the numeric id of a folder (by the names the bell shows) and of a device (by name).
func (o resolveOpts) lookup(rep *report) (folder string, dev device, err error) {
	switch {
	case isNumber(o.Folder):
		folder = o.Folder
	default:
		for _, n := range rep.Notices {
			if n.Group == o.Folder && n.FolderID != "" {
				folder = n.FolderID
			}
		}
		if folder == "" {
			return "", dev, fmt.Errorf("no folder named %q among the bell's notices: give its numeric id", o.Folder)
		}
	}
	for _, d := range rep.Devices {
		if d.Name == o.Device || d.ID == o.Device {
			dev = d
		}
	}
	if dev.ID == "" {
		return "", dev, fmt.Errorf("no device %q on the overview", o.Device)
	}
	return folder, dev, nil
}

// choose picks what the options select out of the dialog's groups.
func (o resolveOpts) choose(groups []heldGroup) ([]plan, error) {
	var out []plan
	for _, g := range groups {
		if o.Scope != "" && !g.matchesScope(o.Scope) {
			continue
		}
		if o.Path == "" {
			if g.Waiting {
				continue // every change already has a decision
			}
			out = append(out, plan{g: g})
			continue
		}
		var rows []heldRow
		dir := strings.Trim(o.Path, "/")
		if gp := strings.Trim(g.Path, "/"); g.Truncated && (under(dir, gp) || under(gp, dir)) {
			return nil, fmt.Errorf("the dialog shows only some of the changes in %s (folded or cut): use -scope for the whole scope", g.Path)
		}
		for _, r := range g.Rows {
			if r.Undecided && !r.Dir && under(r.Path, dir) {
				rows = append(rows, r)
			}
		}
		if len(rows) == 0 {
			continue
		}
		if o.Action == "upload" {
			for _, r := range rows {
				if !r.CanUpload {
					return nil, fmt.Errorf("upload is not possible for %s (the server changed it or the type differs): use discard or overwrite", r.Path)
				}
			}
		}
		out = append(out, plan{g: g, rows: rows})
	}
	return out, nil
}

func (o resolveOpts) form(p plan) url.Values {
	v := url.Values{}
	for k, vs := range p.g.Hidden {
		v[k] = append([]string(nil), vs...)
	}
	if p.rows == nil {
		v.Set("action", o.Action)
	} else {
		for _, r := range p.rows {
			v.Add("r", r.Field)
			v.Add("a", o.Action)
		}
	}
	if o.Save && o.Action == "discard" {
		v.Set("save", "1")
	}
	return v
}

func describe(w io.Writer, o resolveOpts, p plan) {
	g := p.g
	fmt.Fprintf(w, "  scope %s (%s): %s\n", g.Scope, g.Path, g.Why)
	what := "every held change in the scope that the server has not changed since the dialog was read"
	rows := g.Rows
	if p.rows != nil {
		what, rows = fmt.Sprintf("%d held change(s) chosen by -path", len(p.rows)), p.rows
	}
	act := o.Action
	if o.Action == "discard" {
		act += map[bool]string{true: " (the local copy goes to .syncsaved)", false: " (the local copy is deleted)"}[o.Save]
	}
	fmt.Fprintf(w, "    %s: %s\n", act, what)
	for i, r := range rows {
		if i == 20 {
			fmt.Fprintf(w, "    ... and %d more\n", len(rows)-i)
			break
		}
		fmt.Fprintf(w, "    - %s\n", r.Path)
	}
}

func pageOf(c *http.Client, u string) (string, error) {
	b, code, err := get(c, u)
	if err != nil || code != 200 {
		return "", fmt.Errorf("%s: status %d, %v", u, code, err)
	}
	return string(b), nil
}

// state of a submitted plan in a fresh dialog: gone, waiting (decided, the device has not carried it out) or open.
func (o resolveOpts) state(p plan, groups []heldGroup) string {
	var g *heldGroup
	for i := range groups {
		if groups[i].Scope == p.g.Scope {
			g = &groups[i]
		}
	}
	if g == nil {
		return "gone"
	}
	if p.rows == nil {
		if g.Waiting {
			return "waiting"
		}
		return "open"
	}
	state := "gone"
	for _, want := range p.rows {
		for _, r := range g.Rows {
			if r.Path != want.Path {
				continue
			}
			if r.Undecided {
				return "open"
			}
			state = "waiting"
		}
	}
	return state
}

// runResolve decides held changes of one folder and device. Without -yes it only says what it would do. With it, it
// submits the decision as the dialog's form does, then reads the dialog and the bell again (up to -wait) to see the
// entries go. The exit status is 0 when they are gone (or nothing was asked to be submitted), 1 when something
// matched nothing or is still there, 2 when the server refused.
func runResolve(c *http.Client, base string, rep *report, o resolveOpts, w io.Writer) int {
	if err := o.validate(); err != nil {
		fmt.Fprintln(w, "adminstatus:", err)
		return 2
	}
	folder, dev, err := o.lookup(rep)
	if err != nil {
		fmt.Fprintln(w, "adminstatus:", err)
		return 2
	}
	pageURL := base + "/folders/" + folder + "?held=" + dev.ID
	page, err := pageOf(c, pageURL)
	if err != nil {
		fmt.Fprintln(w, "adminstatus:", err)
		return 2
	}
	groups := parseHeld(page, folder)
	plans, err := o.choose(groups)
	if err != nil {
		fmt.Fprintln(w, "adminstatus:", err)
		return 2
	}
	fmt.Fprintf(w, "folder %s, device %s (id %s, online %v): %d held scope(s) in the dialog\n", folder, dev.Name, dev.ID, dev.Online, len(groups))
	if len(plans) == 0 {
		for _, g := range groups {
			fmt.Fprintf(w, "  scope %s (%s): %s%s\n", g.Scope, g.Path, g.Why, map[bool]string{true: " [decision waiting for the device]"}[g.Waiting])
		}
		fmt.Fprintln(w, "nothing matches -scope/-path (or it is already decided)")
		return 1
	}
	for _, p := range plans {
		describe(w, o, p)
	}
	if !o.Yes {
		fmt.Fprintln(w, "dry run: nothing submitted; add -yes to submit")
		return 0
	}
	for _, p := range plans {
		req, _ := http.NewRequest("POST", base+"/folders/"+folder+"/resolve", strings.NewReader(o.form(p).Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept-Language", "en")
		resp, err := c.Do(req)
		if err != nil {
			fmt.Fprintln(w, "adminstatus:", err)
			return 2
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			fmt.Fprintf(w, "adminstatus: the server refused the decision for %s: status %d\n", p.g.Scope, resp.StatusCode)
			return 2
		}
	}
	fmt.Fprintln(w, "submitted; waiting for the device to carry it out")
	deadline := time.Now().Add(o.Wait)
	for {
		states := make([]string, len(plans))
		page, err := pageOf(c, pageURL)
		if err != nil {
			fmt.Fprintln(w, "adminstatus:", err)
			return 2
		}
		left := 0
		after := parseHeld(page, folder)
		for i, p := range plans {
			if states[i] = o.state(p, after); states[i] != "gone" {
				left++
			}
		}
		if left == 0 || !time.Now().Before(deadline) {
			for i, p := range plans {
				fmt.Fprintf(w, "  scope %s: %s\n", p.g.Scope, map[string]string{"gone": "gone from the dialog", "waiting": "decision accepted, the device has not carried it out yet", "open": "still open: the server did not take the decision"}[states[i]])
			}
			if ov, err := pageOf(c, base+"/"); err == nil {
				printNotices(w, parseNotices(ov))
			}
			if left > 0 {
				return 1
			}
			return 0
		}
		time.Sleep(resolvePoll)
	}
}
