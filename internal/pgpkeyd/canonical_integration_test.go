// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package pgpkeyd

// #301 C1's SYMPTOM, which only the real vault can show.
//
// # Why this cannot live in the unit suite
//
// The defect is a disagreement between the string the daemon sealed under and
// the string the store reports back. MemoryVault stores the verbatim id and
// returns the verbatim id, so those two are the same string by construction and
// the disagreement cannot arise there — the daemon would sign happily with a
// lower-cased id and nothing would look wrong. StoreVault round-trips through
// ulid.ULID (k.UserID.String()), which is what makes the two forms diverge, so
// the two failure modes below exist only against a real database:
//
//	sign with the SAME lower-cased id  → rec.UserID != req.UserID → CodeDenied,
//	                                     which cmd/mailroom logs as CONFUSED
//	                                     DEPUTY, "a bug in Mailroom"
//	sign with the CANONICAL id         → ownership passes, unseal derives from
//	                                     rec.UserID ≠ the seal-time string →
//	                                     crypto.Open fails → CodeInternal,
//	                                     logged as store tampering
//
// That is not a divergence between double and real that should be papered over:
// MemoryVault is unfiltered ON PURPOSE (see vault.go) so the service's own
// ownership check can be deleted and shown to matter. Teaching it to canonicalise
// would make it simulate the store, and a double that simulates the thing under
// test is how a suite comes to agree with whatever its author believed.
//
// The unit suite covers the CAUSE — what Generate writes — in canonical_test.go.
// This file covers what the cause does.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/pgpwire"
	"mailroom/internal/store"
)

type storeHarness struct {
	super *pgxpool.Pool
	svc   *Service
}

// newStoreHarness wires the real Service onto the real StoreVault, connected as
// mailroom_app — the production arrangement. The superuser pool is for seeding
// users only; nothing under test reads through it.
func newStoreHarness(t *testing.T) *storeHarness {
	t.Helper()
	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	t.Cleanup(app.Close)

	vault, err := NewStoreVault(store.NewFromPool(app))
	if err != nil {
		t.Fatalf("NewStoreVault: %v", err)
	}
	root := make([]byte, crypto.KeySize)
	for i := range root {
		root[i] = byte(i*11 + 3)
	}
	svc, err := NewService(root, vault, discardLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &storeHarness{super: super, svc: svc}
}

// seedUser inserts one enabled user and returns its CANONICAL id.
func (h *storeHarness) seedUser(t *testing.T) string {
	t.Helper()
	id := ulid.Make().String()
	tag := strings.ToLower(id)
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.users (id, oidc_subject, email, display_name)
		 VALUES ($1, $2, $3, $4)`,
		id, "oidc-"+tag, tag+"@example.com", "Canonical Test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// TestNonCanonicalUserIDDoesNotBrickAKey is #301 C1's regression, end to end,
// through the vault that can actually exhibit it.
func TestNonCanonicalUserIDDoesNotBrickAKey(t *testing.T) {
	h := newStoreHarness(t)
	ctx := context.Background()

	canon := h.seedUser(t)
	lower := nonCanonical(t, canon)
	data := []byte("Content-Type: text/plain\r\n\r\ncanonical\r\n")

	// The key is generated NAMING THE USER IN LOWER CASE, which is the whole
	// premise: ulid.Parse accepts it, so nothing upstream rejects the request.
	gen, err := h.svc.Generate(ctx, pgpwire.GenerateRequest{
		UserID: lower, Email: "canon@example.com", Name: "Canon",
	})
	if err != nil {
		t.Fatalf("Generate with a lower-cased user id: %v", err)
	}

	// ── CONTROL ON THE PREMISE: the store must really report the canonical
	// form. If it echoed the request's spelling there would be nothing to
	// diverge, and every assertion below would pass without testing anything.
	rec, err := h.svc.vault.Get(WithCaller(ctx, canon), gen.Key.KeyID)
	if err != nil {
		t.Fatalf("read the stored key back: %v", err)
	}
	if rec.UserID != canon {
		t.Fatalf("the store reported owner %q, want the canonical %q — this suite's premise does not hold", rec.UserID, canon)
	}

	// ── FAILURE MODE 1 (was CodeDenied → "CONFUSED DEPUTY … a bug in Mailroom").
	t.Run("signing with the same lower-cased id is not a confused deputy", func(t *testing.T) {
		got, err := h.svc.Sign(ctx, pgpwire.SignRequest{
			UserID: lower, KeyID: gen.Key.KeyID, Data: data,
		})
		if err != nil {
			code, _ := pgpwire.CodeOf(err)
			t.Fatalf("Sign refused the owner's own request with code %q: %v", code, err)
		}
		verifySignature(t, gen.Key.PublicKey, data, got.Signature)
	})

	// ── FAILURE MODE 2 (was CodeInternal, logged as store tampering).
	t.Run("signing with the canonical id unseals", func(t *testing.T) {
		got, err := h.svc.Sign(ctx, pgpwire.SignRequest{
			UserID: canon, KeyID: gen.Key.KeyID, Data: data,
		})
		if err != nil {
			code, _ := pgpwire.CodeOf(err)
			t.Fatalf("Sign failed with code %q: %v", code, err)
		}
		verifySignature(t, gen.Key.PublicKey, data, got.Signature)
	})

	t.Run("export works under both spellings", func(t *testing.T) {
		for name, id := range map[string]string{"canonical": canon, "lower-cased": lower} {
			out, err := h.svc.ExportPublic(ctx, pgpwire.ExportPublicRequest{UserID: id, KeyID: gen.Key.KeyID})
			if err != nil {
				t.Errorf("ExportPublic as the %s id: %v", name, err)
				continue
			}
			if out.Key.Fingerprint != gen.Key.Fingerprint {
				t.Errorf("%s: fingerprint = %q, want %q", name, out.Key.Fingerprint, gen.Key.Fingerprint)
			}
		}
	})

	// ── NEGATIVE CONTROL. Canonicalisation folds spellings of ONE id together
	// and must not fold two ids. Against the real vault the refusal comes from
	// RLS first — a foreign key is not visible at all, so the answer is
	// not-found rather than denied. The SERVICE's own denial is proved in the
	// unit suite, against the deliberately unfiltered MemoryVault; here the
	// point is that the database still refuses after canonicalisation.
	t.Run("another user still cannot use this key", func(t *testing.T) {
		other := h.seedUser(t)
		got, err := h.svc.Sign(ctx, pgpwire.SignRequest{
			UserID: other, KeyID: gen.Key.KeyID, Data: data,
		})
		if err == nil {
			t.Fatalf("another user signed with this key: %.60q", got.Signature)
		}
		if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeNotFound {
			t.Errorf("code = %q, want %q (RLS hides the row before the ownership check runs)", code, pgpwire.CodeNotFound)
		}
		if got != nil {
			t.Errorf("a response body accompanied the refusal: %+v", got)
		}
	})
}

// TestNonCanonicalUserIDImportRoundTrips is the same regression for Import,
// which repeats Generate's derive-seal-store sequence in its own code.
func TestNonCanonicalUserIDImportRoundTrips(t *testing.T) {
	h := newStoreHarness(t)
	ctx := context.Background()

	donor := h.seedUser(t)
	gen, err := h.svc.Generate(ctx, pgpwire.GenerateRequest{UserID: donor, Email: "donor@example.com"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	rec, err := h.svc.vault.Get(WithCaller(ctx, donor), gen.Key.KeyID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	wrap, err := WrappingKey(h.svc.root, donor)
	if err != nil {
		t.Fatalf("WrappingKey: %v", err)
	}
	priv, err := crypto.Open(wrap, rec.Sealed, SealAAD(donor, rec.KeyID))
	if err != nil {
		t.Fatalf("open the donor's sealed key: %v", err)
	}
	armored := armorPrivateForTest(t, priv)

	canon := h.seedUser(t)
	lower := nonCanonical(t, canon)
	imp, err := h.svc.Import(ctx, pgpwire.ImportRequest{UserID: lower, PrivateKey: armored})
	if err != nil {
		t.Fatalf("Import with a lower-cased user id: %v", err)
	}

	data := []byte("imported\r\n")
	for name, id := range map[string]string{"canonical": canon, "lower-cased": lower} {
		got, err := h.svc.Sign(ctx, pgpwire.SignRequest{UserID: id, KeyID: imp.Key.KeyID, Data: data})
		if err != nil {
			code, _ := pgpwire.CodeOf(err)
			t.Fatalf("Sign as the %s id: code %q: %v", name, code, err)
		}
		verifySignature(t, imp.Key.PublicKey, data, got.Signature)
	}
}

// verifySignature asks the question a recipient asks. Without it, "Sign
// returned no error" would satisfy every assertion above even if the armor
// covered nothing.
func verifySignature(t *testing.T, armoredPublic string, data []byte, signature string) {
	t.Helper()
	keyring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredPublic))
	if err != nil {
		t.Fatalf("read the exported public key: %v", err)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(
		keyring, bytes.NewReader(data), strings.NewReader(signature), nil); err != nil {
		t.Fatalf("the exported public key did not verify the signature: %v", err)
	}
	// NEGATIVE CONTROL: a verification that cannot fail proves nothing.
	tampered := append([]byte(nil), data...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := openpgp.CheckArmoredDetachedSignature(
		keyring, bytes.NewReader(tampered), strings.NewReader(signature), nil); err == nil {
		t.Fatal("verification accepted tampered data — it cannot observe the failure it claims to exclude")
	}
}
