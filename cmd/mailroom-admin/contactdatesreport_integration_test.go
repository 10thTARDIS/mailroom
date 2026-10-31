// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// contact-dates-report (#816) against a real Postgres: messages whose dates
// differ from the contacts' stored (ingest-time) dates, with ground truth worked
// out by hand. Recipients come from fixture blobs through the real MIME parser.
//
//	alice  From at 400d and 100d ago; stored first -10d, last -5d (ingest time)
//	bob    From at 20d ago; stored now
//	carol  only a Cc of a message the user sent 200d ago; stored now
//	dan    only a Bcc of a message the user sent 5d ago; stored 5d ago (correct)
//	zed    From at 10d ago; stored first -100d (EARLIER than any mail), last -10d
//	erin   a contact with no message at all
//	frank  only From a DRAFT (drafts are excluded) -> no mail found
//
// A second user also has mail From alice, and must not move her dates (RLS).
func TestContactDatesReportSeededMailbox(t *testing.T) {
	h := newUntangleHarness(t)
	tag := "cdates-" + strings.ToLower(store.NewULID().String())
	s := newNudgeSeed(t, h, tag)
	me := "me-" + tag + "@example.com"
	other := newNudgeSeed(t, h, tag+"-2")

	days := func(n float64) time.Time { return s.now.Add(-time.Duration(n * float64(24*time.Hour))) }
	contact := func(addr string, first, last time.Time) {
		cid := store.NewULID().String()
		s.exec(`INSERT INTO app.contacts (id, user_id, display_name) VALUES ($1,$2,'')`, cid, s.user)
		s.exec(`INSERT INTO app.contact_addresses (id, user_id, contact_id, address, first_seen_at, last_seen_at)
		        VALUES ($1,$2,$3,$4,$5,$6)`, store.NewULID().String(), s.user, cid, addr, first, last)
	}
	contact("alice@example.org", days(10), days(5))
	contact("bob@example.org", s.now, s.now)
	contact("carol@example.org", s.now, s.now)
	contact("dan@example.org", days(5), days(5))
	contact("zed@example.org", days(100), days(10))
	contact("erin@example.org", s.now, s.now)
	contact("frank@example.org", s.now, s.now)

	th := s.thread()
	s.add(nmsg{thread: th, daysAgo: 400, from: "alice@example.org", to: me, subject: "one", body: "a", msgID: "<a1@x>"})
	s.add(nmsg{thread: th, daysAgo: 100, from: "alice@example.org", to: me, subject: "two", body: "b", msgID: "<a2@x>"})
	s.add(nmsg{thread: th, daysAgo: 20, from: "bob@example.org", to: me, subject: "three", body: "c", msgID: "<b1@x>"})
	s.add(nmsg{thread: th, daysAgo: 200, from: me, to: "someone@example.net", cc: "carol@example.org", subject: "four", body: "d", msgID: "<c1@x>"})
	s.add(nmsg{thread: th, daysAgo: 5, from: me, to: "someone@example.net", extraHeaders: "Bcc: dan@example.org\r\n", subject: "five", body: "e", msgID: "<d1@x>"})
	s.add(nmsg{thread: th, daysAgo: 10, from: "zed@example.org", to: me, subject: "six", body: "f", msgID: "<z1@x>"})
	s.add(nmsg{thread: th, daysAgo: 3, from: "frank@example.org", to: me, subject: "seven", body: "g", msgID: "<f1@x>", draft: true})
	// another user's mail From alice, newer than hers: must not leak across users.
	other.add(nmsg{thread: other.thread(), daysAgo: 1, from: "alice@example.org", to: "x@example.org", subject: "x", body: "x", msgID: "<o1@x>"})

	ctx := context.Background()
	uid := ulid.MustParse(s.user)
	rep, err := collectContactDates(ctx, h.store, uid, s.reader(), 2 /* small batch: several pages */, s.now, 10)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	if rep.examined != 7 || rep.withMail != 5 || rep.withoutMail != 2 {
		t.Fatalf("totals examined/with/without = %d/%d/%d, want 7/5/2", rep.examined, rep.withMail, rep.withoutMail)
	}
	if len(rep.noMailExamples) != 2 || rep.noMailExamples[0] != "erin@example.org" || rep.noMailExamples[1] != "frank@example.org" {
		t.Errorf("no-mail examples = %v, want erin and frank", rep.noMailExamples)
	}
	if rep.withFrom != 3 || rep.withSent != 2 || rep.withBoth != 0 {
		t.Errorf("directions from/sent/both = %d/%d/%d, want 3/2/0", rep.withFrom, rep.withSent, rep.withBoth)
	}
	if rep.sent.own != 2 || rep.sent.unreadable != 0 {
		t.Errorf("sent bounds = %+v, want 2 own, 0 unreadable", rep.sent)
	}

	// first_seen_at: alice later >1y, bob later 1-30, carol later 30-365,
	// dan within 1d, zed EARLIER 30-365.
	f := rep.first
	if f.within != 1 || f.later[bucketOverYear] != 1 || f.later[bucketDay30] != 1 || f.later[bucketDay365] != 1 || f.earlier[bucketDay365] != 1 {
		t.Errorf("first buckets = %+v", f)
	}
	// last_seen_at: alice later 30-365 (95d), bob later 1-30 (20d), carol later
	// 30-365 (200d), dan within, zed within.
	l := rep.last
	if l.within != 2 || l.later[bucketDay365] != 2 || l.later[bucketDay30] != 1 {
		t.Errorf("last buckets = %+v", l)
	}
	// true last age: alice 100d, carol 200d (<=1y); bob 20d, dan 5d, zed 10d (<=30d).
	if rep.trueAge[age30] != 3 || rep.trueAge[age365] != 2 {
		t.Errorf("true age = %v", rep.trueAge)
	}
	// ...where the stored column says all five were seen within 30 days.
	if rep.storedAge[age30] != 5 {
		t.Errorf("stored age = %v", rep.storedAge)
	}
}
