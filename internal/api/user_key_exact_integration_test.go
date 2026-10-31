// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Exact expiry over JMAP (#813), against the real crypto service and store: a
// date becomes the end of that UTC day in the key's packets, an instant becomes
// itself, and the stored packets of the primary key AND the subkey agree.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
)

func (h *keyHarness) storedValidity(t *testing.T, id string) pgpkeys.Validity {
	t.Helper()
	var pub []byte
	if err := h.super.QueryRow(context.Background(), `SELECT public_key FROM app.user_keys WHERE id = $1`, ulid.MustParse(id).String()).Scan(&pub); err != nil {
		t.Fatal(err)
	}
	v, err := pgpkeys.Assess(pub, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestUserKeyGenerateAndExtendToAnExactDate(t *testing.T) {
	h := newKeyHarness(t)
	a := h.a.UserID
	day := time.Now().UTC().AddDate(0, 0, 14).Format("2006-01-02")
	want, _ := time.Parse("2006-01-02", day)
	want = want.Add(24*time.Hour - time.Second)

	id, key := h.generate(t, a, h.identA, map[string]any{"expiresAt": day})
	if got := parseDate(t, key["expiresAt"]); !got.Equal(want) {
		t.Errorf("UserKey expiresAt = %v, want %v (the end of %s, UTC)", got, want, day)
	}
	v := h.storedValidity(t, id)
	if v.PrimaryExpires == nil || !v.PrimaryExpires.Equal(want) || v.EncryptionExpires == nil || !v.EncryptionExpires.Equal(want) {
		t.Errorf("stored packets: primary %v, subkey %v; want both %v", v.PrimaryExpires, v.EncryptionExpires, want)
	}
	if key["expiringSoon"] != true {
		t.Errorf("a key expiring in 14 days is not expiringSoon: %v", key)
	}

	instant := time.Now().UTC().Add(500 * 24 * time.Hour).Truncate(time.Second)
	out := h.must(t, "UserKey/extend", a, map[string]any{"id": id, "expiresAt": instant.Format(time.RFC3339)})
	if got := parseDate(t, out["expiresAt"]); !got.Equal(instant) {
		t.Errorf("extend expiresAt = %v, want %v", got, instant)
	}
	v = h.storedValidity(t, id)
	if !v.PrimaryExpires.Equal(instant) || !v.EncryptionExpires.Equal(instant) {
		t.Errorf("extended packets: primary %v, subkey %v; want %v", v.PrimaryExpires, v.EncryptionExpires, instant)
	}

	for name, args := range map[string]map[string]any{
		"generate: both":      {"identityId": h.identA.String(), "expiresDays": 30, "expiresAt": day},
		"generate: past":      {"identityId": h.identA.String(), "expiresAt": "2001-01-01"},
		"generate: >10 years": {"identityId": h.identA.String(), "expiresAt": time.Now().AddDate(11, 0, 0).Format("2006-01-02")},
	} {
		calls := h.custody.count()
		if _, mErr := h.invoke(t, "UserKey/generate", a, args); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("%s: %+v, want invalidArguments", name, mErr)
		}
		if h.custody.count() != calls {
			t.Errorf("%s reached the crypto process", name)
		}
	}
	if _, mErr := h.invoke(t, "UserKey/extend", a, map[string]any{"id": id, "days": 10, "expiresAt": day}); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("extend with days and expiresAt: %+v", mErr)
	}
}
