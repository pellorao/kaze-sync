// Package ntfy pushes the few errors a user must know about to an ntfy server (design 4.3). It is off unless the
// private file ntfy.json names a server and a topic. The caller reports a problem on every failure and its clearing
// on every success; the Notifier decides what is worth a message: a problem is sent once it has failed `after`
// times in a row, then not again until it has cleared, and never more than once per Interval per problem; a cleared
// problem that was sent gets one "recovered" message. A global cap bounds what a bug can send. Messages are plain
// short texts the caller composes: no file contents, no paths beyond a folder name, no credentials.
package ntfy

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// File is the name of the private configuration file in the state directory (client) or the data directory (server).
const File = "ntfy.json"

// tunables
const (
	Interval   = time.Hour        // the least time between two messages about one problem
	globalCap  = 20               // messages per Interval over all problems
	postTimout = 10 * time.Second // one POST
)

// Config is the content of ntfy.json. Empty URL or Topic means disabled.
type Config struct {
	URL   string `json:"url"`   // the ntfy server, e.g. https://ntfy.example.org
	Topic string `json:"topic"` // the topic messages go to; it is a secret, it never appears in logs
}

type problem struct {
	n    int       // failures in a row
	sent bool      // a problem message is out and not yet cleared
	last time.Time // when the last problem message was sent
}

// Notifier is safe for concurrent use; a nil *Notifier does nothing.
type Notifier struct {
	endpoint string
	hc       *http.Client
	now      func() time.Time
	prefix   string // "[<device>]" or "[Server]" in every title

	mu    sync.Mutex
	probs map[string]*problem
	win   time.Time // start of the current cap window
	sent  int
}

// Load reads dir/ntfy.json. A missing or incomplete file gives a nil Notifier (disabled); a broken file is an error
// the caller logs, and also disables. name identifies the sender in titles (the device name, or "Server").
func Load(dir, name string) (*Notifier, error) {
	b, err := os.ReadFile(dir + string(os.PathSeparator) + File)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, errors.New(File + ": " + err.Error())
	}
	return New(c, name)
}

// New builds a Notifier; nil when c is empty.
func New(c Config, name string) (*Notifier, error) {
	c.URL, c.Topic = strings.TrimRight(strings.TrimSpace(c.URL), "/"), strings.Trim(strings.TrimSpace(c.Topic), "/")
	if c.URL == "" || c.Topic == "" {
		return nil, nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New(File + ": url must be http:// or https://")
	}
	return &Notifier{
		endpoint: c.URL + "/" + url.PathEscape(c.Topic), hc: &http.Client{Timeout: postTimout}, now: time.Now,
		prefix: "[" + name + "]", probs: map[string]*problem{},
	}, nil
}

// Fail records one failure of the problem key; msg says what is wrong (short, no paths or secrets). It sends when
// the problem has failed `after` times in a row and no message about it went out within Interval.
func (n *Notifier) Fail(key, msg string, after int) {
	if n == nil {
		return
	}
	n.mu.Lock()
	p := n.probs[key]
	if p == nil {
		p = &problem{}
		n.probs[key] = p
	}
	p.n++
	send := p.n >= after && !p.sent && (p.last.IsZero() || n.now().Sub(p.last) >= Interval) && n.allow()
	if send {
		p.sent, p.last = true, n.now()
	}
	n.mu.Unlock()
	if send {
		go n.post("Problem", "warning", msg)
	}
}

// Ok records that the problem key is gone; a problem that was sent gets a "recovered" message.
func (n *Notifier) Ok(key, msg string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	p := n.probs[key]
	if p == nil {
		n.mu.Unlock()
		return
	}
	was := p.sent
	p.n, p.sent = 0, false
	n.mu.Unlock()
	if was {
		go n.post("Recovered", "white_check_mark", msg)
	}
}

// allow counts a message against the global cap; callers hold mu.
func (n *Notifier) allow() bool {
	if now := n.now(); now.Sub(n.win) >= Interval {
		n.win, n.sent = now, 0
	}
	if n.sent >= globalCap {
		return false
	}
	n.sent++
	return true
}

func (n *Notifier) post(kind, tag, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), postTimout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", n.endpoint, strings.NewReader(msg))
	if err != nil {
		return
	}
	req.Header.Set("Title", n.prefix+" "+kind)
	req.Header.Set("Tags", tag)
	if kind == "Problem" {
		req.Header.Set("Priority", "high")
	}
	resp, err := n.hc.Do(req)
	if err != nil { // the URL is not logged: it may hold a secret
		slog.Warn("ntfy: cannot send the message")
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		slog.Warn("ntfy: server refused the message", "status", resp.StatusCode)
	}
}
