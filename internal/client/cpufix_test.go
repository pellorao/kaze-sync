package client

import (
	"testing"
	"time"
)

// kaze-sync/cpufix: the findings of kaze-sync/cpureview.

// A partial scan that finds nothing unsent in what it covers clears the since, also when an earlier scan of the
// same place found a too-fresh file (design 2.8.6): otherwise the first edit after another device's write is frozen.
func TestPartialRetryClearsDirty(t *testing.T) {
	a, b := singlePair(t)
	a.put("d/x.txt", []byte("zero"))
	a.sync()
	b.sync()
	now := time.Unix(0, 5_000_000_000_000)
	a.f.Now = func() time.Time { return now }
	a.f.Stable = 5 * time.Second
	a.put("d/x.txt", []byte("one"))
	a.fs.SetMeta("d/x.txt", now.Add(-time.Second).UnixNano(), false)
	a.scanHint("d") // too fresh
	now = now.Add(10 * time.Second)
	a.scanHint("d") // the partial retry commits it
	b.sync()
	b.put("y.txt", []byte("from b"))
	b.sync()
	a.pull()
	now = now.Add(time.Minute)
	a.put("d/x.txt", []byte("two"))
	a.fs.SetMeta("d/x.txt", now.Add(-time.Minute).UnixNano(), false)
	a.scanHint("d")
	if len(a.f.frozen) != 0 {
		t.Errorf("frozen after pulling the other device's write: %v", a.f.frozen)
	}
}

// Unsent changes in another directory keep the since: a partial scan clears only what it covers.
func TestPartialScanKeepsDirtyOfOtherDirectories(t *testing.T) {
	a, _ := singlePair(t)
	a.put("d/x.txt", []byte("zero"))
	a.sync()
	now := time.Unix(0, 5_000_000_000_000)
	a.f.Now = func() time.Time { return now }
	a.f.Stable = 5 * time.Second
	a.put("d/x.txt", []byte("one"))
	a.fs.SetMeta("d/x.txt", now.Add(-time.Second).UnixNano(), false) // too fresh
	a.put("e/y.txt", []byte("new"))
	a.fs.SetMeta("e/y.txt", now.Add(-time.Minute).UnixNano(), false)
	a.scanHint() // whole: e/y.txt is sent (a since is recorded), d/x.txt is too fresh and still unsent
	if len(a.f.DB.metaPrefix("dirty:")) == 0 {
		t.Fatal("no dirty record while a file is unsent")
	}
	a.scanHint("e") // clean, but does not cover d
	if len(a.f.DB.metaPrefix("dirty:")) == 0 {
		t.Fatal("a scan of e cleared the since of d's unsent file")
	}
	now = now.Add(10 * time.Second)
	a.scanHint("d")
	if got := a.f.DB.metaPrefix("dirty:"); len(got) != 0 {
		t.Fatalf("dirty after d was sent: %v", got)
	}
}

// A retry waits for its time through tasks that do not take it (a pull for a poll notice, the scan of another
// directory's too-fresh file).
func TestScheduleRetrySurvivesPullOnlyTask(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var s schedule
	s.nextFull = t0.Add(time.Hour)
	s.done(testConf, t0, plan{scan: true, dirs: []string{"x"}}, taskResult{retry: true, retryDirs: []string{"y"}})
	s.pull = true
	p, _ := s.take(t0.Add(time.Second))
	s.done(testConf, t0.Add(time.Second), p, taskResult{})
	p, ok := s.take(t0.Add(testConf.RetryAfter + time.Second))
	if !ok || len(p.dirs) != 1 || p.dirs[0] != "y" {
		t.Fatalf("the retry of y is gone: %+v %v", p, ok)
	}
}

func TestScheduleRetrySurvivesUnstableScan(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var s schedule
	s.nextFull = t0.Add(time.Hour)
	s.done(testConf, t0, plan{scan: true, dirs: []string{"x"}}, taskResult{retry: true, retryDirs: []string{"y"}, unstableAt: t0.Add(2 * time.Second), unstableDirs: []string{"x"}})
	p, ok := s.take(t0.Add(2 * time.Second))
	if !ok || len(p.dirs) != 1 || p.dirs[0] != "x" {
		t.Fatalf("plan %+v %v", p, ok)
	}
	s.done(testConf, t0.Add(2*time.Second), p, taskResult{})
	p, ok = s.take(t0.Add(testConf.RetryAfter + time.Second))
	if !ok || len(p.dirs) != 1 || p.dirs[0] != "y" {
		t.Fatalf("the retry of y is gone: %+v %v", p, ok)
	}
}

// A slow retry (file in use) is not pushed back by the tasks between.
func TestScheduleSlowRetryNotPostponedByOtherTasks(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var s schedule
	s.nextFull = t0.Add(time.Hour)
	s.done(testConf, t0, plan{scan: true, dirs: []string{"x"}}, taskResult{slow: true, retryDirs: []string{"y"}})
	at := s.retryAt
	if at.IsZero() {
		t.Fatal("no slow retry")
	}
	for i := 1; i <= 3; i++ {
		now := t0.Add(time.Duration(i*10) * time.Second)
		if !now.Add(testConf.Debounce).Before(at) {
			break
		}
		s.event(testConf, "z", now)
		p, ok := s.take(now.Add(testConf.Debounce))
		if !ok {
			t.Fatal("no scan")
		}
		s.done(testConf, now, p, taskResult{slow: true})
		if !s.retryAt.Equal(at) {
			t.Fatalf("slow retry moved from %v to %v", at.Sub(t0), s.retryAt.Sub(t0))
		}
	}
}

// Only download waits back off; a file that changed while read is looked at again after the normal delay.
func TestScheduleBackoffOnlyForDownloadWaits(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var s schedule
	s.nextFull = t0.Add(time.Hour)
	now := t0
	for i := 0; i < 4; i++ {
		s.done(testConf, now, plan{pull: true}, taskResult{retry: true})
		now = s.retryAt
		p, _ := s.take(now)
		if !p.retry {
			t.Fatal("not a retry")
		}
	}
	s.done(testConf, now, plan{scan: true}, taskResult{retry: true, retryDirs: []string{"y"}})
	if got := s.retryAt.Sub(now); got != testConf.RetryAfter {
		t.Fatalf("a changed-while-read retry waits %v, want %v", got, testConf.RetryAfter)
	}
}
