package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

func TestOldClientRefusalBecomesUpgradeError(t *testing.T) {
	t.Parallel()
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get(proto.VersionHeader))
		http.Error(w, "client version 1 is too old, the server needs 9 or newer: upgrade kaze-syncc", http.StatusUpgradeRequired)
	}))
	defer ts.Close()
	api := &API{Base: ts.URL, Token: "t", HC: ts.Client()}
	ctx := context.Background()
	_, perr := api.Poll(ctx, proto.PollReq{})
	_, cerr := api.Commit(ctx, 1, []proto.Op{{Path: "a"}})
	_, ierr := api.Changes(ctx, 1, 0)
	_, gerr := api.GetBlocks(ctx, []proto.Hash{{}})
	for name, err := range map[string]error{"poll": perr, "commit": cerr, "changes": ierr, "blocks/get": gerr} {
		var ue *UpgradeError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "server needs 9 or newer") || errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, v := range seen {
		if v != strconv.Itoa(proto.AgentVersion) {
			t.Errorf("request sent version header %q", v)
		}
	}
	if len(seen) != 4 {
		t.Errorf("%d requests", len(seen))
	}
}

// The idle timer runs only while a Read waits: a slow consumer must not get the download cancelled.
// Over 100 ms (serial run): a consumer busy for 120 ms must outlast the 40 ms idle timer to prove it is not counted.
func TestIdleBodyDoesNotCountTheConsumersTime(t *testing.T) {
	old := idleTimeout
	idleTimeout = 40 * time.Millisecond
	defer func() { idleTimeout = old }()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("0123456789"))
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		w.Write([]byte("abcdefghij"))
	}))
	defer ts.Close()
	api := &API{Base: ts.URL, Token: "t", HC: ts.Client()}
	body, err := api.GetBlocks(context.Background(), []proto.Hash{{}})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	buf := make([]byte, 10)
	if _, err := io.ReadFull(body, buf); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond) // the consumer is busy writing to a slow disk
	if _, err := io.ReadFull(body, buf); err != nil || string(buf) != "abcdefghij" {
		t.Fatalf("read after the pause: %q %v", buf, err)
	}
}

// A server that sends the headers of a JSON reply and then stalls ends the call at the deadline.
func TestJSONBodyStallTimesOut(t *testing.T) {
	old := responseTimeout
	responseTimeout = 30 * time.Millisecond
	defer func() { responseTimeout = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	a := &API{Base: srv.URL, HC: srv.Client()}
	var out map[string]any
	start := time.Now()
	if err := a.json(context.Background(), "held", struct{}{}, &out); err == nil {
		t.Fatal("stalled body accepted")
	}
	if d := time.Since(start); d > 90*time.Millisecond {
		t.Errorf("took %v", d)
	}
}
