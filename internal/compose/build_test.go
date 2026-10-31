// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"strings"
	"testing"
	"time"
)

func mustBuild(t *testing.T, d Draft) Built {
	t.Helper()
	b, err := Build(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return b
}

// Spec §5: "generate Message-ID under the sending identity's domain (mismatched
// Message-ID domains cost spam-filter points)". The domain comes from the From
// address and from nowhere else — in particular never from the server's
// hostname, which would both look forged AND leak the operator's infrastructure
// into every message that leaves the building (CLAUDE.md).
func TestMessageIDUsesSendingIdentityDomain(t *testing.T) {
	b := mustBuild(t, Draft{
		From:     Address{Email: "store@shop.example.com"},
		To:       []Address{{Email: "someone@example.net"}},
		TextBody: "hi",
	})
	if !strings.HasSuffix(b.MessageID, "@shop.example.com") {
		t.Fatalf("Message-ID = %q, want it under the From domain shop.example.com", b.MessageID)
	}
	if !strings.Contains(string(b.Raw), "Message-ID: <"+b.MessageID+">") {
		t.Fatal("the built bytes must carry the Message-ID header")
	}

	// Two sends never share a Message-ID (it is random, not a counter).
	other := mustBuild(t, Draft{
		From: Address{Email: "store@shop.example.com"},
		To:   []Address{{Email: "someone@example.net"}}, TextBody: "hi",
	})
	if other.MessageID == b.MessageID {
		t.Fatal("Message-IDs must be unpredictable and unique")
	}
}

// RFC 5322 §3.6.4: References = the parent's References, then the parent's
// Message-ID. In-Reply-To = the parent's Message-ID.
func TestReplyThreadingHeaders(t *testing.T) {
	b := mustBuild(t, Draft{
		From:       Address{Email: "you@example.com"},
		To:         []Address{{Email: "them@example.net"}},
		Subject:    "Re: hello",
		InReplyTo:  "parent@example.net",
		References: []string{"root@example.net", "mid@example.net"},
		TextBody:   "yes",
	})
	raw := string(b.Raw)
	if !strings.Contains(raw, "In-Reply-To: <parent@example.net>") {
		t.Error("In-Reply-To must be the parent's Message-ID")
	}
	want := "References: <root@example.net> <mid@example.net> <parent@example.net>"
	if !strings.Contains(raw, want) {
		t.Errorf("References header wrong.\n got: %q\nwant: %q", raw, want)
	}
}

func TestReferencesAreCappedAndDeduped(t *testing.T) {
	var long []string
	for i := 0; i < 50; i++ {
		long = append(long, "id"+string(rune('a'+i%26))+"@example.net")
	}
	refs := buildReferences(append(long, "root@example.net", "root@example.net"), "parent@example.net")
	if len(refs) > maxReferences {
		t.Fatalf("References len = %d, want <= %d", len(refs), maxReferences)
	}
	if refs[len(refs)-1] != "parent@example.net" {
		t.Error("the parent's Message-ID must survive the cap: it is what threading actually uses")
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if seen[r] {
			t.Fatalf("References contains a duplicate: %q", r)
		}
		seen[r] = true
	}
}

// Bcc: STORED bytes keep it (so a resumed draft and the filed Sent copy still
// know who was blind-copied); TRANSMITTED bytes must not (that is the entire
// point of Bcc). The envelope carries them either way.
func TestBccIsStoredButNotTransmitted(t *testing.T) {
	b := mustBuild(t, Draft{
		From:     Address{Email: "you@example.com"},
		To:       []Address{{Email: "them@example.net"}},
		Bcc:      []Address{{Name: "Secret", Email: "quiet@example.org"}},
		TextBody: "hi",
	})
	stored := string(b.Raw)
	if !strings.Contains(stored, "Bcc: ") || !strings.Contains(stored, "quiet@example.org") {
		t.Fatal("the STORED draft must keep its Bcc header")
	}
	wire := string(SendPayload(b.Raw))
	if strings.Contains(wire, "quiet@example.org") || strings.Contains(wire, "Bcc:") {
		t.Fatalf("the TRANSMITTED message must not disclose blind recipients:\n%s", wire)
	}
	if !strings.Contains(wire, "them@example.net") {
		t.Error("stripping Bcc must not disturb the other headers")
	}
	if !strings.Contains(wire, "\r\n\r\nhi\r\n") {
		t.Error("stripping Bcc must not disturb the body")
	}

	if len(b.EnvelopeTo) != 2 {
		t.Fatalf("envelope = %v, want both the To and the Bcc recipient", b.EnvelopeTo)
	}
	var found bool
	for _, r := range b.EnvelopeTo {
		if r == "quiet@example.org" {
			found = true
		}
	}
	if !found {
		t.Error("a Bcc recipient must still be in the SMTP envelope — they have to receive the mail")
	}
}

// A folded Bcc (as another client might have written into the IMAP Drafts
// folder) must be removed in full, continuation lines included.
func TestSendPayloadStripsFoldedBcc(t *testing.T) {
	raw := []byte("From: you@example.com\r\n" +
		"Bcc: one@example.org,\r\n\ttwo@example.org\r\n" +
		"Subject: x\r\n" +
		"\r\nbody\r\n")
	wire := string(SendPayload(raw))
	if strings.Contains(wire, "example.org") {
		t.Fatalf("folded Bcc survived the strip:\n%s", wire)
	}
	if !strings.Contains(wire, "Subject: x") || !strings.Contains(wire, "body") {
		t.Fatalf("strip removed too much:\n%s", wire)
	}
}

// Header injection: a Subject (or display name) carrying CRLF must not be able
// to smuggle in another header field.
func TestHeaderInjectionIsNeutralized(t *testing.T) {
	b := mustBuild(t, Draft{
		From:     Address{Email: "you@example.com"},
		To:       []Address{{Email: "them@example.net"}},
		Subject:  "hi\r\nBcc: attacker@evil.example\r\nX-Evil: yes",
		TextBody: "x",
	})
	raw := string(b.Raw)
	if strings.Contains(raw, "attacker@evil.example") && strings.Contains(raw, "\r\nBcc:") {
		t.Fatalf("CRLF in a subject injected a real header:\n%s", raw)
	}
	if strings.Contains(raw, "\r\nX-Evil:") {
		t.Fatalf("CRLF in a subject injected a real header:\n%s", raw)
	}
}

// ⚠ THE FIRST CASE ASSERTED THE OPPOSITE UNTIL #601, and the scenario is kept
// rather than deleted, like the #589 inversions in ui/outbox.test.ts.
//
// Build builds a DRAFT, and a draft with nobody in the To field is ordinary —
// you write the note at the top of a forward before deciding who needs it. RFC
// 5322 makes destination fields optional. Refusing it here meant the client
// could not autosave one, so everything typed before a recipient was added was
// silently discarded when the composer closed.
//
// The rule was not removed, it MOVED to where it is true: PlanSubmit refuses an
// empty envelope, and TestPlanSubmitStillRefusesADraftWithNoRecipients is the
// control proving it. Without that control this change would be a guard deleted
// rather than relocated.
func TestBuildRequiresRecipientsAndFrom(t *testing.T) {
	if _, err := Build(Draft{From: Address{Email: "you@example.com"}, TextBody: "x"}); err != nil {
		t.Errorf("a DRAFT with no recipients must be accepted (#601): %v", err)
	}
	if _, err := Build(Draft{To: []Address{{Email: "them@example.net"}}, TextBody: "x"}); err == nil {
		t.Error("a message with no From must be refused")
	}
	if _, err := Build(Draft{
		From: Address{Email: "you@example.com"},
		To:   []Address{{Email: "not-an-address"}}, TextBody: "x",
	}); err == nil {
		t.Error("an invalid recipient address must be refused before it reaches SMTP")
	}
}

func TestBuildMultipartAlternative(t *testing.T) {
	b := mustBuild(t, Draft{
		From: Address{Email: "you@example.com"},
		To:   []Address{{Email: "them@example.net"}},
		Date: time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC),
		// Rich compose: both bodies, text first (RFC 2046 §5.1.4).
		TextBody: "plain", HTMLBody: "<p>rich</p>",
	})
	raw := string(b.Raw)
	if !strings.Contains(raw, "Content-Type: multipart/alternative; boundary=") {
		t.Fatal("a message with both bodies must be multipart/alternative")
	}
	plain := strings.Index(raw, "text/plain")
	html := strings.Index(raw, "text/html")
	if plain < 0 || html < 0 || plain > html {
		t.Error("the plain-text alternative must come first (least-rich first)")
	}
	if !strings.Contains(raw, "Date: Sun, 12 Jul 2026 10:00:00 +0000") {
		t.Errorf("Date header missing or not UTC:\n%s", raw)
	}
}

func TestParseEnvelopeReadsBytes(t *testing.T) {
	b := mustBuild(t, Draft{
		From:    Address{Name: "You", Email: "you@example.com"},
		To:      []Address{{Email: "a@example.net"}},
		Cc:      []Address{{Email: "b@example.net"}},
		Bcc:     []Address{{Email: "c@example.org"}},
		Subject: "x", TextBody: "y",
	})
	env := ParseEnvelope(b.Raw)
	if env.From != "you@example.com" {
		t.Errorf("From = %q", env.From)
	}
	if len(env.Recipients) != 3 {
		t.Fatalf("recipients = %v, want To + Cc + Bcc", env.Recipients)
	}
}
