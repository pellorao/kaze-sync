package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if runtime.GOOS == "windows" { // the server runs on OpenWrt only
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// env is a server on a temp data directory with a fake clock.
type env struct {
	t   *testing.T
	s   *Server
	clk *clock
	ts  *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(dir+"/"+VarDir, 0o755)
	db, err := OpenDB(dir+"/"+VarDir+"/meta.db", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	clk := &clock{t: time.Date(2030, 11, 1, 12, 0, 0, 0, time.UTC)}
	s, err := New(dir, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	s.PollTimeout = 5 * time.Millisecond
	ts := httptest.NewTLSServer(s.Handler())
	t.Cleanup(func() { ts.Close(); s.gcWG.Wait(); s.Close() }) // a GC started by a request still writes below dir
	return &env{t: t, s: s, clk: clk, ts: ts}
}

// block stores content in the block store and returns its hash.
func (e *env) block(content []byte) proto.Hash {
	e.t.Helper()
	h := proto.Hash(sha256.Sum256(content))
	if err := e.s.blobs.Put(h, bytes.NewReader(content)); err != nil {
		e.t.Fatal(err)
	}
	return h
}

// op builds an add op for content of at most one block, storing the block.
func (e *env) op(path string, base int64, content string) proto.Op {
	h := e.block([]byte(content))
	return proto.Op{Path: path, BaseSeq: base, Hash: &h, Blocks: oneBlock(content, h), Size: int64(len(content)), MtimeNs: 1}
}

func oneBlock(content string, h proto.Hash) []proto.Hash {
	if content == "" {
		return nil
	}
	return []proto.Hash{h}
}

func (e *env) folder(name string) int64 {
	e.t.Helper()
	id, err := e.s.createFolder(name)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) commit(folder int64, ops ...proto.Op) []proto.Result {
	e.t.Helper()
	res, err := e.s.commit(folder, "dev", ops)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// webClient is a browser-like client with a cookie jar.
func (e *env) webClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Transport: acceptLang{e.ts.Client().Transport, "zh-CN"}, Jar: jar}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// acceptLang gives every request the Accept-Language of a browser, unless the request has its own. The tests read the
// pages in the base language.
type acceptLang struct {
	base http.RoundTripper
	lang string
}

func (a acceptLang) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("Accept-Language") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("Accept-Language", a.lang)
	}
	return a.base.RoundTrip(r)
}

func (e *env) setAdminPassword(pw string) {
	h := sha(normalizeSecret(pw))
	if err := SetMeta(e.s.db, "admin_password_hash", hexOf(h[:])); err != nil {
		e.t.Fatal(err)
	}
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

func (e *env) login(c *http.Client, pw string) *http.Response {
	e.t.Helper()
	resp, err := c.PostForm(e.ts.URL+"/login", url.Values{"password": {pw}})
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func (e *env) post(c *http.Client, path string, form url.Values) (int, string) {
	e.t.Helper()
	resp, err := c.PostForm(e.ts.URL+path, form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *env) get(c *http.Client, path string) (int, string) {
	e.t.Helper()
	resp, err := c.Get(e.ts.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// api sends a device API request.
func (e *env) api(token, path string, body any) (int, []byte) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/"+path, bytes.NewReader(b))
	req.Header.Set(proto.VersionHeader, strconv.Itoa(proto.AgentVersion))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// enrollDevice registers a device directly and returns its id and token.
func (e *env) enrollDevice(name, osName string) (int64, string) {
	e.t.Helper()
	code := "ABCD-EFGH-IJKL"
	e.s.codes[sha(normalizeSecret(code))] = pendingCode{name: name, expires: e.clk.Now().Add(codeLife)}
	status, out := e.api("", "enroll", proto.EnrollReq{Code: code, OS: osName})
	if status != 200 {
		e.t.Fatalf("enroll: %d %s", status, out)
	}
	var r proto.EnrollResp
	json.Unmarshal(out, &r)
	return r.DeviceID, r.Token
}

func (e *env) mapFolder(dev, folder int64, path string) {
	e.t.Helper()
	if _, err := e.s.db.Exec(`INSERT INTO mappings VALUES(?,?,?,0)`, dev, folder, path); err != nil {
		e.t.Fatal(err)
	}
}

func newBearerRequest(url, token string, body []byte) (*http.Request, error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err == nil {
		req.Header.Set(proto.VersionHeader, strconv.Itoa(proto.AgentVersion))
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, err
}
