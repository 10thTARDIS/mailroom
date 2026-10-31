// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Compose-time signing, driven through the real JMAP methods against a real
// database (#47 increment 5).
//
// # Why this exists rather than a unit test
//
// Composer.signSubmission is the seam where the crypto boundary meets the store:
// it looks up the identity's bound key under RLS, calls the signer, writes a new
// blob, and rewrites the Email's bytes through store.ReplaceMessageBytes. None
// of that is reachable without a database, and Submit takes a concrete
// *store.UserTx that cannot be substituted — so the alternative was to ship the
// seam covered only by pgpmime's byte handling and verify it by hand afterwards.
//
// That is precisely how key publication came to ship INERT (#48): every part was
// tested, nothing tested the parts joined together. So the join is tested here,
// through EmailSubmission/set, the method a client actually calls.
//
// The signer is a stub. What is under test is the WIRING — that a bound key
// causes the stored message to be replaced by its signed form, and that an
// identity with no key is left completely alone. The cryptography itself is
// internal/pgpkeyd's, and the RFC 3156 byte handling is internal/pgpmime's.

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/compose"
	"mailroom/internal/store"
)

// stubSigner records what it was asked to sign and returns a syntactically
// valid armored block. pgpmime refuses anything without the BEGIN/END markers,
// so a stub has to look at least this much like a signature.
type stubSigner struct {
	mu        sync.Mutex
	calls     int
	canonical []byte
	err       error
}

func (s *stubSigner) Sign(_ context.Context, _, _ ulid.ULID, data []byte) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.canonical = append([]byte(nil), data...)
	if s.err != nil {
		return "", "", s.err
	}
	return "-----BEGIN PGP SIGNATURE-----\r\n\r\niHUEARYIAB0WIQT\r\n-----END PGP SIGNATURE-----",
		"pgp-sha256", nil
}

func (s *stubSigner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newSigningHarness is newComposeHarness with a signer attached.
func newSigningHarness(t *testing.T, signer compose.Signer) (*composeHarness, blob.BlobStore) {
	t.Helper()
	h := newAPIHarness(t)
	pathKey := make([]byte, 32)
	blobs, err := blob.NewFSBlobStore(t.TempDir()+"/blobs", pathKey)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	c := compose.NewComposer(blobs, fakeKeys{}, time.Millisecond).WithSigner(signer)
	h.api = New(h.store, WithComposer(c))
	return &composeHarness{apiHarness: h, blobs: blobs}, blobs
}

// bindKeyToIdentity seeds a usable signing key for the identity that owns
// address, exactly as `mailroom-admin pgp-generate` + `pgp-bind` would leave it.
// It returns the key's id, which Identity.signingKeyId must report (#492).
func bindKeyToIdentity(t *testing.T, h *composeHarness, userID ulid.ULID, address string) string {
	t.Helper()
	ctx := context.Background()

	var identityID string
	if err := h.super.QueryRow(ctx,
		`SELECT id FROM app.identities WHERE user_id = $1 AND address = $2`,
		userID.String(), address).Scan(&identityID); err != nil {
		t.Fatalf("find identity for %s: %v", address, err)
	}
	keyID := store.NewULID().String()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.user_keys
		   (id, user_id, identity_id, kind, fingerprint, public_key, private_key_ct, dek_id)
		 VALUES ($1,$2,$3,'pgp',$4,'\x99010203'::bytea,'\xdeadbeef'::bytea,$5)`,
		keyID, userID.String(), identityID, "FPR"+keyID, store.NewULID().String()); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	// The identity's column is what decides which key signs (SigningKeyForIdentity
	// joins through it), so binding means writing BOTH sides.
	if _, err := h.super.Exec(ctx,
		`UPDATE app.identities SET default_key_id = $3 WHERE user_id = $1 AND id = $2`,
		userID.String(), identityID, keyID); err != nil {
		t.Fatalf("bind key: %v", err)
	}
	return keyID
}

// composeAndSubmit creates a draft and submits it, returning the Email id and
// the content hash the draft had BEFORE submission.
func composeAndSubmit(t *testing.T, h *composeHarness, userID ulid.ULID, from, body string) (emailID string, before []byte) {
	t.Helper()
	ctx := context.Background()

	res := call(t, h.api.emailSet, userID, map[string]any{
		"create": map[string]any{
			"c1": map[string]any{
				"from":       []map[string]any{{"email": from}},
				"to":         []map[string]any{{"email": "friend@example.net"}},
				"subject":    "signed?",
				"bodyValues": map[string]any{"1": map[string]any{"value": body}},
				"textBody":   []map[string]any{{"partId": "1"}},
			},
		},
	})
	created, _ := res["created"].(map[string]any)
	obj, ok := created["c1"].(map[string]any)
	if !ok {
		t.Fatalf("draft not created: %#v", res)
	}
	emailID, _ = obj["id"].(string)

	if err := h.super.QueryRow(ctx,
		`SELECT content_hash FROM app.messages WHERE user_id = $1 AND id = $2`,
		userID.String(), emailID).Scan(&before); err != nil {
		t.Fatalf("read draft hash: %v", err)
	}

	sub := call(t, h.api.submissionSet, userID, map[string]any{
		"create": map[string]any{"s1": map[string]any{"emailId": emailID}},
	})
	if m, _ := sub["created"].(map[string]any); m["s1"] == nil {
		t.Fatalf("submission not created: %#v", sub)
	}
	return emailID, before
}

func storedBytes(t *testing.T, h *composeHarness, blobs blob.BlobStore, userID ulid.ULID, emailID string) []byte {
	t.Helper()
	ctx := context.Background()
	var hash []byte
	if err := h.super.QueryRow(ctx,
		`SELECT content_hash FROM app.messages WHERE user_id = $1 AND id = $2`,
		userID.String(), emailID).Scan(&hash); err != nil {
		t.Fatalf("read content hash: %v", err)
	}
	dek, _, err := fakeKeys{}.MailDEK(ctx, userID)
	if err != nil {
		t.Fatalf("dek: %v", err)
	}
	var h32 [32]byte
	copy(h32[:], hash)
	rc, err := blobs.Get(ctx, userID, dek, h32)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob bytes: %v", err)
	}
	return raw
}

// TestSubmitSignsAndReplacesTheStoredMessage is the wiring test: a bound key
// must make the STORED message the signed one, not just the transmitted one.
func TestSubmitSignsAndReplacesTheStoredMessage(t *testing.T) {
	signer := &stubSigner{}
	h, blobs := newSigningHarness(t, signer)
	ctx := context.Background()
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	bindKeyToIdentity(t, h, a.UserID, "me@example.com")

	// A non-ASCII body, so this also exercises the quoted-printable path that
	// makes the signed entity 7-bit safe.
	emailID, before := composeAndSubmit(t, h, a.UserID, "me@example.com",
		"Signed — with an em-dash.\n")

	if signer.count() != 1 {
		t.Fatalf("signer called %d times, want exactly 1 — a bound key must be used, and used once",
			signer.count())
	}

	raw := storedBytes(t, h, blobs, a.UserID, emailID)
	if !strings.Contains(string(raw), "multipart/signed") {
		t.Fatalf("the STORED message is not multipart/signed. Signing on the wire instead "+
			"would put the user's own sent mail in their mailbox twice.\n%.400s", raw)
	}
	if !strings.Contains(string(raw), "application/pgp-signature") {
		t.Error("the stored message has no signature part")
	}

	// The content hash MUST have moved: it is what the Sent copy, the dedup and
	// the blob are all keyed on. If it had not, the submission would be pointing
	// at the unsigned bytes while every log line said the message was signed.
	var after []byte
	if err := h.super.QueryRow(ctx,
		`SELECT content_hash FROM app.messages WHERE user_id = $1 AND id = $2`,
		a.UserID.String(), emailID).Scan(&after); err != nil {
		t.Fatalf("read content hash: %v", err)
	}
	if string(after) == string(before) {
		t.Error("the message's content hash did not change — the stored bytes were not replaced")
	}

	// ⚠ AND EVERY OTHER PROJECTION DERIVED FROM THOSE BYTES MOVED WITH THEM
	// (#377). content_hash and size_bytes were updated here from the beginning;
	// has_attachments was not, so signing left the row claiming no attachments
	// while its own bytes carried an application/pgp-signature part — which
	// internal/mime classifies as an attachment, being a non-text leaf.
	//
	// Found on real mail by `mailroom-admin attachment-report`, and the evidence
	// was a natural experiment: a signing key was bound on a known date,
	// and of the messages in Sent exactly those dated on or after that day
	// disagreed with their own bytes.
	//
	// It matters beyond a wrong paperclip: anything that walks has_attachments to
	// find work — an attachment scanner, most obviously (#333) — would silently
	// skip every signed message the user sent.
	var hasAttach bool
	if err := h.super.QueryRow(ctx,
		`SELECT has_attachments FROM app.messages WHERE user_id = $1 AND id = $2`,
		a.UserID.String(), emailID).Scan(&hasAttach); err != nil {
		t.Fatalf("read has_attachments: %v", err)
	}
	if !hasAttach {
		t.Error("has_attachments is false on a message whose stored bytes contain an " +
			"application/pgp-signature part — the projection did not move with the bytes (#377)")
	}

	// THE LOAD-BEARING INVARIANT (CLAUDE.md): every mutation of a JMAP-visible
	// object appends a change_log row in the same transaction. Rewriting an
	// Email's bytes is such a mutation, and a client that never learns of it
	// keeps serving the unsigned version from its cache forever.
	var changes int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.change_log
		  WHERE user_id = $1 AND object_type = 'Email' AND object_id = $2 AND op = 'updated'`,
		a.UserID.String(), emailID).Scan(&changes); err != nil {
		t.Fatalf("count changes: %v", err)
	}
	if changes == 0 {
		t.Error("no Email 'updated' change_log row for the byte replacement — clients would " +
			"never learn the message changed")
	}

	// The signed entity must be 7-bit clean, which is what makes the signature
	// survive an MTA without 8BITMIME. Asserted on what the signer was ACTUALLY
	// handed, not on what we think it was handed.
	for i, b := range signer.canonical {
		if b > 127 || b == 0 {
			t.Fatalf("the bytes handed to the signer contain a non-7-bit byte %#x at offset %d", b, i)
		}
	}
	if !strings.Contains(string(signer.canonical), "quoted-printable") {
		t.Error("the fixture did not exercise quoted-printable — this test would pass " +
			"against an 8bit composer")
	}
}

// TestSubmitLeavesUnsignedMailAloneWhenNoKeyIsBound is the negative control,
// and without it the test above proves much less than it appears to: a composer
// that signed EVERYTHING would satisfy it completely.
//
// It is also the ordinary case. Most users will never have a PGP key, and "no
// key" must mean "send normally" rather than an error on the send path
// everybody uses.
func TestSubmitLeavesUnsignedMailAloneWhenNoKeyIsBound(t *testing.T) {
	signer := &stubSigner{}
	h, blobs := newSigningHarness(t, signer)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	// Deliberately NO bindKeyToIdentity here.

	emailID, before := composeAndSubmit(t, h, a.UserID, "me@example.com",
		"Unsigned — and it should stay that way.\n")

	if signer.count() != 0 {
		t.Errorf("signer was called %d times for an identity with no bound key", signer.count())
	}

	raw := storedBytes(t, h, blobs, a.UserID, emailID)
	if strings.Contains(string(raw), "multipart/signed") {
		t.Error("a message from an identity with no key was signed anyway")
	}

	var after []byte
	if err := h.super.QueryRow(context.Background(),
		`SELECT content_hash FROM app.messages WHERE user_id = $1 AND id = $2`,
		a.UserID.String(), emailID).Scan(&after); err != nil {
		t.Fatalf("read content hash: %v", err)
	}
	if string(after) != string(before) {
		t.Error("the stored bytes were rewritten for an unsigned send — nothing should have " +
			"touched them")
	}

	// The unsigned path must leave has_attachments alone: a plain body with no
	// attachment is false before the send and false after it.
	//
	// ⚠ THIS IS NOT A CONTROL FOR #377's DERIVATION, AND IT WAS WRITTEN AS ONE
	// UNTIL PROBED. The claim was that it catches a lazy fix writing `true`
	// unconditionally. It does not: signSubmission returns early when no key is
	// bound, so ReplaceMessageBytes — and the projection line — never execute on
	// this path. Replacing the derivation with an always-true constant leaves
	// this test PASSING. Measured, not reasoned about.
	//
	// What it does prove is narrower and still worth having: the unsigned send
	// does not touch the flag, so a future change that wrote the projection
	// outside the bound-key check would fail here.
	//
	// ⚠ AND THE DERIVATION ITSELF IS NOT OBSERVABLE FROM ANY TEST, which is the
	// honest position: every signed message gains an application/pgp-signature
	// part, so after signing `true` is always the right answer and a constant
	// would be indistinguishable from the computed value. The value is derived
	// for robustness against pgpmime's output shape changing, not because a test
	// can currently tell the difference. Saying so beats implying coverage that
	// does not exist.
	var hasAttach bool
	if err := h.super.QueryRow(context.Background(),
		`SELECT has_attachments FROM app.messages WHERE user_id = $1 AND id = $2`,
		a.UserID.String(), emailID).Scan(&hasAttach); err != nil {
		t.Fatalf("read has_attachments: %v", err)
	}
	if hasAttach {
		t.Error("has_attachments is true on a plain unsigned message with no attachment — " +
			"the #377 fix is setting the flag rather than deriving it from the bytes")
	}
}
