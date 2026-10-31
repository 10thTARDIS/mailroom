// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// The two key-creation paths must record DIFFERENT origins (#54).
//
// This is the wiring the whole provenance column depends on. If both handlers
// wrote the same value — or neither wrote one — the column would still exist,
// the migration would still apply, every other test would still pass, and
// `delete-user` would confidently report a number that means nothing.

import (
	"context"
	"testing"

	"mailroom/internal/pgpwire"
	"mailroom/internal/store"
)

// TestGenerateAndImportRecordDifferentOrigins asserts both values from ONE
// test, against the SAME key material.
//
// ⚠ THAT IS DELIBERATE. Two separate tests, each asserting its own handler's
// constant, would both pass if someone wired both sites to the same value —
// each would simply assert whatever its own site wrote. Importing the very key
// that was just generated makes the two rows differ in ORIGIN AND NOTHING ELSE:
// same fingerprint, same public key, same private half. A single shared
// constant collapses them and this goes red.
func TestGenerateAndImportRecordDifferentOrigins(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()

	minted := newUserID()
	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{
		UserID: minted, Email: "minted@example.com",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	genRec, err := h.vault.Get(WithCaller(ctx, minted), gen.Key.KeyID)
	if err != nil {
		t.Fatalf("vault.Get after generate: %v", err)
	}
	if genRec.Origin != store.KeyGenerated {
		t.Errorf("a generated key recorded origin %q, want %q — a delete would then "+
			"look safe for the only copy of a private key that exists",
			genRec.Origin, store.KeyGenerated)
	}

	// The SAME key, handed to a different user through the import path.
	armored := armorPrivateForTest(t, h.unsealPrivate(t, gen.Key.KeyID))
	holder := newUserID()
	imp, err := h.client.Import(ctx, pgpwire.ImportRequest{
		UserID: holder, PrivateKey: armored,
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	impRec, err := h.vault.Get(WithCaller(ctx, holder), imp.Key.KeyID)
	if err != nil {
		t.Fatalf("vault.Get after import: %v", err)
	}
	if impRec.Origin != store.KeyImported {
		t.Errorf("an imported key recorded origin %q, want %q", impRec.Origin, store.KeyImported)
	}

	// The assertion that a shared constant cannot survive.
	if genRec.Origin == impRec.Origin {
		t.Fatalf("both paths recorded the same origin (%q) for the same key material — "+
			"the column cannot distinguish the only copy from a duplicate, which is "+
			"the entire reason it exists", genRec.Origin)
	}
	if genRec.Fingerprint != impRec.Fingerprint {
		t.Fatalf("the two rows are not the same key (%q vs %q), so their differing "+
			"origins prove nothing", genRec.Fingerprint, impRec.Fingerprint)
	}
}
