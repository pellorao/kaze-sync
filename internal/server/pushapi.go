package server

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// The subscriptions of the user's browsers and the endpoints that manage them (design 4.3).
// Subscriptions live in meta.push_subs as JSON: no table, no schema change.

const (
	maxSubs      = 10   // at most this many subscriptions, the invalid ones included
	maxEndpoint  = 2048 // bytes of an endpoint URL
	maxBrowser   = 32   // characters of a browser's short name
	pushBodyMax  = 8 << 10
	pushSubsMeta = "push_subs"
)

// pushSub is one subscription. The endpoint is a credential: it is stored here and nowhere else, and the logs
// carry only the host.
type pushSub struct {
	ID       string    `json:"id"` // the first 16 hex digits of the SHA-256 of the endpoint
	Endpoint string    `json:"endpoint"`
	P256dh   string    `json:"p256dh"`
	Auth     string    `json:"auth"`
	Lang     string    `json:"lang"`
	Browser  string    `json:"browser"`
	At       int64     `json:"at"`             // when it was made
	Gone     bool      `json:"gone,omitempty"` // the push service has forgotten it (404 or 410)
	Last     *pushLast `json:"last,omitempty"` // the last push to it
}

type pushLast struct {
	Status int    `json:"status"` // the push service's HTTP status; 0: no answer
	Ms     int64  `json:"ms"`
	At     int64  `json:"at"`
	Err    string `json:"err,omitempty"`
}

// subID is the id of a subscription: the endpoint hashed, so the page can find its own without the endpoint coming back.
func subID(endpoint string) string {
	h := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(h[:8])
}

func pushHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Host
	}
	return ""
}

// subscriptions returns a copy of the subscription list.
func (s *Server) subscriptions() []pushSub {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	s.loadSubsLocked()
	return append([]pushSub(nil), s.subList...)
}

func (s *Server) loadSubsLocked() {
	if s.subsLoaded {
		return
	}
	s.subsLoaded = true
	if v := MetaString(s.db, pushSubsMeta); v != "" {
		if err := json.Unmarshal([]byte(v), &s.subList); err != nil {
			slog.Error("push: meta.push_subs is unreadable", "err", err)
			s.subList = nil
		}
	}
}

// editSubs changes the list under the lock and stores it. Every change goes through here, so two of them never
// overwrite each other; no network request is made inside.
func (s *Server) editSubs(f func([]pushSub) ([]pushSub, error)) error {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	s.loadSubsLocked()
	list, err := f(append([]pushSub(nil), s.subList...))
	if err != nil {
		return err
	}
	b, _ := json.Marshal(list)
	if err := SetMeta(s.db, pushSubsMeta, string(b)); err != nil {
		return err
	}
	s.subList = list
	return nil
}

// recordPush stores the result of a push on its subscription; 404 and 410 mark it invalid, which the bell shows.
func (s *Server) recordPush(id string, res pushResult) {
	gone := false
	err := s.editSubs(func(l []pushSub) ([]pushSub, error) {
		for i := range l {
			if l[i].ID == id {
				l[i].Last = &pushLast{Status: res.Status, Ms: res.Took.Milliseconds(), At: s.now().Unix(), Err: res.Err}
				if res.Gone() && !l[i].Gone {
					l[i].Gone, gone = true, true
				}
			}
		}
		return l, nil
	})
	if err != nil {
		slog.Warn("push: cannot store the result", "err", err)
	}
	if gone {
		s.uiBump()
	}
}

// browserName is a short name for the browser of a request, from its User-Agent.
func browserName(ua string) string {
	b := "Browser"
	for _, c := range []struct{ tag, name string }{{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"SamsungBrowser/", "Samsung"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"}} {
		if strings.Contains(ua, c.tag) {
			b = c.name
			break
		}
	}
	for _, c := range []struct{ tag, name string }{{"Android", "Android"}, {"iPhone", "iOS"}, {"iPad", "iOS"}, {"Windows", "Windows"}, {"Macintosh", "macOS"}, {"Linux", "Linux"}} {
		if strings.Contains(ua, c.tag) {
			return b + " (" + c.name + ")"
		}
	}
	return b
}

// cleanName keeps a browser name the caller gave short and printable.
func cleanName(n string) string {
	n = strings.TrimSpace(n)
	var out []rune
	for _, r := range n {
		if r >= ' ' && r != 0x7f && len(out) < maxBrowser {
			out = append(out, r)
		}
	}
	return string(out)
}

// pushError is the answer to a request that cannot be served.
func pushError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// pushAPI wraps a handler of the push endpoints: an administrator session is required, and a missing one is a 401
// (these are called by script, not followed by a browser).
func (s *Server) pushAPI(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hasSession(r) {
			pushError(w, http.StatusUnauthorized, "login")
			return
		}
		if s.vapid == nil {
			pushError(w, http.StatusInternalServerError, "no_vapid_key")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

// tlsMode says how this server gets its certificate: "ca", "file" or "selfsigned". Browsers register a service worker
// only on a trusted certificate, so Web Push needs the first two (design 4.3).
func (s *Server) tlsMode() string {
	switch {
	case MetaString(s.db, "cert") != "":
		return "file"
	case MetaString(s.db, "tls") == "ca":
		return "ca"
	}
	return "selfsigned"
}

type subView struct {
	ID      string    `json:"id"`
	Browser string    `json:"browser"`
	At      int64     `json:"at"`
	Gone    bool      `json:"gone"`
	Last    *pushLast `json:"last"`
}

// pushList is GET /push/subscriptions: what the settings card shows.
//
//	{"available": bool, "mode": "ca"|"file"|"selfsigned", "key": "<applicationServerKey>", "max": 10,
//	 "subs": [{"id", "browser", "at", "gone", "last": {"status", "ms", "at", "err"} | null}]}
func (s *Server) pushList(w http.ResponseWriter, r *http.Request) {
	mode := s.tlsMode()
	subs := []subView{}
	for _, sub := range s.subscriptions() {
		subs = append(subs, subView{sub.ID, sub.Browser, sub.At, sub.Gone, sub.Last})
	}
	writeJSON(w, map[string]any{"available": mode != "selfsigned", "mode": mode, "key": vapidPublic(s.vapid), "max": maxSubs, "subs": subs})
}

// pushSubscribe is POST /push/subscribe with the browser's PushSubscription as JSON (`subscription.toJSON()`) and an
// optional "name" for the browser; the page language is the request's. It answers {"id"}. 400 for a malformed
// subscription, 409 {"error":"full"} when there are already maxSubs.
func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, pushBodyMax)).Decode(&req); err != nil {
		pushError(w, http.StatusBadRequest, "bad_request")
		return
	}
	u, err := url.Parse(req.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(req.Endpoint) > maxEndpoint {
		pushError(w, http.StatusBadRequest, "bad_endpoint")
		return
	}
	pub, err1 := decodeB64(req.Keys.P256dh)
	auth, err2 := decodeB64(req.Keys.Auth)
	if _, err3 := ecdh.P256().NewPublicKey(pub); err1 != nil || err2 != nil || err3 != nil || len(auth) != 16 {
		pushError(w, http.StatusBadRequest, "bad_keys")
		return
	}
	name := cleanName(req.Name)
	if name == "" || !utf8.ValidString(name) {
		name = browserName(r.UserAgent())
	}
	sub := pushSub{ID: subID(req.Endpoint), Endpoint: req.Endpoint, P256dh: req.Keys.P256dh, Auth: req.Keys.Auth, Lang: requestLang(r), Browser: name, At: s.now().Unix()}
	err = s.editSubs(func(l []pushSub) ([]pushSub, error) {
		for i := range l {
			if l[i].ID == sub.ID { // the same browser again: new keys and language, valid again
				sub.At = l[i].At
				l[i] = sub
				return l, nil
			}
		}
		if len(l) >= maxSubs {
			return nil, errFull
		}
		return append(l, sub), nil
	})
	switch {
	case errors.Is(err, errFull):
		pushError(w, http.StatusConflict, "full")
		return
	case err != nil:
		slog.Error("push: cannot store the subscription", "err", err)
		pushError(w, http.StatusInternalServerError, "store")
		return
	}
	s.forgetSub(sub.ID)
	slog.Info("push: subscribed", "host", pushHost(sub.Endpoint), "browser", sub.Browser)
	s.uiBump()
	writeJSON(w, map[string]string{"id": sub.ID})
}

var errFull = errors.New("too many subscriptions")

// idOf reads the subscription a request names: {"id"} or {"endpoint"}.
func idOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		ID       string `json:"id"`
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, pushBodyMax)).Decode(&req); err != nil || (req.ID == "" && req.Endpoint == "") {
		pushError(w, http.StatusBadRequest, "bad_request")
		return "", false
	}
	if req.ID == "" {
		req.ID = subID(req.Endpoint)
	}
	return req.ID, true
}

// pushUnsubscribe is POST /push/unsubscribe with {"id"} (from the list) or {"endpoint"} (the browser's own); it
// serves both turning push off in a browser and deleting a row of the list. Answers 204, also for an unknown one.
func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	id, ok := idOf(w, r)
	if !ok {
		return
	}
	err := s.editSubs(func(l []pushSub) ([]pushSub, error) {
		out := l[:0]
		for _, sub := range l {
			if sub.ID != id {
				out = append(out, sub)
			}
		}
		return out, nil
	})
	if err != nil {
		pushError(w, http.StatusInternalServerError, "store")
		return
	}
	s.uiBump()
	w.WriteHeader(http.StatusNoContent)
}

// pushTest is POST /push/test with {"id"}: a test push to that one subscription, outside the checks, the limits and
// the once-only rule. Answers {"status", "ms", "err"} (status 0: the push service did not answer); 404 for an unknown
// id.
func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	id, ok := idOf(w, r)
	if !ok {
		return
	}
	var sub *pushSub
	for _, c := range s.subscriptions() {
		if c.ID == id {
			sub = &c
			break
		}
	}
	if sub == nil {
		pushError(w, http.StatusNotFound, "unknown")
		return
	}
	res := s.sendTo(r.Context(), *sub, pushPayload(trFor(sub.Lang).T("push.test"), "test"), "")
	s.recordPush(sub.ID, res)
	writeJSON(w, map[string]any{"status": res.Status, "ms": res.Took.Milliseconds(), "err": res.Err})
}

// serviceWorker serves /sw.js. It needs no session: the browser's update check fetches it without cookies on some
// platforms, and it holds nothing private.
func serviceWorker(w http.ResponseWriter, r *http.Request) {
	b, _ := webFS.ReadFile("web/sw.js")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "sw.js", time.Time{}, strings.NewReader(string(b)))
}

func (s *Server) pushRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /sw.js", serviceWorker)
	mux.HandleFunc("GET /push/subscriptions", s.pushAPI(s.pushList))
	mux.HandleFunc("POST /push/subscribe", s.pushAPI(s.pushSubscribe))
	mux.HandleFunc("POST /push/unsubscribe", s.pushAPI(s.pushUnsubscribe))
	mux.HandleFunc("POST /push/test", s.pushAPI(s.pushTest))
}
