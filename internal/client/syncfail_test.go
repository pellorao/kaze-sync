package client

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

// A folder whose cycles fail failuresBeforeReport times in a row gets a folder error in the status the client sends,
// which the server turns into a Web Push (design 4.3); a success removes it, and an outage of the polls does not count.
func TestSyncFailuresAreReportedAfterRetries(t *testing.T) {
	a := &Agent{}
	r := &runner{}
	a.runners = map[int64]*runner{7: r}
	r.scanned = true
	boom := errors.New("write x: disk full")
	for range failuresBeforeReport - 1 {
		a.trackFailures(r, boom)
	}
	if st := a.states(); len(st[0].Errors) != 0 {
		t.Fatalf("reported too early: %+v", st)
	}
	a.trackFailures(r, boom)
	st := a.states()[0]
	if len(st.Errors) != 1 || st.Errors[0].Kind != proto.KindFolder || !strings.Contains(st.Errors[0].Message, "3 cycles in a row: write x: disk full") || st.State != "error" {
		t.Fatalf("status %+v", st)
	}
	a.trackFailures(r, context.Canceled) // a shutdown is not a failure
	if r.failN != failuresBeforeReport {
		t.Fatal("a cancel counted")
	}
	a.pollDown.Store(true)
	a.trackFailures(r, boom) // the server cannot be reached: the same outage
	if r.failN != failuresBeforeReport {
		t.Fatal("an outage counted")
	}
	a.pollDown.Store(false)
	a.trackFailures(r, nil)
	if st := a.states()[0]; len(st.Errors) != 0 || st.State == "error" {
		t.Fatalf("not cleared: %+v", st)
	}
}
