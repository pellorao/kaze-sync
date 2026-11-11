package client

import (
	"maps"
	"slices"
	"time"
)

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
	dirty        map[string]bool
	debounceAt   time.Time
	firstAt      time.Time // when the oldest pending event arrived; zero when none is pending
	full         bool      // a full scan is due
	pull         bool
	reset        bool // the server went back in time: forget its seqs
	rewatch      bool // the watcher reported lost events: start a new one
	retryAt      time.Time
	holdUntil    time.Time       // a pull or reset left over from a failed task waits for this time (the retry time), so that an unreachable server is not asked in a loop
	fails        int             // consecutive failed tasks: the retry delay doubles with each
	unstableAt   time.Time       // when the youngest file the last scans found too fresh to upload becomes stable; no task clears it, only a scan that no longer sees such a file
	slowTries    int             // retries for files in use so far
	quick        int             // quick retries in a row that ended with something postponed again: the retry delay doubles with each
	retryDirs    map[string]bool // where the next retry scans; a retry without any pulls only
	retryFull    bool            // the next retry scans the whole folder (the last task failed: nothing says where)
	unstableDirs []string        // where the too-fresh files are (Folder.UnstableDirs)
	nextFull     time.Time
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

// maxRetryDelay caps the doubling retry delay after failed tasks (the same cap as the poll loop's).
const maxRetryDelay = time.Minute

func due(t, now time.Time) bool { return !t.IsZero() && !now.Before(t) }

// take returns the work due at now and clears what it takes; ok is false when nothing is due. A retry or the
// time of a too-fresh file scans only the directories it knows about (the whole folder after a failed task); a
// full scan is for the start, a changed configuration, lost events and the periodic scan.
func (s *schedule) take(now time.Time) (p plan, ok bool) {
	held := now.Before(s.holdUntil)
	p = plan{full: s.full || due(s.nextFull, now), pull: s.pull && !held, reset: s.reset && !held}
	p.retry = due(s.retryAt, now)
	dirs := map[string]bool{}
	if len(s.dirty) > 0 && !now.Before(s.debounceAt) {
		for d := range s.dirty {
			dirs[d] = true
		}
		s.dirty = nil
		p.scan = true
	}
	if due(s.unstableAt, now) {
		for _, d := range s.unstableDirs {
			dirs[d] = true
		}
		p.scan = true
	}
	wide := p.full
	if p.retry {
		p.pull = true
		if s.retryFull {
			wide = true
		}
		for d := range s.retryDirs {
			dirs[d] = true
			p.scan = true
		}
		s.retryDirs, s.retryFull = nil, false
	}
	if wide || p.scan && len(dirs) == 0 { // nothing says where: the whole folder
		p.scan, p.pull, wide = true, true, true
	}
	if !p.scan && !p.pull {
		return p, false
	}
	if !wide {
		p.dirs = slices.Sorted(maps.Keys(dirs))
	}
	if p.pull && s.reset {
		p.reset = true // a pull forced by a scan still starts from the reset
	}
	s.full, s.retryAt = false, time.Time{}
	if p.pull {
		s.pull, s.reset = false, false
	}
	return p, true
}

// taskResult is how a task ended.
type taskResult struct {
	err          error
	retry        bool      // something was postponed: run again soon
	slow         bool      // files are in use or locked: retry on the slow schedule (design 5.7)
	unstableAt   time.Time // the folder's own record of too-fresh files after the task (Folder.Unstable)
	unstableDirs []string  // where they are
	retryDirs    []string  // where a retry has to scan (Folder.RetryDirs); none with retry set means a pull only
	unauth       bool      // the server rejected the device token
}

// done records how the task p ended at now.
func (s *schedule) done(c schedConf, now time.Time, p plan, o taskResult) {
	if o.err != nil {
		s.fails++
	} else {
		s.fails = 0
	}
	if o.err == nil && o.retry {
		s.quick++
	} else {
		s.quick = 0
	}
	if o.err != nil {
		s.retryFull = true
	} else {
		if s.retryDirs == nil {
			s.retryDirs = map[string]bool{}
		}
		for _, d := range o.retryDirs {
			s.retryDirs[d] = true
		}
	}
	s.unstableAt, s.unstableDirs = o.unstableAt, o.unstableDirs
	if o.err != nil || o.retry {
		s.retryAt = now.Add(c.RetryAfter)
		if o.err != nil && s.fails > 1 {
			s.retryAt = now.Add(min(c.RetryAfter<<min(s.fails-1, 10), max(maxRetryDelay, c.RetryAfter)))
		}
		if o.err == nil && s.quick > 1 { // the same retry again and again: back off
			s.retryAt = now.Add(min(c.RetryAfter<<min(s.quick-1, 10), max(maxRetryDelay, c.RetryAfter)))
		}
		if o.unauth {
			s.retryAt = now.Add(c.AuthRetry)
		}
	}
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
	if s.retryAt.IsZero() {
		s.retryDirs, s.retryFull = nil, false // no retry is coming: nothing to keep
	}
	if p.full {
		s.nextFull = now.Add(c.ScanInterval)
	}
	if o.err != nil {
		s.pull, s.reset = s.pull || p.pull, s.reset || p.reset
		s.holdUntil = s.retryAt // not before the retry time
	} else if p.pull {
		s.holdUntil = time.Time{}
	}
}

// next is the earliest time something is due, or zero when nothing is scheduled.
func (s *schedule) next() time.Time {
	var next time.Time
	for _, t := range []time.Time{s.retryAt, s.unstableAt, s.nextFull, s.heldWork()} {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if len(s.dirty) > 0 && (next.IsZero() || s.debounceAt.Before(next)) {
		next = s.debounceAt
	}
	return next
}

// heldWork is when the pull or reset left over from a failed task may run, or zero when none waits.
func (s *schedule) heldWork() time.Time {
	if s.pull || s.reset {
		return s.holdUntil
	}
	return time.Time{}
}
