package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const overview = `<a class="logo" href="/">Kaze Sync <span class="ver">0.4.0</span></a>
<tbody>
<tr><td class="top"><span class="dot ok"></span><a class="dn" href="/devices/1">wsl &amp; one</a></td><td class="top hide-sm">linux</td>
<td class="top"><span class="st-ok">Online</span></td>
<td class="top"></td>
<td class="top hide-sm"><span class="num">0.4.0</span></td><td class="top hide-sm">ok</td>
</tr><tr><td class="top"><span class="dot"></span><a class="dn" href="/devices/2">win</a></td><td class="top hide-sm">windows</td>
<td class="top"><span class="muted num">2026-11-17 09:00</span></td>
<td class="top"><div class="status-lines"><div class="stl"><span class="f">x</span> <span class="st-ok">ok</span></div></div></td>
<td class="top hide-sm"><span class="num">0.3.0</span> <span class="st-err">too old</span></td><td class="top hide-sm">-</td>
</tr></tbody>`

func TestParseAndCheck(t *testing.T) {
	rep := parseOverview(overview)
	if rep.ServerVersion != "0.4.0" || len(rep.Devices) != 2 {
		t.Fatalf("%+v", rep)
	}
	a, b := rep.Devices[0], rep.Devices[1]
	if a.Name != "wsl & one" || !a.Online || a.Version != "0.4.0" || a.TooOld {
		t.Errorf("%+v", a)
	}
	if b.Online || b.LastSeen != "2026-11-17 09:00" || b.Version != "0.3.0" || !b.TooOld {
		t.Errorf("%+v", b)
	}
	check(rep, "0.4.0")
	if len(rep.Problems) != 3 { // offline, too old, wrong version
		t.Errorf("problems: %v", rep.Problems)
	}
}

func TestPasswordFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d", "pw")
	if err := storePassword(p, mustStdin(t, "s3cret\n")); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if pw, err := readPassword(p); err != nil || pw != "s3cret" {
		t.Errorf("%q %v", pw, err)
	}
	os.Chmod(p, 0o644)
	if _, err := readPassword(p); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("loose mode accepted or leaked: %v", err)
	}
}

func mustStdin(t *testing.T, s string) *os.File {
	f, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(s)
	f.Seek(0, 0)
	return f
}

func TestLogin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("password") != "pw" {
			http.Error(w, "no", 401)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "s", Value: "1", Path: "/"})
		http.Redirect(w, r, "/", 303)
	})
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if _, err := r.Cookie("s"); err != nil {
				http.Redirect(w, r, "/login", 303)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /{$}", authed(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(overview)) }))
	mux.HandleFunc("GET /push/subscriptions", authed(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"available":true,"mode":"ca","subs":[{"id":"a","browser":"Chrome","gone":true}]}`))
	}))
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{}
	srv.StartTLS()
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	fp := "sha256:" + hex.EncodeToString(sum[:])

	c, err := newClient(fp, "")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := collect(c, srv.URL, "pw")
	if err != nil || len(rep.Devices) != 2 || len(rep.Subs) != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
	check(rep, "")
	if !strings.Contains(strings.Join(rep.Problems, "|"), "invalid (gone)") {
		t.Errorf("gone sub not reported: %v", rep.Problems)
	}
	if _, err := collect(c, srv.URL, "bad"); err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Errorf("bad password: %v", err)
	}
	pemFile := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(pemFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	viaCA, err := newClient("", pemFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(viaCA, srv.URL, "pw"); err != nil {
		t.Errorf("trusting the certificate as CA: %v", err)
	}
	bad, _ := newClient("sha256:"+strings.Repeat("00", 32), "")
	if _, err := collect(bad, srv.URL, "pw"); err == nil {
		t.Error("wrong pin accepted")
	}
}
