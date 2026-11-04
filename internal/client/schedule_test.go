package client

import (
	"errors"
	"testing"
	"time"
)

var testConf = schedConf{Debounce: 2 * time.Second, MaxWait: 10 * time.Second, RetryAfter: 10 * time.Second, AuthRetry: time.Minute,
	ScanInterval: time.Hour, SlowRetry: []time.Duration{30 * time.Second}}

// The scan finds a file written 1 s ago (Stable 5 s), then a peer's news brings a pull-only task: the file must
// still be scanned when it becomes stable, not at the fixed retry time or the hourly scan.
func TestScheduleKeepsUnstableFilesAcrossAPullOnlyTask(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var s schedule
	s.nextFull = t0.Add(time.Hour)
	s.event(testConf, "d", t0.Add(1*time.Second)) // the write
	now := t0.Add(3 * time.Second)                // debounce over
	p, ok := s.take(now)
	if !ok || !p.scan || len(p.dirs) != 1 {
		t.Fatalf("plan %+v %v", p, ok)
	}
	stableAt := t0.Add(6 * time.Second)
	s.done(testConf, now, p, taskResult{retry: false, unstableAt: stableAt})
	if got := s.next(); !got.Equal(stableAt) {
		t.Fatalf("next %v, want the moment the file is stable (%v)", got.Sub(t0), stableAt.Sub(t0))
	}
	// the peer commits: a pull-only task before the retry is due
	s.pull = true
	now = t0.Add(4 * time.Second)
	p, ok = s.take(now)
	if !ok || p.scan || !p.pull {
		t.Fatalf("plan %+v %v", p, ok)
	}
	s.done(testConf, now, p, taskResult{unstableAt: stableAt}) // the folder still remembers the file
	if got := s.next(); !got.Equal(stableAt) {
		t.Fatalf("after the pull-only task next is %v, want %v", got.Sub(t0), stableAt.Sub(t0))
	}
	if _, ok := s.take(t0.Add(5 * time.Second)); ok {
		t.Fatal("work before the file is stable")
	}
	p, ok = s.take(stableAt)
	if !ok || !p.scan || !p.retry || p.dirs != nil {
		t.Fatalf("at the stable time: %+v %v; want a whole scan", p, ok)
	}
	s.done(testConf, stableAt, p, taskResult{}) // uploaded: nothing is unstable any more
	if got := s.next(); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("next %v, want only the periodic scan", got.Sub(t0))
	}
}

// A task that failed is retried at the retry time; a due unstable time must not make it spin.
func TestScheduleFailedTaskDoesNotSpinOnAnOldUnstableTime(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var s schedule
	s.full = true
	p, _ := s.take(t0)
	s.done(testConf, t0, p, taskResult{err: errors.New("boom"), unstableAt: t0.Add(-time.Second)})
	if got := s.next(); got.Before(t0.Add(testConf.RetryAfter)) {
		t.Fatalf("next %v: retry too early", got.Sub(t0))
	}
	if !s.pull && !s.full && p.pull {
		t.Fatal("the pull was not queued again")
	}
}

func TestScheduleSlowRetriesRunOutAfterTheirSteps(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var s schedule
	s.full = true
	p, _ := s.take(t0)
	s.done(testConf, t0, p, taskResult{slow: true})
	if got := s.retryAt.Sub(t0); got != 30*time.Second {
		t.Fatalf("first slow retry in %v", got)
	}
	now := s.retryAt
	p, ok := s.take(now)
	if !ok || !p.retry {
		t.Fatalf("plan %+v %v", p, ok)
	}
	s.done(testConf, now, p, taskResult{slow: true})
	if !s.retryAt.IsZero() {
		t.Fatalf("a retry beyond the schedule at %v", s.retryAt)
	}
}
