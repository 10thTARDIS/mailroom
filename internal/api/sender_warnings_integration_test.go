// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Email.senderWarnings end to end (#397): the store query, the index, the
// judgement and the JSON shape, against a real Postgres under RLS.
//
// internal/senderwarn's own tests cover the judgement in isolation. What THIS
// file exists for is the wiring — that the index is built from the user's real
// contacts and identities, that the property reaches the JSON, and that it is
// SILENT for ordinary mail. The last of those is the one that decides whether
// the feature is usable: the corpus this was designed against fires zero times,
// and a notice that fired on ordinary mail would be retired within a day.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// seedCorrespondent gives the fixture user a contact at the given address, the
// way ingest would.
func seedCorrespondent(t *testing.T, h *composeHarness, f apiFixture, address string) {
	t.Helper()
	contactID := store.NewULID()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.contacts (id, user_id, display_name, created_at) VALUES ($1,$2,$3, now())`,
		contactID.String(), f.UserID.String(), "Correspondent"); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO app.contact_addresses
			(id, user_id, contact_id, address, first_seen_at, last_seen_at, seen_count)
		VALUES ($1,$2,$3,$4, now(), now(), 1)`,
		store.NewULID().String(), f.UserID.String(), contactID.String(), address); err != nil {
		t.Fatalf("seed contact address: %v", err)
	}
}

// seedMessageFrom files a minimal message with a chosen From address.
func seedMessageFrom(t *testing.T, h *composeHarness, f apiFixture, from string) ulid.ULID {
	t.Helper()
	raw := "From: Someone <" + from + ">\r\n" +
		"To: you@example.com\r\nSubject: hello\r\n" +
		"Date: Mon, 6 Jan 2025 10:00:00 +0000\r\n" +
		"Content-Type: text/plain\r\n\r\nhi\r\n"
	id := seedRawMessage(t, h, f, []byte(raw))
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET from_address = $2 WHERE id = $1`, id.String(), from); err != nil {
		t.Fatalf("set from_address: %v", err)
	}
	return id
}

func warningKinds(t *testing.T, h *composeHarness, f apiFixture, id ulid.ULID) []string {
	t.Helper()
	res := call(t, h.api.emailGet, f.UserID, map[string]any{
		"ids":        []string{id.String()},
		"properties": []string{"id", "senderWarnings"},
	})
	list := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get returned %d emails", len(list))
	}
	obj := list[0].(map[string]any)
	raw, present := obj["senderWarnings"]
	if !present {
		t.Fatalf("senderWarnings was not returned at all for %s — a client that asked for a "+
			"property must get an answer for it", id)
	}
	if raw == nil {
		return nil
	}
	var kinds []string
	for _, w := range raw.([]any) {
		kinds = append(kinds, w.(map[string]any)["kind"].(string))
	}
	return kinds
}

// TestSenderWarningsAreSilentForOrdinaryMail is the precision claim, and it is
// the assertion the whole feature lives or dies on.
//
// ⚠ Its negative control is the SECOND HALF of the test rather than a separate
// one: the same request, against a domain that imitates the correspondent seeded
// here, must produce a warning. Without that, "silent" is equally consistent
// with a property that never fires at all — which is exactly how a security
// notice becomes decorative.
func TestSenderWarningsAreSilentForOrdinaryMail(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedCorrespondent(t, h, a, "billing@mailbox.example")

	ordinary := seedMessageFrom(t, h, a, "billing@mailbox.example")
	if got := warningKinds(t, h, a, ordinary); len(got) != 0 {
		t.Errorf("mail from a domain the user corresponds with produced %v — a notice on ordinary "+
			"mail is one the reader stops seeing", got)
	}
	// A domain that is merely unfamiliar is ALSO silent. This is the rule that
	// measurement removed: on a catch-all mailbox 99.4% of mail is from a
	// stranger, so "we have not seen this domain before" is a background colour.
	stranger := seedMessageFrom(t, h, a, "hello@somewhere-new.example")
	if got := warningKinds(t, h, a, stranger); len(got) != 0 {
		t.Errorf("mail from an unfamiliar domain produced %v — external-sender marking was "+
			"deliberately not built, because on a catch-all it fires on nearly everything", got)
	}

	// THE CONTROL, AND IT USED TO BE A LOOKALIKE.
	//
	// ⚠ #411 SUPPRESSED lookalikeDomain, so a domain imitating the seeded
	// correspondent is now silent too — which would leave every assertion above
	// passing against a property that never fires, the exact thing this control
	// exists to exclude. selfSpoof still fires, so it is the control now.
	//
	// Named rather than quietly swapped: if lookalikeDomain is ever re-enabled,
	// the imitating sender belongs back in this test, and the seeded
	// correspondent above is already here waiting for it.
	seedSendable(t, h.super, a, "me@example.com", "")
	spoof := seedMessageFrom(t, h, a, "me@example.com")
	got := warningKinds(t, h, a, spoof)
	if len(got) == 0 {
		t.Fatalf("CONTROL FAILED: the property produced nothing for ANY sender, so the silence " +
			"above proves only that it never fires")
	}
	if got[0] != "selfSpoof" {
		t.Errorf("kinds = %v, want selfSpoof first", got)
	}
}

// ⚠ THIS TEST ASSERTED THAT A LOOKALIKE IS REPORTED. IT NOW ASSERTS THE
// OPPOSITE, AND THAT IS THE POINT OF #411.
//
// The rule was "the sender's domain reduces to the same skeleton as a domain the
// user KNOWS". But ingest upserts every inbound From into contacts on arrival,
// so a lookalike is "known" the moment it sends one message — and the skeleton
// slot then goes to whichever of the pair the index saw first. When the lookalike
// wins it, the warning lands on the GENUINE correspondent and tells the user
// their real contact is an imitation of the impersonator.
//
// A warning that can accuse the innocent is worse than no warning, so it is off
// until "known" has a definition that history can support. See
// senderwarn.lookalikeEnabled.
//
// The DETECTION is unchanged and still covered — senderwarn's unit tests pin the
// skeleton collisions directly, precisely so this suppression does not quietly
// take the mechanism with it.
func TestALookalikeIsNotReportedWhileTheKnownSetCannotBeTrusted(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedCorrespondent(t, h, a, "billing@mailbox.example")

	// The imitator...
	if got := warningKinds(t, h, a, seedMessageFrom(t, h, a, "billing@rnailbox.example")); len(got) != 0 {
		t.Errorf("the imitating domain produced %v — lookalikeDomain is suppressed (#411)", got)
	}
	// ...and, the half that actually matters, the GENUINE correspondent. This is
	// the direction that made suppression necessary rather than merely prudent.
	if got := warningKinds(t, h, a, seedMessageFrom(t, h, a, "billing@mailbox.example")); len(got) != 0 {
		t.Errorf("the GENUINE correspondent produced %v — a security notice pointed at the real "+
			"sender is misdirection, and it is the failure mode #411 exists to prevent", got)
	}
}

// ⚠ THE INDEX IS BUILT FROM THE USER'S OWN MAIL, and this is what says so.
// Without the correspondent seeded, the identical lookalike domain resembles
// nothing and must be silent — so the warning above was produced by the user's
// contacts and not by anything baked into the code.
//
// ⚠ CURRENTLY VACUOUS, AND SAID SO RATHER THAN LEFT LOOKING LIVE (#411).
// lookalikeDomain is suppressed, so this passes whether or not the index is
// built from the user's contacts. It is kept because it is the assertion that
// must hold again the moment the warning is re-enabled — deleting it would mean
// re-deriving it later — but nobody should read a green run here as evidence
// about the index today.
func TestALookalikeOfNothingIsSilent(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	id := seedMessageFrom(t, h, a, "billing@rnailbox.example")
	if got := warningKinds(t, h, a, id); len(got) != 0 {
		t.Errorf("with no correspondent to resemble, %v was still reported — the index is not "+
			"coming from the user's own contacts", got)
	}
}

// RLS: one user's correspondents must not shape another user's warnings.
//
// ⚠ A LEAK HERE WOULD BE A DISCLOSURE, NOT A NUISANCE. `resembles` names a
// domain the user corresponds with, so a warning built from B's contacts and
// shown to A would tell A who B does business with.
//
// ⚠ IT MOVED FROM CONTACTS TO IDENTITIES, AND THAT IS #411'S DOING. The
// correspondent version of this test asserted that A is not warned about a
// domain resembling one of B's contacts — a real disclosure, since `resembles`
// names it. But `resembles` is only ever set by lookalikeDomain, which is now
// suppressed, so BOTH halves went silent and the test's own control caught it:
// "with A's own correspondent seeded there is still no warning".
//
// ⚠ MARKING IT VACUOUS WOULD HAVE BEEN THE LAZY REPAIR. SenderDomainsFor reads
// TWO sets in one UserTx — identities and contacts — and the identity half feeds
// selfSpoof, which still fires. So the isolation assertion stays LIVE by moving
// to the half that can still demonstrate it, rather than being demoted to a
// comment. A leak here is the same disclosure in a different currency: it would
// tell A which domains B owns.
func TestSenderWarningsDoNotCrossUsers(t *testing.T) {
	h := newComposeHarness(t)
	a, b := seedTwo(t, h.super)
	// The identity belongs to B. Nothing about it is A's.
	seedSendable(t, h.super, b, "me@example.com", "")

	id := seedMessageFrom(t, h, a, "me@example.com")
	if got := warningKinds(t, h, a, id); len(got) != 0 {
		t.Errorf("A was warned that a sender claims one of B's OWN domains (%v) — the index is "+
			"being built across users, which tells A which domains B owns", got)
	}
	// THE CONTROL: give A the same identity and selfSpoof appears, so the
	// silence above is isolation rather than a broken fixture or a property that
	// never fires.
	seedSendable(t, h.super, a, "me@example.com", "")
	if got := warningKinds(t, h, a, id); len(got) == 0 || got[0] != "selfSpoof" {
		t.Fatalf("CONTROL FAILED: with A's OWN identity seeded there is still no selfSpoof (%v), "+
			"so the isolation assertion above proves nothing", got)
	}
}

// Self-spoofing: mail claiming the user's own identity domain, from outside,
// without passing authentication.
func TestSelfSpoofIsReported(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	// seedSendable gives the fixture a verified identity to send as; the same
	// row is what makes the domain "ours" for this check.
	seedSendable(t, h.super, a, "me@example.com", "")

	id := seedMessageFrom(t, h, a, "me@example.com")
	got := warningKinds(t, h, a, id)
	if len(got) == 0 || got[0] != "selfSpoof" {
		t.Errorf("kinds = %v, want selfSpoof first — a stranger claiming the user's own domain "+
			"without authenticating is the most serious thing this property says", got)
	}
	// THE CONTROL: an ordinary outside domain, same request, no warning — so the
	// warning above came from the identity row rather than from every message.
	other := seedMessageFrom(t, h, a, "someone@elsewhere.example")
	if w := warningKinds(t, h, a, other); len(w) != 0 {
		t.Errorf("an unrelated sender produced %v", w)
	}
}

// seedMessageWithAuth files a message with a chosen From and a chosen set of
// Authentication-Results headers, so a test can exercise the seam between what
// a header SAYS and who SAID it (#414).
func seedMessageWithAuth(t *testing.T, h *composeHarness, f apiFixture, from string, authHeaders ...string) ulid.ULID {
	t.Helper()
	raw := "From: Someone <" + from + ">\r\nTo: you@example.com\r\nSubject: hello\r\n" +
		"Date: Mon, 6 Jan 2025 10:00:00 +0000\r\n"
	for _, a := range authHeaders {
		raw += "Authentication-Results: " + a + "\r\n"
	}
	raw += "Content-Type: text/plain\r\n\r\nhi\r\n"
	id := seedRawMessage(t, h, f, []byte(raw))
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET from_address = $2 WHERE id = $1`, id.String(), from); err != nil {
		t.Fatalf("set from_address: %v", err)
	}
	return id
}

// ⚠ #414: A SENDER MUST NOT BE ABLE TO SWITCH OFF THE WARNING ABOUT THEIR OWN
// SPOOFING.
//
// selfSpoof is suppressed when DKIM or DMARC affirmatively passed. RFC 8601 §5
// makes an MTA strip only Authentication-Results headers bearing ITS OWN
// authserv-id, so a header claiming any other id travels through an ordinary MX
// untouched — and the parser used to believe it.
//
// ⚠ THE OLD UNIT TEST COULD NOT SEE THIS. It took AuthPassed as a BOOLEAN INPUT,
// so it tested the rule given an answer and never crossed the seam where the
// answer is chosen. This test starts from the header bytes.
func TestAForgedAuthHeaderDoesNotSuppressSelfSpoof(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")

	// ── CONTROL: with no auth headers at all, selfSpoof fires. ──────────────
	// Everything below is "the warning survived", which is also what a broken
	// fixture, a wrong identity row and an unseeded domain all look like.
	if got := warningKinds(t, h, a, seedMessageFrom(t, h, a, "me@example.com")); len(got) == 0 || got[0] != "selfSpoof" {
		t.Fatalf("CONTROL FAILED: selfSpoof did not fire on plain own-domain mail (%v), so its "+
			"survival below proves nothing", got)
	}

	// ── The attack: the sender writes their own passing header. ─────────────
	forged := seedMessageWithAuth(t, h, a, "me@example.com", "x.example; dkim=pass")
	got := warningKinds(t, h, a, forged)
	found := false
	for _, k := range got {
		if k == "selfSpoof" {
			found = true
		}
	}
	if !found {
		t.Errorf("a message claiming the user's OWN domain, carrying an Authentication-Results "+
			"header the SENDER wrote, was not warned about (%v). That header costs one line and "+
			"nothing strips it, so the most serious of the three warnings would be opt-out for "+
			"the attacker at zero cost", got)
	}
}

// The other half, and it is what stops the fix from being "ignore auth entirely":
// a verdict from an authserv-id the operator NAMED still suppresses the warning.
func TestATrustedAuthHeaderStillSuppressesSelfSpoof(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedSendable(t, h.super, a, "me@example.com", "")
	// The deployment names its provider as the authenticating hop.
	h.api.trustedAuthservIDs = []string{"mx.provider.example"}

	// ── CONTROL: an UNTRUSTED id with the same verdict does NOT suppress. ───
	// Without this, "suppressed" below is also what "auth is ignored and
	// selfSpoof never fires" looks like.
	if got := warningKinds(t, h, a, seedMessageWithAuth(t, h, a, "me@example.com", "x.example; dkim=pass")); len(got) == 0 {
		t.Fatalf("CONTROL FAILED: an untrusted authserv-id suppressed the warning, so the " +
			"suppression below is not evidence that identity is being checked")
	}

	trusted := seedMessageWithAuth(t, h, a, "me@example.com", "mx.provider.example; dkim=pass")
	if got := warningKinds(t, h, a, trusted); len(got) != 0 {
		t.Errorf("a genuine pass from the CONFIGURED authserv-id still produced %v — the fix has "+
			"become 'never believe authentication', which turns a precise warning into one that "+
			"fires on every own-domain message a provider correctly authenticated", got)
	}
}

// #754 end to end: a message with TWO From fields — or an empty From field
// beside a real one — raises multipleFrom through the real Email/get, from
// the real bytes; the same message with one From field does not.
func TestSenderWarningsFlagMultipleFromFields(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	seedCorrespondent(t, h, a, "billing@mailbox.example")
	msg := func(fromLines string) ulid.ULID {
		id := seedRawMessage(t, h, a, []byte(fromLines+
			"To: you@example.com\r\nSubject: hello\r\nDate: Mon, 6 Jan 2025 10:00:00 +0000\r\n"+
			"Content-Type: text/plain\r\n\r\nhi\r\n"))
		if _, err := h.super.Exec(context.Background(),
			`UPDATE app.messages SET from_address = 'billing@mailbox.example' WHERE id = $1`, id.String()); err != nil {
			t.Fatal(err)
		}
		return id
	}
	one := msg("From: Billing <billing@mailbox.example>\r\n")
	if got := warningKinds(t, h, a, one); len(got) != 0 {
		t.Fatalf("CONTROL: one From field produced %v", got)
	}
	for name, lines := range map[string]string{
		"two From fields":         "From: billing@mailbox.example\r\nFrom: mallory@evil.example\r\n",
		"an empty From beside it": "From: billing@mailbox.example\r\nFrom:\r\n",
	} {
		got := warningKinds(t, h, a, msg(lines))
		if len(got) == 0 || got[0] != "multipleFrom" {
			t.Errorf("%s: warnings = %v, want multipleFrom", name, got)
		}
	}
}
