// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"
	"time"

	"mailroom/internal/mime"
	"mailroom/internal/store"
)

func TestBucketDelta(t *testing.T) {
	h := time.Hour
	for _, tc := range []struct {
		name      string
		d         time.Duration
		wantB     int
		wantLater bool
	}{
		{"zero", 0, bucketWithinDay, false},
		{"just under a day later", 23*h + 59*time.Minute, bucketWithinDay, false},
		{"just under a day earlier", -(23*h + 59*time.Minute), bucketWithinDay, false},
		{"exactly a day later", 24 * h, bucketDay30, true},
		{"exactly a day earlier", -24 * h, bucketDay30, false},
		{"29 days later", 29 * day, bucketDay30, true},
		{"30 days later", 30 * day, bucketDay365, true},
		{"364 days earlier", -364 * day, bucketDay365, false},
		{"365 days later", 365 * day, bucketOverYear, true},
		{"5 years earlier", -5 * 365 * day, bucketOverYear, false},
	} {
		b, later := bucketDelta(tc.d)
		if b != tc.wantB || later != tc.wantLater {
			t.Errorf("%s: bucketDelta(%v) = (%d,%v), want (%d,%v)", tc.name, tc.d, b, later, tc.wantB, tc.wantLater)
		}
	}
}

func TestAgeBucket(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		ago  time.Duration
		want int
	}{
		{-time.Hour, ageFuture},
		{0, age30},
		{30 * day, age30},
		{30*day + time.Second, age90},
		{90 * day, age90},
		{91 * day, age365},
		{365 * day, age365},
		{366 * day, ageOver},
	} {
		if got := ageBucket(now, now.Add(-tc.ago)); got != tc.want {
			t.Errorf("ageBucket(%v ago) = %d, want %d", tc.ago, got, tc.want)
		}
	}
}

func TestBuildContactDatesReport(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	rows := []store.ContactMailDate{
		// from-only; stored (ingest) dates are recent, true ones old.
		{Address: "a@example.org", FirstSeenAt: now.Add(-10 * day), LastSeenAt: now.Add(-5 * day),
			FromCount: 2, FromFirst: ago(400 * day), FromLast: ago(100 * day)},
		// sent-only.
		{Address: "b@example.org", FirstSeenAt: now, LastSeenAt: now},
		// both directions: the sent side extends the range at the late end.
		{Address: "c@example.org", FirstSeenAt: now, LastSeenAt: now,
			FromCount: 1, FromFirst: ago(50 * day), FromLast: ago(50 * day)},
		// nothing.
		{Address: "d@example.org", FirstSeenAt: now, LastSeenAt: now},
		{Address: "e@example.org", FirstSeenAt: now, LastSeenAt: now},
	}
	sent := map[string]*span{
		"b@example.org": {first: now.Add(-200 * day), last: now.Add(-200 * day), n: 1},
		"c@example.org": {first: now.Add(-60 * day), last: now.Add(-3 * day), n: 2},
	}
	rep := buildContactDatesReport(rows, sent, now, 10)

	if rep.examined != 5 || rep.withMail != 3 || rep.withoutMail != 2 {
		t.Fatalf("totals = %d/%d/%d, want 5/3/2", rep.examined, rep.withMail, rep.withoutMail)
	}
	if rep.withFrom != 2 || rep.withSent != 2 || rep.withBoth != 1 {
		t.Errorf("directions from/sent/both = %d/%d/%d, want 2/2/1", rep.withFrom, rep.withSent, rep.withBoth)
	}
	if len(rep.noMailExamples) != 2 || rep.noMailExamples[0] != "d@example.org" {
		t.Errorf("no-mail examples = %v", rep.noMailExamples)
	}
	// first: a later >1y (390d), b later 30-365 (200d), c later 30-365 (60d).
	if rep.first.later[bucketOverYear] != 1 || rep.first.later[bucketDay365] != 2 || rep.first.within != 0 {
		t.Errorf("first buckets = %+v", rep.first)
	}
	// last: a later 30-365 (95d), b later 30-365 (200d), c later 1-30 (3d).
	if rep.last.later[bucketDay365] != 2 || rep.last.later[bucketDay30] != 1 {
		t.Errorf("last buckets = %+v", rep.last)
	}
	// true last age: a 100d (<=1y), b 200d (<=1y), c 3d (<=30d).
	if rep.trueAge[age30] != 1 || rep.trueAge[age365] != 2 {
		t.Errorf("true age = %v", rep.trueAge)
	}
	// stored last age: a 5d, b 0, c 0 -> all <=30d.
	if rep.storedAge[age30] != 3 {
		t.Errorf("stored age = %v", rep.storedAge)
	}
	if len(rep.sample) != 3 {
		t.Fatalf("sample = %d rows, want 3", len(rep.sample))
	}
	// c's true range combines both directions.
	c := rep.sample[2]
	if c.address != "c@example.org" || !c.trueFirst.Equal(now.Add(-60*day)) || !c.trueLast.Equal(now.Add(-3*day)) {
		t.Errorf("c sample = %+v", c)
	}
}

func TestBuildContactDatesReportEmpty(t *testing.T) {
	rep := buildContactDatesReport(nil, nil, time.Now(), 10)
	if rep.examined != 0 || rep.withMail != 0 || rep.withoutMail != 0 || rep.sample != nil {
		t.Errorf("empty report = %+v", rep)
	}
}

func TestPickSample(t *testing.T) {
	rows := make([]sampleRow, 10)
	for i := range rows {
		rows[i].address = string(rune('a' + i))
	}
	got := pickSample(rows, 3)
	if len(got) != 3 || got[0].address != "a" || got[1].address != "d" || got[2].address != "g" {
		t.Errorf("pickSample = %v", got)
	}
	if len(pickSample(rows, 50)) != 10 || pickSample(rows, 0) != nil {
		t.Error("pickSample bounds wrong")
	}
}

func TestRecipientAddresses(t *testing.T) {
	raw := "From: Me <me@example.com>\r\nTo: Alice <Alice@Example.org>, bob@example.org\r\n" +
		"Cc: alice@example.org\r\nBcc: Carol <carol@example.org>\r\nSubject: x\r\n\r\nbody\r\n"
	msg, err := mime.ParseBytes([]byte(raw))
	if err != nil || msg == nil {
		t.Fatalf("parse: %v", err)
	}
	got := strings.Join(recipientAddresses(msg.Header), ",")
	if got != "alice@example.org,bob@example.org,carol@example.org" {
		t.Errorf("recipients = %s (want To, Cc and Bcc, deduplicated, lowercased)", got)
	}
}
