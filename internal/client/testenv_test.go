package client

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
	"kaze-sync/internal/server"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// cenv is a real server in the process plus an admin session on its Web UI.
type cenv struct {
	t       *testing.T
	data    string
	srv     *server.Server
	ts      *httptest.Server
	admin   *http.Client
	fp      string
	folders int64
	devices int
	sdb     *sql.DB // a second connection to the server's database, for assertions
}

func newCEnv(t *testing.T) *cenv {
	t.Helper()
	data := t.TempDir()
	db, err := server.OpenDB(filepath.Join(data, "meta.db"), true)
	if err != nil || server.Migrate(db) != nil {
		t.Fatal(err)
	}
	pw := sha256.Sum256([]byte("PW")) // passwords are compared upper-cased
	server.SetMeta(db, "admin_password_hash", hex.EncodeToString(pw[:]))
	db.Close()
	s, err := server.New(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewTLSServer(s.Handler())
	jar, _ := cookiejar.New(nil)
	admin := ts.Client()
	admin.Jar = jar
	admin.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	sum := sha256.Sum256(ts.Certificate().Raw)
	sdb, err := server.OpenDB(filepath.Join(data, "meta.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	e := &cenv{t: t, data: data, srv: s, ts: ts, admin: admin, fp: "sha256:" + hex.EncodeToString(sum[:]), sdb: sdb}
	t.Cleanup(func() { sdb.Close(); ts.Close(); s.Close() })
	if st, _ := e.post("/login", url.Values{"password": {"PW"}}); st != 303 {
		t.Fatalf("admin login: %d", st)
	}
	return e
}

func (e *cenv) post(path string, form url.Values) (int, string) {
	e.t.Helper()
	resp, err := e.admin.PostForm(e.ts.URL+path, form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *cenv) newFolder(name string) int64 {
	e.t.Helper()
	if st, _ := e.post("/folders/new", url.Values{"name": {name}}); st != 303 {
		e.t.Fatalf("new folder: %d", st)
	}
	e.folders++
	return e.folders
}

// newDevice adds and enrolls a Linux device through the Web UI and the client's Enroll.
func (e *cenv) newDevice() (Device, *API) {
	e.t.Helper()
	dir := e.t.TempDir()
	if err := Enroll(context.Background(), dir, e.ts.URL, e.code(), e.fp); err != nil {
		e.t.Fatal(err)
	}
	dev, err := LoadDevice(dir)
	if err != nil {
		e.t.Fatal(err)
	}
	hc, err := NewHTTPClient(e.fp)
	if err != nil {
		e.t.Fatal(err)
	}
	return dev, &API{Base: dev.Server, Token: dev.Token, HC: hc}
}

// code adds a device in the Web UI and returns its enrollment code.
func (e *cenv) code() string {
	e.t.Helper()
	e.devices++
	st, body := e.post("/devices/add", url.Values{"name": {"dev" + strconv.Itoa(e.devices)}})
	m := regexp.MustCompile(`enroll \S+ ([A-Z2-7]{4}-[A-Z2-7]{4}-[A-Z2-7]{4})`).FindStringSubmatch(body)
	if st != 200 || m == nil {
		e.t.Fatalf("add device: %d\n%s", st, body)
	}
	return m[1]
}

func (e *cenv) mapFolder(dev Device, folder int64, path string, adopt bool) {
	e.t.Helper()
	form := url.Values{"folder": {strconv.FormatInt(folder, 10)}, "path": {path}, "action": {"save"}}
	if adopt {
		form.Set("adopt", "1")
	}
	if st, body := e.post("/devices/"+strconv.FormatInt(dev.DeviceID, 10)+"/mapping", form); st != 303 {
		e.t.Fatalf("map folder: %d %s", st, body)
	}
}

// serverFile returns the current content of a path rebuilt from the block store; ok is false for missing or deleted.
func (e *cenv) serverFile(folder int64, p string) (content []byte, ok bool) {
	e.t.Helper()
	var blocks []byte
	var hash []byte
	err := e.sdb.QueryRow(`SELECT hash, blocks FROM files WHERE folder_id=? AND path=?`, folder, p).Scan(&hash, &blocks)
	if err != nil || hash == nil {
		return nil, false
	}
	for i := 0; i < len(blocks); i += 32 {
		x := hex.EncodeToString(blocks[i : i+32])
		b, err := os.ReadFile(filepath.Join(e.data, "blobs", x[:2], x[2:4], x))
		if err != nil {
			e.t.Fatalf("block %s of %s missing: %v", x, p, err)
		}
		content = append(content, b...)
	}
	return content, true
}

func (e *cenv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.sdb.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// engine is a Folder over an in-memory file system and its own state database.
type engine struct {
	t      *testing.T
	fs     *cfs.Mem
	dbPath string
	f      *Folder
}

func (e *cenv) engine(api *API, id int64, rules []string, adopt bool) *engine {
	e.t.Helper()
	g := &engine{t: e.t, fs: cfs.NewMem(cfs.MemOptions{}), dbPath: filepath.Join(e.t.TempDir(), "folder.db")}
	g.restart(api, proto.FolderConfig{ID: id, Rules: rules, Adopt: adopt}, g.fs)
	return g
}

// restart rebuilds the Folder like a process restart would: same file system and database file.
func (g *engine) restart(api *API, fc proto.FolderConfig, fsys cfs.FS) {
	g.t.Helper()
	if g.f != nil {
		g.f.DB.Close()
	}
	db, err := openLocal(g.dbPath)
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { db.Close() })
	g.f = NewFolder(fc, fsys, db, api, false)
	g.f.Stable = 0
}

func (g *engine) scan() Report {
	g.t.Helper()
	rep, err := g.f.Scan(context.Background())
	if err != nil {
		g.t.Fatal(err)
	}
	return rep
}

// put writes (replacing) a file in the in-memory file system.
func (g *engine) put(p string, data []byte) {
	g.t.Helper()
	putFile(g.t, g.fs, p, data)
}

func putFile(t *testing.T, m cfs.FS, p string, data []byte) {
	t.Helper()
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		if err := m.MkdirAll(p[:i]); err != nil {
			t.Fatal(err)
		}
	}
	m.Remove(p)
	w, _, err := m.OpenAppend(p)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	w.Close()
}

// countTransport sums the bytes of block uploads.
type countTransport struct {
	base    http.RoundTripper
	packets atomic.Int64
	bytes   atomic.Int64
}

func (c *countTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if filepath.Base(r.URL.Path) == "blocks" {
		c.packets.Add(1)
		c.bytes.Add(r.ContentLength)
	}
	return c.base.RoundTrip(r)
}

// pseudoRandom returns n deterministic bytes that differ per seed.
func pseudoRandom(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}
