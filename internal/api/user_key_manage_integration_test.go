// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Own-key management over JMAP (#796): UserKey/generate, /import, /set,
// /extend, /revoke, /export, against the REAL crypto service
// (pgpkeyd.Service over pgpkeyd.StoreVault, so every key it makes is a real
// app.user_keys row written by the real store path) and the real store.
//
// Change rows are counted per object type for every mutation, and every no-op
// is asserted to write none.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/eddsa"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeyd"
	"mailroom/internal/pgpwire"
	"mailroom/internal/store"
	"mailroom/internal/wkd"
)

// inProcCustody is cmd/mailroom's cryptoCustody over an in-process Service:
// the same request shapes and the same one-line error mapping
// (*pgpwire.Error → *CustodyError).
type inProcCustody struct {
	svc   *pgpkeyd.Service
	mu    sync.Mutex
	calls int
}

func (c *inProcCustody) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *inProcCustody) called() {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
}

func wireToCustody(err error) error {
	var werr *pgpwire.Error
	if errors.As(err, &werr) {
		return &CustodyError{Code: string(werr.Code), Message: werr.Message}
	}
	return err
}

func (c *inProcCustody) GenerateKey(ctx context.Context, userID ulid.ULID, name, email string, exp KeyExpiry) (CustodyKey, error) {
	c.called()
	r, err := c.svc.Generate(ctx, pgpwire.GenerateRequest{UserID: userID.String(), Name: name, Email: email, ExpiresDays: exp.Days, ExpiresAt: exp.At})
	if err != nil {
		return CustodyKey{}, wireToCustody(err)
	}
	return CustodyKey{KeyID: r.Key.KeyID, Fingerprint: r.Key.Fingerprint}, nil
}

func (c *inProcCustody) ImportKey(ctx context.Context, userID ulid.ULID, armored, pass string) (CustodyKey, bool, error) {
	c.called()
	r, err := c.svc.Import(ctx, pgpwire.ImportRequest{UserID: userID.String(), PrivateKey: armored, Passphrase: pass})
	if err != nil {
		return CustodyKey{}, false, wireToCustody(err)
	}
	return CustodyKey{KeyID: r.Key.KeyID, Fingerprint: r.Key.Fingerprint}, r.AlreadyStored, nil
}

func (c *inProcCustody) ExtendKey(ctx context.Context, userID, keyID ulid.ULID, exp KeyExpiry) (CustodyKey, error) {
	c.called()
	req := pgpwire.ExtendRequest{UserID: userID.String(), KeyID: keyID.String(), ExpiresAt: exp.At}
	if exp.Days != nil {
		req.Days = *exp.Days
	}
	r, err := c.svc.Extend(ctx, req)
	if err != nil {
		return CustodyKey{}, wireToCustody(err)
	}
	return CustodyKey{KeyID: r.Key.KeyID, Fingerprint: r.Key.Fingerprint}, nil
}

func (c *inProcCustody) RevokeKey(ctx context.Context, userID, keyID ulid.ULID, reason, confirm string) (CustodyKey, error) {
	c.called()
	r, err := c.svc.Revoke(ctx, pgpwire.RevokeRequest{UserID: userID.String(), KeyID: keyID.String(), Reason: reason, Confirm: confirm})
	if err != nil {
		return CustodyKey{}, wireToCustody(err)
	}
	return CustodyKey{KeyID: r.Key.KeyID, Fingerprint: r.Key.Fingerprint}, nil
}

func (c *inProcCustody) ExportKey(ctx context.Context, userID, keyID ulid.ULID, pass string) (CustodyKey, string, error) {
	c.called()
	r, err := c.svc.ExportWrapped(ctx, pgpwire.ExportWrappedRequest{UserID: userID.String(), KeyID: keyID.String(), Passphrase: pass})
	if err != nil {
		return CustodyKey{}, "", wireToCustody(err)
	}
	return CustodyKey{KeyID: r.Key.KeyID, Fingerprint: r.Key.Fingerprint}, r.PrivateKey, nil
}

// newInProcCustody builds the real crypto service over the store, logging to
// log (nil: discard).
func newInProcCustody(t *testing.T, st *store.Store, log io.Writer) *inProcCustody {
	t.Helper()
	if log == nil {
		log = io.Discard
	}
	root := make([]byte, 32)
	for i := range root {
		root[i] = byte(i*5 + 3)
	}
	vault, err := pgpkeyd.NewStoreVault(st)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := pgpkeyd.NewService(root, vault, slog.New(slog.NewTextHandler(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &inProcCustody{svc: svc}
}

type keyHarness struct {
	*apiHarness
	custody   *inProcCustody
	cryptoLog *bytes.Buffer
	a, b      apiFixture
	identA    ulid.ULID // me@example.com, fixed
	identB    ulid.ULID // you@example.com, fixed (user B)
}

func newKeyHarness(t *testing.T) *keyHarness {
	t.Helper()
	base := newAPIHarness(t)
	logBuf := &bytes.Buffer{}
	c := newInProcCustody(t, base.store, logBuf)
	base.api = New(base.store, WithKeyCustody(c), WithWKDDomain("Example.com"))
	h := &keyHarness{apiHarness: base, custody: c, cryptoLog: logBuf}
	h.a, h.b = seedTwo(t, base.super)
	seedSendable(t, base.super, h.a, "me@example.com", "wild.example")
	seedSendable(t, base.super, h.b, "you@example.com", "")
	h.identA = h.identityID(t, h.a.UserID, "me@example.com")
	h.identB = h.identityID(t, h.b.UserID, "you@example.com")
	return h
}

func (h *keyHarness) identityID(t *testing.T, user ulid.ULID, addr string) ulid.ULID {
	t.Helper()
	var s string
	if err := h.super.QueryRow(context.Background(),
		`SELECT id FROM app.identities WHERE user_id = $1 AND address = $2`, user.String(), addr).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return ulid.MustParse(s)
}

func (h *keyHarness) wildcardID(t *testing.T) ulid.ULID {
	t.Helper()
	var s string
	if err := h.super.QueryRow(context.Background(),
		`SELECT id FROM app.identities WHERE user_id = $1 AND kind = 'domain_wildcard'`, h.a.UserID.String()).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return ulid.MustParse(s)
}

// invoke calls a method and returns the decoded result or the method error.
func (h *keyHarness) invoke(t *testing.T, method string, user ulid.ULID, args any) (map[string]any, *MethodError) {
	t.Helper()
	raw, mErr := h.api.methods[method](context.Background(), user, mustJSON(t, args))
	if mErr != nil {
		return nil, mErr
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return out, nil
}

func (h *keyHarness) must(t *testing.T, method string, user ulid.ULID, args any) map[string]any {
	t.Helper()
	out, mErr := h.invoke(t, method, user, args)
	if mErr != nil {
		t.Fatalf("%s: %+v", method, mErr)
	}
	return out
}

// rows counts the change_log rows of one object type for a user.
func (h *keyHarness) rows(t *testing.T, user ulid.ULID, objType string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = $2`, user.String(), objType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// delta runs fn and reports how many Key and Identity rows it appended for user.
func (h *keyHarness) delta(t *testing.T, user ulid.ULID, fn func()) (keys, idents int) {
	t.Helper()
	k0, i0 := h.rows(t, user, "Key"), h.rows(t, user, "Identity")
	fn()
	return h.rows(t, user, "Key") - k0, h.rows(t, user, "Identity") - i0
}

func (h *keyHarness) generate(t *testing.T, user, ident ulid.ULID, extra map[string]any) (string, map[string]any) {
	t.Helper()
	args := map[string]any{"identityId": ident.String()}
	for k, v := range extra {
		args[k] = v
	}
	out := h.must(t, "UserKey/generate", user, args)
	return out["id"].(string), out["key"].(map[string]any)
}

func (h *keyHarness) update(t *testing.T, user ulid.ULID, id string, patch map[string]any) (ok bool, setErr map[string]any) {
	t.Helper()
	out := h.must(t, "UserKey/set", user, map[string]any{"update": map[string]any{id: patch}})
	if up, _ := out["updated"].(map[string]any); up != nil {
		if _, ok := up[id]; ok {
			return true, nil
		}
	}
	nu, _ := out["notUpdated"].(map[string]any)
	e, _ := nu[id].(map[string]any)
	return false, e
}

func (h *keyHarness) getKey(t *testing.T, user ulid.ULID, id string) (map[string]any, map[string]any) {
	t.Helper()
	out := h.must(t, "UserKey/get", user, map[string]any{"ids": []string{id}})
	list := out["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("UserKey/get %s: %v", id, out)
	}
	return list[0].(map[string]any), out
}

func parseDate(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("not a date: %v", v)
	}
	d, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func near(a, b time.Time) bool { d := a.Sub(b); return d < time.Hour && d > -time.Hour }

func TestUserKeyGenerateBindPublishRetire(t *testing.T) {
	h := newKeyHarness(t)
	a := h.a.UserID

	var id string
	var key map[string]any
	if k, i := h.delta(t, a, func() { id, key = h.generate(t, a, h.identA, nil) }); k != 1 || i != 0 {
		t.Errorf("generate wrote Key %d Identity %d rows, want 1/0", k, i)
	}
	if exp := parseDate(t, key["expiresAt"]); !near(exp, time.Now().Add(730*24*time.Hour)) {
		t.Errorf("expiresAt = %v, want two years out", exp)
	}
	if key["usable"] != true || key["canSign"] != true || key["expiringSoon"] != false || key["expired"] != false ||
		key["origin"] != "generated" || key["identityId"] != nil || key["wkdUrl"] != nil {
		t.Errorf("fresh key = %v", key)
	}

	// Bind: Key + Identity; again: nothing.
	if k, i := h.delta(t, a, func() {
		if ok, e := h.update(t, a, id, map[string]any{"identityId": h.identA.String()}); !ok {
			t.Fatalf("bind: %v", e)
		}
	}); k != 1 || i != 1 {
		t.Errorf("bind wrote %d/%d rows, want 1/1", k, i)
	}
	if k, i := h.delta(t, a, func() { h.update(t, a, id, map[string]any{"identityId": h.identA.String()}) }); k != 0 || i != 0 {
		t.Errorf("re-bind (no-op) wrote %d/%d rows, want 0/0", k, i)
	}
	key, get := h.getKey(t, a, id)
	wantURL := "https://example.com" + wkd.DirectPath("me")
	if key["identityId"] != h.identA.String() || key["wkdUrl"] != wantURL || key["wkdServable"] != true {
		t.Errorf("bound key = %v, want wkdUrl %s servable", key, wantURL)
	}
	if get["wkdConfigured"] != true || get["wkdDomain"] != "example.com" {
		t.Errorf("UserKey/get wkd facts = %v/%v", get["wkdConfigured"], get["wkdDomain"])
	}

	// Publishing where nothing would serve it is refused, writing nothing.
	if k, i := h.delta(t, a, func() {
		h.api.wkdDomain = ""
		if ok, e := h.update(t, a, id, map[string]any{"wkdPublished": true}); ok || e["type"] != setWKDNotConfigured {
			t.Errorf("publish with WKD off: ok=%v %v", ok, e)
		}
		h.api.wkdDomain = "other.example"
		if ok, e := h.update(t, a, id, map[string]any{"wkdPublished": true}); ok || e["type"] != setNotPublishable {
			t.Errorf("publish for another domain: ok=%v %v", ok, e)
		}
		h.api.wkdDomain = "example.com"
	}); k != 0 || i != 0 {
		t.Errorf("refused publishes wrote %d/%d rows", k, i)
	}

	// Publish: Key + Identity (the hash); again: nothing.
	if k, i := h.delta(t, a, func() {
		if ok, e := h.update(t, a, id, map[string]any{"wkdPublished": true}); !ok {
			t.Fatalf("publish: %v", e)
		}
	}); k != 1 || i != 1 {
		t.Errorf("publish wrote %d/%d rows, want 1/1", k, i)
	}
	if k, i := h.delta(t, a, func() { h.update(t, a, id, map[string]any{"wkdPublished": true}) }); k != 0 || i != 0 {
		t.Errorf("re-publish (no-op) wrote %d/%d rows, want 0/0", k, i)
	}
	if pk, found, err := h.store.LookupWKDKey(context.Background(), "example.com", wkd.HashLocalPart("me")); err != nil || !found || pk.Fingerprint != key["fingerprint"] {
		t.Errorf("WKD lookup after publish: found=%v err=%v fpr=%q", found, err, pk.Fingerprint)
	}

	// Unbinding a published key is refused and writes nothing.
	if k, i := h.delta(t, a, func() {
		if ok, e := h.update(t, a, id, map[string]any{"identityId": nil}); ok || e["type"] != setKeyIsPublished {
			t.Errorf("unbind while published: ok=%v %v, want keyIsPublished", ok, e)
		}
	}); k != 0 || i != 0 {
		t.Errorf("refused unbind wrote %d/%d rows", k, i)
	}

	// Unpublish: Key + Identity (last key, so the hash clears); again: nothing.
	if k, i := h.delta(t, a, func() { h.update(t, a, id, map[string]any{"wkdPublished": false}) }); k != 1 || i != 1 {
		t.Errorf("unpublish wrote %d/%d rows, want 1/1", k, i)
	}
	if k, i := h.delta(t, a, func() { h.update(t, a, id, map[string]any{"wkdPublished": false}) }); k != 0 || i != 0 {
		t.Errorf("re-unpublish (no-op) wrote %d/%d rows, want 0/0", k, i)
	}

	// Two kinds of change in one update are refused.
	if ok, e := h.update(t, a, id, map[string]any{"identityId": nil, "wkdPublished": true}); ok || e["type"] != setOneChangeAtATime {
		t.Errorf("two changes: ok=%v %v", ok, e)
	}
	// Retiring the bound default without unbinding is refused.
	if k, i := h.delta(t, a, func() {
		if ok, e := h.update(t, a, id, map[string]any{"decryptOnly": true}); ok || e["type"] != setKeyIsDefault {
			t.Errorf("retire bound key: ok=%v %v, want keyIsDefault", ok, e)
		}
	}); k != 0 || i != 0 {
		t.Errorf("refused retire wrote %d/%d rows", k, i)
	}
	// Retire and unbind in one step: Key + Identity.
	if k, i := h.delta(t, a, func() {
		if ok, e := h.update(t, a, id, map[string]any{"decryptOnly": true, "identityId": nil}); !ok {
			t.Fatalf("retire+unbind: %v", e)
		}
	}); k != 1 || i != 1 {
		t.Errorf("retire+unbind wrote %d/%d rows, want 1/1", k, i)
	}
	key, _ = h.getKey(t, a, id)
	if key["decryptOnly"] != true || key["usable"] != false || key["canSign"] != false || key["expiringSoon"] != false {
		t.Errorf("retired key = %v", key)
	}
	if k, i := h.delta(t, a, func() { h.update(t, a, id, map[string]any{"decryptOnly": true}) }); k != 0 || i != 0 {
		t.Errorf("re-retire (no-op) wrote %d/%d rows", k, i)
	}
	if ok, e := h.update(t, a, id, map[string]any{"decryptOnly": false}); ok || e["type"] != setRetireIsPermanent {
		t.Errorf("un-retire: ok=%v %v", ok, e)
	}
	if ok, e := h.update(t, a, id, map[string]any{"identityId": h.identA.String()}); ok || e["type"] != setKeyRetired {
		t.Errorf("binding a retired key: ok=%v %v", ok, e)
	}
	// Keys are never destroyed through /set.
	out := h.must(t, "UserKey/set", a, map[string]any{"destroy": []string{id}})
	if nd, _ := out["notDestroyed"].(map[string]any); nd == nil || nd[id] == nil {
		t.Errorf("destroy was not refused: %v", out)
	}
}

func TestUserKeyGenerateRefusalsAndBind(t *testing.T) {
	h := newKeyHarness(t)
	a := h.a.UserID
	if _, mErr := h.invoke(t, "UserKey/generate", a, map[string]any{"identityId": h.wildcardID(t).String()}); mErr == nil || mErr.Type != errWildcardIdentity {
		t.Errorf("wildcard identity: %+v", mErr)
	}
	for _, d := range []int{-1, MaxKeyLifetimeDays + 1} {
		if _, mErr := h.invoke(t, "UserKey/generate", a, map[string]any{"identityId": h.identA.String(), "expiresDays": d}); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("expiresDays %d: %+v", d, mErr)
		}
	}
	if _, mErr := h.invoke(t, "UserKey/generate", a, map[string]any{"identityId": h.identB.String()}); mErr == nil || mErr.Type != errNotFound {
		t.Errorf("another user's identity: %+v, want notFound", mErr)
	}
	// bind: true → one Key created, plus the binding's Key and Identity rows.
	var key map[string]any
	if k, i := h.delta(t, a, func() { _, key = h.generate(t, a, h.identA, map[string]any{"bind": true, "expiresDays": 0}) }); k != 2 || i != 1 {
		t.Errorf("generate+bind wrote %d/%d rows, want 2/1", k, i)
	}
	if key["identityId"] != h.identA.String() || key["expiresAt"] != nil {
		t.Errorf("generate+bind (never expires) = %v", key)
	}
	ids := h.must(t, "Identity/get", a, map[string]any{"ids": []string{h.identA.String()}})["list"].([]any)
	if got := ids[0].(map[string]any)["signingKeyId"]; got != key["id"] {
		t.Errorf("Identity.signingKeyId = %v, want %v", got, key["id"])
	}
}

func TestUserKeyShortLivedExpiryAndExtend(t *testing.T) {
	h := newKeyHarness(t)
	a := h.a.UserID
	id, key := h.generate(t, a, h.identA, map[string]any{"expiresDays": 3})
	if exp := parseDate(t, key["expiresAt"]); !near(exp, time.Now().Add(3*24*time.Hour)) {
		t.Errorf("expiresAt = %v, want 3 days out", exp)
	}
	if key["expiringSoon"] != true || key["usable"] != true {
		t.Errorf("3-day key = %v, want expiringSoon", key)
	}
	fpr := key["fingerprint"]
	if _, mErr := h.invoke(t, "UserKey/extend", a, map[string]any{"id": id, "days": 0}); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("extend 0 days: %+v", mErr)
	}
	var out map[string]any
	if k, i := h.delta(t, a, func() { out = h.must(t, "UserKey/extend", a, map[string]any{"id": id, "days": 400}) }); k != 1 || i != 0 {
		t.Errorf("extend wrote %d/%d rows, want 1/0", k, i)
	}
	key = out["key"].(map[string]any)
	if exp := parseDate(t, out["expiresAt"]); !near(exp, time.Now().Add(400*24*time.Hour)) {
		t.Errorf("extended expiresAt = %v", exp)
	}
	if key["expiringSoon"] != false || key["fingerprint"] != fpr {
		t.Errorf("extended key = %v (fingerprint must not change)", key)
	}
}

func TestUserKeyRevoke(t *testing.T) {
	h := newKeyHarness(t)
	a := h.a.UserID
	id, key := h.generate(t, a, h.identA, map[string]any{"bind": true})
	fpr := key["fingerprint"].(string)
	last8 := fpr[len(fpr)-8:]
	wrong := "00000000"
	if last8 == wrong {
		wrong = "11111111"
	}
	if k, i := h.delta(t, a, func() {
		if _, mErr := h.invoke(t, "UserKey/revoke", a, map[string]any{"id": id, "reason": "compromised", "confirm": wrong}); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("wrong confirm: %+v", mErr)
		}
		if _, mErr := h.invoke(t, "UserKey/revoke", a, map[string]any{"id": id, "reason": "bored", "confirm": last8}); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("bad reason: %+v", mErr)
		}
	}); k != 0 || i != 0 {
		t.Errorf("refused revokes wrote %d/%d rows", k, i)
	}
	var out map[string]any
	if k, i := h.delta(t, a, func() {
		out = h.must(t, "UserKey/revoke", a, map[string]any{"id": id, "reason": "compromised", "confirm": strings.ToLower(last8)})
	}); k != 1 || i != 1 {
		t.Errorf("revoke wrote %d/%d rows, want 1/1 (the key, and the identity it signed for)", k, i)
	}
	key = out["key"].(map[string]any)
	if out["revokedAt"] == nil || key["usable"] != false || key["canSign"] != false || key["decryptOnly"] != true {
		t.Errorf("revoked key = %v", key)
	}
	if _, mErr := h.invoke(t, "UserKey/extend", a, map[string]any{"id": id, "days": 30}); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("extending a revoked key: %+v", mErr)
	}
	ids := h.must(t, "Identity/get", a, map[string]any{"ids": []string{h.identA.String()}})["list"].([]any)
	if got := ids[0].(map[string]any)["signingKeyId"]; got != nil {
		t.Errorf("Identity.signingKeyId = %v after revocation, want null", got)
	}
}

func randomSecret(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

func TestUserKeyExportImportRoundTrip(t *testing.T) {
	h := newKeyHarness(t)
	a, b := h.a.UserID, h.b.UserID
	id, key := h.generate(t, a, h.identA, nil)
	pass := randomSecret(t, "pass-")

	if _, mErr := h.invoke(t, "UserKey/export", a, map[string]any{"id": id, "passphrase": "short"}); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("short passphrase: %+v", mErr)
	}
	var out map[string]any
	if k, i := h.delta(t, a, func() { out = h.must(t, "UserKey/export", a, map[string]any{"id": id, "passphrase": pass}) }); k != 0 || i != 0 {
		t.Errorf("export wrote %d/%d rows, want none", k, i)
	}
	armored, _ := out["armoredKey"].(string)
	if strings.Contains(mustJSONString(t, out), pass) {
		t.Fatal("the export response contains the passphrase")
	}
	ring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil || len(ring) != 1 {
		t.Fatalf("exported key does not read: %v", err)
	}
	if ring[0].PrivateKey == nil || !ring[0].PrivateKey.Encrypted {
		t.Fatal("the exported private key is NOT passphrase-encrypted")
	}
	if err := ring[0].PrivateKey.Decrypt([]byte(pass)); err != nil {
		t.Fatalf("the export does not open with its passphrase: %v", err)
	}
	if out["fingerprint"] != key["fingerprint"] || !strings.HasSuffix(out["fileName"].(string), ".asc") {
		t.Errorf("export = fingerprint %v fileName %v", out["fingerprint"], out["fileName"])
	}

	// Same key back into the same user: already stored, no rows.
	if k, _ := h.delta(t, a, func() {
		if r := h.must(t, "UserKey/import", a, map[string]any{"privateKey": armored, "passphrase": pass}); r["alreadyStored"] != true || r["id"] != id {
			t.Errorf("re-import: %v", r)
		}
	}); k != 0 {
		t.Errorf("re-import wrote %d Key rows, want 0", k)
	}
	// Into user B with the wrong passphrase: invalidKey, nothing stored, the
	// passphrase not echoed.
	wrong := randomSecret(t, "wrong-")
	if k, _ := h.delta(t, b, func() {
		_, mErr := h.invoke(t, "UserKey/import", b, map[string]any{"privateKey": armored, "passphrase": wrong})
		if mErr == nil || mErr.Type != errInvalidKey {
			t.Errorf("wrong passphrase: %+v, want invalidKey", mErr)
		} else if strings.Contains(mErr.Description, wrong) || strings.Contains(mErr.Description, "PRIVATE") {
			t.Errorf("the refusal quotes its input: %q", mErr.Description)
		}
	}); k != 0 {
		t.Errorf("failed import wrote %d rows", k)
	}
	// The binary form, base64'd, with the right passphrase: B now holds it.
	blk, err := armor.Decode(strings.NewReader(armored))
	if err != nil {
		t.Fatal(err)
	}
	bin, _ := io.ReadAll(blk.Body)
	var imported map[string]any
	if k, _ := h.delta(t, b, func() {
		imported = h.must(t, "UserKey/import", b, map[string]any{"privateKey": base64.StdEncoding.EncodeToString(bin), "passphrase": pass})
	}); k != 1 {
		t.Errorf("import wrote %d Key rows, want 1", k)
	}
	ik := imported["key"].(map[string]any)
	if imported["alreadyStored"] != false || ik["fingerprint"] != key["fingerprint"] || ik["origin"] != "imported" || ik["canSign"] != true {
		t.Errorf("imported = %v", imported)
	}
	// A public key is refused with its own type.
	if _, mErr := h.invoke(t, "UserKey/import", b, map[string]any{"privateKey": armoredPub(t, ring[0])}); mErr == nil || mErr.Type != errPublicKeyRefused {
		t.Errorf("public key: %+v, want publicKeyRefused", mErr)
	}
}

// TestKeyMethodsLeakNothing drives every method that reaches the crypto process,
// on success and failure paths, with the application's and the crypto
// process's logs captured, and looks for the secrets in everything that came
// back or was written: the passphrases, the sealed ciphertext, and the private
// scalar itself. The detector is proved to fire on synthetic leaks first.
func TestKeyMethodsLeakNothing(t *testing.T) {
	h := newKeyHarness(t)
	a, b := h.a.UserID, h.b.UserID
	var appLog bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&appLog, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pass, wrong := randomSecret(t, "pass-"), randomSecret(t, "wrong-")
	var corpus []string
	record := func(out map[string]any, mErr *MethodError) {
		if out != nil {
			corpus = append(corpus, mustJSONString(t, out))
		}
		if mErr != nil {
			corpus = append(corpus, mustJSONString(t, mErr))
		}
	}
	id, _ := h.generate(t, a, h.identA, map[string]any{"bind": true})
	record(h.invoke(t, "UserKey/get", a, map[string]any{}))
	record(h.invoke(t, "UserKey/extend", a, map[string]any{"id": id, "days": 100}))
	record(h.invoke(t, "UserKey/extend", a, map[string]any{"id": id, "days": -1}))
	exp, mErr := h.invoke(t, "UserKey/export", a, map[string]any{"id": id, "passphrase": pass})
	if mErr != nil {
		t.Fatal(mErr)
	}
	armored := exp["armoredKey"].(string)
	delete(exp, "armoredKey") // checked separately below: it IS a (wrapped) private key
	record(exp, nil)
	record(h.invoke(t, "UserKey/export", a, map[string]any{"id": id, "passphrase": "x"}))
	record(h.invoke(t, "UserKey/import", b, map[string]any{"privateKey": armored, "passphrase": wrong}))
	record(h.invoke(t, "UserKey/import", b, map[string]any{"privateKey": "not a key " + wrong, "passphrase": wrong}))
	record(h.invoke(t, "UserKey/import", b, map[string]any{"privateKey": armored, "passphrase": pass}))
	record(h.invoke(t, "UserKey/generate", b, map[string]any{"identityId": h.identB.String()}))
	// #817: replace reaches the crypto process (generate) and the store, on
	// success and on refusal (the second call names a key now retired).
	id2, _ := h.generate(t, a, h.identA, map[string]any{"bind": true})
	rep, mErr := h.invoke(t, "UserKey/replace", a, map[string]any{"id": id2})
	if mErr != nil {
		t.Fatalf("replace: %+v", mErr)
	}
	record(rep, nil)
	record(h.invoke(t, "UserKey/replace", a, map[string]any{"id": id2}))
	replacedBy := rep["id"].(string)
	// Rebind the first key so the revoke below still runs on the key that
	// signs for the address.
	if ok, e := h.update(t, a, id, map[string]any{"identityId": h.identA.String()}); !ok {
		t.Fatalf("rebind: %v", e)
	}
	// Revoke last: everything above ran on a live key.
	_, keyGet := h.getKey(t, a, id)
	fpr := keyGet["list"].([]any)[0].(map[string]any)["fingerprint"].(string)
	record(h.invoke(t, "UserKey/revoke", a, map[string]any{"id": id, "reason": "compromised", "confirm": "00000001"}))
	record(h.invoke(t, "UserKey/revoke", a, map[string]any{"id": id, "reason": "compromised", "confirm": fpr[len(fpr)-8:]}))

	// The needles, obtained out of band: the sealed ciphertext from the row,
	// and the private scalar from opening the export with its passphrase.
	var ct, ct2, ct3 []byte
	if err := h.super.QueryRow(context.Background(), `SELECT private_key_ct FROM app.user_keys WHERE id = $1`, id).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if err := h.super.QueryRow(context.Background(), `SELECT private_key_ct FROM app.user_keys WHERE id = $1`, id2).Scan(&ct2); err != nil {
		t.Fatal(err)
	}
	if err := h.super.QueryRow(context.Background(), `SELECT private_key_ct FROM app.user_keys WHERE id = $1`, replacedBy).Scan(&ct3); err != nil {
		t.Fatal(err)
	}
	ring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		t.Fatal(err)
	}
	if err := ring[0].PrivateKey.Decrypt([]byte(pass)); err != nil {
		t.Fatal(err)
	}
	priv, ok := ring[0].PrivateKey.PrivateKey.(*eddsa.PrivateKey)
	if !ok || len(priv.D) == 0 {
		t.Fatalf("unexpected private key type %T", ring[0].PrivateKey.PrivateKey)
	}
	needles := map[string]string{
		"passphrase":          pass,
		"wrong passphrase":    wrong,
		"sealed key (hex)":    hex.EncodeToString(ct),
		"sealed key (base64)": base64.StdEncoding.EncodeToString(ct),
		// #817: the replaced key's and its replacement's sealed halves.
		"replaced sealed (hex)":    hex.EncodeToString(ct2),
		"replacement sealed (hex)": hex.EncodeToString(ct3),
		"replacement sealed (b64)": base64.StdEncoding.EncodeToString(ct3),
		"private scalar hex":       hex.EncodeToString(priv.D),
		"private scalar b64":       base64.StdEncoding.EncodeToString(priv.D),
		"private armor":            "PRIVATE KEY BLOCK",
	}
	leaks := func(hay string) []string {
		var found []string
		for name, n := range needles {
			if strings.Contains(hay, n) {
				found = append(found, name)
			}
		}
		return found
	}
	// The detector must SEE a leak before its silence means anything.
	for name, n := range needles {
		if len(leaks(`{"description":"oops `+n+`"}`)) == 0 {
			t.Fatalf("detector is blind to %s", name)
		}
	}
	hays := append(corpus, appLog.String(), h.cryptoLog.String())
	for i, hay := range hays {
		if found := leaks(hay); len(found) > 0 {
			t.Errorf("haystack %d leaks %v: %.300s", i, found, hay)
		}
	}
	// The export's own payload: a wrapped key, so the scalar and the passphrase
	// must not appear in it in the clear.
	for _, n := range []string{pass, hex.EncodeToString(priv.D), base64.StdEncoding.EncodeToString(priv.D)} {
		if strings.Contains(armored, n) {
			t.Error("the wrapped export carries a secret in the clear")
		}
	}
	if len(corpus) < 10 || h.cryptoLog.Len() == 0 {
		t.Fatalf("only %d responses and %d bytes of crypto log examined; the probe did not run", len(corpus), h.cryptoLog.Len())
	}
}

func TestKeyOperationsAreRateLimited(t *testing.T) {
	h := newKeyHarness(t)
	a, b := h.a.UserID, h.b.UserID
	h.api.keyOpLimiter = NewRateLimiter(RateLimitConfig{PerSecond: 1e-9, Burst: 2})
	id, key := h.generate(t, a, h.identA, map[string]any{"bind": true}) // token 1
	pass := randomSecret(t, "pass-")
	exp := h.must(t, "UserKey/export", a, map[string]any{"id": id, "passphrase": pass}) // token 2
	before := h.custody.count()
	for name, call := range map[string][2]any{
		"UserKey/export":   {"UserKey/export", map[string]any{"id": id, "passphrase": pass}},
		"UserKey/generate": {"UserKey/generate", map[string]any{"identityId": h.identA.String()}},
		"UserKey/import":   {"UserKey/import", map[string]any{"privateKey": exp["armoredKey"], "passphrase": pass}},
		"UserKey/replace":  {"UserKey/replace", map[string]any{"id": id}},
	} {
		if _, mErr := h.invoke(t, call[0].(string), a, call[1]); mErr == nil || mErr.Type != errRateLimited {
			t.Errorf("%s over budget: %+v, want rateLimited", name, mErr)
		}
	}
	if h.custody.count() != before {
		t.Errorf("a rate-limited call still reached the crypto process")
	}
	// Per user: B's budget is untouched.
	h.generate(t, b, h.identB, nil)
	// Extend and revoke are not limited: a compromised key can always be revoked.
	h.must(t, "UserKey/extend", a, map[string]any{"id": id, "days": 10})
	fpr := key["fingerprint"].(string)
	h.must(t, "UserKey/revoke", a, map[string]any{"id": id, "reason": "compromised", "confirm": fpr[len(fpr)-8:]})
}

func TestKeyManagementUnavailableWithoutCustody(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	for _, m := range []string{"UserKey/generate", "UserKey/import", "UserKey/extend", "UserKey/revoke", "UserKey/export"} {
		if _, mErr := h.api.methods[m](context.Background(), a.UserID, json.RawMessage(`{}`)); mErr == nil || mErr.Type != errKeyManagementUnavailable {
			t.Errorf("%s with no custody: %+v", m, mErr)
		}
	}
}
