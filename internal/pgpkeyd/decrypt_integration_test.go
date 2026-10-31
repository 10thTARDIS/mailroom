// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package pgpkeyd

// Decrypt against the REAL vault (#47 6b): StoreVault's List under RLS, a key
// retired in the database (decrypt_only AND revoked_at), and the audit row the
// crypto process writes into app.pgp_decrypt_audit as mailroom_app.

import (
	"context"
	"testing"

	"mailroom/internal/pgpwire"
)

func TestStoreVaultDecryptWithARetiredKeyAndAudits(t *testing.T) {
	h := newStoreHarness(t)
	ctx := context.Background()
	user := h.seedUser(t)
	other := h.seedUser(t)

	gen, err := h.svc.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "vault@example.com"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	ct := encryptTo(t, pubRing(t, gen.Key.PublicKey), []byte(secretEntity), nil)

	// Retire it the way the database records retirement.
	if _, err := h.super.Exec(ctx,
		`UPDATE app.user_keys SET decrypt_only = true, revoked_at = now() WHERE id = $1`, gen.Key.KeyID); err != nil {
		t.Fatal(err)
	}
	// CONTROL: the retirement is visible to the service — Sign refuses it.
	if _, err := h.svc.Sign(ctx, pgpwire.SignRequest{UserID: user, KeyID: gen.Key.KeyID, Data: []byte("x")}); err == nil {
		t.Fatal("CONTROL FAILED: Sign accepted the retired key, so the retirement did not apply")
	}

	msgID := newMessageID()
	resp, err := h.svc.Decrypt(ctx, pgpwire.DecryptRequest{
		UserID: user, MessageID: msgID, Purpose: pgpwire.DecryptForAttachment, Ciphertext: ct,
	})
	if err != nil {
		t.Fatalf("a retired key did not decrypt through the real vault: %v", err)
	}
	if string(resp.Entity) != secretEntity {
		t.Errorf("entity = %q", resp.Entity)
	}

	// Another user's request is offered only that user's keys (RLS + the filter).
	if _, err := h.svc.Decrypt(ctx, pgpwire.DecryptRequest{
		UserID: other, MessageID: msgID, Purpose: pgpwire.DecryptForBody, Ciphertext: ct,
	}); err == nil {
		t.Error("another user decrypted this user's mail")
	}

	// The trail, as the operator would read it.
	var outcome, purpose, fpr string
	if err := h.super.QueryRow(ctx, `
		SELECT outcome, purpose, coalesce(key_fingerprint, '')
		  FROM app.pgp_decrypt_audit WHERE user_id = $1 AND message_id = $2`, user, msgID).
		Scan(&outcome, &purpose, &fpr); err != nil {
		t.Fatalf("no audit row for the decryption: %v", err)
	}
	if outcome != "decrypted" || purpose != "attachment" || fpr != gen.Key.Fingerprint {
		t.Errorf("audit row = (%s, %s, %s), want (decrypted, attachment, %s)", outcome, purpose, fpr, gen.Key.Fingerprint)
	}
	var failed int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM app.pgp_decrypt_audit WHERE user_id = $1 AND outcome = 'failed'`, other).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Errorf("the other user's failed attempt left %d audit rows, want 1", failed)
	}
}
