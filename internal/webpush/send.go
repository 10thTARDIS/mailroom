// SPDX-License-Identifier: AGPL-3.0-only

package webpush

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Subscription is one browser push registration, as stored per user. The keys are
// the base64url values the browser produced at subscribe time (p256dh is the
// user-agent public key, auth is the 16-byte auth secret). This package decodes
// them; callers hand over the stored strings verbatim.
type Subscription struct {
	Endpoint string
	P256dh   string // base64url, uncompressed P-256 public key
	Auth     string // base64url, 16-byte auth secret
}

// HTTPClient is the minimal seam send needs, so tests inject a fake and net/http
// never has to be imported by the store or api layers. *http.Client satisfies it.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Result classifies the outcome of a single push POST so the caller can act
// without inspecting HTTP status codes.
type Result int

const (
	// Delivered: the push service accepted the message (2xx).
	Delivered Result = iota
	// Gone: the subscription is permanently dead (404/410) and should be removed.
	Gone
	// Transient: a temporary failure (network error, 429, 5xx); keep the
	// subscription and let a later mail-arrival retry it.
	Transient
)

// defaultTTL is the push message time-to-live in seconds: how long the push
// service holds an undelivered message. A day is a reasonable ceiling for "you
// have new mail" — beyond that the count is stale anyway.
const defaultTTL = 24 * time.Hour

// Sender POSTs encrypted, VAPID-authenticated push messages. It holds the derived
// application keypair and a pluggable HTTP client. It is safe for concurrent use.
type Sender struct {
	vapid   *VAPIDKey
	client  HTTPClient
	contact string // VAPID `sub` claim (mailto:/https:); "" uses the placeholder
}

// NewSender builds a Sender over the derived VAPID key and an HTTP client. contact
// is the operator's VAPID `sub` (empty is allowed and defaults to a placeholder).
func NewSender(vapid *VAPIDKey, client HTTPClient, contact string) *Sender {
	return &Sender{vapid: vapid, client: client, contact: contact}
}

// Send encrypts payload for one subscription and POSTs it. It returns a Result
// the caller uses to decide whether to drop the subscription; the error is
// non-nil only alongside Transient (or a client-side encoding failure), so a
// caller can log-and-continue without failing its surrounding work.
func (s *Sender) Send(ctx context.Context, sub Subscription, payload []byte) (Result, error) {
	p256dh, err := decodeKey(sub.P256dh)
	if err != nil {
		return Transient, fmt.Errorf("webpush: decode p256dh: %w", err)
	}
	auth, err := decodeKey(sub.Auth)
	if err != nil {
		return Transient, fmt.Errorf("webpush: decode auth: %w", err)
	}
	body, err := Encrypt(p256dh, auth, payload)
	if err != nil {
		return Transient, err
	}
	authHeader, err := s.vapid.AuthorizationHeader(sub.Endpoint, s.contact)
	if err != nil {
		return Transient, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Transient, fmt.Errorf("webpush: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(int(defaultTTL.Seconds())))
	req.Header.Set("Urgency", "normal")
	req.Header.Set("Authorization", authHeader)

	resp, err := s.client.Do(req)
	if err != nil {
		return Transient, fmt.Errorf("webpush: post to %s: %w", originForLog(sub.Endpoint), err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return Delivered, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return Gone, nil
	default:
		return Transient, fmt.Errorf("webpush: push service returned %d", resp.StatusCode)
	}
}

// decodeKey decodes a base64url subscription key, tolerating both padded and
// unpadded input (browsers emit unpadded; some libraries pad).
func decodeKey(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(trimPadding(s)); err == nil {
		return b, nil
	}
	// Fall back to standard base64 in case a client used the non-URL alphabet.
	if b, err := base64.RawStdEncoding.DecodeString(trimPadding(s)); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("webpush: not valid base64: %q", s)
}

func trimPadding(s string) string {
	for len(s) > 0 && s[len(s)-1] == '=' {
		s = s[:len(s)-1]
	}
	return s
}

// originForLog returns just the endpoint origin for error messages, keeping the
// (sensitive) subscription-specific path out of logs.
func originForLog(endpoint string) string {
	if origin, err := originOf(endpoint); err == nil {
		return origin
	}
	return "push service"
}
