// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// #301 C3 — the boot-time count behind "N identities have bound signing keys
// and no signer is configured".
//
// # Why this is an integration test and not a unit one
//
// The whole risk in C3's fix is a count that is always zero: a warning that
// never fires is indistinguishable from a deployment that has nothing to warn
// about, and this project has paid for that confusion before (CLAUDE.md — "code
// that ran without erroring while doing nothing"). The count reaches across
// ListActiveUserIDs (a SECURITY DEFINER), a per-user transaction under RLS, and
// SigningKeyForIdentity's join through app.identities.default_key_id. Every one
// of those can return empty for a reason unrelated to the answer, and none of
// them exists without a database.
//
// # Why the assertions are DELTAS
//
// The integration lane runs every package against one throwaway PostgreSQL, and
// countBoundSigningKeys is deliberately global — it is asked at boot, about the
// whole instance. Other packages' fixtures are therefore in the same database,
// so an absolute "want 1" would be a flake waiting for the test order to
// change. Each case measures the count before and after its own seeding and
// asserts on the difference, which is the quantity the code is actually
// responsible for.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

type signerHarness struct {
	super *pgxpool.Pool
	store *store.Store
}

func newSignerHarness(t *testing.T) *signerHarness {
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
	return &signerHarness{super: super, store: store.NewFromPool(app)}
}

// seedUserWithAccount inserts an enabled user and one account, the two rows an
// identity needs to exist at all.
func (h *signerHarness) seedUserWithAccount(t *testing.T) (userID, accountID ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	userID, accountID = store.NewULID(), store.NewULID()
	tag := strings.ToLower(userID.String())
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.users (id, oidc_subject, email, display_name)
		 VALUES ($1, $2, $3, 'Signer Count')`,
		userID.String(), "oidc-"+tag, tag+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		 VALUES ($1, $2, 'primary', 'imap.example.com', 'smtp.example.com', $3, $4)`,
		accountID.String(), userID.String(), []byte{0x01}, store.NewULID().String()); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return userID, accountID
}

// seedIdentity creates one fixed identity for the user.
func (h *signerHarness) seedIdentity(t *testing.T, userID, accountID ulid.ULID) ulid.ULID {
	t.Helper()
	var id ulid.ULID
	err := h.store.UserTx(context.Background(), userID, func(tx *store.UserTx) error {
		var e error
		id, e = tx.CreateIdentity(context.Background(), store.IdentityRow{
			AccountID: accountID,
			Kind:      store.IdentityFixed,
			Address:   strings.ToLower(store.NewULID().String()) + "@example.com",
			Enabled:   true,
		})
		return e
	})
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	return id
}

// bindKey inserts a PGP key for the user and makes it the identity's default.
// It goes through the same store calls the crypto process uses, so what is
// counted is what a real binding produces.
func (h *signerHarness) bindKey(t *testing.T, userID, identityID ulid.ULID, retired bool) {
	t.Helper()
	ctx := context.Background()
	keyID := store.NewULID()
	err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if _, e := tx.InsertUserKey(ctx, store.UserKey{
			ID:          keyID,
			UserID:      userID,
			Kind:        store.KeyPGP,
			Fingerprint: strings.ToUpper(strings.ReplaceAll(keyID.String(), "-", "")),
			// Binary packets, not armor: InsertUserKey refuses armor because the
			// WKD endpoint serves this column verbatim.
			PublicKey:    []byte{0x98, 0x33, 0x04, 0x01},
			PrivateKeyCT: []byte{0x01, 0x02, 0x03, 0x04},
			DecryptOnly:  retired,
		}); e != nil {
			return e
		}
		bound, e := tx.BindKeyToIdentity(ctx, keyID, identityID)
		if e != nil {
			return e
		}
		if !bound {
			t.Fatal("BindKeyToIdentity reported no binding")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bind key: %v", err)
	}
}

func (h *signerHarness) count(t *testing.T) (identities, users int) {
	t.Helper()
	identities, users, err := countBoundSigningKeys(context.Background(), h.store)
	if err != nil {
		t.Fatalf("countBoundSigningKeys: %v", err)
	}
	return identities, users
}

// countAfterSeeding is count with the assertion that makes a zero readable.
// A count of 0 identities over 0 users is not a finding about identities — it is
// the check having nothing to look at — so every measurement taken AFTER this
// test has seeded a user asserts that at least one user was scanned. Taken
// before seeding, the same assertion would fail legitimately: cmd/mailroom is
// the first package the integration lane runs, against an empty database.
func (h *signerHarness) countAfterSeeding(t *testing.T) int {
	t.Helper()
	identities, users := h.count(t)
	if users == 0 {
		t.Fatal("no active users were scanned at all — the count would be zero " +
			"whatever the identities look like, so it proves nothing")
	}
	return identities
}

func TestCountBoundSigningKeys(t *testing.T) {
	h := newSignerHarness(t)

	// ── The measurement that matters: a bound, usable key must move the count.
	// A count stuck at zero is the failure this whole test exists to exclude.
	t.Run("a bound key is counted", func(t *testing.T) {
		before, _ := h.count(t)
		user, account := h.seedUserWithAccount(t)
		identity := h.seedIdentity(t, user, account)
		h.bindKey(t, user, identity, false)
		if got := h.countAfterSeeding(t) - before; got != 1 {
			t.Errorf("the count moved by %d, want 1", got)
		}
	})

	// ── NEGATIVE CONTROL: an identity with no key must not move it. Without
	// this, a count that returned "every identity" would pass the case above.
	t.Run("an identity with no key is not counted", func(t *testing.T) {
		before, _ := h.count(t)
		user, account := h.seedUserWithAccount(t)
		h.seedIdentity(t, user, account)
		if got := h.countAfterSeeding(t) - before; got != 0 {
			t.Errorf("the count moved by %d for an identity with no key, want 0", got)
		}
	})

	// ── The count must match the SEND PATH's predicate, not a looser one. A
	// retired key is excluded by SigningKeyForIdentity, so compose would never
	// have signed with it and nothing is being downgraded by its absence.
	// Counting it would overstate the warning and train an operator to ignore it.
	t.Run("a retired key is not counted", func(t *testing.T) {
		before, _ := h.count(t)
		user, account := h.seedUserWithAccount(t)
		identity := h.seedIdentity(t, user, account)
		h.bindKey(t, user, identity, true)
		if got := h.countAfterSeeding(t) - before; got != 0 {
			t.Errorf("the count moved by %d for a retired key, want 0", got)
		}
	})

	// ── A disabled user's identities are out of scope: ListActiveUserIDs does
	// not return them, and that instance is not sending their mail either.
	t.Run("a disabled user is not scanned", func(t *testing.T) {
		user, account := h.seedUserWithAccount(t)
		identity := h.seedIdentity(t, user, account)
		h.bindKey(t, user, identity, false)
		before := h.countAfterSeeding(t)
		if _, err := h.super.Exec(context.Background(),
			`UPDATE app.users SET disabled_at = now() WHERE id = $1`, user.String()); err != nil {
			t.Fatalf("disable user: %v", err)
		}
		if got := before - h.countAfterSeeding(t); got != 1 {
			t.Errorf("disabling the user changed the count by %d, want 1", got)
		}
	})
}
