// SPDX-License-Identifier: AGPL-3.0-only

// Package webpush implements the server half of Web Push (RFC 8030) for Mailroom:
// the VAPID application keypair (RFC 8292), the aes128gcm payload encryption
// (RFC 8291 over RFC 8188), and the HTTP send. It is an anti-corruption layer
// (spec §3): the sync loop and the store talk to this package's own types, never
// to net/http or a third-party push library, and only this package owns the
// hand-rolled RFC 8291 crypto (tested against the published test vectors).
//
// Design authority is ADR-016: the VAPID keypair is HKDF-derived from the master
// key (salt=nil, info "mailroom-vapid-v1"), so it is stable across restarts with
// no new secret to store, and sends run per-user under RLS (no bypass).
package webpush

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"mailroom/internal/crypto"
)

// vapidKeyInfo is the HKDF domain-separation label for the VAPID private scalar
// (ADR-016). It is derived from the master key with salt=nil, the same shape as
// the blob-path key — an application-level key, not per-user.
const vapidKeyInfo = "mailroom-vapid-v1"

// vapidTokenTTL is how long a signed VAPID JWT stays valid. RFC 8292 caps the
// horizon at 24h; 12h is a comfortable margin that still tolerates clock skew.
const vapidTokenTTL = 12 * time.Hour

// defaultVAPIDSub is the `sub` claim used when no operator contact is configured.
// It is a neutral placeholder per the open-source portability rule (CLAUDE.md);
// a real deploy overrides it with the operator's mailto/https contact.
const defaultVAPIDSub = "mailto:you@example.com"

// VAPIDKey is the server's single Web Push application keypair. Its public key is
// handed to every browser at subscribe time (applicationServerKey) and echoed as
// the `k` parameter of every push; its private scalar signs the per-push VAPID
// JWT. Derived deterministically from the master key, so it survives restarts and
// redeploys and needs no separate secret (ADR-016).
type VAPIDKey struct {
	signKey *ecdsa.PrivateKey // ES256 signer for the VAPID JWT
	public  []byte            // 65-byte uncompressed P-256 point (0x04||X||Y)
}

// DeriveVAPIDKey reconstructs the VAPID keypair from the 32-byte master key. The
// derived scalar is validated as a P-256 private key by crypto/ecdh (which
// rejects a zero or out-of-range scalar); the error is surfaced, never assumed
// away. The master key is the caller's to zeroize; this function copies nothing
// of it beyond the derived scalar, which it scrubs before returning.
func DeriveVAPIDKey(master []byte) (*VAPIDKey, error) {
	scalar, err := crypto.DeriveKey(master, nil, vapidKeyInfo)
	if err != nil {
		return nil, fmt.Errorf("webpush: derive vapid scalar: %w", err)
	}
	defer crypto.Zero(scalar)

	// crypto/ecdh validates the scalar is in [1, N-1] for P-256.
	priv, err := ecdh.P256().NewPrivateKey(scalar)
	if err != nil {
		return nil, fmt.Errorf("webpush: derived vapid scalar is not a valid P-256 key: %w", err)
	}
	public := priv.PublicKey().Bytes() // uncompressed 0x04||X||Y, 65 bytes

	// Build the ES256 signer from the SAME scalar.
	//
	// This used to hand-assemble an ecdsa.PrivateKey by writing X, Y and D as
	// big.Ints taken from the ecdh public bytes. Those three fields are
	// deprecated as of Go 1.26 — writing raw coordinates can produce invalid
	// keys and defeats the package's internal invariants — and staticcheck
	// (SA1019) fails the build on them.
	//
	// ParseRawPrivateKey is the replacement and is strictly better here: it
	// derives the public point itself rather than trusting bytes we sliced, and
	// it rejects a scalar that is zero or not reduced modulo the curve order.
	// That check is redundant with ecdh.P256().NewPrivateKey above, which is
	// fine — this is a key derived from the master secret, and two independent
	// validations of it cost nothing.
	if len(public) != p256PointLen {
		return nil, fmt.Errorf("webpush: unexpected P-256 public length %d", len(public))
	}
	signKey, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), scalar)
	if err != nil {
		return nil, fmt.Errorf("webpush: derived vapid scalar is not a valid ES256 key: %w", err)
	}
	return &VAPIDKey{signKey: signKey, public: public}, nil
}

// PublicKeyBase64URL is the unpadded base64url of the 65-byte uncompressed public
// point. This is the exact value the client passes to pushManager.subscribe as
// applicationServerKey and that the server advertises in the session capability.
func (k *VAPIDKey) PublicKeyBase64URL() string {
	return base64.RawURLEncoding.EncodeToString(k.public)
}

// AuthorizationHeader builds the RFC 8292 VAPID `Authorization` header value for a
// push to endpoint: `vapid t=<jwt>,k=<pubkey>`. The JWT's audience is the origin
// of the endpoint, and `sub` is the operator contact (defaulting to a placeholder
// mailto when empty).
func (k *VAPIDKey) AuthorizationHeader(endpoint, sub string) (string, error) {
	aud, err := originOf(endpoint)
	if err != nil {
		return "", err
	}
	token, err := k.signJWT(aud, sub, time.Now())
	if err != nil {
		return "", err
	}
	return "vapid t=" + token + ",k=" + k.PublicKeyBase64URL(), nil
}

// signJWT produces the compact ES256 JWT for one audience. Header is
// {typ:"JWT", alg:"ES256"}; claims are {aud, exp, sub}. `now` is a parameter so
// the token's expiry is testable.
func (k *VAPIDKey) signJWT(aud, sub string, now time.Time) (string, error) {
	if sub == "" {
		sub = defaultVAPIDSub
	}
	claims := map[string]any{
		"aud": aud,
		"exp": now.Add(vapidTokenTTL).Unix(),
		"sub": sub,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("webpush: marshal vapid claims: %w", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: k.signKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", fmt.Errorf("webpush: build vapid signer: %w", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", fmt.Errorf("webpush: sign vapid jwt: %w", err)
	}
	compact, err := obj.CompactSerialize()
	if err != nil {
		return "", fmt.Errorf("webpush: serialize vapid jwt: %w", err)
	}
	return compact, nil
}

// originOf returns the scheme://host origin of a push endpoint — the value the
// VAPID `aud` claim MUST carry (RFC 8292 §2). A relative or malformed endpoint is
// an error rather than an empty audience a push service would reject anyway.
func originOf(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("webpush: parse endpoint: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("webpush: endpoint is not an absolute URL")
	}
	return u.Scheme + "://" + u.Host, nil
}
