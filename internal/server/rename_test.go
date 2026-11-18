package server

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

// TestRenameDevice: a rename checks the name like enrollment, keeps the token, announces the name in the
// configuration, moves the current rows to the new name and leaves older versions under the old one (design 2.5).
func TestRenameDevice(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	id, tok := e.enrollDevice("laptop", "linux")
	e.enrollDevice("desk", "linux")
	f := e.folder("Docs")
	e.mapFolder(id, f, "/home/u/Docs")
	if st, _ := e.post(c, "/devices/add", url.Values{"name": {"phone"}}); st != 200 { // a pending enrollment code
		t.Fatalf("add device: %d", st)
	}
	commit := func(path string, base int64, content string) int64 { // within the write window of the device's last write
		t.Helper()
		st, out := e.api(tok, "commit", proto.CommitReq{Folder: f, Ops: []proto.Op{e.op(path, base, content)}})
		var resp proto.CommitResp
		if err := json.Unmarshal(out, &resp); st != 200 || err != nil || len(resp.Results) != 1 || resp.Results[0].Status != "ok" {
			t.Fatalf("commit %s: %d %s", path, st, out)
		}
		return resp.Results[0].Seq
	}
	pollName := func() string {
		t.Helper()
		st, out := e.api(tok, "poll", proto.PollReq{ConfigVersion: -1})
		var resp proto.PollResp
		if err := json.Unmarshal(out, &resp); st != 200 || err != nil || resp.Config == nil {
			t.Fatalf("poll: %d %s", st, out)
		}
		return resp.Config.Name
	}
	commit("a.txt", commit("a.txt", 0, "v1"), "v2")
	if n := pollName(); n != "laptop" {
		t.Fatalf("config name %q", n)
	}
	page := "/devices/" + strconv.FormatInt(id, 10)
	ver := metaInt(e.s.db, "config_version")
	for _, tc := range []struct {
		name   string
		status int
		msg    string
	}{
		{"bad name!", 400, "设备名称"}, {"", 400, "设备名称"}, {strings.Repeat("x", 33), 400, "设备名称"},
		{"WEB", 400, ""}, {"desk", 409, "已有同名设备"}, {"phone", 409, "已有同名设备"},
	} {
		st, body := e.post(c, page+"/rename", url.Values{"name": {tc.name}})
		if st != tc.status || !strings.Contains(body, tc.msg) || !strings.Contains(body, `id="dlg-rename" class="dlg" data-autoopen`) {
			t.Fatalf("rename to %q: %d", tc.name, st)
		}
	}
	if st, _ := e.post(c, page+"/rename", url.Values{"name": {" laptop "}}); st != 303 || metaInt(e.s.db, "config_version") != ver {
		t.Fatalf("same name: %d, config version %d -> %d", st, ver, metaInt(e.s.db, "config_version"))
	}

	resp, err := c.PostForm(e.ts.URL+page+"/rename", url.Values{"name": {"laptop-2"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 303 || resp.Header.Get("Location") != page+"?ok=saved" {
		t.Fatalf("rename: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if v := metaInt(e.s.db, "config_version"); v != ver+1 {
		t.Fatalf("config version %d, want %d", v, ver+1)
	}
	if n := pollName(); n != "laptop-2" { // the same token still works and carries the new name
		t.Fatalf("config name after rename %q", n)
	}
	commit("b.txt", 0, "b") // the device's own write under the old name a moment ago is not another device's
	db, _ := e.s.folderDB(f)
	for q, want := range map[string]string{
		`SELECT device FROM files WHERE path='a.txt'`:    "laptop-2", // the current rows take the new name
		`SELECT device FROM files WHERE path='b.txt'`:    "laptop-2",
		`SELECT device FROM versions WHERE path='a.txt'`: "laptop", // old versions keep the name they were written under
	} {
		var dev string
		if err := db.QueryRow(q).Scan(&dev); err != nil || dev != want {
			t.Fatalf("%s: %q, want %q (%v)", q, dev, want, err)
		}
	}
	if _, body := e.get(c, page); !strings.Contains(body, `value="laptop-2"`) || !strings.Contains(body, "laptop-2") {
		t.Fatal("device page does not show the new name")
	}
	if st, _ := e.post(c, page+"/rename", url.Values{"name": {"Laptop-2"}}); st != 303 { // only the case changes
		t.Fatalf("case-only rename: %d", st)
	}
	// A commit authenticated under the old name that reaches the folder after the rename writes the current name.
	if res, err := e.s.commitFor(f, id, "laptop", []proto.Op{e.op("c.txt", 0, "c")}); err != nil || res[0].Status != "ok" {
		t.Fatalf("late commit: %v %+v", err, res)
	}
	var dev string
	if db.QueryRow(`SELECT device FROM files WHERE path='c.txt'`).Scan(&dev); dev != "Laptop-2" {
		t.Fatalf("late commit wrote %q", dev)
	}
	if st, _ := e.post(c, "/devices/add", url.Values{"name": {"laptop"}}); st != 200 { // the old name is free again
		t.Fatalf("add a device under the old name: %d", st)
	}
	if st, _ := e.post(c, "/devices/999/rename", url.Values{"name": {"x"}}); st != 404 {
		t.Fatalf("unknown device: %d", st)
	}
}
