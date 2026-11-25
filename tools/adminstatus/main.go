// Command adminstatus logs in to a kaze-syncd web UI as the administrator and prints the server's state: the
// server version, each device (online, last connection, client version, too-old warning) and the notification
// (Web Push) subscriptions, so an operator or an automation can check a deployment without a browser.
//
//	go run ./tools/adminstatus -url https://HOST:PORT [-fp sha256:HEX | -ca CA.pem] [-pwfile FILE] [-expect VERSION] [-json]
//	go run ./tools/adminstatus -setup           # store the password in the password file (read from stdin)
//	go run ./tools/adminstatus -rename OLD=NEW  # rename a device (design 2.5), then print the state as above
//
// The password is read only from the password file (default $XDG_CONFIG_HOME/kaze-sync/admin-password, which must
// be a regular file readable by its owner only) and is never printed, put on a command line or logged. -url, -fp
// and -ca default to $KAZE_STATUS_URL, $KAZE_STATUS_FP and $KAZE_STATUS_CA. -fp pins the server certificate, -ca
// trusts the local CA that signed it; without either the system trust store decides.
//
// Exit status: 0 all checks pass, 1 a check failed (the report says which), 2 cannot log in or read the server.
package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

type device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	OS       string `json:"os"`
	Online   bool   `json:"online"`
	LastSeen string `json:"last_seen,omitempty"` // as the page shows it, in the server's time zone
	Version  string `json:"version"`
	TooOld   bool   `json:"too_old"`
}

type sub struct {
	ID      string `json:"id"`
	Browser string `json:"browser"`
	Gone    bool   `json:"gone"`
	Last    *struct {
		Status int    `json:"status"`
		At     int64  `json:"at"`
		Err    string `json:"err"`
	} `json:"last"`
}

type report struct {
	ServerVersion string   `json:"server_version"`
	Devices       []device `json:"devices"`
	PushMode      string   `json:"push_mode"`
	PushAvailable bool     `json:"push_available"`
	Subs          []sub    `json:"subs"`
	Problems      []string `json:"problems"`
}

func main() {
	var (
		base   = flag.String("url", os.Getenv("KAZE_STATUS_URL"), "server URL, https://HOST:PORT")
		fp     = flag.String("fp", os.Getenv("KAZE_STATUS_FP"), "pin the server certificate: sha256:HEX")
		ca     = flag.String("ca", os.Getenv("KAZE_STATUS_CA"), "PEM file with the CA certificate that signed the server's (kaze-syncd ca --export)")
		pwfile = flag.String("pwfile", defaultPwFile(), "file holding the administrator password")
		expect = flag.String("expect", "", "every device and the server must show this version")
		asJSON = flag.Bool("json", false, "print JSON")
		setup  = flag.Bool("setup", false, "read the password from stdin and store it in -pwfile (mode 0600)")
		rename = flag.String("rename", "", "rename a device first: OLD=NEW")
	)
	flag.Parse()
	if *setup {
		if err := storePassword(*pwfile, os.Stdin); err != nil {
			fail(err)
		}
		fmt.Println("password stored in", *pwfile)
		return
	}
	if *base == "" {
		fail(errors.New("no server: give -url or set KAZE_STATUS_URL"))
	}
	if !strings.HasPrefix(strings.ToLower(*base), "https://") { // the password goes in the login form
		fail(errors.New("-url must start with https://: the password is sent in the login request"))
	}
	pw, err := readPassword(*pwfile)
	if err != nil {
		fail(err)
	}
	c, err := newClient(*fp, *ca)
	if err != nil {
		fail(err)
	}
	rep, err := collect(c, strings.TrimRight(*base, "/"), pw, *rename)
	if err != nil {
		fail(err)
	}
	check(rep, *expect)
	if *asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
	} else {
		printReport(rep)
	}
	if len(rep.Problems) > 0 {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "adminstatus:", err)
	os.Exit(2)
}

func defaultPwFile() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "kaze-sync", "admin-password")
}

// storePassword writes the first line of r to path with mode 0600. On a terminal the echo is switched off.
func storePassword(path string, r *os.File) error {
	if fi, err := r.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "Administrator password: ")
		off := exec.Command("stty", "-echo")
		off.Stdin = r
		if off.Run() == nil {
			defer func() {
				on := exec.Command("stty", "echo")
				on.Stdin = r
				on.Run()
				fmt.Fprintln(os.Stderr)
			}()
		}
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("no password on stdin")
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return errors.New("empty password")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	os.Remove(tmp) // a stale file could have a loose mode or be a link
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// readPassword reads the password file, refusing one that others can read or that is not a regular file.
func readPassword(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("password file %s: %v (create it with -setup)", path, errors.Unwrap(err))
	}
	if !fi.Mode().IsRegular() || (runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0) { // Windows has no such bits
		return "", fmt.Errorf("password file %s must be a regular file with mode 0600 (is %v)", path, fi.Mode())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	pw := strings.TrimSpace(string(b))
	if pw == "" {
		return "", fmt.Errorf("password file %s is empty", path)
	}
	return pw, nil
}

func newClient(fp, caFile string) (*http.Client, error) {
	jar, _ := cookiejar.New(nil)
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no certificate", caFile)
		}
	}
	if fp != "" {
		want, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(fp), "sha256:"))
		if err != nil || len(want) != sha256.Size {
			return nil, errors.New("-fp must be sha256:<64 hex digits>")
		}
		cfg.InsecureSkipVerify = true // replaced by the pin below
		cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			if got := sha256.Sum256(raw[0]); string(got[:]) != string(want) {
				return errors.New("server certificate does not match -fp")
			}
			return nil
		}
	}
	return &http.Client{
		Jar:       jar,
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // the login answers 303; the cookie is what we need
		},
	}, nil
}

func get(c *http.Client, u string) ([]byte, int, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept-Language", "en")
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return b, resp.StatusCode, err
}

// collect logs in and reads the state; with rename (OLD=NEW) it renames that device after reading the overview and
// reads the overview again.
func collect(c *http.Client, base, pw, rename string) (*report, error) {
	req, _ := http.NewRequest("POST", base+"/login", strings.NewReader(url.Values{"password": {pw}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept-Language", "en")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, errors.New("login refused: this IP is locked out after failed logins; wait and retry")
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("login refused: wrong password (the password file is stale; run -setup)")
	case resp.StatusCode != http.StatusSeeOther:
		return nil, fmt.Errorf("login: unexpected status %d", resp.StatusCode)
	}
	page, code, err := get(c, base+"/")
	if err != nil || code != 200 {
		return nil, fmt.Errorf("overview: status %d, %v", code, err)
	}
	rep := parseOverview(string(page))
	if rename != "" {
		if err := renameDevice(c, base, rep, rename); err != nil {
			return nil, err
		}
		if page, code, err = get(c, base+"/"); err != nil || code != 200 {
			return nil, fmt.Errorf("overview after the rename: status %d, %v", code, err)
		}
		rep = parseOverview(string(page))
	}
	if pb, code, err := get(c, base+"/push/subscriptions"); err == nil && code == 200 {
		var p struct {
			Available bool   `json:"available"`
			Mode      string `json:"mode"`
			Subs      []sub  `json:"subs"`
		}
		if err := json.Unmarshal(pb, &p); err != nil || p.Mode == "" {
			rep.Problems = append(rep.Problems, "/push/subscriptions answered something unreadable")
		} else {
			rep.PushAvailable, rep.PushMode, rep.Subs = p.Available, p.Mode, p.Subs
		}
	} else {
		rep.Problems = append(rep.Problems, fmt.Sprintf("cannot read /push/subscriptions (status %d)", code))
	}
	return rep, nil
}

// renameDevice posts the device page's rename form for the device named OLD in spec OLD=NEW.
func renameDevice(c *http.Client, base string, rep *report, spec string) error {
	old, name, ok := strings.Cut(spec, "=")
	if !ok || old == "" || name == "" {
		return errors.New("-rename wants OLD=NEW")
	}
	id := ""
	for _, d := range rep.Devices {
		if d.Name == old {
			id = d.ID
		}
	}
	if id == "" {
		return fmt.Errorf("rename: no device named %q on the overview", old)
	}
	req, _ := http.NewRequest("POST", base+"/devices/"+id+"/rename", strings.NewReader(url.Values{"name": {name}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept-Language", "en")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		msg := ""
		if m := fieldErrRe.FindSubmatch(b); m != nil {
			msg = ": " + html.UnescapeString(string(m[1]))
		}
		return fmt.Errorf("rename %s to %s refused: status %d%s", old, name, resp.StatusCode, msg)
	}
	fmt.Fprintf(os.Stderr, "renamed device %s (id %s) to %s\n", old, id, name)
	return nil
}

var (
	fieldErrRe = regexp.MustCompile(`<span class="err-msg">([^<]+)</span>`)
	verRe      = regexp.MustCompile(`<span class="ver">([^<]*)</span>`)
	rowRe      = regexp.MustCompile(`(?s)<tr><td class="top"><span class="dot( ok)?"></span><a class="dn" href="/devices/(\d+)">(.*?)</a></td><td class="top hide-sm">(.*?)</td>\s*<td class="top">(.*?)</td>.*?<td class="top hide-sm">(?:<span class="vline">)?<span class="num">(.*?)</span>(.*?)</td>`)
	seenRe     = regexp.MustCompile(`<span class="muted num">(.*?)</span>`)
)

// parseOverview reads the server version and the device table from the overview page.
func parseOverview(page string) *report {
	rep := &report{Devices: []device{}, Subs: []sub{}, Problems: []string{}}
	if m := verRe.FindStringSubmatch(page); m != nil {
		rep.ServerVersion = html.UnescapeString(m[1])
	}
	for _, m := range rowRe.FindAllStringSubmatch(page, -1) {
		d := device{ID: m[2], Name: html.UnescapeString(m[3]), OS: html.UnescapeString(m[4]), Online: m[1] != "", Version: html.UnescapeString(m[6]), TooOld: strings.Contains(m[7], "st-err")}
		if s := seenRe.FindStringSubmatch(m[5]); s != nil {
			d.LastSeen = html.UnescapeString(s[1])
		}
		rep.Devices = append(rep.Devices, d)
	}
	return rep
}

// check fills rep.Problems.
func check(rep *report, expect string) {
	if rep.ServerVersion == "" {
		rep.Problems = append(rep.Problems, "server version not found on the overview page (page layout changed?)")
	}
	if len(rep.Devices) == 0 {
		rep.Problems = append(rep.Problems, "no devices found")
	}
	if expect != "" && rep.ServerVersion != "" && strings.TrimPrefix(rep.ServerVersion, "v") != strings.TrimPrefix(expect, "v") {
		rep.Problems = append(rep.Problems, fmt.Sprintf("server version %s, expected %s", rep.ServerVersion, expect))
	}
	for _, d := range rep.Devices {
		if !d.Online {
			rep.Problems = append(rep.Problems, fmt.Sprintf("device %s is offline (last %s)", d.Name, d.LastSeen))
		}
		if d.TooOld {
			rep.Problems = append(rep.Problems, fmt.Sprintf("device %s client %s is flagged too old", d.Name, d.Version))
		}
		if expect != "" && strings.TrimPrefix(d.Version, "v") != strings.TrimPrefix(expect, "v") {
			rep.Problems = append(rep.Problems, fmt.Sprintf("device %s client version %s, expected %s", d.Name, d.Version, expect))
		}
	}
	// A self-signed certificate is a supported mode in which Web Push is off by design (design 4.3): shown, not a problem.
	for _, s := range rep.Subs {
		switch {
		case s.Gone:
			rep.Problems = append(rep.Problems, fmt.Sprintf("notification subscription %s (%s) is invalid (gone)", s.ID, s.Browser))
		case s.Last != nil && (s.Last.Status < 200 || s.Last.Status >= 300):
			rep.Problems = append(rep.Problems, fmt.Sprintf("notification subscription %s (%s): last push status %d %s", s.ID, s.Browser, s.Last.Status, s.Last.Err))
		}
	}
}

func printReport(rep *report) {
	fmt.Println("server version:", rep.ServerVersion)
	fmt.Printf("%-16s %-10s %-8s %-10s %s\n", "device", "os", "online", "version", "last connection")
	for _, d := range rep.Devices {
		fmt.Printf("%-16s %-10s %-8v %-10s %s\n", d.Name, d.OS, d.Online, d.Version, d.LastSeen)
	}
	fmt.Printf("notifications: mode=%s available=%v subscriptions=%d\n", rep.PushMode, rep.PushAvailable, len(rep.Subs))
	for _, s := range rep.Subs {
		last := "never pushed"
		if s.Last != nil {
			last = fmt.Sprintf("last push status %d at %s", s.Last.Status, time.Unix(s.Last.At, 0).Format("2006-01-02 15:04"))
		}
		fmt.Printf("  %s %s gone=%v %s\n", s.ID, s.Browser, s.Gone, last)
	}
	if len(rep.Problems) == 0 {
		fmt.Println("result: OK")
		return
	}
	fmt.Println("result: PROBLEMS")
	for _, p := range rep.Problems {
		fmt.Println("  -", p)
	}
}
