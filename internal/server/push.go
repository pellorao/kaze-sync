package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"kaze-sync/internal/proto"
)

// Web Push to the user's browsers (design 4.3). Every push forwards one notification of the bell: the events are
// computed from the same state, and nothing is stored but the subscriptions (pushapi.go).

// tunables of the push checks (design 4.3)
const (
	pushEvery       = 5 * time.Minute  // the slow check: the disk and the devices that have not connected
	pushMinGap      = 10 * time.Second // the least time between two checks after a state change
	pushHeldWait    = 2 * time.Minute  // a change on hold is pushed when it is still there after this long
	pushErrorsWait  = 5 * time.Minute  // so are the errors a device reports: it retries first
	pushRepeat      = time.Hour        // the least time between two pushes of one cause
	pushRetryFirst  = time.Minute      // the first retry of a temporary failure; the later ones come every pushRepeat
	pushCap         = 20               // pushes per pushRepeat over all causes: a bug must not flood the phone
	pushDiskLow     = 256 << 20        // free bytes on the data disk below which the user is told
	pushNotifyQuery = "/?notify=1"     // the page a push opens: the web UI with the bell open
)

// pushEvent is one cause of a push: a notification of the bell that is there now.
type pushEvent struct {
	key            string // the cause: <kind>-<folder>-<device>, or just the kind; ids, not names
	kind           string
	folder, device int64
	n              int // changes on hold, errors, days offline
	wait           time.Duration
}

// pushKey is what is known about one cause between checks.
type pushKey struct {
	active    bool
	notBefore time.Time            // the earliest time to push: the wait, and the hour since the last push of it
	lastSent  time.Time            // the last time a push of it was delivered
	done      map[string]bool      // subscriptions that got it (or refused it for good) while it is there
	retry     map[string]pushRetry // subscriptions that failed for a while
}

type pushRetry struct {
	n  int
	at time.Time // the earliest time to try again
}

// pushEvents lists the causes there are now. It reads memory only (the reports, the three server states and the
// offline devices of the last slow check), so a check after a state change costs no query.
func (s *Server) pushEvents() []pushEvent {
	var out []pushEvent
	s.mu.Lock()
	for dev, rep := range s.reports {
		d := strconv.FormatInt(dev, 10)
		if rep.tooOld > 0 {
			out = append(out, pushEvent{key: "old-" + d, kind: "old", device: dev})
		}
		held := map[int64]int{}
		for f, items := range rep.held {
			held[f] += len(items)
		}
		for f, n := range rep.heldCut {
			held[f] += n
		}
		for f, n := range held {
			if n > 0 {
				out = append(out, pushEvent{key: "held-" + strconv.FormatInt(f, 10) + "-" + d, kind: "held", folder: f, device: dev, n: n, wait: pushHeldWait})
			}
		}
		for _, fs := range rep.req.Folders {
			n := 0
			for _, e := range fs.Errors {
				if e.Kind == proto.KindRejected || e.Kind == proto.KindIO || e.Kind == proto.KindFolder {
					n++
				}
			}
			if n > 0 {
				out = append(out, pushEvent{key: "errors-" + strconv.FormatInt(fs.ID, 10) + "-" + d, kind: "errors", folder: fs.ID, device: dev, n: n, wait: pushErrorsWait})
			}
		}
	}
	for _, c := range []struct {
		on   bool
		kind string
	}{{s.diskLow, "disk"}, {s.gcFailed, "gc"}, {s.certFailed, "cert"}} {
		if c.on {
			out = append(out, pushEvent{key: c.kind, kind: c.kind})
		}
	}
	s.mu.Unlock()
	s.pushMu.Lock()
	for dev, days := range s.pushOffline {
		out = append(out, pushEvent{key: "offline-" + strconv.FormatInt(dev, 10), kind: "offline", device: dev, n: days})
	}
	s.pushMu.Unlock()
	return out
}

// checkDisk looks at the free space of the data disk; the bell and the push both read the result.
func (s *Server) checkDisk() {
	free, ok := freeSpace(s.varDir)
	if !ok {
		return
	}
	s.setServerState(&s.diskLow, free < pushDiskLow)
}

// setServerState sets one of the three server-side problem flags and tells the web UI when it changed.
func (s *Server) setServerState(flag *bool, on bool) {
	s.mu.Lock()
	changed := *flag != on
	*flag = on
	if changed {
		s.bumpLocked()
	}
	s.mu.Unlock()
}

// refreshOffline recomputes the devices that have not connected for offlineAfter (it queries, so only the slow check).
func (s *Server) refreshOffline() {
	off, err := s.offlineDevices()
	if err != nil {
		slog.Warn("push check", "err", err)
		return
	}
	m := map[int64]int{}
	for _, d := range off {
		m[d.ID] = d.Days
	}
	s.pushMu.Lock()
	s.pushOffline = m
	s.pushMu.Unlock()
}

// pushCheck is one look at the causes: it starts the pushes that are due and returns how long until the next
// cause leaves its wait (0: none is waiting). full adds the slow part (disk, offline devices).
func (s *Server) pushCheck(full bool) time.Duration {
	if s.vapid == nil {
		return 0
	}
	if full {
		s.checkDisk()
		s.refreshOffline()
	}
	evs := s.pushEvents()
	subs := s.subscriptions()
	now := s.now()
	type job struct {
		ev      pushEvent
		targets []pushSub
	}
	var jobs []job
	var next time.Duration
	s.pushMu.Lock()
	if now.Sub(s.pushWin) >= pushRepeat || now.Before(s.pushWin) {
		s.pushWin, s.pushSent = now, 0
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		seen[ev.key] = true
		k := s.pkeys[ev.key]
		if k == nil {
			k = &pushKey{}
			s.pkeys[ev.key] = k
		}
		if !k.active {
			k.active, k.done, k.retry = true, map[string]bool{}, map[string]pushRetry{}
			k.notBefore = now.Add(ev.wait)
			if !k.lastSent.IsZero() && k.lastSent.Add(pushRepeat).After(k.notBefore) {
				k.notBefore = k.lastSent.Add(pushRepeat)
			}
		}
		if now.Before(k.notBefore) {
			if d := k.notBefore.Sub(now); next == 0 || d < next {
				next = d
			}
			continue
		}
		var targets []pushSub
		for _, sub := range subs {
			if sub.Gone || k.done[sub.ID] || now.Before(k.retry[sub.ID].at) {
				continue
			}
			if s.pushSent >= pushCap {
				slog.Warn("push: hourly cap reached, not sending", "key", ev.key)
				break
			}
			s.pushSent++
			r := k.retry[sub.ID] // not again before this one has ended: a check 10 s later must not send it twice
			r.at = now.Add(2 * pushTimeout)
			k.retry[sub.ID] = r
			targets = append(targets, sub)
		}
		if len(targets) > 0 {
			jobs = append(jobs, job{ev, targets})
		}
	}
	for key, k := range s.pkeys {
		if seen[key] {
			continue
		}
		k.active, k.done, k.retry = false, nil, nil
		if k.lastSent.IsZero() || now.Sub(k.lastSent) >= pushRepeat {
			delete(s.pkeys, key)
		}
	}
	s.pushMu.Unlock()
	for _, j := range jobs {
		s.pushWG.Add(1)
		go func() {
			defer s.pushWG.Done()
			s.pushSend(j.ev, j.targets)
		}()
	}
	return next
}

// pushBody is the text of a push in a language: the bell's notification, without paths, file contents, error texts or
// credentials. "" when the folder or the device is gone (the report of a deleted folder lingers until the device
// polls again).
func (s *Server) pushBody(ev pushEvent, lang string) string {
	t := trFor(lang)
	var folder, device string
	if ev.folder != 0 {
		if s.db.QueryRow(`SELECT name FROM folders WHERE id=?`, ev.folder).Scan(&folder) != nil {
			return ""
		}
	}
	if ev.device != 0 {
		if s.db.QueryRow(`SELECT name FROM devices WHERE id=?`, ev.device).Scan(&device) != nil {
			return ""
		}
	}
	switch ev.kind {
	case "held":
		return t.T("push.held", "device", device, "folder", folder, "n", ev.n)
	case "errors":
		return t.T("push.errors", "device", device, "folder", folder, "n", ev.n)
	case "old":
		return t.T("push.old", "device", device)
	case "offline":
		return t.T("push.offline", "device", device, "d", ev.n)
	case "disk":
		return t.T("push.disk")
	case "gc":
		return t.T("push.gc")
	case "cert":
		return t.T("push.cert")
	}
	return ""
}

// pushPayload is the JSON a browser's service worker gets (sw.js).
func pushPayload(body, tag string) []byte {
	b, _ := json.Marshal(map[string]string{"title": "kaze-sync", "body": body, "tag": tag, "url": pushNotifyQuery})
	return b
}

// pushTopic is the Topic header of a cause: a short hash, so that a phone that is off keeps only the latest message
// about it. The header allows 32 URL-safe characters.
func pushTopic(key string) string {
	h := sha256.Sum256([]byte(key))
	return b64.EncodeToString(h[:18])
}

// pushSend delivers one cause to its subscriptions and records the results.
func (s *Server) pushSend(ev pushEvent, targets []pushSub) {
	start := time.Now()
	ok := 0
	for _, sub := range targets {
		body := s.pushBody(ev, sub.Lang)
		if body == "" { // the folder or device is gone: nothing to tell
			s.pushMu.Lock()
			if k := s.pkeys[ev.key]; k != nil && k.active {
				k.done[sub.ID] = true
			}
			s.pushMu.Unlock()
			continue
		}
		res := s.sendTo(context.Background(), sub, pushPayload(body, ev.key), pushTopic(ev.key))
		s.pushResult(ev.key, sub, res)
		if res.OK() {
			ok++
		}
	}
	slog.Info("push", "key", ev.key, "subscriptions", len(targets), "delivered", ok, "took", time.Since(start).Round(time.Millisecond))
}

// pushResult records how one push ended: delivered, to be retried, given up, or the subscription is gone.
func (s *Server) pushResult(key string, sub pushSub, res pushResult) {
	now := s.now()
	s.pushMu.Lock()
	if k := s.pkeys[key]; k != nil && k.active {
		switch {
		case res.OK():
			k.done[sub.ID], k.lastSent = true, now
			delete(k.retry, sub.ID)
		case res.Gone():
			k.done[sub.ID] = true
		case res.Temporary():
			r := k.retry[sub.ID]
			wait := pushRepeat
			if r.n == 0 {
				wait = pushRetryFirst
			}
			k.retry[sub.ID] = pushRetry{n: r.n + 1, at: now.Add(wait)}
		default: // another 4xx: sending it again would not change the answer
			k.done[sub.ID] = true
		}
	}
	s.pushMu.Unlock()
	if res.Status == 401 || res.Status == 403 {
		slog.Error("push: the push service rejected VAPID", "host", pushHost(sub.Endpoint), "status", res.Status)
	} else if !res.OK() {
		slog.Warn("push: not delivered", "host", pushHost(sub.Endpoint), "status", res.Status, "err", res.Err)
	}
	s.recordPush(sub.ID, res)
}

// pushLoop runs the checks: after a state change (at most every pushMinGap), when a cause leaves its wait, and every
// pushEvery for the slow part. Idle, only the slow check runs.
func (s *Server) pushLoop(ctx context.Context) {
	fullAt := time.Time{}
	for {
		s.mu.Lock()
		changed := s.uiCh
		s.mu.Unlock()
		started := time.Now()
		full := started.Sub(fullAt) >= pushEvery
		if full {
			fullAt = started
		}
		next := s.pushCheck(full)
		wake := time.NewTimer(time.Hour) // a cause is waiting: look again when its wait ends
		if next > 0 {
			wake.Reset(next + time.Second)
		} else {
			wake.Stop()
		}
		slow := time.NewTimer(max(pushEvery-time.Since(fullAt), time.Second))
		stop := false
		select {
		case <-ctx.Done():
			stop = true
		case <-wake.C:
		case <-slow.C:
		case <-changed:
			select {
			case <-ctx.Done():
				stop = true
			case <-time.After(max(pushMinGap-time.Since(started), 0)):
			}
		}
		wake.Stop()
		slow.Stop()
		if stop {
			return
		}
	}
}
