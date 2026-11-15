package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

// Only the throw-away server and its generated test data are used; no client process is started.
func testWeb(t *testing.T) *driver {
	t.Helper()
	base, _, password, stop := localServer(t.TempDir())
	t.Cleanup(stop)
	d := &driver{web: newWeb(base)}
	t.Cleanup(d.web.hc.CloseIdleConnections)
	if st, err := d.web.login(password); err != nil || st != http.StatusSeeOther {
		t.Fatalf("test login: %d %v", st, err)
	}
	return d
}

func testAPI(t *testing.T, d *driver, token, path string, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", d.web.base+"/api/v1/"+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(proto.VersionHeader, strconv.Itoa(proto.AgentVersion))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := d.web.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("test API %s: %d", path, resp.StatusCode)
	}
	return out
}

func testDevice(t *testing.T, d *driver, name string) proto.EnrollResp {
	t.Helper()
	code, err := d.web.addDevice(name)
	if err != nil {
		t.Fatal(err)
	}
	var dev proto.EnrollResp
	if err := json.Unmarshal(testAPI(t, d, "", "enroll", proto.EnrollReq{Code: code, OS: "linux"}), &dev); err != nil {
		t.Fatal(err)
	}
	return dev
}

func testFolder(t *testing.T, d *driver, name string) string {
	t.Helper()
	st, _, h, err := d.web.do("POST", "/folders/new", url.Values{"name": {name}}, d.web.base)
	if err != nil || st != 303 {
		t.Fatalf("new folder: %d %v", st, err)
	}
	return strings.TrimPrefix(h.Get("Location"), "/folders/")
}

func TestIDOfRenderedOverview(t *testing.T) {
	d := testWeb(t)
	a := testDevice(t, d, "wct-a")
	b := testDevice(t, d, "wct-b")
	folder := testFolder(t, d, "wct-docs & notes")
	for _, tc := range []struct{ kind, name, id string }{
		{"devices", "wct-a", strconv.FormatInt(a.DeviceID, 10)},
		{"devices", "wct-b", strconv.FormatInt(b.DeviceID, 10)},
		{"folders", "wct-docs & notes", folder},
	} {
		if got, err := d.web.idOf(tc.kind, tc.name); err != nil || got != tc.id {
			t.Errorf("idOf(%q, %q) = %q, %v; want %s", tc.kind, tc.name, got, err, tc.id)
		}
		links, err := d.listLinks(tc.kind)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, link := range links {
			if link.id == tc.id && link.name == tc.name {
				found = true
			}
		}
		if !found {
			t.Errorf("listLinks(%s) lost %q", tc.kind, tc.name)
		}
	}
	if _, err := d.web.idOf("devices", "missing"); err == nil {
		t.Error("missing device accepted")
	}
	_, page, err := d.web.get("/")
	if err != nil {
		t.Fatal(err)
	}
	row := overviewRow(page, "devices", strconv.FormatInt(b.DeviceID, 10))
	if !strings.Contains(row, ">wct-b</a>") || strings.Contains(row, ">wct-a</a>") {
		t.Error("wrong overview row")
	}
}

func TestRenderedFolderParsing(t *testing.T) {
	d := testWeb(t)
	dev := testDevice(t, d, "wct-a")
	id := strconv.FormatInt(dev.DeviceID, 10)
	folder := testFolder(t, d, "wct-files")
	fid, _ := strconv.ParseInt(folder, 10, 64)
	if err := d.web.mapFolder(id, folder, "/test/wct-files", true); err != nil {
		t.Fatal(err)
	}
	// An empty file needs no blocks and exercises the actual file-row template.
	hash := proto.Hash(sha256.Sum256(nil))
	name := "a & b.txt"
	testAPI(t, d, dev.Token, "commit", proto.CommitReq{Folder: fid, Ops: []proto.Op{{Path: name, Hash: &hash, MtimeNs: time.Now().UnixNano()}}})
	page, err := d.web.folderPage(folder, "")
	if err != nil {
		t.Fatal(err)
	}
	if fileRow(page, name) == "" || fileRow(page, "missing") != "" {
		t.Error("file row lookup failed")
	}
	if err := d.waitFile(folder, "", name, "0 B", time.Second); err != nil {
		t.Fatal(err)
	}
	testAPI(t, d, dev.Token, "poll", proto.PollReq{Folders: []proto.FolderState{{ID: fid, State: "error", Errors: []proto.FileError{
		{Path: "bad", Kind: proto.KindIO, Message: "test error", Since: time.Now().Unix()},
	}}}})
	if errs, _, err := d.summary("wct-files"); err != nil || errs != "1" {
		t.Errorf("summary = %q, %v", errs, err)
	}
	_, settings, err := d.web.get("/settings")
	if err != nil {
		t.Fatal(err)
	}
	if d.rulesText(settings) == "" {
		t.Error("global rules textarea not found")
	}
}
