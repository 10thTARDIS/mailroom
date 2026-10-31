// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for WKD publication (Forgejo #48, migration 00031).
//
// The anonymous lookup path is the only place in Mailroom where a stranger's
// request reaches another user's rows, so the tests here are less about "does
// publishing work" and more about the shape of the hole it opens.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/wkd"
)

// joeDoeHash is the draft's published test vector, for the local part
// "joe.doe". It is NOT the hash of any address this suite seeds, and is used
// only where a WRONG hash is the point — see the mismatch subtest.
const joeDoeHash = "iy9q119eutrkn8s1mk4r39qejnbu3n5q"

// realPublicKey is a genuine OpenPGP public key in the representation the
// column holds: BINARY packets, produced by the same serializer the crypto
// process uses.
//
// It is generated rather than pasted because the bug this suite failed to catch
// was a representation mismatch, and a hand-made stand-in cannot expose one: it
// encodes the author's belief about the format, which is precisely the thing in
// question. The previous fixture was "\x99\x01\x02binary-public-key" — bytes no
// producer in this codebase emits and no consumer can parse.
func realPublicKey(t *testing.T, email string) []byte {
	t.Helper()
	entity, err := openpgp.NewEntity("Fixture", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
		Curve:     packet.Curve25519,
		V6Keys:    false,
	})
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	var buf bytes.Buffer
	if err := entity.Serialize(&buf); err != nil {
		t.Fatalf("serialize fixture public key: %v", err)
	}
	return buf.Bytes()
}

// seedPublishableKey inserts a key bound to the user's seeded fixed identity.
func seedPublishableKey(t *testing.T, s *Store, userID, identityID ulid.ULID) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	keyID := NewULID()
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		if _, err := tx.InsertUserKey(ctx, UserKey{
			ID: keyID, UserID: userID, Kind: KeyPGP,
			Fingerprint:  "PUB" + keyID.String(),
			PublicKey:    realPublicKey(t, "fixture@example.com"),
			PrivateKeyCT: []byte{0xDE, 0xAD, 0xBE, 0xEF},
		}); err != nil {
			return err
		}
		bound, err := tx.BindKeyToIdentity(ctx, keyID, identityID)
		if err != nil {
			return err
		}
		if !bound {
			t.Fatal("could not bind the seeded key to the seeded identity")
		}
		return nil
	}); err != nil {
		t.Fatalf("seed publishable key: %v", err)
	}
	return keyID
}

// identityAddress reads the seeded fixed identity's address, since the fixture
// chooses it (a-<ULID>@example.com).
func identityAddress(t *testing.T, h *harness, identityID string) string {
	t.Helper()
	var addr string
	if err := h.super.QueryRow(context.Background(),
		`SELECT address::text FROM app.identities WHERE id = $1`, identityID).Scan(&addr); err != nil {
		t.Fatalf("read identity address: %v", err)
	}
	if !strings.Contains(addr, "@") {
		t.Fatalf("seeded identity %q is not an address", addr)
	}
	return addr
}

// publishedAs derives the domain and the WKD hash a seeded identity is
// genuinely reachable at.
//
// ⚠ THIS REPLACED A CONSTANT. The earlier version published the draft's test
// vector for "joe.doe" against an identity addressed a-<ULID>@example.com, and
// the anonymous lookup then found it — because publication and lookup used the
// same wrong string. That test passed identically against an implementation
// that filed every key under one constant hash, which is to say it tested
// nothing about the address-to-path mapping WKD entirely consists of.
func publishedAs(t *testing.T, h *harness, identityID string) (domain, hash string) {
	t.Helper()
	localPart, domain, err := wkd.SplitAddress(identityAddress(t, h, identityID))
	if err != nil {
		t.Fatalf("split seeded address: %v", err)
	}
	return domain, wkd.HashLocalPart(localPart)
}

// TestPublishThenAnonymousLookupFindsTheKey is the end-to-end publication
// property, and it carries its own before/after control: the SAME lookup must
// find nothing before publishing and the key after. Without the "before" half,
// a lookup that always succeeded would pass.
func TestPublishThenAnonymousLookupFindsTheKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()

	userID := ulid.MustParse(a.ID)
	identityID := ulid.MustParse(a.IdentityID)
	keyID := seedPublishableKey(t, s, userID, identityID)
	domain, hash := publishedAs(t, h, a.IdentityID)

	if _, found, err := s.LookupWKDKey(ctx, domain, hash); err != nil {
		t.Fatalf("lookup before publishing: %v", err)
	} else if found {
		t.Fatal("an unpublished key was already visible to an anonymous lookup")
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		ok, err := tx.PublishKey(ctx, keyID, hash)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("PublishKey reported not-found for the user's own key")
		}
		return nil
	}); err != nil {
		t.Fatalf("PublishKey: %v", err)
	}

	got, found, err := s.LookupWKDKey(ctx, domain, hash)
	if err != nil {
		t.Fatalf("lookup after publishing: %v", err)
	}
	if !found {
		t.Fatal("a published key is not visible to an anonymous lookup — WKD would serve 404 for every correspondent")
	}
	if len(got.PublicKey) == 0 {
		t.Error("the lookup returned an empty public key")
	}
	if got.Fingerprint == "" {
		t.Error("the lookup returned no fingerprint")
	}
	// WHAT COMES BACK MUST BE SERVABLE, not merely present. internal/keypub
	// writes these bytes to the wire unchanged and refuses armor with a 500, so
	// "found a row" is not the property that matters to a correspondent.
	if bytes.Contains(got.PublicKey, []byte("-----BEGIN PGP")) {
		t.Error("the published key is ASCII-armored; the WKD endpoint refuses to serve it")
	}
	if _, err := openpgp.ReadKeyRing(bytes.NewReader(got.PublicKey)); err != nil {
		t.Errorf("the published bytes are not a readable OpenPGP key: %v", err)
	}

	// And withdrawal must actually withdraw.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, err := tx.UnpublishKey(ctx, keyID)
		return err
	}); err != nil {
		t.Fatalf("UnpublishKey: %v", err)
	}
	if _, found, err := s.LookupWKDKey(ctx, domain, hash); err != nil {
		t.Fatalf("lookup after unpublishing: %v", err)
	} else if found {
		t.Error("an unpublished key is still served to anonymous callers")
	}
}

// TestPublishRefusesAHashThatIsNotTheAddress. The failure this guards is silent
// by construction: the row commits, wkd_published reads true, and the key is
// filed under a directory name no correspondent's client will ever ask for.
// Nothing errors, and the only symptom is in someone else's mail client.
//
// The wrong hash used here is the draft's own vector for "joe.doe" — which is
// exactly what the earlier version of this suite published against every
// identity, and which the lookup then dutifully found.
func TestPublishRefusesAHashThatIsNotTheAddress(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()

	userID := ulid.MustParse(a.ID)
	keyID := seedPublishableKey(t, s, userID, ulid.MustParse(a.IdentityID))
	domain, rightHash := publishedAs(t, h, a.IdentityID)
	if joeDoeHash == rightHash {
		t.Fatal("the seeded address hashes to the draft's vector; this test has no negative case")
	}

	err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.PublishKey(ctx, keyID, joeDoeHash)
		return e
	})
	if !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("err = %v, want ErrNotPublishable — a key published under a hash that is not "+
			"its address's is unfindable, and nothing else reports that", err)
	}

	// And nothing was written: neither under the wrong name nor the right one.
	for name, hash := range map[string]string{"the wrong hash": joeDoeHash, "the right hash": rightHash} {
		if _, found, err := s.LookupWKDKey(ctx, domain, hash); err != nil {
			t.Fatalf("lookup at %s: %v", name, err)
		} else if found {
			t.Errorf("the refused publication still made the key visible at %s", name)
		}
	}
}

// TestWKDDefinerCannotReadPrivateKeyMaterial is the property this whole design
// is built around, and it is enforced by PRIVILEGE rather than by code.
//
// ⚠ REWRITTEN FOR #266, and the rewrite is the point. This test used to pin the
// ACCEPTED GAP — it asserted that mailroom_owner COULD read private_key_ct —
// with a comment saying it should go red when the definer moved to a
// non-owning role, and that the red would be the reminder. It did, and this is
// the reminder being acted on. It now asserts the denial, which is what #48
// claimed three times and never had.
//
// # Why the role changed rather than the grants
//
// A SECURITY DEFINER that runs as the TABLE OWNER can never be narrowed.
// PostgreSQL executes a foreign key's referential-integrity action with the
// privileges of the table owner, so removing SELECT from that role removes it
// from every cascade at the same time — which is what migration 00032 did, and
// it was live in production for fifteen hours. The definer now runs as
// mailroom_wkd, which owns NO tables, holds only the two column grants the
// lookup needs, and has nothing else in the database running as it.
//
// This asserts the PRIVILEGE, not the function body. A test that only checked
// what wkd_public_key returns would pass equally against a schema where the
// column was readable and the function merely chose not to read it — a
// completely different, and much weaker, guarantee.
func TestWKDDefinerCannotReadPrivateKeyMaterial(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// ---- 1. THE STRUCTURAL PROPERTY, WITHOUT WHICH THE REST IS DECORATION ----
	//
	// mailroom_wkd must own nothing. An owner holds every privilege on what it
	// owns implicitly, so a role that owns even one table cannot be narrowed on
	// it — that single fact is what defeated #48, #257 and 00032 in turn.
	var ownedByWKD int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM pg_class WHERE relowner = 'mailroom_wkd'::regrole
		  AND relkind IN ('r','p','v','m','S')`).Scan(&ownedByWKD); err != nil {
		t.Fatalf("count relations owned by mailroom_wkd: %v", err)
	}
	if ownedByWKD != 0 {
		t.Errorf("mailroom_wkd owns %d relations, want 0 — the moment it owns anything it "+
			"holds every privilege on it implicitly, and every column grant below becomes "+
			"decoration exactly as #48's did", ownedByWKD)
	}
	// CONTROL ON THAT QUERY: mailroom_owner DOES own relations, so a count of 0
	// above cannot be an artefact of a query that finds nothing at all.
	var ownedByOwner int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM pg_class WHERE relowner = 'mailroom_owner'::regrole
		  AND relkind IN ('r','p','v','m','S')`).Scan(&ownedByOwner); err != nil {
		t.Fatalf("count relations owned by mailroom_owner: %v", err)
	}
	if ownedByOwner == 0 {
		t.Fatal("mailroom_owner owns nothing either — this harness is not applying the schema " +
			"under production ownership, so nothing in this test means what it says")
	}

	// ---- 2. AND THE DEFINER ACTUALLY RUNS AS IT ------------------------------
	// The grants below are irrelevant if the function still runs as the owner.
	var definerOwner string
	if err := h.super.QueryRow(ctx, `
		SELECT pg_get_userbyid(p.proowner)
		  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = 'app' AND p.proname = 'wkd_public_key'`).Scan(&definerOwner); err != nil {
		t.Fatalf("read wkd_public_key owner: %v", err)
	}
	if definerOwner != "mailroom_wkd" {
		t.Fatalf("app.wkd_public_key is owned by %q, want mailroom_wkd — a definer owned by a "+
			"table owner cannot be narrowed at all (#266)", definerOwner)
	}

	// ---- 3. THE PRIVILEGE ITSELF, BOTH DIRECTIONS ---------------------------
	priv := func(table, column string) bool {
		var ok bool
		if err := h.super.QueryRow(ctx,
			`SELECT has_column_privilege('mailroom_wkd', $1, $2, 'SELECT')`, table, column,
		).Scan(&ok); err != nil {
			t.Fatalf("has_column_privilege(%s.%s): %v", table, column, err)
		}
		return ok
	}
	if priv("app.user_keys", "private_key_ct") {
		t.Error("the WKD definer can read private_key_ct — the anonymous, cross-user surface " +
			"has the sealed key material within reach of a rewritten function body")
	}
	if priv("app.user_keys", "dek_id") || priv("app.user_keys", "envelope_ver") {
		t.Error("the WKD definer can read the envelope metadata describing how to open the " +
			"sealed material; it is excluded from the grant for the same reason as the ciphertext")
	}
	// POSITIVE CONTROL. Without it, a role denied EVERYTHING would satisfy the
	// assertions above — and that role breaks every WKD lookup.
	if !priv("app.user_keys", "public_key") {
		t.Error("the WKD definer cannot read public_key either — the assertions above prove " +
			"nothing, and the lookup cannot work")
	}
	// app.identities holds nothing sealed, so this is privacy rather than key
	// compromise. Narrowed anyway.
	if priv("app.identities", "signature_html") {
		t.Error("the WKD definer can read identity signature HTML, which the anonymous " +
			"directory has no business seeing")
	}
	if !priv("app.identities", "address") {
		t.Error("the WKD definer cannot read the address it is supposed to return")
	}

	// ---- 4. THE CATALOG IS NOT THE ONLY WITNESS -----------------------------
	// has_column_privilege could in principle disagree with the executor. Ask
	// the executor, both ways, as the role itself.
	conn, err := h.super.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE mailroom_wkd`); err != nil {
		t.Fatalf("SET ROLE mailroom_wkd: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `RESET ROLE`) }()

	if _, err := conn.Exec(ctx, `SELECT private_key_ct FROM app.user_keys`); err == nil {
		t.Error("SELECT private_key_ct SUCCEEDED as mailroom_wkd — the privilege catalog and " +
			"the executor disagree, and the executor is what an attacker gets")
	} else if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Errorf("SELECT private_key_ct failed for the WRONG reason (%v) — this assertion would "+
			"pass even with the privilege in place", err)
	}
	if _, err := conn.Exec(ctx, `SELECT id, public_key, fingerprint FROM app.user_keys`); err != nil {
		t.Errorf("SELECT of the GRANTED columns was refused (%v) — the definer's own query "+
			"would fail and every WKD lookup with it", err)
	}
}

// TestWKDDefinerNarrowingDoesNotBreakCascades is the other half of #266, and it
// exists because the two properties have each broken the other.
//
// ⚠ Migration 00032 achieved the denial and took every foreign-key cascade into
// app.user_keys and app.identities down with it — for fifteen hours, in
// production — because PostgreSQL runs referential-integrity actions with the
// privileges of the TABLE OWNER. 00034 restored the grant and the denial became
// decoration again. Asserting either property alone is what let that happen
// twice, so both are asserted here, in one test, on the four FK paths 00034
// names by name.
//
// The deletes are issued as the SUPERUSER on purpose. It bypasses RLS, so the
// rows are genuinely reachable — but the RI action underneath still executes as
// mailroom_owner, which is the privilege path being tested. (Issued as
// mailroom_owner instead, RLS would silently filter the DELETE to zero rows and
// this test would pass without deleting anything.)
func TestWKDDefinerNarrowingDoesNotBreakCascades(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	seed := func(t *testing.T) (a fixtureUser, keyID ulid.ULID) {
		t.Helper()
		a, _ = seedTwoUsers(t, h.super)
		keyID = seedPublishableKey(t, h.appStore(),
			ulid.MustParse(a.ID), ulid.MustParse(a.IdentityID))
		return a, keyID
	}

	t.Run("delete a key clears identities.default_key_id", func(t *testing.T) {
		a, keyID := seed(t)
		if _, err := h.super.Exec(ctx,
			`UPDATE app.identities SET default_key_id = $1 WHERE id = $2`,
			keyID.String(), a.IdentityID); err != nil {
			t.Fatalf("bind default key: %v", err)
		}
		if _, err := h.super.Exec(ctx, `DELETE FROM app.user_keys WHERE id = $1`, keyID.String()); err != nil {
			t.Fatalf("delete key: %v — ON DELETE SET NULL into app.identities is refused, "+
				"which is migration 00032's outage", err)
		}
		var cleared bool
		if err := h.super.QueryRow(ctx,
			`SELECT default_key_id IS NULL FROM app.identities WHERE id = $1`, a.IdentityID).Scan(&cleared); err != nil {
			t.Fatalf("read identity: %v", err)
		}
		if !cleared {
			t.Error("default_key_id survived the key it points at")
		}
	})

	t.Run("delete an identity clears user_keys.identity_id", func(t *testing.T) {
		a, _ := seed(t)

		// ⚠ A SECOND identity, not the fixture's own. app.submissions.identity_id
		// is `NOT NULL REFERENCES app.identities(id)` with no ON DELETE action —
		// deliberately restricting, because an identity that has sent mail must
		// not vanish from the outbound record — and every fixture user has a
		// submission (the RLS table inventory requires one). Deleting the seeded
		// identity therefore fails on submissions_identity_id_fkey, which is a
		// data-model rule with nothing to do with the privilege path this test
		// exists for. Measured, not assumed: the first version of this subtest
		// did exactly that, and its failure named the wrong constraint.
		identityID := NewULID()
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.identities (id, user_id, account_id, kind, address)
			VALUES ($1, $2, $3, 'fixed', $4)`,
			identityID.String(), a.ID, a.AccountID,
			"cascade-"+identityID.String()+"@example.com"); err != nil {
			t.Fatalf("seed a submission-free identity: %v", err)
		}
		keyID := seedPublishableKey(t, h.appStore(), ulid.MustParse(a.ID), identityID)

		if _, err := h.super.Exec(ctx, `DELETE FROM app.identities WHERE id = $1`, identityID.String()); err != nil {
			t.Fatalf("delete identity: %v — ON DELETE SET NULL into app.user_keys is refused", err)
		}
		var cleared bool
		if err := h.super.QueryRow(ctx,
			`SELECT identity_id IS NULL FROM app.user_keys WHERE id = $1`, keyID.String()).Scan(&cleared); err != nil {
			t.Fatalf("read key: %v", err)
		}
		if !cleared {
			t.Error("the key still points at an identity that no longer exists")
		}
	})

	t.Run("delete an account cascades to its identities", func(t *testing.T) {
		a, _ := seed(t)
		if _, err := h.super.Exec(ctx, `DELETE FROM app.accounts WHERE id = $1`, a.AccountID); err != nil {
			t.Fatalf("delete account: %v — ON DELETE CASCADE into app.identities is refused, "+
				"which is exactly how #266 was found", err)
		}
		var n int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.identities WHERE account_id = $1`, a.AccountID).Scan(&n); err != nil {
			t.Fatalf("count identities: %v", err)
		}
		if n != 0 {
			t.Errorf("%d identities outlived their account", n)
		}
	})

	t.Run("delete a user cascades to its keys", func(t *testing.T) {
		a, _ := seed(t)
		if _, err := h.super.Exec(ctx, `DELETE FROM app.users WHERE id = $1`, a.ID); err != nil {
			t.Fatalf("delete user: %v — ON DELETE CASCADE into app.user_keys is refused", err)
		}
		var n int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.user_keys WHERE user_id = $1`, a.ID).Scan(&n); err != nil {
			t.Fatalf("count keys: %v", err)
		}
		if n != 0 {
			t.Errorf("%d keys outlived their user", n)
		}
	})
}

// TestPublishRefusesWhatWKDCannotRepresent. Each of these would produce a
// directory entry that actively misleads a correspondent.
func TestPublishRefusesWhatWKDCannotRepresent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	userID := ulid.MustParse(a.ID)
	identityID := ulid.MustParse(a.IdentityID)
	// The CORRECT hash throughout, so each refusal below is provably for the
	// reason it names rather than incidentally for a bad hash.
	_, hash := publishedAs(t, h, a.IdentityID)

	t.Run("an unbound key", func(t *testing.T) {
		keyID := NewULID()
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			_, err := tx.InsertUserKey(ctx, UserKey{
				ID: keyID, UserID: userID, Kind: KeyPGP,
				Fingerprint: "UNBOUND" + keyID.String(),
				PublicKey:   realPublicKey(t, "unbound@example.com"), PrivateKeyCT: []byte{0x01},
			})
			return err
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			_, err := tx.PublishKey(ctx, keyID, hash)
			return err
		})
		if !errors.Is(err, ErrNotPublishable) {
			t.Errorf("err = %v, want ErrNotPublishable — WKD publishes a key AT an address", err)
		}
	})

	t.Run("a retired key", func(t *testing.T) {
		keyID := seedPublishableKey(t, s, userID, identityID)
		if _, err := h.super.Exec(ctx,
			`UPDATE app.user_keys SET decrypt_only = true WHERE id = $1`, keyID.String()); err != nil {
			t.Fatalf("retire: %v", err)
		}
		err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			_, err := tx.PublishKey(ctx, keyID, hash)
			return err
		})
		if !errors.Is(err, ErrNotPublishable) {
			t.Errorf("err = %v, want ErrNotPublishable — a retired key must not invite new mail", err)
		}
	})

	t.Run("an armored public key never reaches the column", func(t *testing.T) {
		// Publication's precondition, enforced one layer down: WKD serves this
		// column verbatim, so armor stored here would 500 at the endpoint and
		// make published-vs-unpublished distinguishable to an anonymous caller.
		err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			_, err := tx.InsertUserKey(ctx, UserKey{
				ID: NewULID(), UserID: userID, Kind: KeyPGP,
				Fingerprint:  "ARMORED" + NewULID().String(),
				PublicKey:    []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\nx\n-----END PGP PUBLIC KEY BLOCK-----"),
				PrivateKeyCT: []byte{0x02},
			})
			return err
		})
		if !errors.Is(err, ErrKeyInvalid) {
			t.Errorf("err = %v, want ErrKeyInvalid — armor in this column is unservable over WKD", err)
		}
	})
}

// TestAnotherUsersKeyCannotBePublishedByYou: publication is a write, and writes
// are RLS-scoped like every other. Naming someone else's key id must do nothing.
func TestAnotherUsersKeyCannotBePublishedByYou(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()

	aKey := seedPublishableKey(t, s, ulid.MustParse(a.ID), ulid.MustParse(a.IdentityID))
	_, aHash := publishedAs(t, h, a.IdentityID)

	if err := s.UserTx(ctx, ulid.MustParse(b.ID), func(tx *UserTx) error {
		ok, err := tx.PublishKey(ctx, aKey, aHash)
		if err != nil {
			return err
		}
		if ok {
			t.Error("B published A's key")
		}
		return nil
	}); err != nil && !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("cross-user publish: %v", err)
	}

	var published bool
	if err := h.super.QueryRow(ctx,
		`SELECT wkd_published FROM app.user_keys WHERE id = $1`, aKey.String()).Scan(&published); err != nil {
		t.Fatalf("read: %v", err)
	}
	if published {
		t.Error("A's key was published by B")
	}
}
