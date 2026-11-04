package client

import "time"

// schedConf holds the tunables the schedule needs (copied from the Agent).
type schedConf struct {
	Debounce, MaxWait, RetryAfter, AuthRetry, ScanInterval time.Duration
	SlowRetry                                              []time.Duration
}

// schedule decides when one folder is scanned and pulled. It has no goroutines, timers or locks (the caller
// serialises access) and takes the time as an argument, so tests drive it with a fake clock. Every flag and
// wake-up time of a folder lives here: events, news and retries are recorded with the methods below, take
// returns the work due now, done records how it ended, and next says when to ask again.
type schedule struct {
	dirty      map[string]bool
	debounceAt time.Time
	firstAt    time.Time // when the oldest pending event arrived; zero when none is pending
	full       bool      // a full scan is due
	pull       bool
	reset      bool // the server went back in time: forget its seqs
	rewatch    bool // the watcher reported lost events: start a new one
	retryAt    time.Time
	unstableAt time.Time // when the youngest file the last scans found too fresh to upload becomes stable; no task clears it, only a scan that no longer sees such a file
	slowTries  int       // retries for files in use so far
	nextFull   time.Time
}

// plan is the work of one folder in one cycle.
type plan struct {
	dirs       []string
	full, scan bool
	pull       bool
	reset      bool
	retry      bool // run because a retry was due
}

// event records a watcher hint and moves the scan time: trailing debounce, capped at MaxWait after the first
// pending event so that a steady stream of writes cannot postpone the scan forever.
func (s *schedule) event(c schedConf, d string, now time.Time) {
	if s.dirty == nil {
		s.dirty = map[string]bool{}
	}
	if len(s.dirty) == 0 {
		s.firstAt = now
	}
	s.dirty[d] = true
	s.rewatch = s.rewatch || d == ""
	s.debounceAt = now.Add(c.Debounce)
	if limit := s.firstAt.Add(c.MaxWait); c.MaxWait > 0 && s.debounceAt.After(limit) {
		s.debounceAt = limit
	}
}

func due(t, now time.Time) bool { return !t.IsZero() && !now.Before(t) }

// take returns the work due at now and clears what it takes; ok is false when nothing is due. A due retry
// (also one for a file that was too fresh) scans the whole folder: the files it is about may be anywhere.
func (s *schedule) take(now time.Time) (p plan, ok bool) {
	p = plan{full: s.full || due(s.nextFull, now), pull: s.pull, reset: s.reset}
	p.retry = due(s.retryAt, now) || due(s.unstableAt, now)
	if len(s.dirty) > 0 && !now.Before(s.debounceAt) {
		for d := range s.dirty {
			p.dirs = append(p.dirs, d)
		}
		s.dirty = nil
		p.scan = true
	}
	if p.full || p.retry {
		p.scan, p.pull = true, true
	}
	if !p.scan && !p.pull {
		return p, false
	}
	if p.full || p.retry {
		p.dirs = nil
	}
	s.full, s.pull, s.reset, s.retryAt = false, false, false, time.Time{}
	return p, true
}

// taskResult is how a task ended.
type taskResult struct {
	err        error
	retry      bool      // something was postponed: run again soon
	slow       bool      // files are in use or locked: retry on the slow schedule (design 5.7)
	unstableAt time.Time // the folder's own record of too-fresh files after the task (Folder.Unstable)
	watching   bool      // the folder has a watcher
	reported   bool      // the task produced a status report
	unauth     bool      // the server rejected the device token
}

// done records how the task p ended at now.
func (s *schedule) done(c schedConf, now time.Time, p plan, o taskResult) {
	if o.err != nil || o.retry {
		s.retryAt = now.Add(c.RetryAfter)
		if o.unauth {
			s.retryAt = now.Add(c.AuthRetry)
		}
	}
	s.unstableAt = o.unstableAt
	if o.err != nil && !s.unstableAt.IsZero() && s.unstableAt.Before(s.retryAt) {
		s.unstableAt = s.retryAt // a failed task is retried by retryAt; a due unstableAt would spin
	}
	if !o.slow {
		s.slowTries = 0
	} else if o.err == nil && !o.retry {
		if p.retry {
			s.slowTries++
		}
		if s.slowTries < len(c.SlowRetry) {
			s.retryAt = now.Add(c.SlowRetry[s.slowTries])
		}
	}
	if p.full {
		s.nextFull = now.Add(c.ScanInterval)
		if o.watching && s.nextFull.Sub(now) > 0 && o.reported && !p.scan {
			s.full = true
		}
	}
	if o.err != nil {
		s.pull, s.reset = s.pull || p.pull, s.reset || p.reset
	}
}

// next is the earliest time something is due, or zero when nothing is scheduled.
func (s *schedule) next() time.Time {
	var next time.Time
	for _, t := range []time.Time{s.retryAt, s.unstableAt, s.nextFull} {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if len(s.dirty) > 0 && (next.IsZero() || s.debounceAt.Before(next)) {
		next = s.debounceAt
	}
	return next
}
