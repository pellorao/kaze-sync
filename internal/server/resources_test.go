package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"kaze-sync/internal/proto"
	"kaze-sync/internal/resources"
)

func TestResourceEndpointSessionAndLatestReport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/"+VarDir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(dir+"/"+VarDir+"/meta.db", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = Migrate(db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.db.Exec(`INSERT INTO devices(id,name,os,token_hash,created_at,last_seen) VALUES(1,'metrics-test','linux','test',0,0)`); err != nil {
		t.Fatal(err)
	}
	s.sessions["test-session"] = time.Now().Add(time.Hour)
	request := func(auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/resources", nil)
		if auth {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "test-session"})
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := request(false); w.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated %d", w.Code)
	}
	c := resources.New()
	c.Observe(time.Now(), resources.Reading{RSS: 30_000_000, CPU: 1})
	var req proto.PollReq
	b, _ := json.Marshal(map[string]any{"resources": c.Days(time.Now())})
	if err = json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	s.pollStart(1, req)
	w := request(true)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response %d %s", w.Code, w.Body.String())
	}
	var out resourceResponse
	if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Devices) != 1 || out.Devices[0].OS != "linux" || len(out.Devices[0].Days) != 1 {
		t.Fatalf("response %s", w.Body.String())
	}
	// Resource changes alone must not refresh UI generation, and a legacy poll clears them.
	gen := s.uiGen
	req.Resources[0].CPUSeconds = 123
	s.pollStart(1, req)
	if s.uiGen != gen {
		t.Fatal("resource samples refresh UI")
	}
	s.pollStart(1, proto.PollReq{})
	if w = request(true); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Devices[0].Days) != 0 {
		t.Fatalf("legacy response %s", w.Body.String())
	}
}

func TestClientResourceRetentionBounds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	c := resources.New()
	c.Observe(now, resources.Reading{RSS: 30_000_000, CPU: 1})
	d := c.Days(now)[0]
	d.GCCycles = make([]resources.Cycle, 1000)
	got := clientResourceDays([]resources.Day{d, d, d}, now)
	if len(got) != 2 || got[0].GCCycles != nil {
		t.Fatal("unbounded device retention")
	}
	if len(clientResourceDays([]resources.Day{d}, now.AddDate(0, 0, 5))) != 0 {
		t.Fatal("stale device data")
	}
	d.CoverageSeconds = 86401
	if len(clientResourceDays([]resources.Day{d}, now)) != 0 {
		t.Fatal("invalid coverage")
	}
}
