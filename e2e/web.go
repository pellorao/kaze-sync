package main

import (
	"crypto/tls"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
)

// webClient is a Web UI session. It never follows redirects, so tests see them.
type webClient struct {
	base string
	hc   *http.Client
}

func newWeb(base string) *webClient {
	jar, _ := cookiejar.New(nil)
	return &webClient{base: base, hc: &http.Client{
		Jar:           jar,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // the certificate is checked by the clients' enrollment, not here
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (w *webClient) do(method, path string, form url.Values, origin string) (int, string, http.Header, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, w.base+path, body)
	if err != nil {
		return 0, "", nil, err
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header, nil
}

func (w *webClient) get(path string) (int, string, error) {
	st, b, _, err := w.do("GET", path, nil, "")
	return st, b, err
}

// post sends a form with the server's own Origin, like a browser on the page.
func (w *webClient) post(path string, form url.Values) (int, string, error) {
	st, b, _, err := w.do("POST", path, form, w.base)
	return st, b, err
}

func (w *webClient) login(password string) (int, error) {
	st, _, err := w.post("/login", url.Values{"password": {password}})
	return st, err
}

// ok posts a form and demands a redirect or 200.
func (w *webClient) ok(path string, form url.Values) (string, error) {
	st, b, err := w.post(path, form)
	if err != nil {
		return "", err
	}
	if st != 200 && st != 303 {
		return "", fmt.Errorf("POST %s: %d %s", path, st, firstLine(b))
	}
	return b, nil
}

func firstLine(s string) string {
	s = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")[:min(200, len(strings.Join(strings.Fields(s), " ")))]
}

// idOf finds the id in the overview's link to a device or folder.
func (w *webClient) idOf(kind, name string) (string, error) {
	_, b, err := w.get("/")
	if err != nil {
		return "", err
	}
	m := regexp.MustCompile(`<a href="/` + kind + `/(\d+)">` + regexp.QuoteMeta(html.EscapeString(name)) + `</a>`).FindStringSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("no %s %q on the overview", kind, name)
	}
	return m[1], nil
}

// addDevice returns the enrollment code for a new device.
func (w *webClient) addDevice(name string) (string, error) {
	b, err := w.ok("/devices/add", url.Values{"name": {name}})
	if err != nil {
		return "", err
	}
	m := regexp.MustCompile(`<code>([A-Z2-7-]+)</code>`).FindStringSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("no code in the answer for device %s", name)
	}
	return m[1], nil
}

func (w *webClient) newFolder(name string) (string, error) {
	if _, err := w.ok("/folders/new", url.Values{"name": {name}}); err != nil {
		return "", err
	}
	return w.idOf("folders", name)
}

func (w *webClient) mapFolder(device, folder, path string, adopt bool) error {
	f := url.Values{"folder": {folder}, "path": {path}, "action": {"save"}}
	if adopt {
		f.Set("adopt", "1")
	}
	_, err := w.ok("/devices/"+device+"/mapping", f)
	return err
}

// folderPage returns the HTML of a folder page.
func (w *webClient) folderPage(id, query string) (string, error) {
	st, b, err := w.get("/folders/" + id + "?" + query)
	if err == nil && st != 200 {
		err = fmt.Errorf("folder page %s: %d", id, st)
	}
	return b, err
}

// fileRow returns the table row of a file name in the folder page, "" when absent.
func fileRow(page, name string) string {
	m := regexp.MustCompile(`(?s)<tr>\s*<td>` + regexp.QuoteMeta(html.EscapeString(name)) + `( <span[^>]*>\(已删除\)</span>)?</td>(.*?)</tr>`).FindStringSubmatch(page)
	if m == nil {
		return ""
	}
	return m[0]
}

// restoreDir restores a directory to a point in time (UTC) and returns the redirect's query: "restored=N&failed=M".
func (w *webClient) restoreDir(folder, dir, at string) (string, error) {
	st, b, h, err := w.do("POST", "/folders/"+folder+"/restore-dir", url.Values{"dir": {dir}, "time": {at}}, w.base)
	if err != nil {
		return "", err
	}
	if st != 303 {
		return "", fmt.Errorf("restore-dir: %d %s", st, firstLine(b))
	}
	loc := h.Get("Location")
	return loc[strings.Index(loc, "restored="):], nil
}
