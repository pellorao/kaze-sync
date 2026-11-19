package server

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// pushReq sends a JSON request of the push API as the browser would.
func pushReq(t *testing.T, e *env, c *http.Client, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func browserSub(endpoint string) map[string]any {
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	return map[string]any{"endpoint": endpoint, "keys": map[string]string{"p256dh": b64.EncodeToString(k.PublicKey().Bytes()), "auth": b64.EncodeToString(auth)}}
}

func adminClient(e *env) *http.Client {
	c := e.webClient()
	e.setAdminPassword("pw")
	e.login(c, "pw")
	return c
}

func TestPushAPIRequiresASession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.webClient()
	for _, r := range [][2]string{{"GET", "/push/subscriptions"}, {"POST", "/push/subscribe"}, {"POST", "/push/unsubscribe"}, {"POST", "/push/test"}} {
		if st, out := pushReq(t, e, c, r[0], r[1], map[string]any{}); st != 401 || out["error"] != "login" {
			t.Errorf("%s %s: %d %v", r[0], r[1], st, out)
		}
	}
}

func TestPushAPISubscribeListTestUnsubscribe(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	c := adminClient(e)
	st, out := pushReq(t, e, c, "GET", "/push/subscriptions", nil)
	if st != 200 || out["available"] != false || out["mode"] != "selfsigned" || out["key"] != vapidPublic(e.s.vapid) || out["max"] != float64(10) || len(out["subs"].([]any)) != 0 {
		t.Fatalf("empty list: %d %v", st, out)
	}
	SetMeta(e.s.db, "tls", "ca")
	if _, out = pushReq(t, e, c, "GET", "/push/subscriptions", nil); out["available"] != true || out["mode"] != "ca" {
		t.Fatalf("ca mode: %v", out)
	}
	// subscribe with a real key pair the fake service can decrypt for
	sub := fp.sub(e, "tmp", "en")
	e.s.editSubs(func([]pushSub) ([]pushSub, error) { return nil, nil })
	body := map[string]any{"endpoint": sub.Endpoint, "keys": map[string]string{"p256dh": sub.P256dh, "auth": sub.Auth}, "name": "My phone"}
	req, _ := http.NewRequest("POST", e.ts.URL+"/push/subscribe", bytes.NewReader(mustJSON(body)))
	req.Header.Set("Accept-Language", "ja")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 14) Chrome/130.0 Mobile Safari/537.36")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var sr map[string]string
	json.NewDecoder(resp.Body).Decode(&sr)
	resp.Body.Close()
	if resp.StatusCode != 200 || sr["id"] != sub.ID {
		t.Fatalf("subscribe: %d %v", resp.StatusCode, sr)
	}
	stored := e.s.subscriptions()
	if len(stored) != 1 || stored[0].Lang != "ja" || stored[0].Browser != "My phone" || stored[0].Endpoint != sub.Endpoint {
		t.Fatalf("stored %+v", stored)
	}
	// without a name the browser is named from its User-Agent; subscribing again replaces, not adds
	delete(body, "name")
	req, _ = http.NewRequest("POST", e.ts.URL+"/push/subscribe", bytes.NewReader(mustJSON(body)))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 14) Chrome/130.0 Mobile Safari/537.36")
	resp, _ = c.Do(req)
	resp.Body.Close()
	if stored = e.s.subscriptions(); len(stored) != 1 || stored[0].Browser != "Chrome (Android)" {
		t.Fatalf("resubscribe %+v", stored)
	}
	st, out = pushReq(t, e, c, "GET", "/push/subscriptions", nil)
	row := out["subs"].([]any)[0].(map[string]any)
	if row["id"] != sub.ID || row["browser"] != "Chrome (Android)" || row["gone"] != false || row["last"] != nil {
		t.Fatalf("list row %v", row)
	}
	if strings.Contains(mustJSONString(out), "endpoint") || strings.Contains(mustJSONString(out), sub.Auth) {
		t.Error("the list gives out the endpoint or the keys")
	}
	// the test push reaches only that subscription and is recorded on it
	st, out = pushReq(t, e, c, "POST", "/push/test", map[string]string{"id": sub.ID})
	if st != 200 || out["status"] != float64(201) {
		t.Fatalf("test: %d %v", st, out)
	}
	if got := fp.take(); len(got) != 1 || !strings.HasPrefix(got[0].msg["tag"], "test-") {
		t.Fatalf("test push: %+v", got)
	}
	if l := e.s.subscriptions()[0].Last; l == nil || l.Status != 201 {
		t.Fatalf("result not recorded: %+v", l)
	}
	if st, _ = pushReq(t, e, c, "POST", "/push/test", map[string]string{"id": "nope"}); st != 404 {
		t.Errorf("unknown test: %d", st)
	}
	fp.setStatus("/tmp", 410)
	pushReq(t, e, c, "POST", "/push/test", map[string]string{"id": sub.ID})
	if _, out = pushReq(t, e, c, "GET", "/push/subscriptions", nil); out["subs"].([]any)[0].(map[string]any)["gone"] != true {
		t.Error("a 410 does not mark the subscription invalid")
	}
	// the browser turns push off by its endpoint; a row is deleted by id
	st, _ = pushReq(t, e, c, "POST", "/push/unsubscribe", map[string]string{"endpoint": sub.Endpoint})
	if st != 204 || len(e.s.subscriptions()) != 0 {
		t.Fatalf("unsubscribe: %d %+v", st, e.s.subscriptions())
	}
	if st, _ = pushReq(t, e, c, "POST", "/push/unsubscribe", map[string]string{"id": sub.ID}); st != 204 {
		t.Errorf("unsubscribing twice: %d", st)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func mustJSONString(v any) string { return string(mustJSON(v)) }

func TestPushAPIRefusesBadAndTooManySubscriptions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := adminClient(e)
	good := browserSub("https://push.example/a")
	bad := map[string]map[string]any{
		"http endpoint": browserSub("http://push.example/a"),
		"no host":       browserSub("https:///a"),
		"long endpoint": browserSub("https://push.example/" + strings.Repeat("a", maxEndpoint)),
		"bad p256dh":    {"endpoint": "https://push.example/b", "keys": map[string]string{"p256dh": "AAAA", "auth": good["keys"].(map[string]string)["auth"]}},
		"bad auth":      {"endpoint": "https://push.example/b", "keys": map[string]string{"p256dh": good["keys"].(map[string]string)["p256dh"], "auth": "AAAA"}},
	}
	for name, b := range bad {
		if st, out := pushReq(t, e, c, "POST", "/push/subscribe", b); st != 400 {
			t.Errorf("%s: %d %v", name, st, out)
		}
	}
	if st, _ := pushReq(t, e, c, "POST", "/push/subscribe", nil); st != 400 {
		t.Errorf("empty body: %d", st)
	}
	for i := range maxSubs {
		if st, out := pushReq(t, e, c, "POST", "/push/subscribe", browserSub("https://push.example/"+itoa(int64(i)))); st != 200 {
			t.Fatalf("subscription %d: %d %v", i, st, out)
		}
	}
	if st, out := pushReq(t, e, c, "POST", "/push/subscribe", browserSub("https://push.example/one-too-many")); st != 409 || out["error"] != "full" {
		t.Fatalf("over the limit: %d %v", st, out)
	}
	// a subscription that went invalid still takes a place
	if len(e.s.subscriptions()) != maxSubs {
		t.Fatal("wrong count")
	}
}

func TestServiceWorkerNeedsNoSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	st, body := e.get(e.webClient(), "/sw.js")
	if st != 200 || !strings.Contains(body, `addEventListener("push"`) || !strings.Contains(body, "notificationclick") || !strings.Contains(body, "clients.claim()") || !strings.Contains(body, "renotify: !!d.tag") {
		t.Fatalf("sw.js: %d %.80s", st, body)
	}
	resp, _ := e.webClient().Get(e.ts.URL + "/sw.js")
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("content type %q", ct)
	}
}

func TestSubscriptionsSurviveAndOffGivesNoPush(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	fp.sub(e, "phone", "en")
	// a new Server on the same database reads the same subscriptions and the same key
	s2 := newServer(e.s.db, e.s.varDir, e.s.blobs, e.s.now)
	if got := s2.subscriptions(); len(got) != 1 || got[0].Browser != "phone" {
		t.Fatalf("after a restart: %+v", got)
	}
	if !s2.vapid.Equal(e.s.vapid) {
		t.Fatal("the VAPID key changed")
	}
	s2.vapid = nil // no key: push is off and nothing panics
	s2.pushCheck(true)
}

func TestPushLateGoneDoesNotInvalidateANewSubscription(t *testing.T) {
	e := newEnv(t)
	fp := newFakePush(t)
	c := adminClient(e)
	if err := SetMeta(e.s.db, "tls", "ca"); err != nil {
		t.Fatal(err)
	}
	sub := fp.sub(e, "phone", "en")
	fp.setStatus("/phone", 410)
	started, unblock := fp.pause("/phone")
	defer func() { unblock(); e.s.pushWG.Wait() }()
	e.s.setServerState(&e.s.gcFailed, true)
	e.s.pushCheck(false)
	waitForPush(t, started)

	body := map[string]any{"endpoint": sub.Endpoint, "keys": map[string]string{"p256dh": sub.P256dh, "auth": sub.Auth}}
	if status, out := pushReq(t, e, c, "POST", "/push/subscribe", body); status != 200 {
		t.Fatalf("resubscribe: %d %v", status, out)
	}
	unblock()
	e.s.pushWG.Wait()
	if cur := e.s.subscriptions()[0]; cur.Gone || cur.Last != nil {
		t.Fatalf("late result updated the new subscription: gone=%v last=%+v", cur.Gone, cur.Last)
	}
	fp.take()
	fp.setStatus("/phone", 201)
	e.check()
	if got := fp.take(); len(got) != 1 {
		t.Fatalf("new subscription received %d pushes, want one", len(got))
	}
}

// The push card's helpers (which problem it names, this browser's subscription id, the server key) are tested with node's
// built-in runner; the id must be the server's subID of the same endpoint.
func TestPushCardScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	ep := "https://fcm.googleapis.com/fcm/send/dXk3:APA91b-Hx_y"
	cmd := exec.Command(node, "--test", "push.test.js")
	cmd.Dir = "jstest"
	cmd.Env = append(os.Environ(), "KZ_PUSH_ENDPOINT="+ep, "KZ_PUSH_ID="+subID(ep))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// The settings page carries the push card (filled by app.js) with a no-JavaScript line, and the bell's warning about an
// invalid subscription links to the card.
func TestSettingsHasThePushCard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	_, body := e.get(c, "/settings")
	for _, want := range []string{`id="push" data-pushcard`, "data-push-nojs", `role="switch"`, "data-push-toggle", "data-push-list"} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page lacks %s", want)
		}
	}
	lay, _ := fs.ReadFile(webFS, "web/layout.html")
	if !strings.Contains(string(lay), `"notif.srv.pushgone" "href" "/settings#push"`) {
		t.Error("the bell's pushgone item does not link to the push card")
	}
}
