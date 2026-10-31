// SPDX-License-Identifier: AGPL-3.0-only

package compose

// The re-send gate (#491): outboundAlreadyPrepared decides whether a message has
// already been through prepareOutbound, and prepareOutbound leaves it alone when
// it has.
//
// The hazard this file exists for is proved first, in
// TestSigningOurOwnOutputAgainNestsTheSignature: signing twice does not fail, it
// produces a message that still verifies while burying the real signature and
// (upstream of here) making a receiver discard the sender's key. A gate against a
// failure mode that never errors is exactly the case CLAUDE.md's "the absence of
// an error proves nothing" is about, so the double-signed shape is measured
// rather than described.

import (
	"bytes"
	"strings"
	"testing"

	"mailroom/internal/autocrypt"
	"mailroom/internal/mime"
	"mailroom/internal/pgpmime"
)

// unsignedDraft is what Build produces: a plain message with no Autocrypt header
// and no signature. The ordinary first submit.
func unsignedDraft(t *testing.T) []byte {
	t.Helper()
	built, err := Build(Draft{
		From:     Address{Email: outboundFrom},
		To:       []Address{{Email: "bob@example.net"}},
		Subject:  "not yet prepared",
		TextBody: "Body with an em-dash — so it is quoted-printable too.\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return built.Raw
}

// preparedOutput is prepareOutbound's own output on an instance WITH a crypto
// process: the Autocrypt header first, then the RFC 3156 wrapper — in that order
// and by the same two calls prepareOutbound makes.
func preparedOutput(t *testing.T) []byte {
	t.Helper()
	advertised := prependHeader(unsignedDraft(t), autocrypt.HeaderName,
		autocrypt.Header(outboundFrom, fakeKey(), autocryptPreferMutual))
	signed, _ := signWithStub(t, advertised)
	return signed
}

// TestSigningOurOwnOutputAgainNestsTheSignature is the negative control for the
// whole file: it measures what the gate prevents.
func TestSigningOurOwnOutputAgainNestsTheSignature(t *testing.T) {
	once := preparedOutput(t)
	twice, _ := signWithStub(t, once)

	if got := bytes.Count(twice, []byte("multipart/signed")); got != 2 {
		t.Fatalf("multipart/signed appears %d times after signing twice, want 2 — the "+
			"fixture does not reproduce #491 and the gate below would be guarding nothing", got)
	}
	// It does not error, and it does not even fail to verify: the outer signature
	// covers the inner signed entity. That is why this needed a gate rather than a
	// validation.
	parts, err := pgpmime.SplitSigned(twice)
	if err != nil {
		t.Fatalf("the doubly-signed message does not even split: %v", err)
	}
	if !bytes.Contains(parts.Entity, []byte("multipart/signed")) {
		t.Error("the outer signature does not cover a nested multipart/signed — this test is " +
			"no longer describing the #491 shape")
	}
	// And the Autocrypt half, which is the part that silently loses the key:
	// prepareOutbound would have prepended a second header here, and Level 1 makes
	// a receiver ignore BOTH.
	twiceAdvertised := prependHeader(twice, autocrypt.HeaderName,
		autocrypt.Header(outboundFrom, fakeKey(), autocryptPreferMutual))
	if _, ok, err := autocrypt.Parse(headerValues(t, twiceAdvertised, autocrypt.HeaderName)); ok || err == nil {
		t.Errorf("two Autocrypt headers parsed as usable (ok=%v err=%v); Level 1 requires "+
			"both to be ignored, which is why a second one deletes the user's key rather "+
			"than duplicating it", ok, err)
	}
}

func TestOutboundAlreadyPrepared(t *testing.T) {
	// A received multipart/signed from somebody else, of the kind an IMAP client
	// can leave in Drafts (a reply being composed against a signed message, or a
	// message copied there). It is signed, it carries ITS AUTHOR's Autocrypt
	// header — and it is not ours.
	foreign := func() []byte {
		raw := []byte("From: Someone Else <stranger@example.org>\r\n" +
			"To: " + outboundFrom + "\r\n" +
			"Subject: signed, by them\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			"their body\r\n")
		raw = prependHeader(raw, autocrypt.HeaderName,
			autocrypt.Header("stranger@example.org", fakeKey(), false))
		signed, _ := signWithStub(t, raw)
		return signed
	}

	cases := []struct {
		name    string
		raw     []byte
		from    string
		want    bool
		wantWhy preparedReason
		whyItIs string
	}{{
		name:    "an unsigned draft",
		raw:     unsignedDraft(t),
		from:    outboundFrom,
		want:    false,
		wantWhy: reasonUnprepared,
		whyItIs: "the ordinary first submit: nothing has prepared these bytes, so they must be prepared now",
	}, {
		name:    "our own output from prepareOutbound",
		raw:     preparedOutput(t),
		from:    outboundFrom,
		want:    true,
		wantWhy: reasonSignedAndAdvertised,
		whyItIs: "#491 itself: the re-send after an Undo Send reads back exactly these bytes",
	}, {
		name:    "signed but with no Autocrypt header",
		raw:     func() []byte { s, _ := signWithStub(t, unsignedDraft(t)); return s }(),
		from:    outboundFrom,
		want:    true,
		wantWhy: reasonSigned,
		whyItIs: "what prepareOutbound emits for a bound key with no public half — signing must not nest",
	}, {
		name:    "an Autocrypt header but no signature",
		raw:     rawWithAutocrypt(t),
		from:    outboundFrom,
		want:    true,
		wantWhy: reasonAdvertised,
		whyItIs: "what prepareOutbound emits on an instance with NO crypto process, which is most of them — " +
			"requiring a signature here would add a second Autocrypt header on every such re-send",
	}, {
		name:    "a received multipart/signed from someone else",
		raw:     foreign(),
		from:    outboundFrom,
		want:    false,
		wantWhy: reasonFromMismatch,
		whyItIs: "their signature is not our preparation: it is their From and their key, so this send " +
			"still needs ours",
	}, {
		name:    "no sending address",
		raw:     preparedOutput(t),
		from:    "  ",
		want:    false,
		wantWhy: reasonNoSendingAddress,
		whyItIs: "with nothing to compare against, 'already prepared' cannot be established — and the " +
			"safe answer is to prepare",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := outboundAlreadyPrepared(tc.raw, tc.from)
			if got != tc.want {
				t.Errorf("outboundAlreadyPrepared = %v (%s), want %v — %s", got, why, tc.want, tc.whyItIs)
			}
			if why != tc.wantWhy {
				t.Errorf("reason = %q, want %q", why, tc.wantWhy)
			}
		})
	}
}

// TestOutboundAlreadyPreparedIsCaseAndFormInsensitiveOnTheFrom pins the
// comparison to store.NormalizeAddress rather than to string equality: a
// display-name form and a different case are the SAME address, and answering
// "not ours" for either would sign the message a second time.
func TestOutboundAlreadyPreparedIsCaseAndFormInsensitiveOnTheFrom(t *testing.T) {
	raw := preparedOutput(t)
	for _, from := range []string{outboundFrom, strings.ToUpper(outboundFrom), " " + outboundFrom + " "} {
		if prepared, why := outboundAlreadyPrepared(raw, from); !prepared {
			t.Errorf("from %q: not recognised as ours (%s)", from, why)
		}
	}
	// And the control: a different address at the same domain is NOT ours.
	if prepared, why := outboundAlreadyPrepared(raw, "someone-else@example.com"); prepared {
		t.Errorf("a different local part was accepted as ours (%s) — the comparison is not "+
			"looking at the address", why)
	}
}

// headerValues reads every value of one header off raw, the way a receiving
// client would: from the OUTER message only.
func headerValues(t *testing.T, raw []byte, name string) []string {
	t.Helper()
	parsed, err := mime.ParseBytes(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return parsed.Header.Values(name)
}
