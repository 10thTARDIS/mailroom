// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// LEARNED SORTING, v1.104 beta 2 (ADR-032 amendment): trusted senders, a
// single filing, the stricter undo, recency and Forget, end to end through the
// real ingest path and the real Re-file write.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/store"
)

func (f *fixture) lsTrust(t *testing.T, addr string, on bool) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err := tx.SetSenderTrusted(ctx, addr, on)
		return err
	}); err != nil {
		t.Fatalf("trust %s: %v", addr, err)
	}
}

func (f *fixture) lsForget(t *testing.T, kind placement.KeyKind, value string) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err := tx.ForgetSender(ctx, kind, value)
		return err
	}); err != nil {
		t.Fatalf("forget %s: %v", value, err)
	}
}

func (f *fixture) lsBasis(t *testing.T, msg interface{ String() string }) string {
	t.Helper()
	var s string
	if err := f.super.QueryRow(context.Background(),
		`SELECT basis FROM app.learned_placements WHERE message_id=$1`, msg.String()).Scan(&s); err != nil {
		t.Fatalf("placement basis: %v", err)
	}
	return s
}

// A single filing of an address now places its next message, records that it
// did so on a single filing, and the report shows it as its own row.
func TestLearnedSortingASingleFilingPlaces(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	// Control: no filing, no placement.
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "no filings")
	f.lpFile(t, "bob@example.com", 1, areas, "Updates")
	f.lpWantFilings(t, "bob@example.com", areas["Updates"], 1)
	msg := f.lpIngest(t, "bob@example.com")
	if n := f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Fatalf("one filing: learned Updates label = %d, want 1", n)
	}
	if got := f.lsBasis(t, msg); got != "single" {
		t.Errorf("basis = %q, want single", got)
	}
	// The domain keeps its threshold: one filing of bob teaches nothing about carol.
	f.assertDeskPlaced(t, f.lpIngest(t, "carol@example.com"), areas, "a domain does not act on one address's single filing")

	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		rep, err := tx.PlacementReport(ctx)
		if err != nil {
			return err
		}
		m := rep.Origins.ArrivalByBasis[placement.BasisSingle]
		if m[store.PlacementOpen] != 1 {
			t.Errorf("report: single-filing open = %v, want 1 (its own row)", rep.Origins.ArrivalByBasis)
		}
		if rep.ConfidentBasis[placement.KeyAddress][placement.BasisSingle] != 1 {
			t.Errorf("report: confident single-filing addresses = %v, want 1", rep.ConfidentBasis)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A correction of a single-filing placement blocks the key until TWO more
// agreeing filings arrive; one is not enough.
func TestLearnedSortingASingleFilingCorrectionNeedsTwoMoreFilings(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "bob@example.com", 1, areas, "Updates")
	placed := f.lpIngest(t, "bob@example.com")
	if f.lsBasis(t, placed) != "single" {
		t.Fatal("fixture: the placement must be on a single filing")
	}
	f.lpUpdate(t, placed, store.EmailUpdate{RemoveLabelIDs: []ulid.ULID{areas["Updates"].LabelID}})
	if got := f.lpOutcome(t, placed); got != "corrected//true" {
		t.Fatalf("outcome = %s, want corrected", got)
	}
	f.assertDeskPlaced(t, f.lpIngest(t, "bob@example.com"), areas, "right after the correction")

	time.Sleep(10 * time.Millisecond)
	f.lpFile(t, "bob@example.com", 1, areas, "Updates") // ONE newer filing: lifts an ordinary correction, not this one
	f.assertDeskPlaced(t, f.lpIngest(t, "bob@example.com"), areas, "one newer filing after a single-filing correction")

	time.Sleep(10 * time.Millisecond)
	f.lpFile(t, "bob@example.com", 1, areas, "Updates") // the second
	again := f.lpIngest(t, "bob@example.com")
	if n := f.lpLabelBy(t, again, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("two newer agreeing filings: learned = %d, want 1 (the key acts again)", n)
	}
}

// Recency: a mixed key's newest three filings into one area act; two do not.
func TestLearnedSortingRecency(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	// Learning off while the filings are made, so none of them is a correction.
	f.lpSwitch(t, false)
	f.lpFile(t, "alice@example.com", 2, areas, "Promotions")
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	f.lpSwitch(t, true)
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "a run of 2 on a mixed key")

	f.lpSwitch(t, false)
	f.lpFile(t, "alice@example.com", 1, areas, "Updates")
	f.lpSwitch(t, true)
	msg := f.lpIngest(t, "alice@example.com")
	if n := f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Fatalf("a run of 3: learned Updates label = %d, want 1", n)
	}
	if got := f.lsBasis(t, msg); got != "recency" {
		t.Errorf("basis = %q, want recency", got)
	}

	// An interleaved filing breaks the run.
	f.lpSwitch(t, false)
	f.lpFile(t, "dana@example.com", 3, areas, "Updates")
	f.lpFile(t, "dana@example.com", 1, areas, "Promotions")
	f.lpFile(t, "dana@example.com", 2, areas, "Updates")
	f.lpSwitch(t, true)
	f.assertDeskPlaced(t, f.lpIngest(t, "dana@example.com"), areas, "an interleaved filing")
}

// Forget hides older filings and corrections; a later filing counts again.
func TestLearnedSortingForget(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	if n := f.lpLabelBy(t, f.lpIngest(t, "alice@example.com"), areas["Updates"], store.LabelByLearned); n != 1 {
		t.Fatal("fixture: alice must act before Forget")
	}
	time.Sleep(10 * time.Millisecond)
	f.lsForget(t, placement.KeyAddress, "alice@example.com")
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "after Forget")

	time.Sleep(10 * time.Millisecond)
	f.lpFile(t, "alice@example.com", 1, areas, "Updates") // newer than the mark: counts again
	if n := f.lpLabelBy(t, f.lpIngest(t, "alice@example.com"), areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("a filing after Forget: learned = %d, want 1", n)
	}

	// A domain Forget hides the domain's evidence, not an address's own.
	f.lpFile(t, "x1@shop.example", 2, areas, "Promotions")
	f.lpFile(t, "x2@shop.example", 1, areas, "Promotions")
	if n := f.lpLabelBy(t, f.lpIngest(t, "newcomer@shop.example"), areas["Promotions"], store.LabelByLearned); n != 1 {
		t.Fatal("fixture: the domain must act before Forget")
	}
	time.Sleep(10 * time.Millisecond)
	f.lsForget(t, placement.KeyDomain, "shop.example")
	f.assertDeskPlaced(t, f.lpIngest(t, "newcomer2@shop.example"), areas, "after a domain Forget")
	if n := f.lpLabelBy(t, f.lpIngest(t, "x1@shop.example"), areas["Promotions"], store.LabelByLearned); n != 1 {
		t.Errorf("a domain Forget must leave an address's own evidence alone: learned = %d, want 1", n)
	}
}

// Trusted senders: with a trusted authserv-id configured and no
// Authentication-Results at all, an untrusted known sender is refused and a
// trusted one is placed; trust waives nothing else.
func TestLearnedSortingTrustedSenders(t *testing.T) {
	f := newFixture(t)
	f.lpWithTrustedAuthserv(t, "mx.trusted.example")
	areas := f.lpAreas(t)
	f.lpAddIdentity(t, "me@mine.example")
	f.lpFile(t, "robot@example.org", 2, areas, "Updates")
	f.lpFile(t, "boss@mine.example", 2, areas, "Updates")

	f.assertDeskPlaced(t, f.lpIngest(t, "robot@example.org"), areas, "untrusted, no authentication results")
	f.lsTrust(t, "robot@example.org", true)
	if n := f.lpLabelBy(t, f.lpIngest(t, "robot@example.org"), areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("trusted, no authentication results: learned = %d, want 1", n)
	}
	// Not for a sibling address.
	f.lpFile(t, "other@example.org", 2, areas, "Updates")
	f.assertDeskPlaced(t, f.lpIngest(t, "other@example.org"), areas, "a sibling of a trusted address")

	// The trusted header's failure still refuses, even beside a forged pass.
	f.assertDeskPlaced(t, f.lpIngest(t, "robot@example.org",
		"Authentication-Results: evil.example; dmarc=pass header.from=example.org; dkim=pass header.d=example.org",
		"Authentication-Results: mx.trusted.example; dmarc=fail header.from=example.org"),
		areas, "trusted dmarc=fail beside a forged untrusted pass")

	// Trust does not switch off the self-spoof warning (the user's own domain,
	// an unaligned DKIM pass, no DMARC).
	f.lsTrust(t, "boss@mine.example", true)
	f.assertDeskPlaced(t, f.lpIngest(t, "boss@mine.example",
		"Authentication-Results: mx.trusted.example; dkim=pass header.d=other.example"),
		areas, "trusted own-domain address with an unaligned pass")

	// Untrusting puts it back.
	f.lsTrust(t, "robot@example.org", false)
	f.assertDeskPlaced(t, f.lpIngest(t, "robot@example.org"), areas, "after untrust")
}
