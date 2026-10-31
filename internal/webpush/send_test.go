// SPDX-License-Identifier: AGPL-3.0-only

package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fakeClient struct {
	status int
	err    error
	got    *http.Request
	body   []byte
}

func (f *fakeClient) Do(req *http.Request) (*http.Response, error) {
	f.got = req
	if req.Body != nil {
		f.body, _ = io.ReadAll(req.Body)
	}
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}, nil
}

func testSubscription(t *testing.T) Subscription {
	t.Helper()
	uaPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	auth := make([]byte, authSecretLen)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("auth: %v", err)
	}
	return Subscription{
		Endpoint: "https://push.example.com/send/device-1",
		P256dh:   base64.RawURLEncoding.EncodeToString(uaPriv.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
	}
}

func newTestSender(t *testing.T, client HTTPClient) *Sender {
	t.Helper()
	k, err := DeriveVAPIDKey(testMaster())
	if err != nil {
		t.Fatalf("derive vapid: %v", err)
	}
	return NewSender(k, client, "mailto:ops@example.com")
}

func TestSendDelivered(t *testing.T) {
	fc := &fakeClient{status: http.StatusCreated}
	s := newTestSender(t, fc)
	res, err := s.Send(context.Background(), testSubscription(t), []byte(`{"title":"New mail"}`))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res != Delivered {
		t.Errorf("result = %v, want Delivered", res)
	}
	// The request carries the aes128gcm encoding, a VAPID authorization, and a TTL.
	if enc := fc.got.Header.Get("Content-Encoding"); enc != "aes128gcm" {
		t.Errorf("Content-Encoding = %q", enc)
	}
	if auth := fc.got.Header.Get("Authorization"); !strings.HasPrefix(auth, "vapid t=") {
		t.Errorf("Authorization = %q", auth)
	}
	if ttl := fc.got.Header.Get("TTL"); ttl == "" {
		t.Error("TTL header missing")
	}
	// The body is the encrypted aes128gcm payload, not the plaintext.
	if strings.Contains(string(fc.body), "New mail") {
		t.Error("request body leaked plaintext (payload was not encrypted)")
	}
	if len(fc.body) == 0 {
		t.Error("request body is empty")
	}
}

func TestSendGone(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		fc := &fakeClient{status: status}
		s := newTestSender(t, fc)
		res, err := s.Send(context.Background(), testSubscription(t), []byte("x"))
		if err != nil {
			t.Fatalf("send (%d): %v", status, err)
		}
		if res != Gone {
			t.Errorf("status %d: result = %v, want Gone", status, res)
		}
	}
}

func TestSendTransient(t *testing.T) {
	// A 5xx is transient (keep the subscription).
	fc := &fakeClient{status: http.StatusServiceUnavailable}
	s := newTestSender(t, fc)
	if res, err := s.Send(context.Background(), testSubscription(t), []byte("x")); res != Transient || err == nil {
		t.Errorf("5xx: result=%v err=%v, want Transient + error", res, err)
	}

	// A network error is transient too.
	fc2 := &fakeClient{err: errors.New("connection refused")}
	s2 := newTestSender(t, fc2)
	if res, err := s2.Send(context.Background(), testSubscription(t), []byte("x")); res != Transient || err == nil {
		t.Errorf("network error: result=%v err=%v, want Transient + error", res, err)
	}
}

func TestSendRejectsBadSubscriptionKeys(t *testing.T) {
	fc := &fakeClient{status: http.StatusCreated}
	s := newTestSender(t, fc)
	sub := Subscription{Endpoint: "https://push.example.com/x", P256dh: "!!!not-base64!!!", Auth: "AAAA"}
	if _, err := s.Send(context.Background(), sub, []byte("x")); err == nil {
		t.Error("Send accepted an undecodable p256dh key")
	}
	if fc.got != nil {
		t.Error("Send issued an HTTP request despite a bad subscription key")
	}
}
