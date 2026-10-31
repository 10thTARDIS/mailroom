// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// export-wrapped is the only method whose RESPONSE carries private key material
// (spec §9, ADR-024 §4 as amended, #54), so it gets its own boundary tests
// rather than relying on the generic ones.

import (
	"context"
	"strings"
	"testing"

	"mailroom/internal/pgpwire"
)

const goodPassphrase = "correct horse battery staple"

// TestExportWrappedRoundTripsUnderTheUsersPassphrase is the positive control for
// the whole operation: the export must be a REAL key that opens with the
// passphrase and is the same key it claims to be.
//
// ⚠ Without this, every refusal-shaped assertion in this file is satisfied by a
// daemon that returns garbage. "It is encrypted" and "the wrong passphrase
// fails" are both true of a random blob.
func TestExportWrappedRoundTripsUnderTheUsersPassphrase(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{
		UserID: user, Name: "Wrapped", Email: "wrapped@example.com",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var resp pgpwire.ExportWrappedResponse
	if err := h.client.Call(ctx, pgpwire.MethodExportWrapped, pgpwire.ExportWrappedRequest{
		UserID: user, KeyID: gen.Key.KeyID, Passphrase: goodPassphrase,
	}, &resp); err != nil {
		t.Fatalf("ExportWrapped: %v", err)
	}
	if resp.Key.Fingerprint != gen.Key.Fingerprint {
		t.Errorf("exported fingerprint %q, want %q", resp.Key.Fingerprint, gen.Key.Fingerprint)
	}

	ent := readArmoredKey(t, resp.PrivateKey)
	if !ent.PrivateKey.Encrypted {
		t.Fatal("the exported key is not encrypted at all")
	}
	if err := ent.PrivateKey.Decrypt([]byte(goodPassphrase)); err != nil {
		t.Fatalf("the exported key does not open with its own passphrase: %v", err)
	}
	// ⚠ EVERY SUBKEY TOO. A modern key encrypts to its SUBKEY, so an export
	// whose primary opens and whose encryption subkey does not is an export that
	// cannot read the user's mail — the exact thing it exists to preserve.
	for i, sub := range ent.Subkeys {
		if sub.PrivateKey == nil {
			continue
		}
		if err := sub.PrivateKey.Decrypt([]byte(goodPassphrase)); err != nil {
			t.Errorf("subkey %d does not open with the passphrase: %v", i, err)
		}
	}

	// The fingerprint is the identity check that matters: it proves this is the
	// user's key and not some other key the daemon happened to have.
	if got := fingerprint(ent); got != gen.Key.Fingerprint {
		t.Errorf("the exported key's fingerprint is %s, want %s", got, gen.Key.Fingerprint)
	}
}

// TestExportWrappedFailsClosedForAnotherUsersKey. The confused-deputy property,
// on the one method where getting it wrong hands over the key itself.
//
// ⚠ IT CARRIES ITS OWN POSITIVE CONTROL: the owner must succeed on the same key
// in the same test. A daemon that refused every export would otherwise pass.
func TestExportWrappedFailsClosedForAnotherUsersKey(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	owner, stranger := newUserID(), newUserID()

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{
		UserID: owner, Email: "owner@example.com",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// The refusal.
	var stolen pgpwire.ExportWrappedResponse
	err = h.client.Call(ctx, pgpwire.MethodExportWrapped, pgpwire.ExportWrappedRequest{
		UserID: stranger, KeyID: gen.Key.KeyID, Passphrase: goodPassphrase,
	}, &stolen)
	if err == nil {
		t.Fatal("a stranger exported another user's private key")
	}
	code, ok := pgpwire.CodeOf(err)
	if !ok {
		t.Fatalf("refusal was not a wire error: %v", err)
	}
	// ⚠ THE CODE IS ASSERTED, not merely the failure. A refusal for some other
	// reason — a bad argument, an internal fault — would leave the boundary
	// untested while looking exactly like this.
	if code != pgpwire.CodeDenied {
		t.Errorf("code = %q, want %q — refused, but not for the reason that proves the ownership check ran", code, pgpwire.CodeDenied)
	}
	if strings.Contains(stolen.PrivateKey, "PRIVATE KEY") {
		t.Fatal("the refused response carried a private key anyway")
	}

	// The positive control, same key, same method.
	var mine pgpwire.ExportWrappedResponse
	if err := h.client.Call(ctx, pgpwire.MethodExportWrapped, pgpwire.ExportWrappedRequest{
		UserID: owner, KeyID: gen.Key.KeyID, Passphrase: goodPassphrase,
	}, &mine); err != nil {
		t.Fatalf("the OWNER could not export their own key (%v) — the refusal above "+
			"proves nothing if nobody can export at all", err)
	}
	if mine.PrivateKey == "" {
		t.Error("the owner's export returned no key")
	}
}

// TestExportWrappedRefusesAWeakPassphrase.
//
// ⚠ The passphrase is the ONLY thing protecting the artefact — this process
// cannot open it and cannot help later — so a one-character one would make the
// operation a raw key export with extra steps.
func TestExportWrappedRefusesAWeakPassphrase(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{
		UserID: user, Email: "weak@example.com",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var resp pgpwire.ExportWrappedResponse
	err = h.client.Call(ctx, pgpwire.MethodExportWrapped, pgpwire.ExportWrappedRequest{
		UserID: user, KeyID: gen.Key.KeyID, Passphrase: "short",
	}, &resp)
	if err == nil {
		t.Fatal("a five-character passphrase was accepted")
	}
	if resp.PrivateKey != "" {
		t.Error("the refused response carried a key anyway")
	}

	// ⚠ The boundary is AT the minimum, not somewhere near it: one character
	// under must fail and exactly the minimum must succeed. Without both, the
	// check could be off by any amount and still look enforced.
	justUnder := strings.Repeat("a", pgpwire.MinExportPassphrase-1)
	if err := h.client.Call(ctx, pgpwire.MethodExportWrapped, pgpwire.ExportWrappedRequest{
		UserID: user, KeyID: gen.Key.KeyID, Passphrase: justUnder,
	}, &resp); err == nil {
		t.Errorf("a passphrase of %d characters was accepted; the minimum is %d",
			len(justUnder), pgpwire.MinExportPassphrase)
	}
	exactly := strings.Repeat("a", pgpwire.MinExportPassphrase)
	if err := h.client.Call(ctx, pgpwire.MethodExportWrapped, pgpwire.ExportWrappedRequest{
		UserID: user, KeyID: gen.Key.KeyID, Passphrase: exactly,
	}, &resp); err != nil {
		t.Errorf("a passphrase of exactly the minimum (%d) was refused: %v",
			pgpwire.MinExportPassphrase, err)
	}
}

// TestExportWrappedExportsARetiredKey.
//
// ⚠ Sign refuses a retired key; this must NOT. A retired key is precisely the
// one a user most needs out of the system: it can never sign again, and it is
// the only thing that opens the mail already encrypted to it. Refusing it would
// strand the archive the export exists to preserve.
func TestExportWrappedExportsARetiredKey(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{
		UserID: user, Email: "retired@example.com",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	h.mutateRecord(t, gen.Key.KeyID, func(r *Record) { r.DecryptOnly = true })

	// The premise: signing with it is refused, so "retired" really is in effect.
	if _, err := h.client.Sign(ctx, pgpwire.SignRequest{
		UserID: user, KeyID: gen.Key.KeyID, Data: []byte("x"),
	}); err == nil {
		t.Fatal("the key is not actually retired — Sign accepted it, so this test's " +
			"premise does not hold")
	}

	var resp pgpwire.ExportWrappedResponse
	if err := h.client.Call(ctx, pgpwire.MethodExportWrapped, pgpwire.ExportWrappedRequest{
		UserID: user, KeyID: gen.Key.KeyID, Passphrase: goodPassphrase,
	}, &resp); err != nil {
		t.Fatalf("a retired key could not be exported (%v) — the user cannot take out "+
			"the key that opens their own archive", err)
	}
	ent := readArmoredKey(t, resp.PrivateKey)
	if err := ent.PrivateKey.Decrypt([]byte(goodPassphrase)); err != nil {
		t.Errorf("the retired key's export does not open: %v", err)
	}
}
