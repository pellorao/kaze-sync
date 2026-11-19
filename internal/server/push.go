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
	notBefore time.Time               // the earliest time to push: the wait, and the hour since the last push of it
	lastSent  time.Time               // the last time a push of it was delivered
	done      map[string]bool         // subscriptions that got it (or refused it for good) while it is there
	retry     map[string]pushRetry    // subscriptions that failed for a while
	pending   map[string]*pushAttempt // one reservation for each queued or sending subscription
}

type pushAttempt struct {
	sub      pushSub
	canceled bool // the cause went away or the subscription was replaced
}

type pushRetry struct {
	n  int
	at time.Time // the earliest time to try again
}

// pushEvents lists the current causes in memory. The mapping snapshot is refreshed after configuration changes.
func (s *Server) pushEvents() []pushEvent {
	var out []pushEvent
	s.mu.Lock()
	mappings := s.noticeMappingsLocked()
	for dev, rep := range s.reports {
		d := strconv.FormatInt(dev, 10)
		if rep.tooOld > 0 {
			out = append(out, pushEvent{key: "old-" + d, kind: "old", device: dev})
		}
		for f, items := range rep.held {
			if n := len(items) + rep.heldCut[f]; n > 0 && mappings[noticeMapping{dev, f}] {
				out = append(out, pushEvent{key: "held-" + strconv.FormatInt(f, 10) + "-" + d, kind: "held", folder: f, device: dev, n: n, wait: pushHeldWait})
			}
		}
		for _, fs := range rep.req.Folders {
			if !mappings[noticeMapping{dev, fs.ID}] {
				continue
			}
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
	for _, n := range s.serverProblemsLocked() {
		out = append(out, pushEvent{key: n.Kind, kind: n.Kind})
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
	if full { // the bell shows these whether or not push is on
		s.checkDisk()
		s.refreshOffline()
	}
	if s.vapid == nil {
		return 0
	}
	subs := s.subscriptions()
	if len(subs) == 0 {
		return 0
	}
	evs := s.pushEvents()
	now := s.now()
	type job struct {
		ev      pushEvent
		key     *pushKey
		targets []*pushAttempt
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
			k = &pushKey{pending: map[string]*pushAttempt{}}
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
		var targets []*pushAttempt
		for _, sub := range subs {
			if sub.Gone || k.pending[sub.ID] != nil || k.done[sub.ID] || now.Before(k.retry[sub.ID].at) {
				continue
			}
			if s.pushSent >= pushCap {
				slog.Warn("push: hourly cap reached, not sending", "key", ev.key)
				break
			}
			s.pushSent++
			a := &pushAttempt{sub: sub}
			k.pending[sub.ID] = a
			targets = append(targets, a)
		}
		if len(targets) > 0 {
			jobs = append(jobs, job{ev, k, targets})
		}
	}
	for key, k := range s.pkeys {
		if seen[key] {
			continue
		}
		k.active, k.done, k.retry = false, nil, nil
		for _, a := range k.pending {
			a.canceled = true
		}
		if len(k.pending) == 0 && (k.lastSent.IsZero() || now.Sub(k.lastSent) >= pushRepeat) {
			delete(s.pkeys, key)
		}
	}
	s.pushMu.Unlock()
	for _, j := range jobs {
		s.pushWG.Add(1)
		go func() {
			defer s.pushWG.Done()
			s.pushSend(j.ev, j.key, j.targets)
		}()
	}
	return next
}

// pushBody is the text of a push in a language: the bell's notification, without paths, file contents, error texts or
// credentials. A queued folder notice still needs the device and folder to be mapped when it reaches the sender.
func (s *Server) pushBody(ev pushEvent, lang string) string {
	t := trFor(lang)
	var folder, device string
	if ev.folder != 0 {
		if s.db.QueryRow(`SELECT f.name, d.name`+noticeMappingFrom+` WHERE f.id=? AND d.id=?`, ev.folder, ev.device).Scan(&folder, &device) != nil {
			return ""
		}
	} else if ev.device != 0 {
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
func (s *Server) pushSend(ev pushEvent, k *pushKey, targets []*pushAttempt) {
	start := time.Now()
	ok := 0
	for _, a := range targets {
		sub := a.sub
		body := s.pushBody(ev, sub.Lang)
		s.pushMu.Lock()
		canceled := a.canceled
		s.pushMu.Unlock()
		var res *pushResult
		if !canceled && body != "" && s.pushSubActive(sub) {
			r := s.sendTo(context.Background(), sub, pushPayload(body, ev.key), pushTopic(ev.key))
			switch {
			case r.OK():
				ok++
			case r.Status == 401 || r.Status == 403:
				slog.Error("push: the push service rejected VAPID", "host", pushHost(sub.Endpoint), "status", r.Status)
			default:
				slog.Warn("push: not delivered", "host", pushHost(sub.Endpoint), "status", r.Status, "err", r.Err)
			}
			if s.recordPush(sub, r) {
				res = &r
			}
		}
		s.finishPush(k, a, res)
	}
	slog.Info("push", "key", ev.key, "subscriptions", len(targets), "delivered", ok, "took", time.Since(start).Round(time.Millisecond))
}

// finishPush releases the reservation. A canceled attempt cannot update the current delivery or retry state.
func (s *Server) finishPush(k *pushKey, a *pushAttempt, res *pushResult) {
	id := a.sub.ID
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	delete(k.pending, id)
	if a.canceled || res == nil {
		return
	}
	switch {
	case res.OK():
		k.done[id], k.lastSent = true, s.now()
		delete(k.retry, id)
	case res.Temporary():
		r := k.retry[id]
		wait := pushRepeat
		if r.n == 0 {
			wait = pushRetryFirst
		}
		k.retry[id] = pushRetry{n: r.n + 1, at: s.now().Add(wait)}
	default: // 4xx other than 429: sending it again would not change the answer
		k.done[id] = true
	}
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

// forgetSub drops what the checks know about a subscription that was made again, so that the causes still active are
// pushed to it (an earlier 410 had marked them done for it).
func (s *Server) forgetSub(id string) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	for _, k := range s.pkeys {
		delete(k.done, id)
		delete(k.retry, id)
		if a := k.pending[id]; a != nil {
			a.canceled = true
		}
	}
}
