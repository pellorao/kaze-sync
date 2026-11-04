package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"kaze-sync/internal/proto"
)

func (a *Agent) pollLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		a.mu.Lock()
		wait := a.unplanned()
		a.mu.Unlock()
		if wait { // the server answers at once for a folder without a cursor: give the loop a moment to create its runner
			select {
			case <-ctx.Done():
			case <-a.applied:
			case <-time.After(time.Second):
			}
		}
		a.mu.Lock()
		req := proto.PollReq{Cursors: map[int64]int64{}, ConfigVersion: a.cfg.Version, AgentVersion: proto.AgentVersion, OS: a.OS, Hostname: a.Hostname, Folders: a.states()}
		for id, r := range a.runners {
			cur := r.seen
			if (r.busy || r.s.pull || !r.ready) && r.acked >= 0 {
				cur = max(cur, r.acked) // news that is being handled, or cannot be until the folder is usable, must not wake the poll again
			}
			if cur >= 0 {
				req.Cursors[id] = cur
			}
		}
		for id, o := range a.openRetry {
			if o.seen >= 0 {
				req.Cursors[id] = o.seen
			}
		}
		pctx, cancel := context.WithCancel(ctx)
		a.cancel, a.pollAt, a.sig, a.statusAt = cancel, time.Now(), fmt.Sprint(req.Folders), time.Time{}
		a.mu.Unlock()
		resp, err := a.API.Poll(pctx, req)
		cancelled := pctx.Err() != nil
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case cancelled: // restarted to report a changed status
			continue
		case err != nil:
			wait := backoff
			if ue := (*UpgradeError)(nil); errors.As(err, &ue) {
				slog.Error("client too old, sync is stopped until kaze-syncc is upgraded", "err", err, "retry_in", wait)
			} else if errors.Is(err, ErrUnauthorized) { // revoked: asking often only adds load (design 2.5)
				wait = a.AuthRetry
				slog.Error("sync is stopped, the server does not accept this device", "err", err, "retry_in", wait)
			} else {
				slog.Warn("poll failed", "err", err, "retry_in", wait)
			}
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			backoff = min(2*backoff, time.Minute)
			continue
		}
		backoff = time.Second
		if !a.handlePoll(req, resp) {
			continue
		}
		a.mu.Lock()
		n := a.failures
		a.mu.Unlock()
		if n > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(min(time.Second<<min(n, 6), time.Minute)):
			}
		}
	}
}

// handlePoll takes in the answer to a poll request and reports whether it carried news.
func (a *Agent) handlePoll(req proto.PollReq, resp proto.PollResp) bool {
	news := false
	if resp.Config != nil {
		if err := writeJSON(filepath.Join(a.Dir, "config.json"), resp.Config); err != nil {
			slog.Error("cannot cache configuration", "err", err)
		}
		a.mu.Lock()
		a.cfg = *resp.Config
		a.mu.Unlock()
		news = true
	}
	a.mu.Lock()
	if resp.Epoch != a.epoch { // another server history: every folder reads again (Folder.checkEpoch decides)
		a.epoch = resp.Epoch
		for _, r := range a.runners {
			r.s.pull = true
		}
		news = true
	}
	for id, o := range a.openRetry {
		if seq, ok := resp.Cursors[id]; ok {
			o.seen = seq
		}
	}
	for id, r := range a.runners {
		if seq, ok := resp.Cursors[id]; ok {
			if have, ok := req.Cursors[id]; ok && seq < have { // the server has less than we knew: a restored backup
				slog.Warn("the server's latest seq is behind ours; reading the folder again", "folder", id, "server", seq, "ours", have)
				r.s.reset, r.s.pull, r.seen, r.acked, news = true, true, -1, seq, true // the next poll carries the server's seq, not the stale one
				continue
			}
			if seq == r.seen {
				continue
			}
			r.acked = seq
			if r.ready || r.seen >= 0 { // a folder that cannot be pulled has no news for us; it pulls when it becomes ready
				r.s.pull, news = true, true
			}
		}
	}
	a.mu.Unlock()
	if news {
		slog.Debug("poll news", "cursors", fmt.Sprint(resp.Cursors), "config", resp.Config != nil)
		a.wake()
	}
	return news
}
