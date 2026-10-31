// SPDX-License-Identifier: AGPL-3.0-only

package ingest

// Tests for re-delivery dedup (#206).
//
// The cases are not invented. Every "collapses" case below is a shape `diff-duplicates`
// actually found in a real mailbox — `Received:` differing in nearly every group, and
// nine further per-delivery fields from two providers in the rest.
//
// The "stays separate" cases are ADR-001's own stated hazards, and they matter MORE than
// the collapsing ones: over-merging silently loses mail, while under-merging is a visible
// annoyance a person can act on. If this file ever has to choose, it chooses separate.

import (
	"crypto/sha256"
	"strings"
	"testing"
)

// msg builds an RFC 5322 message with CRLF line endings, as the wire uses.
func msg(headers []string, body string) []byte {
	return []byte(strings.Join(headers, "\r\n") + "\r\n\r\n" + body)
}

// base is the identity-bearing part shared by the copies of one message.
var base = []string{
	"Message-ID: <0100019a1c7e5d02-83b0f6a4@example.com>",
	"Date: Wed, 30 Jul 2026 16:50:31 +0000",
	"From: Example Games <info@games.example>",
	"To: news@example.net",
	"Subject: New release - Sample Roleplaying Game is available now!",
}

const body = "Hello, here is a newsletter.\r\n\r\nRegards.\r\n"

func same(t *testing.T, a, b []byte, why string) {
	t.Helper()
	if DedupHash(a) != DedupHash(b) {
		t.Errorf("%s: hashes differ, want the copies to collapse", why)
	}
}

func differ(t *testing.T, a, b []byte, why string) {
	t.Helper()
	if DedupHash(a) == DedupHash(b) {
		t.Errorf("%s: hashes MATCH, want these to stay separate messages", why)
	}
}

func TestDedupHashCollapsesRealReDeliveries(t *testing.T) {
	// Nearly every real group differed only here.
	t.Run("Received differs (nearly every real group)", func(t *testing.T) {
		a := msg(append([]string{"Received: from a.example ([10.0.0.1]) by mx1; Wed, 30 Jul 2026 16:50:56 +0000"}, base...), body)
		b := msg(append([]string{"Received: from a.example ([10.0.0.2]) by mx2; Wed, 30 Jul 2026 16:50:57 +0000"}, base...), body)
		same(t, a, b, "a differing Received line")
	})

	t.Run("several Received lines, different count", func(t *testing.T) {
		a := msg(append([]string{"Received: by mx1; t1", "Received: by mx0; t0"}, base...), body)
		b := msg(append([]string{"Received: by mx2; t2"}, base...), body)
		same(t, a, b, "a different number of relay hops")
	})

	// The remaining groups: the Gmail forwarder's and Migadu's per-delivery stamps.
	t.Run("provider stamps differ (the remaining real groups)", func(t *testing.T) {
		a := msg(append([]string{
			"X-Gm-Message-State: AOJu0Yx1",
			"X-Google-Dkim-Signature: v=1; a=rsa-sha256; d=1e100.net; s=20230601; h=x; b=AAAA",
			"X-Forwarded-Encrypted: i=1; AJvYcCV1",
			"X-Migadu-Queue-Id: 4B2C1",
			"X-Migadu-Spam-Score: -0.11",
			"X-Migadu-Flow: FLOW_IN",
			"X-Report-Abuse: abuse@example.com",
			"DKIM-Signature: v=1; a=rsa-sha256; d=example.com; b=ONE",
		}, base...), body)
		b := msg(append([]string{
			"X-Gm-Message-State: AOJu0Yz9",
			"X-Google-Dkim-Signature: v=1; a=rsa-sha256; d=1e100.net; s=20230601; h=x; b=BBBB",
			"X-Forwarded-Encrypted: i=1; AJvYcCW7",
			"X-Migadu-Queue-Id: 9F7DA",
			"X-Migadu-Spam-Score: -0.09",
			"X-Migadu-Flow: FLOW_OUT",
			"X-Report-Abuse: abuse-2@example.com",
			"DKIM-Signature: v=1; a=rsa-sha256; d=example.com; b=TWO",
		}, base...), body)
		same(t, a, b, "differing provider stamps and a re-signed DKIM")
	})

	// A header the sender wrote is not in the allowlist either, so it does not affect
	// the hash. Worth pinning: it is the direct consequence of choosing an allowlist,
	// and someone will eventually wonder whether X-Mailer should count.
	t.Run("a sender's own X- header does not split copies", func(t *testing.T) {
		a := msg(append([]string{"X-Mailer: Sendgrid 1.0"}, base...), body)
		b := msg(base, body)
		same(t, a, b, "one copy carrying an extra non-identity header")
	})

	t.Run("header ORDER does not matter", func(t *testing.T) {
		reordered := []string{base[4], base[0], base[3], base[1], base[2]}
		same(t, msg(base, body), msg(reordered, body), "the same headers in a different order")
	})

	t.Run("header FOLDING does not matter", func(t *testing.T) {
		folded := []string{
			base[0], base[1], base[2], base[3],
			"Subject: New release - Sample\r\n Roleplaying Game is available now!",
		}
		same(t, msg(base, body), msg(folded, body), "a Subject refolded by a relay")
	})

	t.Run("body line endings do not matter", func(t *testing.T) {
		lf := []byte(strings.Join(base, "\r\n") + "\r\n\r\n" + strings.ReplaceAll(body, "\r\n", "\n"))
		same(t, msg(base, body), lf, "a transport rewriting CRLF to LF")
	})
}

func TestDedupHashKeepsGenuinelyDifferentMessagesApart(t *testing.T) {
	// ADR-001'S STATED HAZARD, and the reason the body is in the hash. A list copy
	// with a rewritten footer shares the Message-ID; merging them would lose the
	// footer difference, which is exactly what ADR-001 refused to allow.
	t.Run("a mailing-list copy with a rewritten footer", func(t *testing.T) {
		direct := msg(base, body)
		listCopy := msg(base, body+"\r\n--\r\nYou are subscribed to example-list.\r\n")
		differ(t, direct, listCopy, "same Message-ID, footer added by the list")
	})

	t.Run("malicious Message-ID reuse with different content", func(t *testing.T) {
		real := msg(base, body)
		spoof := msg(base, "Please wire the money to account 12345.\r\n")
		differ(t, real, spoof, "same Message-ID, entirely different body")
	})

	for _, c := range []struct{ name, header string }{
		{"a different Subject", "Subject: Something else entirely"},
		{"a different From", "From: Someone Else <other@example.com>"},
		{"a different Date", "Date: Thu, 31 Jul 2026 09:00:00 +0000"},
		{"a different To (the catchall case)", "To: shop@example.net"},
		{"an added Cc", "Cc: someone@example.org"},
		{"a different Message-ID", "Message-ID: <other@example.com>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			field := strings.SplitN(c.header, ":", 2)[0]
			replaced := make([]string, 0, len(base)+1)
			found := false
			for _, h := range base {
				if strings.HasPrefix(h, field+":") {
					replaced = append(replaced, c.header)
					found = true
					continue
				}
				replaced = append(replaced, h)
			}
			if !found {
				replaced = append(replaced, c.header)
			}
			differ(t, msg(base, body), msg(replaced, body), c.name)
		})
	}

	// Adjacent empty fields must not be able to shift a value from one into another.
	// Without the field name in the hash, `Subject: X` + no Cc would collide with
	// `Cc: X` + no Subject.
	t.Run("a value cannot migrate between empty fields", func(t *testing.T) {
		a := msg([]string{base[0], base[1], base[2], base[3], "Subject: X"}, body)
		b := msg([]string{base[0], base[1], base[2], base[3], "Cc: X"}, body)
		differ(t, a, b, "the same value in a different field")
	})
}

func TestDedupHashIsTotalAndStable(t *testing.T) {
	// Unparseable input must never panic, and must fall back to hashing the raw bytes
	// — i.e. behave exactly as dedup did before this existed. The fallback direction
	// is deliberate: it under-merges rather than over-merges.
	t.Run("unparseable input falls back to the raw bytes", func(t *testing.T) {
		// net/mail is FAR more tolerant than it looks — an empty header block, a header
		// block ended by EOF, and even a NUL inside a field name all parse. These two
		// are the ones actually verified to fail, so they are the ones asserted; do not
		// add a case here without checking it really does fail to parse.
		for _, raw := range []string{"", "not a message"} {
			got := DedupHash([]byte(raw))
			if got != sha256.Sum256([]byte(raw)) {
				t.Errorf("DedupHash(%q) did not fall back to the raw-byte hash", raw)
			}
		}
	})

	// A header block terminated by EOF rather than a blank line is malformed, but
	// net/mail accepts it and yields an empty body — so it takes the allowlist path,
	// not the fallback. Pinned because it is surprising, and because the surprise is
	// harmless: such a message still hashes deterministically and by its own fields.
	t.Run("a header block ended by EOF parses rather than falling back", func(t *testing.T) {
		raw := []byte("Subject: no blank line")
		if DedupHash(raw) == sha256.Sum256(raw) {
			t.Error("expected the parsed path, not the raw-byte fallback")
		}
		if DedupHash(raw) != DedupHash([]byte("Subject: no blank line")) {
			t.Error("not deterministic")
		}
	})

	t.Run("it is deterministic across repeated calls", func(t *testing.T) {
		m := msg(base, body)
		first := DedupHash(m)
		for i := 0; i < 50; i++ {
			if DedupHash(m) != first {
				t.Fatal("DedupHash is not deterministic — a map iteration order leaked in")
			}
		}
	})

	t.Run("a message with an empty body still hashes", func(t *testing.T) {
		a := msg(base, "")
		b := msg(base, "x")
		differ(t, a, b, "empty body vs one byte of body")
	})

	t.Run("a large body does not blow up", func(t *testing.T) {
		big := strings.Repeat("line of text\r\n", 200_000)
		if DedupHash(msg(base, big)) == ([32]byte{}) {
			t.Error("hashed to zero")
		}
	})
}
