package server

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Web Push to a browser's push service (design 4.3): the payload is encrypted as RFC 8291 says (aes128gcm) with the
// standard library only, the request is authorized by a VAPID JWT (RFC 8292).

const (
	pushTimeout = 10 * time.Second // one POST to a push service
	pushTTL     = 24 * 3600        // the push service keeps a message for a phone that is off for this long
	vapidSub    = "https://github.com/pellorao/kaze-sync"
	jwtLife     = 12 * time.Hour
	jwtRenew    = time.Hour // a cached JWT with less than this left is signed again
	recordSize  = 4096
)

// pushClient sends the requests to the push services; tests replace it. It honours HTTPS_PROXY.
var pushClient = &http.Client{Timeout: pushTimeout}

// SetPushClient replaces the client that sends the pushes; the end-to-end driver points it at its fake push service.
func SetPushClient(c *http.Client) { pushClient = c }

var b64 = base64.RawURLEncoding

// decodeB64 reads base64url with or without padding.
func decodeB64(s string) ([]byte, error) {
	for len(s) > 0 && s[len(s)-1] == '=' {
		s = s[:len(s)-1]
	}
	return b64.DecodeString(s)
}

// encryptPush encrypts plain for the browser whose subscription holds uaPub (65-byte P-256 point) and auth (16 bytes).
// asPriv and salt are the sender's key and salt (random in production, fixed in the RFC's test vector).
func encryptPush(plain, uaPub, auth []byte, asPriv *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	ua, err := ecdh.P256().NewPublicKey(uaPub)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	if len(auth) != 16 || len(salt) != 16 {
		return nil, errors.New("auth and salt must be 16 bytes")
	}
	secret, err := asPriv.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPub := asPriv.PublicKey().Bytes()
	prk, err := hkdf.Extract(sha256.New, secret, auth)
	if err != nil {
		return nil, err
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPub...), asPub...)
	ikm, err := hkdf.Expand(sha256.New, prk, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	prk, err = hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	if len(plain) > recordSize-21-len(asPub)-17 { // one record, and the whole body within the 4096 bytes a push service accepts
		return nil, errors.New("payload too long")
	}
	blk, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(salt)
	binary.Write(&out, binary.BigEndian, uint32(recordSize))
	out.WriteByte(byte(len(asPub)))
	out.Write(asPub)
	return gcm.Seal(out.Bytes(), nonce, append(append([]byte{}, plain...), 2), nil), nil
}

// decryptPush is the receiving side, for tests (and for the daily check's receiver).
func decryptPush(body, uaPriv, auth []byte) ([]byte, error) {
	if len(body) < 16+4+1+65+17 {
		return nil, errors.New("short body")
	}
	salt, idlen := body[:16], int(body[20])
	if len(body) < 21+idlen+17 {
		return nil, errors.New("short body")
	}
	asPubB, ct := body[21:21+idlen], body[21+idlen:]
	priv, err := ecdh.P256().NewPrivateKey(uaPriv)
	if err != nil {
		return nil, err
	}
	asPub, err := ecdh.P256().NewPublicKey(asPubB)
	if err != nil {
		return nil, err
	}
	secret, err := priv.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	prk, _ := hkdf.Extract(sha256.New, secret, auth)
	keyInfo := append(append([]byte("WebPush: info\x00"), priv.PublicKey().Bytes()...), asPubB...)
	ikm, _ := hkdf.Expand(sha256.New, prk, string(keyInfo), 32)
	prk, _ = hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	blk, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(blk)
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(pt, 2)
	if i < 0 {
		return nil, errors.New("no record delimiter")
	}
	return pt[:i], nil
}

// vapidKey returns the server's VAPID key, creating it when the database has none. It runs before the server takes
// requests, so two keys are never made. The private key (PKCS #8) lives in meta.vapid_key and nowhere else.
func vapidKey(db *sql.DB) (*ecdsa.PrivateKey, error) {
	if v := MetaString(db, "vapid_key"); v != "" {
		der, err := decodeB64(v)
		if err != nil {
			return nil, err
		}
		k, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, err
		}
		if ek, ok := k.(*ecdsa.PrivateKey); ok && ek.Curve == elliptic.P256() {
			return ek, nil
		}
		return nil, errors.New("meta.vapid_key is not a P-256 key")
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := SetMeta(db, "vapid_key", b64.EncodeToString(der)); err != nil {
		return nil, err
	}
	return k, nil
}

// vapidPublic is the key a browser subscribes with (applicationServerKey): the uncompressed point, base64url.
func vapidPublic(k *ecdsa.PrivateKey) string {
	e, err := k.PublicKey.ECDH()
	if err != nil {
		return ""
	}
	return b64.EncodeToString(e.Bytes())
}

// signJWT makes the ES256 JWT of a VAPID request to a push service of origin aud.
func signJWT(k *ecdsa.PrivateKey, aud string, exp time.Time) (string, error) {
	claims, _ := json.Marshal(map[string]any{"aud": aud, "exp": exp.Unix(), "sub": vapidSub})
	msg := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, k, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return msg + "." + b64.EncodeToString(sig), nil
}

// verifyJWT checks a token made by signJWT and returns its claims (tests).
func verifyJWT(pub *ecdsa.PublicKey, tok string) (map[string]any, error) {
	var parts [3]string
	n := 0
	for i, start := 0, 0; i <= len(tok); i++ {
		if i == len(tok) || tok[i] == '.' {
			if n == 3 {
				return nil, errors.New("too many parts")
			}
			parts[n], start = tok[start:i], i+1
			n++
		}
	}
	if n != 3 {
		return nil, errors.New("not a JWT")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, errors.New("bad signature")
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, errors.New("signature does not verify")
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c map[string]any
	return c, json.Unmarshal(raw, &c)
}

// jwtFor is the cached JWT for a push service origin: one per origin, signed again when less than jwtRenew is left.
func (s *Server) jwtFor(aud string) (string, error) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	if c, ok := s.jwts[aud]; ok && c.exp.Sub(s.now()) >= jwtRenew {
		return c.tok, nil
	}
	exp := s.now().Add(jwtLife)
	tok, err := signJWT(s.vapid, aud, exp)
	if err != nil {
		return "", err
	}
	s.jwts[aud] = jwtCache{tok, exp}
	return tok, nil
}

type jwtCache struct {
	tok string
	exp time.Time
}

// pushResult is how one POST to a push service ended.
type pushResult struct {
	Status int           // the HTTP status; 0 when the request did not get an answer
	Took   time.Duration //
	Err    string        // what failed when Status is 0 (no URL: it is a credential)
}

// Temporary says that trying again later may work (design 4.3 "去重与限流").
func (r pushResult) Temporary() bool {
	return r.Status == 0 || r.Status == http.StatusTooManyRequests || r.Status >= 500
}

func (r pushResult) OK() bool { return r.Status/100 == 2 }
func (r pushResult) Gone() bool {
	return r.Status == http.StatusNotFound || r.Status == http.StatusGone
}

// sendTo sends one encrypted payload to one subscription; topic replaces an older message about the same cause that
// the phone has not received yet.
func (s *Server) sendTo(ctx context.Context, sub pushSub, payload []byte, topic string) pushResult {
	start := time.Now()
	fail := func(msg string) pushResult { return pushResult{Took: time.Since(start), Err: msg} }
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fail("bad endpoint")
	}
	uaPub, err1 := decodeB64(sub.P256dh)
	auth, err2 := decodeB64(sub.Auth)
	if err1 != nil || err2 != nil {
		return fail("bad subscription keys")
	}
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return fail("key generation")
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	body, err := encryptPush(payload, uaPub, auth, asPriv, salt)
	if err != nil {
		return fail(err.Error())
	}
	tok, err := s.jwtFor(u.Scheme + "://" + u.Host)
	if err != nil {
		return fail("signing")
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fail("request")
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(pushTTL))
	req.Header.Set("Urgency", "normal")
	if topic != "" {
		req.Header.Set("Topic", topic)
	}
	req.Header.Set("Authorization", "vapid t="+tok+", k="+vapidPublic(s.vapid))
	resp, err := pushClient.Do(req)
	if err != nil { // the error text holds the URL: not kept
		if errors.Is(err, context.DeadlineExceeded) {
			return fail("timeout")
		}
		return fail("connection failed")
	}
	resp.Body.Close()
	return pushResult{Status: resp.StatusCode, Took: time.Since(start)}
}
