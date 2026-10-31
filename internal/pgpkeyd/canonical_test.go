// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// #301 C1 — a non-canonical ULID must not reach the KDF or the seal AAD.
//
// # What can and cannot be seen from here
//
// The audit says MemoryVault masks this defect, and that is TRUE OF THE
// SYMPTOM. MemoryVault stores whatever string it is handed and hands the same
// one back, so seal time and unseal time always agree with each other no matter
// which spelling arrived — the two failure modes (a bogus CONFUSED DEPUTY, and
// an unseal failure logged as store tampering) require the store to REPORT a
// different string than the daemon sealed under, which only the real vault
// does. They are proved in canonical_integration_test.go, against StoreVault.
//
// The CAUSE, though, is visible right here and does not need a database: what
// Generate WROTE. If the record it inserts carries the verbatim request string,
// then any vault that round-trips ids through ulid.ULID — which every real one
// must, because that is what the column type is — will report something else
// later. So this file asserts on the id the service stores and the id it
// reports, and the integration file asserts on the consequences.
//
// Written the other way round, "the unit suite structurally cannot see C1", the
// package would have ended up with one test in the slow lane and no fast
// regression at all.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/pgpwire"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// nonCanonical returns a spelling of id that ulid.Parse accepts and that is not
// id.String(). Crockford base32 folds case, so lower-casing is enough — and the
// helper asserts BOTH halves of that claim, because a "non-canonical" input
// that either fails to parse or is already canonical would make every test
// below pass for the wrong reason.
func nonCanonical(t *testing.T, id string) string {
	t.Helper()
	lower := strings.ToLower(id)
	if lower == id {
		t.Fatalf("%q is unchanged by lower-casing; it is not a non-canonical spelling", id)
	}
	parsed, err := ulid.Parse(lower)
	if err != nil {
		t.Fatalf("ulid.Parse(%q) rejected the lower-cased form: %v — the premise of #301 C1 does not hold", lower, err)
	}
	if parsed.String() != id {
		t.Fatalf("ulid.Parse(%q).String() = %q, want %q", lower, parsed.String(), id)
	}
	return lower
}

func TestValidUserIDCanonicalises(t *testing.T) {
	canon := newUserID()
	lower := nonCanonical(t, canon)

	got, err := validUserID(lower)
	if err != nil {
		t.Fatalf("validUserID(%q): %v", lower, err)
	}
	if got != canon {
		t.Errorf("validUserID(%q) = %q, want the canonical %q", lower, got, canon)
	}

	// Acceptance must not widen. Whitespace around an id was rejected before
	// canonicalisation existed and stays rejected: canonicalising is about the
	// SPELLING of an id that parses, not about parsing more things.
	for _, bad := range []string{"", "   ", "alice", " " + canon, canon + "\n"} {
		if _, err := validUserID(bad); err == nil {
			t.Errorf("validUserID(%q) accepted", bad)
		} else if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
			t.Errorf("validUserID(%q) code = %q, want %q", bad, code, pgpwire.CodeInvalidRequest)
		}
	}
}

// TestGenerateStoresCanonicalUserID is the fast-lane regression for C1's cause.
//
// The record's UserID is the string the store will echo back on every later
// read, and it is also the string Generate sealed under (the same variable
// feeds WrappingKey and SealAAD). If the two can differ, the key is unusable;
// asserting the stored one is canonical is asserting they cannot.
func TestGenerateStoresCanonicalUserID(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()

	canon := newUserID()
	lower := nonCanonical(t, canon)

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{
		UserID: lower, Email: "canonical@example.com",
	})
	if err != nil {
		t.Fatalf("Generate with a lower-cased user id: %v", err)
	}

	rec, err := h.vault.Get(ctx, gen.Key.KeyID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	if rec.UserID != canon {
		t.Errorf("stored owner = %q, want the canonical %q — the sealed key and the row "+
			"will disagree the moment the store reports the id back", rec.UserID, canon)
	}
	if gen.Key.UserID != canon {
		t.Errorf("response owner = %q, want the canonical %q", gen.Key.UserID, canon)
	}

	// The sealed bytes must open under the CANONICAL id, because that is what a
	// real vault will report at unseal time. This is the assertion that ties the
	// stored string to the derivation rather than merely inspecting a field.
	if _, err := h.svc.unseal(Record{
		KeyID: rec.KeyID, UserID: canon, Sealed: rec.Sealed,
	}); err != nil {
		t.Errorf("the sealed key does not open under the canonical owner: %v", err)
	}

	// Every method must accept both spellings of the caller's own id and treat
	// them as the same user. Before the fix the lower-cased Sign was refused as
	// a confused deputy.
	for _, spelling := range []struct{ name, id string }{
		{"canonical", canon},
		{"lower-cased", lower},
	} {
		t.Run("sign as "+spelling.name, func(t *testing.T) {
			got, err := h.client.Sign(ctx, pgpwire.SignRequest{
				UserID: spelling.id, KeyID: gen.Key.KeyID, Data: []byte("hello"),
			})
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if !strings.Contains(got.Signature, "-----BEGIN PGP SIGNATURE-----") {
				t.Errorf("not an armored signature: %.60q", got.Signature)
			}
		})
		t.Run("export as "+spelling.name, func(t *testing.T) {
			if _, err := h.client.ExportPublic(ctx, pgpwire.ExportPublicRequest{
				UserID: spelling.id, KeyID: gen.Key.KeyID,
			}); err != nil {
				t.Fatalf("ExportPublic: %v", err)
			}
		})
	}

	// ── NEGATIVE CONTROL. Canonicalisation folds spellings of ONE id together;
	// it must not fold two different ids. Without this, a validUserID that
	// returned rec.UserID — or a comparison someone "simplified" away — would
	// pass everything above and open the boundary.
	t.Run("a different user is still refused", func(t *testing.T) {
		other := newUserID()
		if _, err := h.client.Sign(ctx, pgpwire.SignRequest{
			UserID: other, KeyID: gen.Key.KeyID, Data: []byte("hello"),
		}); err == nil {
			t.Fatal("another user signed with this key — the boundary is open")
		} else if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeDenied {
			t.Errorf("code = %q, want %q", code, pgpwire.CodeDenied)
		}
	})
}

// TestImportStoresCanonicalUserID is TestGenerateStoresCanonicalUserID for the
// other sealing path. Import repeats Generate's derive-seal-store sequence with
// its own copy of the code, so it can regress on its own.
func TestImportStoresCanonicalUserID(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()

	// A real key to import: generate one for an unrelated user and take the
	// private half out of band, the way import_test.go does.
	donor := newUserID()
	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: donor, Email: "donor@example.com"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	armored := armorPrivateForTest(t, h.unsealPrivate(t, gen.Key.KeyID))

	canon := newUserID()
	lower := nonCanonical(t, canon)

	imp, err := h.client.Import(ctx, pgpwire.ImportRequest{UserID: lower, PrivateKey: armored})
	if err != nil {
		t.Fatalf("Import with a lower-cased user id: %v", err)
	}
	if imp.Key.UserID != canon {
		t.Errorf("response owner = %q, want the canonical %q", imp.Key.UserID, canon)
	}
	rec, err := h.vault.Get(ctx, imp.Key.KeyID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	if rec.UserID != canon {
		t.Errorf("stored owner = %q, want the canonical %q", rec.UserID, canon)
	}
	if _, err := h.svc.unseal(Record{KeyID: rec.KeyID, UserID: canon, Sealed: rec.Sealed}); err != nil {
		t.Errorf("the re-sealed imported key does not open under the canonical owner: %v", err)
	}
}

// TestBindIdentityUsesTheValidatedIdentityID covers the cousin of C1 one field
// over: BindIdentity validated the TRIMMED identity id and then passed the
// UNTRIMMED one to the vault, so a padded id passed validation and matched no
// row. The recording vault below is the only way to see which string actually
// left the service.
func TestBindIdentityUsesTheValidatedIdentityID(t *testing.T) {
	ctx := context.Background()
	canonUser := newUserID()
	canonIdentity := ulid.Make().String()

	for _, tc := range []struct{ name, sent string }{
		{"canonical", canonIdentity},
		{"lower-cased", strings.ToLower(canonIdentity)},
		{"padded", "  " + canonIdentity + "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vault := &bindRecorder{MemoryVault: NewMemoryVault()}
			svc, err := NewService(make([]byte, crypto.KeySize), vault, discardLogger())
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			gen, err := svc.Generate(ctx, pgpwire.GenerateRequest{
				UserID: canonUser, Email: "binder@example.com",
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if _, err := svc.BindIdentity(ctx, pgpwire.BindIdentityRequest{
				UserID: strings.ToLower(canonUser), KeyID: gen.Key.KeyID, IdentityID: tc.sent,
			}); err != nil {
				t.Fatalf("BindIdentity: %v", err)
			}
			if vault.gotIdentity != canonIdentity {
				t.Errorf("vault was asked to bind identity %q, want the canonical %q",
					vault.gotIdentity, canonIdentity)
			}
			if vault.gotCaller != canonUser {
				t.Errorf("vault saw caller %q, want the canonical %q", vault.gotCaller, canonUser)
			}
		})
	}
}

// bindRecorder is a MemoryVault that can bind, and remembers what it was asked
// to bind. It exists because the identity id never reaches a place the service
// reports — the only observer is the vault.
type bindRecorder struct {
	*MemoryVault
	gotIdentity string
	gotCaller   string
}

func (b *bindRecorder) BindIdentity(ctx context.Context, keyID, identityID string) (bool, error) {
	b.gotIdentity = identityID
	if caller, ok := callerFrom(ctx); ok {
		b.gotCaller = caller
	}
	return true, nil
}
