// SPDX-License-Identifier: AGPL-3.0-only

package webpush

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

func testMaster() []byte {
	m := make([]byte, 32)
	for i := range m {
		m[i] = byte(i + 1)
	}
	return m
}

func TestDeriveVAPIDKeyDeterministic(t *testing.T) {
	k1, err := DeriveVAPIDKey(testMaster())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	k2, err := DeriveVAPIDKey(testMaster())
	if err != nil {
		t.Fatalf("derive again: %v", err)
	}
	if k1.PublicKeyBase64URL() != k2.PublicKeyBase64URL() {
		t.Errorf("VAPID public key is not stable across derivations:\n %s\n %s",
			k1.PublicKeyBase64URL(), k2.PublicKeyBase64URL())
	}
	// The advertised applicationServerKey must be an uncompressed 65-byte P-256
	// point (0x04 || X || Y).
	pub, err := base64.RawURLEncoding.DecodeString(k1.PublicKeyBase64URL())
	if err != nil {
		t.Fatalf("public key is not unpadded base64url: %v", err)
	}
	if len(pub) != p256PointLen || pub[0] != 0x04 {
		t.Errorf("public key is not a 65-byte uncompressed point: len=%d first=0x%02x", len(pub), pub[0])
	}
}

func TestDeriveVAPIDKeyRejectsShortMaster(t *testing.T) {
	if _, err := DeriveVAPIDKey(make([]byte, 16)); err == nil {
		t.Error("DeriveVAPIDKey accepted a 16-byte master key")
	}
}

func TestVAPIDAuthorizationHeader(t *testing.T) {
	k, err := DeriveVAPIDKey(testMaster())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	now := time.Now()
	endpoint := "https://push.example.com/send/abc123?token=xyz"
	hdr, err := k.AuthorizationHeader(endpoint, "mailto:ops@example.com")
	if err != nil {
		t.Fatalf("authorization header: %v", err)
	}

	// Shape: vapid t=<jwt>,k=<pubkey>
	if !strings.HasPrefix(hdr, "vapid t=") {
		t.Fatalf("header does not start with 'vapid t=': %q", hdr)
	}
	rest := strings.TrimPrefix(hdr, "vapid t=")
	parts := strings.SplitN(rest, ",k=", 2)
	if len(parts) != 2 {
		t.Fatalf("header missing ,k= segment: %q", hdr)
	}
	token, kParam := parts[0], parts[1]
	if kParam != k.PublicKeyBase64URL() {
		t.Errorf("k param = %q, want %q", kParam, k.PublicKeyBase64URL())
	}

	// The JWT header is {typ:"JWT", alg:"ES256"}.
	segs := strings.Split(token, ".")
	if len(segs) != 3 {
		t.Fatalf("JWT is not three segments: %q", token)
	}
	var jhdr struct {
		Typ string `json:"typ"`
		Alg string `json:"alg"`
	}
	decodeJSONSeg(t, segs[0], &jhdr)
	if jhdr.Typ != "JWT" || jhdr.Alg != "ES256" {
		t.Errorf("JWT header = %+v, want typ=JWT alg=ES256", jhdr)
	}

	// The signature verifies under the derived public key (ES256), and the claims
	// carry the endpoint origin, a future expiry, and the configured subject.
	jws, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatalf("parse signed: %v", err)
	}
	payload, err := jws.Verify(k.signKey.Public())
	if err != nil {
		t.Fatalf("ES256 signature did not verify under the VAPID public key: %v", err)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims.Aud != "https://push.example.com" {
		t.Errorf("aud = %q, want https://push.example.com", claims.Aud)
	}
	if claims.Sub != "mailto:ops@example.com" {
		t.Errorf("sub = %q", claims.Sub)
	}
	if claims.Exp <= now.Unix() {
		t.Errorf("exp %d is not in the future (now %d)", claims.Exp, now.Unix())
	}
	if claims.Exp > now.Add(vapidTokenTTL+time.Minute).Unix() {
		t.Errorf("exp %d exceeds the %s horizon", claims.Exp, vapidTokenTTL)
	}
}

func TestVAPIDDefaultSubject(t *testing.T) {
	k, err := DeriveVAPIDKey(testMaster())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	token, err := k.signJWT("https://push.example.com", "", time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	segs := strings.Split(token, ".")
	var claims struct {
		Sub string `json:"sub"`
	}
	decodeJSONSeg(t, segs[1], &claims)
	if claims.Sub != defaultVAPIDSub {
		t.Errorf("default sub = %q, want %q", claims.Sub, defaultVAPIDSub)
	}
}

func TestVAPIDRejectsRelativeEndpoint(t *testing.T) {
	k, err := DeriveVAPIDKey(testMaster())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if _, err := k.AuthorizationHeader("/not/absolute", ""); err == nil {
		t.Error("AuthorizationHeader accepted a relative endpoint")
	}
}

func decodeJSONSeg(t *testing.T, seg string, v any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("decode JWT segment: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal JWT segment: %v", err)
	}
}

// TestVAPIDSignatureVerifiesUnderTheADVERTISEDKey closes a gap that only became
// load-bearing when DeriveVAPIDKey stopped hand-assembling its ecdsa.PrivateKey.
//
// ⚠ WHY THE EXISTING COVERAGE WAS NOT ENOUGH. TestVAPIDAuthorizationHeader
// verifies the JWT with `k.signKey.Public()` — the signer's own public key. That
// is self-referential: it proves the signature is internally consistent and says
// nothing about whether the point Mailroom ADVERTISES is the same one. It checks
// `kParam == k.PublicKeyBase64URL()`, but both sides of that comparison are
// `k.public`, so it compares a value to itself.
//
// The two used to be the same object by construction: the signer was built from
// slices of `public`, so they could not disagree. They are now derived
// independently — `public` from crypto/ecdh, the signer from
// ecdsa.ParseRawPrivateKey — and nothing in the old suite would notice if those
// diverged. The failure would be invisible locally and total in production:
// every push service verifies the JWT against the `k` parameter, so every push
// for every user would be rejected as a bad signature, with a header that still
// looks perfectly well-formed.
//
// So this verifies the token the way a push service actually does: parse `k` out
// of the header, rebuild a public key from those bytes alone, and check the
// signature under it.
func TestVAPIDSignatureVerifiesUnderTheADVERTISEDKey(t *testing.T) {
	k, err := DeriveVAPIDKey(testMaster())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	hdr, err := k.AuthorizationHeader("https://push.example.com/send/abc123", "mailto:ops@example.com")
	if err != nil {
		t.Fatalf("authorization header: %v", err)
	}
	rest := strings.TrimPrefix(hdr, "vapid t=")
	parts := strings.SplitN(rest, ",k=", 2)
	if len(parts) != 2 {
		t.Fatalf("header missing ,k= segment: %q", hdr)
	}
	token, kParam := parts[0], parts[1]

	// Rebuild the verifying key from the ADVERTISED bytes only. Nothing derived
	// from k.signKey may be used past this point — that is the whole test.
	pubBytes, err := base64.RawURLEncoding.DecodeString(kParam)
	if err != nil {
		t.Fatalf("k param is not base64url: %v", err)
	}
	advertised, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pubBytes)
	if err != nil {
		t.Fatalf("the advertised k is not a valid P-256 point: %v", err)
	}

	jws, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatalf("parse signed: %v", err)
	}
	if _, err := jws.Verify(advertised); err != nil {
		t.Fatalf("the VAPID JWT does not verify under the key the header ADVERTISES: %v\n"+
			"Mailroom is signing with one key and telling push services to check another, "+
			"so every push would be rejected. Check that DeriveVAPIDKey's signer and its "+
			"`public` bytes still come from the same scalar.", err)
	}

	// NEGATIVE CONTROL. A different master must produce a key this token does NOT
	// verify under — otherwise the assertion above could be passing because
	// jws.Verify is lenient rather than because the keys agree.
	other := testMaster()
	other[0] ^= 0xFF
	k2, err := DeriveVAPIDKey(other)
	if err != nil {
		t.Fatalf("derive second key: %v", err)
	}
	if _, err := jws.Verify(k2.signKey.Public()); err == nil {
		t.Fatal("the token verified under an UNRELATED key — jws.Verify is not actually " +
			"checking the signature, so the assertion above proves nothing")
	}
}
