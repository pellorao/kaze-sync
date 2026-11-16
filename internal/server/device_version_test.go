package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

func TestDevicePagesShowReleaseVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/"+VarDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(dir+"/"+VarDir+"/meta.db", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	now := time.Now()
	s, err := New(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if rep := s.reports[1]; rep != nil && rep.offline != nil {
			rep.offline.Stop()
		}
		s.Close()
	}()
	if _, err := s.db.Exec(`INSERT INTO devices(id,name,os,token_hash,created_at,last_seen) VALUES(1,'release-test','linux','test',0,0)`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, release          string
		reported, old, offline bool
	}{
		{name: "not reported"},
		{name: "legacy", reported: true},
		{name: "release", reported: true, release: "1.2.3"},
		{name: "offline", reported: true, release: "1.2.3", offline: true},
		{name: "upgrade required", old: true, release: "1.2.3"},
		{name: "updated release", reported: true, release: "1.2.4"},
		{name: "legacy replaces release", reported: true},
	} {
		if tc.reported {
			req := map[string]any{"agent_version": 6, "config_version": -1}
			if tc.release != "" {
				req["client_version"] = tc.release
			}
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			var poll proto.PollReq
			if err := json.Unmarshal(body, &poll); err != nil {
				t.Fatal(err)
			}
			s.pollStart(1, poll)
			s.pollEnd(1)
		}
		if tc.old {
			s.noteTooOld(1, 1)
		}
		if tc.offline {
			now = now.Add(onlineWindow + time.Second)
		}
		want := tc.release
		if want == "" {
			want = "-"
		}
		for _, lang := range languages {
			for _, path := range []string{"/", "/devices/1"} {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				r.Header.Set("Accept-Language", lang.Code)
				r.SetPathValue("id", "1")
				w := httptest.NewRecorder()
				handler := s.overview
				if path != "/" {
					handler = s.devicePageH
				}
				if err := handler(w, r); err != nil {
					t.Fatal(err)
				}
				fragment := `<span class="num">` + want + `</span>`
				if path != "/" {
					fragment = dictOf(lang.Code)["dev.client"] + " " + want
				}
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), fragment) {
					t.Errorf("%s %s %s: missing %q", tc.name, lang.Code, path, fragment)
				}
				warning := `<span class="st-err">` + dictOf(lang.Code)["st.tooOld"] + `</span>`
				if strings.Contains(w.Body.String(), warning) != tc.old {
					t.Errorf("%s %s %s: unexpected upgrade warning", tc.name, lang.Code, path)
				}
				if strings.Contains(w.Body.String(), `<span class="num">6</span>`) || strings.Contains(w.Body.String(), `st-err">1`) {
					t.Errorf("%s %s %s: protocol version is visible", tc.name, lang.Code, path)
				}
			}
		}
	}
}
