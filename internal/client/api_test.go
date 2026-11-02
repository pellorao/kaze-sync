package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

func TestOldClientRefusalBecomesUpgradeError(t *testing.T) {
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
