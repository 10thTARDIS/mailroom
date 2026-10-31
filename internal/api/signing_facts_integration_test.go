// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The signing facts a send line can trust (#492), and the re-send that must not
// sign twice (#491) — both driven through the JMAP methods a client calls, against
// a real database.
//
// # Why these are integration tests
//
// #491's bug does not exist inside prepareOutbound. It exists in the ROUND TRIP:
// submit rewrites the stored blob, the undo cancels the submission, and the second
// submit reads those rewritten bytes back out of the blob store. Nothing short of
// the real store, the real blob store and the real EmailSubmission/set can produce
// the input the gate has to recognise, which is why the unit tests in
// internal/compose/prepared_test.go cover the DECISION and this file covers
// the PATH.
//
// #492's two facts are similar: signingKeyId is a LEFT JOIN whose whole point is
// RLS-scoped SQL, and the capability object is assembled by the session handler.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/pgpmime"
	"mailroom/internal/store"
)

// ---- #491: one signature, one Autocrypt header, on a re-send ---------------

// TestResendAfterAnUndoSendIsPreparedExactlyOnce is #491.
//
// The sequence is the one a user performs with one click in the Outbox: send,
// "Undo", then "Send now". Before the gate in compose/prepared.go, the second
// submit signed the FIRST submit's output — the stored message ended up a
// multipart/signed inside a multipart/signed, carrying two Autocrypt headers,
// which Autocrypt Level 1 requires a receiver to ignore BOTH of. Nothing errored.
func TestResendAfterAnUndoSendIsPreparedExactlyOnce(t *testing.T) {
	signer := &stubSigner{}
	h, blobs := newSigningHarness(t, signer)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	bindKeyToIdentity(t, h, a.UserID, "me@example.com")

	emailID, _ := composeAndSubmit(t, h, a.UserID, "me@example.com",
		"Sent, undone, sent again — with an em-dash.\n")

	// PRECONDITION. Without a signature on the first submit there is no second
	// layer to be prevented, and everything below would pass against a composer
	// that never signed at all.
	if signer.count() != 1 {
		t.Fatalf("signer called %d times on the first submit, want 1 — this test would "+
			"prove nothing about double signing", signer.count())
	}
	signedOnce := append([]byte(nil), signer.canonical...)
	hashAfterFirst := contentHash(t, h, a.UserID, emailID)

	// Undo Send, inside the window (the harness undo window is 1ms and no
	// dispatcher runs here, so the job is still 'pending').
	subID := onlySubmissionID(t, h, a.UserID)
	undo := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"update": map[string]any{subID: map[string]any{"undoStatus": "canceled"}},
	})
	if upd, _ := undo["updated"].(map[string]any); len(upd) != 1 {
		t.Fatalf("the undo was refused: %#v", undo)
	}

	// "Send now": the same draft, submitted again. THE BLESSED PATH — see
	// db/schema.sql at app.submissions on why no constraint forbids it.
	again := call(t, h.api.submissionSet, a.UserID, map[string]any{
		"create": map[string]any{"s2": map[string]any{"emailId": emailID}},
	})
	if m, _ := again["created"].(map[string]any); m["s2"] == nil {
		t.Fatalf("the re-send after an undo was refused: %#v", again)
	}
	// Control on the fixture: the re-send really did happen, so a green result
	// below cannot come from the second submit having been rejected.
	if got := sendCount(t, h, `SELECT count(*) FROM app.submissions WHERE user_id=$1`,
		a.UserID.String()); got != 2 {
		t.Fatalf("submissions = %d, want 2 — the re-send did not reach the store, so the "+
			"assertions below are vacuous", got)
	}

	raw := storedBytes(t, h, blobs, a.UserID, emailID)

	// THE TWO ASSERTIONS #491 IS ABOUT.
	if got := bytes.Count(raw, []byte("Content-Type: multipart/signed")); got != 1 {
		t.Errorf("the stored message has %d `Content-Type: multipart/signed` headers, want 1 — "+
			"a re-send wrapped the already-signed message in a second signature\n%.600s", got, raw)
	}
	if got := bytes.Count(raw, []byte("Autocrypt:")); got != 1 {
		t.Errorf("the stored message has %d Autocrypt headers, want 1 — Autocrypt Level 1 "+
			"requires a receiver to IGNORE ALL of them when there is more than one, so a "+
			"second header does not duplicate the user's key, it withdraws it", got)
	}

	// The signer was not asked again, so no second signature exists anywhere —
	// including over bytes nothing kept.
	if got := signer.count(); got != 1 {
		t.Errorf("signer called %d times across both submits, want 1", got)
	}

	// AND THE SIGNATURE STILL COVERS WHAT WAS SIGNED. This is the verification
	// that matters here and it needs no key: RFC 3156 verification is "canonicalise
	// the entity that arrived and hash it", so the entity in the stored message
	// must be byte-identical to what the signer was handed. The cryptography is
	// internal/pgpkeyd's; what could break on this path is the BYTES.
	parts, err := pgpmime.SplitSigned(raw)
	if err != nil {
		t.Fatalf("the stored message is not a splittable multipart/signed: %v", err)
	}
	if got := pgpmime.Canonicalize(parts.Entity); !bytes.Equal(got, signedOnce) {
		t.Errorf("the signed entity is no longer the bytes that were signed (%d bytes vs %d) — "+
			"every recipient would see `bad signature`", len(got), len(signedOnce))
	}

	// The bytes were not rewritten a second time, which is also what keeps the
	// blob store, the Sent copy and the dedup hash pointing at one object.
	if got := contentHash(t, h, a.UserID, emailID); !bytes.Equal(got, hashAfterFirst) {
		t.Error("the content hash moved on the re-send: the stored bytes were replaced again")
	}
}

// contentHash reads a message's content_hash straight from the row.
func contentHash(t *testing.T, h *composeHarness, userID ulid.ULID, emailID string) []byte {
	t.Helper()
	var hash []byte
	if err := h.super.QueryRow(context.Background(),
		`SELECT content_hash FROM app.messages WHERE user_id = $1 AND id = $2`,
		userID.String(), emailID).Scan(&hash); err != nil {
		t.Fatalf("read content hash: %v", err)
	}
	return hash
}

// onlySubmissionID returns the id of the user's one submission, and fails if
// there is not exactly one — a wrong id here would cancel nothing and the test
// would look green for the wrong reason.
func onlySubmissionID(t *testing.T, h *composeHarness, userID ulid.ULID) string {
	t.Helper()
	rows, err := h.super.Query(context.Background(),
		`SELECT id FROM app.submissions WHERE user_id = $1`, userID.String())
	if err != nil {
		t.Fatalf("read submissions: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan submission id: %v", err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 {
		t.Fatalf("submissions = %d, want exactly 1", len(ids))
	}
	return ids[0]
}

// ---- #492: Identity.signingKeyId ------------------------------------------

// seedIdentity adds one more verified fixed identity to a user (seedSendable
// brings the mailboxes and the first one) and returns its id.
func seedIdentity(t *testing.T, h *composeHarness, f apiFixture, address string) string {
	t.Helper()
	id := store.NewULID().String()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.identities (id, user_id, account_id, kind, address, enabled)
		 VALUES ($1,$2,$3,'fixed',$4,true)`,
		id, f.UserID.String(), f.AccountID.String(), address); err != nil {
		t.Fatalf("seed identity %s: %v", address, err)
	}
	return id
}

// identitiesByEmail runs Identity/get with ids:null and indexes the result by
// address, which is how these tests name identities.
func identitiesByEmail(t *testing.T, h *composeHarness, userID ulid.ULID, props ...string) map[string]map[string]any {
	t.Helper()
	args := map[string]any{"ids": nil}
	if len(props) > 0 {
		args["properties"] = props
	}
	res := call(t, h.api.identityGet, userID, args)
	list, _ := res["list"].([]any)
	out := make(map[string]map[string]any, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("Identity/get list entry is not an object: %#v", item)
		}
		email, _ := obj["email"].(string)
		if email == "" {
			// Only when `properties` excluded it: index by id instead, which is
			// always present.
			email, _ = obj["id"].(string)
		}
		out[email] = obj
	}
	return out
}

// TestIdentityGetReportsAUsableBoundSigningKey is #492 item 1: signingKeyId is
// the bound key while that key can still sign, and null otherwise.
//
// ⚠ THE RESTORE AT THE END IS NOT TIDINESS. Four of the cases below expect null,
// and a projection that returned null unconditionally — the exact shape of the
// bug this property could ship with — would satisfy all four. The last step puts
// the key back and requires the id to reappear.
func TestIdentityGetReportsAUsableBoundSigningKey(t *testing.T) {
	// No signer: signingKeyId is a fact about the KEY, not about whether this
	// server has a crypto process. That is the capability object's job, below.
	h := newComposeHarness(t)
	ctx := context.Background()
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "signs@example.com", "")
	seedIdentity(t, h, a, "plain@example.com")
	keyID := bindKeyToIdentity(t, h, a.UserID, "signs@example.com")

	signingKeyOf := func(address string) any {
		t.Helper()
		obj, ok := identitiesByEmail(t, h, a.UserID)[address]
		if !ok {
			t.Fatalf("Identity/get did not return %s", address)
		}
		return obj["signingKeyId"]
	}

	if got := signingKeyOf("signs@example.com"); got != keyID {
		t.Errorf("signingKeyId = %v, want the bound key %s", got, keyID)
	}
	// The identity with nothing bound. Most identities are this one — including
	// every alias derived under a catchall — and a client must be able to tell
	// them apart from a signing identity without asking about keys.
	if got := signingKeyOf("plain@example.com"); got != nil {
		t.Errorf("signingKeyId = %v on an identity with no key bound, want null", got)
	}

	// Served by default (above), and selectable. `properties` is how a settings
	// screen asks for one field, so a property that is not selectable is one a
	// client has to over-fetch to read.
	selected := identitiesByEmail(t, h, a.UserID, "signingKeyId")
	found := false
	for id, obj := range selected {
		if _, ok := obj["signingKeyId"]; !ok {
			t.Errorf("identity %s: signingKeyId was requested by `properties` and not returned: %#v", id, obj)
		}
		if _, ok := obj["email"]; ok {
			t.Errorf("identity %s: `properties: [signingKeyId]` returned `email` too: %#v", id, obj)
		}
		if obj["signingKeyId"] == keyID {
			found = true
		}
	}
	if !found {
		t.Errorf("no identity reported the bound key under `properties: [signingKeyId]`: %#v", selected)
	}

	// The three states that make a bound key unusable for signing. Each one is
	// also a state the SEND path refuses (store.SigningKeyForIdentity shares the
	// predicate), so reporting the key here would promise a signature the send
	// would then decline to make.
	for _, tc := range []struct {
		name string
		sql  string
		why  string
	}{
		{"revoked", `UPDATE app.user_keys SET revoked_at = now() WHERE id = $1`,
			"a revoked key must never sign again"},
		{"decrypt-only", `UPDATE app.user_keys SET revoked_at = NULL, decrypt_only = true WHERE id = $1`,
			"a retired key is kept so old mail stays readable (spec §9), not to sign new mail"},
		{"no private material", `UPDATE app.user_keys SET decrypt_only = false, private_key_ct = NULL WHERE id = $1`,
			"a public-only row has no private half anywhere, so the crypto process would refuse it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.super.Exec(ctx, tc.sql, keyID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got := signingKeyOf("signs@example.com"); got != nil {
				t.Errorf("signingKeyId = %v for a %s key, want null — %s", got, tc.name, tc.why)
			}
		})
	}

	// THE CONTROL. Put the key back and the id must come back with it; without
	// this, a projection hard-coded to null would have passed every case above.
	if _, err := h.super.Exec(ctx,
		`UPDATE app.user_keys
		    SET revoked_at = NULL, decrypt_only = false, private_key_ct = '\xdeadbeef'::bytea
		  WHERE id = $1`, keyID); err != nil {
		t.Fatalf("restore key: %v", err)
	}
	if got := signingKeyOf("signs@example.com"); got != keyID {
		t.Errorf("signingKeyId = %v after restoring the key, want %s — the property may be "+
			"reporting null unconditionally, which would have satisfied every case above",
			got, keyID)
	}
}

// TestIdentitySetRefusesSigningKeyId: the property is READ-ONLY. A client that
// could patch it would be choosing which key signs its mail — and the private
// half only ever exists behind the crypto boundary (ADR-024), so there is no
// version of this that JMAP should be able to do.
func TestIdentitySetRefusesSigningKeyId(t *testing.T) {
	h := newComposeHarness(t)
	ctx := context.Background()
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "signs@example.com", "")
	other := seedIdentity(t, h, a, "plain@example.com")
	keyID := bindKeyToIdentity(t, h, a.UserID, "signs@example.com")

	// The interesting attempt: bind the OTHER identity to a key it does not have.
	res := call(t, h.api.identitySet, a.UserID, map[string]any{
		"update": map[string]any{other: map[string]any{"signingKeyId": keyID}},
	})
	e := setError(t, res, "notUpdated", other)
	if e["type"] != setInvalidProps {
		t.Errorf("SetError type = %v, want %s", e["type"], setInvalidProps)
	}
	if props, _ := e["properties"].([]any); len(props) != 1 || props[0] != "signingKeyId" {
		t.Errorf("properties = %v, want exactly [signingKeyId] so a client learns what to drop", props)
	}

	// NOTHING MOVED. A refusal that still wrote the column would be the whole bug.
	var bound *string
	if err := h.super.QueryRow(ctx,
		`SELECT default_key_id FROM app.identities WHERE id = $1`, other).Scan(&bound); err != nil {
		t.Fatalf("read default_key_id: %v", err)
	}
	if bound != nil {
		t.Errorf("default_key_id = %v after a refused patch, want NULL", *bound)
	}

	// And the pre-existing behaviour this relies on: any unknown patch key is
	// invalidProperties, so a client cannot smuggle a server-owned field in under
	// a name we have not enumerated.
	unknown := call(t, h.api.identitySet, a.UserID, map[string]any{
		"update": map[string]any{other: map[string]any{"notAnIdentityProperty": "x"}},
	})
	ue := setError(t, unknown, "notUpdated", other)
	if ue["type"] != setInvalidProps {
		t.Errorf("SetError type for an unknown property = %v, want %s", ue["type"], setInvalidProps)
	}
}

// ---- #492: urn:mailroom:pgp {signing} -------------------------------------

// TestSessionAdvertisesWhetherThisServerCanSign is #492 item 2. The fact comes
// off the composer's own signer — the field prepareOutbound consults — so the two
// cannot disagree.
//
// Both maps are checked. A client reading accountCapabilities (where RFC 8620
// puts per-account semantics) must not find a placeholder that contradicts the
// top-level object.
func TestSessionAdvertisesWhetherThisServerCanSign(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness func(t *testing.T) *composeHarness
		want    bool
	}{{
		name: "with a crypto process wired",
		harness: func(t *testing.T) *composeHarness {
			h, _ := newSigningHarness(t, &stubSigner{})
			return h
		},
		want: true,
	}, {
		name:    "with no crypto process",
		harness: newComposeHarness,
		want:    false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.harness(t)
			a, _ := seedTwo(t, h.super)

			req := httptest.NewRequest(http.MethodGet, "/jmap/session", nil)
			rec := httptest.NewRecorder()
			auth.DevAuth(http.HandlerFunc(h.api.SessionHandler), a.UserID).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("session: status %d", rec.Code)
			}
			var session struct {
				Capabilities map[string]struct {
					Signing *bool `json:"signing"`
				} `json:"capabilities"`
				Accounts map[string]struct {
					AccountCapabilities map[string]struct {
						Signing *bool `json:"signing"`
					} `json:"accountCapabilities"`
				} `json:"accounts"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
				t.Fatalf("decode session: %v", err)
			}

			top, ok := session.Capabilities[capPGP]
			if !ok {
				t.Fatalf("%s is missing from the top-level capabilities", capPGP)
			}
			if top.Signing == nil {
				t.Fatalf("%s carries no `signing` property: a client cannot tell whether this "+
					"server can sign, and an absent boolean reads as false in every JSON client",
					capPGP)
			}
			if *top.Signing != tc.want {
				t.Errorf("capabilities[%s].signing = %v, want %v", capPGP, *top.Signing, tc.want)
			}
			for id, acct := range session.Accounts {
				per, ok := acct.AccountCapabilities[capPGP]
				if !ok {
					t.Fatalf("%s is missing from accountCapabilities of %s", capPGP, id)
				}
				if per.Signing == nil || *per.Signing != tc.want {
					t.Errorf("accountCapabilities[%s].signing = %v, want %v — the per-account "+
						"object still carries capability.go's fail-safe placeholder",
						capPGP, orNil(per.Signing), tc.want)
				}
			}
		})
	}
}

// orNil renders a *bool for an error message: the VALUE, or nil when absent. A
// pointer printed with %v is an address, which says nothing about what the server
// advertised.
func orNil(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}
