// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// UserKey/replace certifies the new key with the old one (ADR-054 section 1):
// through the real crypto service and the real store. The expired-certifier case
// is covered against the real clock-controlled service in pgpkeyd's tests; here
// the plumbing of its answer is checked with a custody that reports it.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
)

func (h *keyHarness) publicKeyOf(t *testing.T, id string) []byte {
	t.Helper()
	var pub []byte
	if err := h.super.QueryRow(context.Background(),
		`SELECT public_key FROM app.user_keys WHERE id = $1`, id).Scan(&pub); err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestReplaceCertifiesTheNewKeyByDefault(t *testing.T) {
	h := newKeyHarness(t)
	a := h.a.UserID
	oldID, _ := h.generate(t, a, h.identA, map[string]any{"bind": true})
	oldPub := h.publicKeyOf(t, oldID)

	rep := h.must(t, "UserKey/replace", a, map[string]any{"id": oldID})
	if rep["certified"] != true || rep["certifyReason"] != nil {
		t.Fatalf("replace = certified %v reason %v, want certified", rep["certified"], rep["certifyReason"])
	}
	newPub := h.publicKeyOf(t, rep["id"].(string))
	if !pgpkeys.CertifiedBy(newPub, oldPub, "me@example.com") {
		t.Fatal("the stored new key carries no certification that verifies under the old key")
	}
	// The retired old key and its bound identity moved as before.
	if rep["replacedId"] != oldID {
		t.Errorf("replacedId = %v", rep["replacedId"])
	}

	// OPT OUT: certify:false is today's behaviour, and says so.
	second := h.must(t, "UserKey/replace", a, map[string]any{"id": rep["id"], "certify": false})
	if second["certified"] != false || second["certifyReason"] != nil {
		t.Errorf("opt-out replace = %v / %v", second["certified"], second["certifyReason"])
	}
	if pgpkeys.CertifiedBy(h.publicKeyOf(t, second["id"].(string)), h.publicKeyOf(t, rep["id"].(string)), "me@example.com") {
		t.Error("certify:false still certified")
	}
}

// expiredCertifier is a custody whose certifier cannot sign: the new key is made
// uncertified and says why.
type expiredCertifier struct{ *inProcCustody }

func (c expiredCertifier) GenerateKeyCertifiedBy(ctx context.Context, userID ulid.ULID, name, email string, exp KeyExpiry, _ ulid.ULID) (CustodyKey, error) {
	k, err := c.GenerateKey(ctx, userID, name, email, exp)
	k.CertifyReason = "oldKeyExpired"
	return k, err
}

func TestReplaceWithAnExpiredOldKeyProceedsUncertified(t *testing.T) {
	h := newKeyHarness(t)
	a := h.a.UserID
	oldID, _ := h.generate(t, a, h.identA, map[string]any{"bind": true})
	h.api.custody = expiredCertifier{h.custody}

	rep, mErr := h.invoke(t, "UserKey/replace", a, map[string]any{"id": oldID})
	if mErr != nil {
		t.Fatalf("an expired old key refused the replacement: %+v", mErr)
	}
	if rep["certified"] != false || rep["certifyReason"] != "oldKeyExpired" {
		t.Errorf("replace = certified %v reason %v", rep["certified"], rep["certifyReason"])
	}
	if rep["replacedId"] != oldID {
		t.Errorf("the replacement did not happen: %v", rep)
	}
}
