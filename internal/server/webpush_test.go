package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decodeB64(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The example of RFC 8291 section 5 and appendix A.
const (
	rfcPlain   = "When I grow up, I want to be a watermelon"
	rfcUAPriv  = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	rfcUAPub   = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	rfcASPriv  = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	rfcAuth    = "BTBZMqHH6r4Tts7J_aSIgg"
	rfcSalt    = "DGv6ra1nlYgDCS1FRnbzlw"
	rfcMessage = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
)

func TestEncryptMatchesRFC8291Vector(t *testing.T) {
	t.Parallel()
	asPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, rfcASPriv))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encryptPush([]byte(rfcPlain), mustB64(t, rfcUAPub), mustB64(t, rfcAuth), asPriv, mustB64(t, rfcSalt))
	if err != nil {
		t.Fatal(err)
	}
	if b64.EncodeToString(got) != rfcMessage {
		t.Fatalf("ciphertext differs from the RFC's:\n got %s\nwant %s", b64.EncodeToString(got), rfcMessage)
	}
	pt, err := decryptPush(mustB64(t, rfcMessage), mustB64(t, rfcUAPriv), mustB64(t, rfcAuth))
	if err != nil || string(pt) != rfcPlain {
		t.Fatalf("decrypting the RFC's message: %q %v", pt, err)
	}
}

func TestEncryptRoundTripAndRejects(t *testing.T) {
	t.Parallel()
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	as, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth, salt := make([]byte, 16), make([]byte, 16)
	rand.Read(auth)
	rand.Read(salt)
	msg := `{"title":"kaze-sync","body":"日本語 ok"}`
	ct, err := encryptPush([]byte(msg), ua.PublicKey().Bytes(), auth, as, salt)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := decryptPush(ct, ua.Bytes(), auth)
	if err != nil || string(pt) != msg {
		t.Fatalf("round trip: %q %v", pt, err)
	}
	if _, err := decryptPush(ct, ua.Bytes(), make([]byte, 16)); err == nil {
		t.Error("a wrong auth secret decrypted")
	}
	if _, err := encryptPush([]byte(msg), []byte("not a point"), auth, as, salt); err == nil {
		t.Error("a bad p256dh was accepted")
	}
	if _, err := encryptPush(make([]byte, 4000), ua.PublicKey().Bytes(), auth, as, salt); err == nil {
		t.Error("a payload over one record was accepted")
	}
}

func TestVapidKeyIsMadeOnceAndJWTVerifies(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	k1, err := vapidKey(e.s.db)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := vapidKey(e.s.db)
	if !k1.Equal(k2) {
		t.Fatal("a second call made another key")
	}
	if !k1.Equal(e.s.vapid) {
		t.Fatal("the server does not use the stored key")
	}
	pub := mustB64(t, vapidPublic(k1))
	if len(pub) != 65 || pub[0] != 4 {
		t.Fatalf("the public key is not an uncompressed point: %d bytes", len(pub))
	}
	tok, err := signJWT(k1, "https://push.example", e.clk.Now().Add(jwtLife))
	if err != nil {
		t.Fatal(err)
	}
	c, err := verifyJWT(&k1.PublicKey, tok)
	if err != nil || c["aud"] != "https://push.example" || !strings.HasPrefix(c["sub"].(string), "https://") {
		t.Fatalf("claims %v, err %v", c, err)
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := verifyJWT(&other.PublicKey, tok); err == nil {
		t.Error("another key verified the token")
	}
	// cached per origin, signed again when less than an hour is left
	a, _ := e.s.jwtFor("https://push.example")
	b, _ := e.s.jwtFor("https://push.example")
	if a != b {
		t.Error("the JWT was not cached")
	}
	e.clk.Add(jwtLife - 30*time.Minute)
	if c, _ := e.s.jwtFor("https://push.example"); c == a {
		t.Error("a JWT with under an hour left was kept")
	}
}

// decryptPush is the receiving side of encryptPush.
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
