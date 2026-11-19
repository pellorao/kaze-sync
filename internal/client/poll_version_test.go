package client

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/proto"
	"kaze-sync/internal/version"
)

type versionTransport struct{ requests chan map[string]any }

func (v versionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	v.requests <- req
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestAgentPollReportsReleaseVersion(t *testing.T) {
	t.Parallel()
	requests := make(chan map[string]any, 1)
	a := &Agent{Dir: t.TempDir(), API: &API{Base: "https://test.invalid", Token: "test", HC: &http.Client{Transport: versionTransport{requests}}}, NewFS: cfs.OS}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case req := <-requests:
		if days, ok := req["resources"].([]any); !ok || len(days) != 1 {
			t.Errorf("resources = %v, want initial day summary", req["resources"])
		}
		if req["client_version"] != version.Version {
			t.Errorf("client_version = %v, want %q", req["client_version"], version.Version)
		}
		if req["agent_version"] != float64(proto.AgentVersion) {
			t.Errorf("agent_version = %v, want %d", req["agent_version"], proto.AgentVersion)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not poll")
	}
}
