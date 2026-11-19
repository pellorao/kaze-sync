package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

// fakePush is a push service on 127.0.0.1: it decrypts what it gets with the keys of the subscriptions made here.
type fakePush struct {
	t           *testing.T
	srv         *httptest.Server
	mu          sync.Mutex
	status      map[string]int // path -> the status to answer (default 201)
	keys        map[string]*ecdh.PrivateKey
	auths       map[string][]byte
	got         []pushed
	beforeReply func(string)
}

type pushed struct {
	path, topic, ttl, authz string
	msg                     map[string]string
}

func (f *fakePush) setStatus(path string, st int) {
	f.mu.Lock()
	f.status[path] = st
	f.mu.Unlock()
}

func newFakePush(t *testing.T) *fakePush {
	f := &fakePush{t: t, status: map[string]int{}, keys: map[string]*ecdh.PrivateKey{}, auths: map[string][]byte{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.beforeReply != nil {
			f.beforeReply(r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		p := pushed{path: r.URL.Path, topic: r.Header.Get("Topic"), ttl: r.Header.Get("TTL"), authz: r.Header.Get("Authorization")}
		if r.Header.Get("Content-Encoding") != "aes128gcm" {
			t.Errorf("Content-Encoding %q", r.Header.Get("Content-Encoding"))
		}
		if pt, err := decryptPush(body, f.keys[r.URL.Path].Bytes(), f.auths[r.URL.Path]); err != nil {
			t.Errorf("cannot decrypt the push: %v", err)
		} else if err := json.Unmarshal(pt, &p.msg); err != nil {
			t.Errorf("the payload is not JSON: %v", err)
		}
		f.got = append(f.got, p)
		st := f.status[r.URL.Path]
		if st == 0 {
			st = 201
		}
		w.WriteHeader(st)
	}))
	old := pushClient
	pushClient = f.srv.Client()
	t.Cleanup(func() { pushClient = old; f.srv.Close() })
	return f
}

// sub adds a subscription named name for a browser that speaks lang, as if the browser had subscribed.
func (f *fakePush) sub(e *env, name, lang string) pushSub {
	f.t.Helper()
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	path := "/" + name
	f.keys[path], f.auths[path] = k, auth
	sub := pushSub{ID: subID(f.srv.URL + path), Endpoint: f.srv.URL + path, P256dh: b64.EncodeToString(k.PublicKey().Bytes()), Auth: b64.EncodeToString(auth), Lang: lang, Browser: name, At: e.clk.Now().Unix()}
	if err := e.s.editSubs(func(l []pushSub) ([]pushSub, error) { return append(l, sub), nil }); err != nil {
		f.t.Fatal(err)
	}
	return sub
}

// take returns what arrived since the last call.
func (f *fakePush) take() []pushed {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.got
	f.got = nil
	return out
}

func (f *fakePush) pause(path string) (<-chan struct{}, func()) {
	started, release := make(chan struct{}), make(chan struct{})
	signal := sync.OnceFunc(func() { close(started) })
	f.beforeReply = func(p string) {
		if p == path {
			signal()
			<-release
		}
	}
	return started, sync.OnceFunc(func() { close(release) })
}

func waitForPush(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first send did not reach the push service")
	}
}

// check runs a full check and waits for the pushes it started.
func (e *env) check() {
	e.t.Helper()
	e.s.pushCheck(true)
	e.s.pushWG.Wait()
}

func (e *env) heldFor(tok string, folder int64) {
	e.t.Helper()
	e.heldReport(tok, folder, proto.HeldItem{Path: "sub/secret-report.docx", Scope: "d:sub", Reason: "owner", Kind: "modify", At: e.clk.Now().Unix()})
}

func TestPushQueuedSubscriptionsUseOneReservation(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "slow", "en")
	fp.sub(e, "queued", "en")
	started, unblock := fp.pause("/slow")
	defer func() { unblock(); e.s.pushWG.Wait() }()
	e.s.setServerState(&e.s.gcFailed, true)
	e.s.pushCheck(false)
	waitForPush(t, started)
	e.clk.Add(2*pushTimeout + time.Second)
	e.s.pushCheck(false)
	unblock()
	e.s.pushWG.Wait()
	if got := fp.take(); len(got) != 2 || got[0].path != "/slow" || got[1].path != "/queued" || e.s.pushSent != 2 {
		t.Fatalf("requests = %d, hourly reservations = %d; want one request and reservation per subscription", len(got), e.s.pushSent)
	}
}

func TestPushDropsCanceledQueueAndIgnoresItsLateResult(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "slow", "en")
	fp.sub(e, "queued", "en")
	started, unblock := fp.pause("/slow")
	defer func() { unblock(); e.s.pushWG.Wait() }()
	e.s.setServerState(&e.s.gcFailed, true)
	e.s.pushCheck(false)
	waitForPush(t, started)

	e.s.setServerState(&e.s.gcFailed, false)
	e.s.pushCheck(false)
	e.s.setServerState(&e.s.gcFailed, true)
	e.s.pushCheck(false)
	unblock()
	e.s.pushWG.Wait()
	if got := fp.take(); len(got) != 1 || got[0].path != "/slow" {
		t.Fatalf("canceled batch sent %d requests; want only the request already in flight", len(got))
	}
	e.check()
	if got := fp.take(); len(got) != 2 {
		t.Fatalf("returned cause sent %d requests; want a fresh push to each subscription", len(got))
	}
}

func TestPushHeldWaitsThenGoesOnceWithAnEncryptedPayload(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "phone", "en")
	f := e.folder("docs")
	dev, tok := e.enrollDevice("laptop", "linux")
	e.mapFolder(dev, f, "/docs")
	e.check()
	if got := fp.take(); len(got) != 0 {
		t.Fatalf("nothing is wrong yet, got %+v", got)
	}
	e.heldFor(tok, f)
	e.check()
	e.clk.Add(pushHeldWait - time.Second)
	e.check()
	if got := fp.take(); len(got) != 0 {
		t.Fatalf("pushed before the wait ended: %+v", got)
	}
	e.clk.Add(2 * time.Second)
	for range 3 {
		e.check()
	}
	got := fp.take()
	if len(got) != 1 {
		t.Fatalf("want one push, got %+v", got)
	}
	p := got[0]
	if p.msg["title"] != "kaze-sync" || p.msg["body"] != "laptop has 1 change in folder docs waiting for your decision" || p.msg["tag"] != "held-"+itoa(f)+"-"+itoa(dev) || p.msg["url"] != "/?notify=1" {
		t.Errorf("payload %v", p.msg)
	}
	if strings.Contains(p.msg["body"], "secret") || strings.Contains(p.msg["body"], "sub/") {
		t.Errorf("the body names a path: %q", p.msg["body"])
	}
	if p.ttl != "86400" || len(p.topic) != 24 || !strings.HasPrefix(p.authz, "vapid t=") || !strings.Contains(p.authz, ", k="+vapidPublic(e.s.vapid)) {
		t.Errorf("headers: ttl %q topic %q auth %q", p.ttl, p.topic, p.authz)
	}
	// the JWT is for the push service's origin and verifies with the server's key
	tok1 := strings.TrimPrefix(strings.SplitN(p.authz, ",", 2)[0], "vapid t=")
	if c, err := verifyJWT(&e.s.vapid.PublicKey, tok1); err != nil || c["aud"] != fp.srv.URL {
		t.Errorf("jwt: %v %v", c, err)
	}
	// it stays quiet while the cause stays
	e.clk.Add(10 * time.Minute)
	e.check()
	if got := fp.take(); len(got) != 0 {
		t.Fatalf("pushed again: %+v", got)
	}
}

func TestPushRepeatsOnlyAfterAnHourWhenTheCauseComesBack(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "phone", "en")
	f := e.folder("docs")
	dev, tok := e.enrollDevice("laptop", "linux")
	e.mapFolder(dev, f, "/docs")
	e.heldFor(tok, f)
	e.check()
	e.clk.Add(pushHeldWait)
	e.check()
	if len(fp.take()) != 1 {
		t.Fatal("no first push")
	}
	e.heldReport(tok, f) // decided: the list is empty
	e.check()
	e.clk.Add(time.Minute)
	e.heldFor(tok, f) // it is back
	e.check()
	e.clk.Add(pushHeldWait + time.Minute)
	e.check()
	if got := fp.take(); len(got) != 0 {
		t.Fatalf("pushed again within the hour: %+v", got)
	}
	e.clk.Add(time.Hour)
	e.check()
	if got := fp.take(); len(got) != 1 {
		t.Fatalf("not pushed an hour after the last one: %+v", got)
	}
}

func TestPushDeviceErrorsWaitFiveMinutesAndSayNothingOfThem(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "phone", "zh-CN")
	f := e.folder("docs")
	dev, tok := e.enrollDevice("laptop", "linux")
	e.mapFolder(dev, f, "/docs")
	poll := func(errs ...proto.FileError) {
		st, out := e.api(tok, "poll", proto.PollReq{ConfigVersion: metaInt(e.s.db, "config_version"), Cursors: map[int64]int64{f: e.seq(f)},
			Folders: []proto.FolderState{{ID: f, State: "error", Errors: errs}}})
		if st != 200 {
			t.Fatalf("poll %d %s", st, out)
		}
	}
	poll(proto.FileError{Path: "private/x.txt", Kind: proto.KindIO, Message: "disk full /home/u", Since: 1}, proto.FileError{Path: "a", Kind: proto.KindInUse})
	e.check()
	e.clk.Add(pushErrorsWait - time.Second)
	e.check()
	if len(fp.take()) != 0 {
		t.Fatal("pushed before the wait")
	}
	e.clk.Add(2 * time.Second)
	poll(proto.FileError{Path: "private/x.txt", Kind: proto.KindIO, Message: "disk full /home/u", Since: 1}, proto.FileError{Path: "a", Kind: proto.KindInUse})
	e.check()
	got := fp.take()
	if len(got) != 1 || got[0].msg["body"] != "laptop 在文件夹 docs 中报告了 1 个错误" {
		t.Fatalf("got %+v", got)
	}
	if b := got[0].msg["body"]; strings.Contains(b, "private") || strings.Contains(b, "disk full") {
		t.Errorf("the body tells what the device reported: %q", b)
	}
}

func TestBellAndPushFollowCurrentMappings(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "phone", "en")
	c := adminClient(e)
	f := e.folder("docs")
	dev, tok := e.enrollDevice("laptop", "linux")
	path := "/devices/" + itoa(dev) + "/mapping"
	changeMapping := func(action string) {
		t.Helper()
		if st, _ := e.post(c, path, url.Values{"folder": {itoa(f)}, "action": {action}, "path": {"/docs"}}); st != 303 {
			t.Fatalf("mapping %s: %d", action, st)
		}
	}
	pollError := func() {
		t.Helper()
		st, out := e.api(tok, "poll", proto.PollReq{ConfigVersion: -1, Folders: []proto.FolderState{{ID: f,
			Errors: []proto.FileError{{Path: "x", Kind: proto.KindIO}}}}})
		if st != 200 {
			t.Fatalf("poll: %d %s", st, out)
		}
	}
	check := func() {
		e.s.pushCheck(false)
		e.s.pushWG.Wait()
	}

	changeMapping("")
	e.heldFor(tok, f)
	pollError()
	check()
	if v := e.s.notifications(); v.Errors != 2 {
		t.Fatalf("bell on the mapping: %d errors, want two", v.Errors)
	}
	changeMapping("delete")
	pollError() // the client can still be using the old configuration
	if st, _ := e.api(tok, "held", proto.HeldReq{Folder: f}); st != http.StatusForbidden {
		t.Fatalf("held report on the removed mapping: %d", st)
	}
	e.clk.Add(pushErrorsWait)
	check()
	if v := e.s.notifications(); v.Errors != 0 {
		t.Fatalf("bell on the removed mapping: %d errors", v.Errors)
	}
	if got := fp.take(); len(got) != 0 || e.s.pushSent != 0 {
		t.Fatalf("removed mapping: %d pushes and %d hourly reservations", len(got), e.s.pushSent)
	}

	changeMapping("")
	e.heldFor(tok, f)
	pollError()
	check()
	if got := fp.take(); len(got) != 0 {
		t.Fatalf("remapping bypassed the wait: %d pushes", len(got))
	}
	e.clk.Add(pushErrorsWait)
	check()
	if got := fp.take(); len(got) != 2 {
		t.Fatalf("remapped folder got %d pushes, want two", len(got))
	}
}

func TestPushOfflineAfterThreeDaysAndServerCauses(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "phone", "en")
	e.enrollDevice("laptop", "linux")
	e.check()
	e.clk.Add(3*24*time.Hour - time.Minute)
	e.check()
	if len(fp.take()) != 0 {
		t.Fatal("pushed before 3 days")
	}
	e.clk.Add(2 * time.Minute)
	e.check()
	got := fp.take()
	if len(got) != 1 || got[0].msg["body"] != "Device laptop has not connected for 3 days" {
		t.Fatalf("offline: %+v", got)
	}
	old := freeSpace
	defer func() { freeSpace = old }()
	freeSpace = func(string) (uint64, bool) { return 10 << 20, true }
	e.check()
	got = fp.take()
	if len(got) != 1 || !strings.Contains(got[0].msg["body"], "below 256 MB") || got[0].msg["tag"] != "disk" {
		t.Fatalf("disk: %+v", got)
	}
	freeSpace = func(string) (uint64, bool) { return 10 << 30, true }
	e.check()
	if len(fp.take()) != 0 {
		t.Fatal("pushed a recovery")
	}
	e.s.gcDone(io.ErrUnexpectedEOF)
	e.s.setServerState(&e.s.certFailed, true)
	e.check()
	got = fp.take()
	tags := map[string]bool{}
	for _, p := range got {
		tags[p.msg["tag"]] = true
	}
	if len(got) != 2 || !tags["gc"] || !tags["cert"] {
		t.Fatalf("gc and cert: %+v", got)
	}
	if MetaString(e.s.db, "last_gc_err") == "" {
		t.Error("the GC failure was not kept for a restart")
	}
	e.s.gcDone(nil)
	if MetaString(e.s.db, "last_gc_err") != "" || e.s.gcFailed {
		t.Error("a good GC does not clear the failure")
	}
}

func TestPushOldClientIsPushedAtOnceAndShownInTheBell(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "phone", "en")
	dev, tok := e.enrollDevice("laptop", "linux")
	gen := e.s.uiGen
	b, _ := json.Marshal(proto.PollReq{})
	req, _ := newBearerRequest(e.ts.URL+"/api/v1/poll", tok, b)
	req.Header.Set(proto.VersionHeader, "1")
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("an old client was served")
	}
	if e.s.uiGen == gen {
		t.Error("the web UI was not told")
	}
	v := e.s.notifications()
	if len(v.Old) != 1 || v.Old[0].ID != dev || v.Errors != 1 {
		t.Fatalf("bell %+v", v)
	}
	e.check()
	if got := fp.take(); len(got) != 1 || got[0].msg["body"] != "The client of device laptop is too old, sync has stopped" {
		t.Fatalf("got %+v", got)
	}
}

func TestPushTemporaryFailureRetriesAtOnceThenHourlyAndGoneMarksTheSub(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "flaky", "en")
	fp.sub(e, "gone", "en")
	fp.sub(e, "fine", "en")
	fp.setStatus("/flaky", 503)
	fp.setStatus("/gone", 410)
	e.s.setServerState(&e.s.gcFailed, true)
	e.check()
	if got := fp.take(); len(got) != 3 {
		t.Fatalf("first round: %+v", got)
	}
	subs := e.s.subscriptions()
	if !subs[1].Gone || subs[0].Gone || subs[2].Gone || subs[1].Last.Status != 410 || subs[0].Last.Status != 503 {
		t.Fatalf("subs %+v", subs)
	}
	v := e.s.notifications()
	if len(v.Server) != 2 || v.Server[1].Kind != "pushgone" || v.Server[1].Name != "gone" || v.Errors != 1 || v.Warnings != 1 {
		t.Fatalf("the bell: %+v", v)
	}
	e.check() // too early to retry
	if len(fp.take()) != 0 {
		t.Fatal("retried at once")
	}
	e.clk.Add(pushRetryFirst + time.Second)
	e.check()
	if got := fp.take(); len(got) != 1 || got[0].path != "/flaky" {
		t.Fatalf("first retry: %+v", got)
	}
	e.clk.Add(10 * time.Minute)
	e.check()
	if len(fp.take()) != 0 {
		t.Fatal("second retry before an hour")
	}
	fp.setStatus("/flaky", 201)
	e.clk.Add(time.Hour)
	e.check()
	if got := fp.take(); len(got) != 1 {
		t.Fatalf("hourly retry: %+v", got)
	}
	e.clk.Add(time.Hour)
	e.check()
	if len(fp.take()) != 0 {
		t.Fatal("delivered ones are sent again")
	}
}

func TestPushCapAndNoSubscriptions(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	e.s.setServerState(&e.s.gcFailed, true)
	e.check() // no subscription: nothing to do, and no panic
	for i := range 25 {
		fp.sub(e, "b"+itoa(int64(i)), "en")
	}
	// only maxSubs are allowed through the API, but the cap must hold whatever the list holds
	e.check()
	if got := fp.take(); len(got) != pushCap {
		t.Fatalf("the cap: %d pushes, want %d", len(got), pushCap)
	}
	e.check()
	if got := fp.take(); len(got) != 0 {
		t.Fatalf("over the cap: %d", len(got))
	}
	e.clk.Add(pushRepeat + time.Minute)
	e.check()
	if got := fp.take(); len(got) != 5 {
		t.Fatalf("the rest after the hour: %d", len(got))
	}
}

func TestBellListsTheServersOwnProblemsAndOfflineAfterThreeDays(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.enrollDevice("laptop", "linux")
	if v := e.s.notifications(); v.Total() != 0 {
		t.Fatalf("a quiet server has notifications: %+v", v)
	}
	e.clk.Add(3*24*time.Hour + time.Minute)
	e.s.setServerState(&e.s.diskLow, true)
	e.s.setServerState(&e.s.gcFailed, true)
	e.s.setServerState(&e.s.certFailed, true)
	v := e.s.notifications()
	if len(v.Offline) != 1 || len(v.Server) != 3 || v.Errors != 3 || v.Warnings != 1 {
		t.Fatalf("bell %+v", v)
	}
	c := e.webClient()
	e.setAdminPassword("pw")
	e.login(c, "pw")
	_, page := e.get(c, "/")
	for _, want := range []string{"服务端数据盘剩余空间低于 256 MB", "服务端每日清理 (GC) 失败", "服务端证书无法续期", `id="ng-srv"`, "3 天未连接"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	e.s.setServerState(&e.s.diskLow, false)
	if v := e.s.notifications(); len(v.Server) != 2 {
		t.Errorf("the disk notice stays: %+v", v.Server)
	}
}

func TestPushLoopRunsAndStops(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "phone", "en")
	e.s.setServerState(&e.s.gcFailed, true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.s.pushLoop(ctx); close(done) }()
	defer func() { cancel(); <-done; e.s.pushWG.Wait() }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fp.mu.Lock()
		n := len(fp.got)
		fp.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the loop pushed nothing")
}

func TestResubscribingGetsTheCausesStillActive(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "phone", "en")
	fp.setStatus("/phone", 410)
	e.s.setServerState(&e.s.gcFailed, true)
	e.check()
	if len(fp.take()) != 1 {
		t.Fatal("no first push")
	}
	fp.setStatus("/phone", 201)
	e.s.forgetSub(subID(fp.srv.URL + "/phone")) // what POST /push/subscribe does for the same endpoint
	e.editSubsGone(false)
	e.clk.Add(time.Minute)
	e.check()
	if got := fp.take(); len(got) != 1 {
		t.Fatalf("not pushed after subscribing again: %+v", got)
	}
}

func (e *env) editSubsGone(gone bool) {
	e.s.editSubs(func(l []pushSub) ([]pushSub, error) {
		for i := range l {
			l[i].Gone = gone
		}
		return l, nil
	})
}

// The mapping snapshot of the push checks is read outside s.mu (design 4.3): the global lock stays free while the
// query waits for the database.
func TestNoticeMappingsQueryOutsideLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := e.folder("docs")
	dev, _ := e.enrollDevice("laptop", "linux")
	if st, _ := e.post(adminClient(e), "/devices/"+itoa(dev)+"/mapping", url.Values{"folder": {itoa(f)}, "action": {"add"}, "path": {"/docs"}}); st != 303 {
		t.Fatalf("map: %d", st)
	}
	e.s.db.SetMaxOpenConns(1)
	conn, err := e.s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.s.mu.Lock()
	e.s.dropNoticeMappingsLocked()
	e.s.mu.Unlock()
	done := make(chan map[noticeMapping]bool, 1)
	go func() { m, _ := e.s.noticeMappingSnapshot(); done <- m }()
	time.Sleep(20 * time.Millisecond) // the query is waiting for the only connection
	if !e.s.mu.TryLock() {
		conn.Close()
		t.Fatal("s.mu is held while the mapping query runs")
	}
	e.s.mu.Unlock()
	conn.Close()
	if m := <-done; !m[noticeMapping{dev, f}] {
		t.Fatalf("snapshot %v lacks the mapping", m)
	}
}
