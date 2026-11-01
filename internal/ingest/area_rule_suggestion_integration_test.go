// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// "ALWAYS PUT MAIL FROM X IN <AREA>?" (store.RuleSuggestion) end to end. It is
// tested here, beside learned placement, because the whole point is that the
// offer is made only when learned placement is NOT going to do the job, and
// that is only meaningful against the real ingest path and the real Re-file
// write (store.ApplyEmailUpdate), never hand-written label rows.
//
// Every "no suggestion" assertion is paired with a "suggestion" control built
// from the same fixture, so a test cannot pass because the method always says
// no.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/mime"
	"mailroom/internal/placement"
	"mailroom/internal/rules"
	"mailroom/internal/store"
)

const rsSender = "alice@example.com"

// rsSenderFunc is the sender source the API gives the store, over this
// fixture's blob store.
func (f *fixture) rsSenderFunc(trusted ...string) store.RuleSuggestionSenderFunc {
	return func(ctx context.Context, c store.ExplainCandidate) (placement.Sender, error) {
		raw, err := readCandidateBytes(ctx, f.ing.blobs, f.userID, f.dek, c.ContentHash)
		if err != nil {
			return placement.Sender{}, err
		}
		parsed, err := mime.ParseBytes(raw)
		if err != nil {
			return placement.Sender{}, err
		}
		return LearnedSender(c.From, parsed, trusted, c.ReceivedAt), nil
	}
}

func (f *fixture) rsAsk(t *testing.T, area store.AreaView, address string, trusted ...string) store.RuleSuggestion {
	t.Helper()
	ctx := context.Background()
	var out store.RuleSuggestion
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var err error
		out, err = tx.RuleSuggestion(ctx, area.ID, address, f.rsSenderFunc(trusted...))
		return err
	}); err != nil {
		t.Fatalf("RuleSuggestion: %v", err)
	}
	return out
}

func (f *fixture) rsWant(t *testing.T, got store.RuleSuggestion, suggest bool, reason string, filings int) {
	t.Helper()
	if got.Suggest != suggest || got.Reason != reason || got.Filings != filings {
		t.Fatalf("RuleSuggestion = {suggest:%v reason:%q filings:%d}, want {suggest:%v reason:%q filings:%d}",
			got.Suggest, got.Reason, got.Filings, suggest, reason, filings)
	}
}

// rsMixedSeed files `n` of alice's messages into Updates and one into
// Promotions, with learned placement OFF while seeding (as
// TestLearnedPlacementMixedEvidenceAbstains does: with it on, a later Re-file
// of a learned message would be a correction and change the evidence), and
// returns the areas. The evidence is mixed, so learned placement abstains.
func (f *fixture) rsMixedSeed(t *testing.T, n int) map[string]store.AreaView {
	t.Helper()
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, rsSender, 1, areas, "Promotions")
	f.lpFile(t, rsSender, n, areas, "Updates")
	f.lpSwitch(t, true)
	f.lpWantFilings(t, rsSender, areas["Updates"], n)
	f.lpWantFilings(t, rsSender, areas["Promotions"], 1)
	return areas
}

// backdate moves the Updates filings of the sender's messages into the past.
func (f *fixture) rsBackdateFilings(t *testing.T, area store.AreaView, days int) {
	t.Helper()
	tag, err := f.super.Exec(context.Background(), `
		UPDATE app.message_labels ml SET created_at = now() - make_interval(days => $4)
		  FROM app.messages m
		 WHERE m.id = ml.message_id AND m.user_id = $1 AND m.from_address = $2 AND ml.label_id = $3`,
		f.userID.String(), rsSender, area.LabelID.String(), days)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatal("fixture: backdated no filings")
	}
}

// TestRuleSuggestionThreshold: three of the user's own recent filings earn the
// offer; two do not.
func TestRuleSuggestionThreshold(t *testing.T) {
	f := newFixture(t)
	areas := f.rsMixedSeed(t, 2)
	// Two Updates filings, one Promotions: mixed evidence, but only two filings
	// into Updates.
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), false, store.RuleReasonFewFilings, 2)

	// The third filing earns it, and the reason is why learned placement will
	// not do it: the evidence is mixed.
	f.lpSwitch(t, false)
	f.lpFile(t, rsSender, 1, areas, "Updates")
	f.lpSwitch(t, true)
	f.lpWantFilings(t, rsSender, areas["Updates"], 3)
	got := f.rsAsk(t, areas["Updates"], rsSender)
	f.rsWant(t, got, true, string(placement.Mixed), 3)
	if got.Address != rsSender || got.AreaID != areas["Updates"].ID {
		t.Errorf("answer names (%s, %s), want (%s, %s)", got.Address, got.AreaID, rsSender, areas["Updates"].ID)
	}
	// The address is matched case-insensitively, as the evidence is.
	f.rsWant(t, f.rsAsk(t, areas["Updates"], " Alice@Example.com "), true, string(placement.Mixed), 3)
	// And it is about THIS area: Promotions has one filing.
	f.rsWant(t, f.rsAsk(t, areas["Promotions"], rsSender), false, store.RuleReasonFewFilings, 1)
}

// TestRuleSuggestionOnlyCountsRecentFilings: filings older than 30 days no
// longer count, and the user's own filings are the only thing that does.
func TestRuleSuggestionOnlyCountsRecentFilings(t *testing.T) {
	f := newFixture(t)
	areas := f.rsMixedSeed(t, 3)
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), true, string(placement.Mixed), 3)

	f.rsBackdateFilings(t, areas["Updates"], 29)
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), true, string(placement.Mixed), 3)
	f.rsBackdateFilings(t, areas["Updates"], 31)
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), false, store.RuleReasonFewFilings, 0)

	// A filing made by something other than the user is not the user's decision.
	f.rsBackdateFilings(t, areas["Updates"], 1)
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), true, string(placement.Mixed), 3)
	if _, err := f.super.Exec(context.Background(), `
		UPDATE app.message_labels ml SET applied_by = 'rule'
		  FROM app.messages m
		 WHERE m.id = ml.message_id AND m.user_id = $1 AND m.from_address = $2 AND ml.label_id = $3`,
		f.userID.String(), rsSender, areas["Updates"].LabelID.String()); err != nil {
		t.Fatal(err)
	}
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), false, store.RuleReasonFewFilings, 0)
}

// TestRuleSuggestionNotWhenARuleCoversIt: an ENABLED rule that names the sender
// and files into the area suppresses the offer; a disabled rule, a rule for
// another sender, and a rule filing elsewhere do not.
func TestRuleSuggestionNotWhenARuleCoversIt(t *testing.T) {
	f := newFixture(t)
	areas := f.rsMixedSeed(t, 3)
	ctx := context.Background()
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), true, string(placement.Mixed), 3)

	mk := func(name, from string, enabled bool, area store.AreaView) ulid.ULID {
		t.Helper()
		var id ulid.ULID
		if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
			var err error
			id, err = tx.CreateRule(ctx, store.RuleRow{
				Name: name, Priority: 100, Enabled: enabled,
				Conditions: rules.Condition{All: []rules.Condition{{Field: rules.FieldFrom, Op: rules.OpEquals, Value: from}}},
				Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: area.LabelID.String()}},
			})
			return err
		}); err != nil {
			t.Fatalf("create rule: %v", err)
		}
		return id
	}
	mk("other sender", "bob@example.com", true, areas["Updates"])
	mk("other area", rsSender, true, areas["Forums"])
	off := mk("disabled", rsSender, false, areas["Updates"])
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), true, string(placement.Mixed), 3)

	// Enabling the covering rule is the one change that silences it.
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		on := true
		_, err := tx.UpdateRule(ctx, off, store.RulePatch{Enabled: &on})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), false, store.RuleReasonCovered, 3)
}

// TestRuleSuggestionDismissal: "Not now" silences the offer for 30 days, a
// re-dismissal refreshes that, and one user's dismissal is invisible to another.
func TestRuleSuggestionDismissal(t *testing.T) {
	f := newFixture(t)
	areas := f.rsMixedSeed(t, 3)
	ctx := context.Background()
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), true, string(placement.Mixed), 3)

	dismiss := func(fx *fixture, area store.AreaView, addr string) bool {
		t.Helper()
		var found bool
		if err := fx.st.UserTx(ctx, fx.userID, func(tx *store.UserTx) error {
			var err error
			found, err = tx.DismissRuleSuggestion(ctx, area.ID, addr)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return found
	}
	if !dismiss(f, areas["Updates"], "Alice@example.com") {
		t.Fatal("dismissing against the user's own area was refused")
	}
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), false, store.RuleReasonDismissed, 3)

	// Backdated past the window, the offer is back.
	if _, err := f.super.Exec(ctx, `UPDATE app.rule_suggestion_dismissals SET dismissed_at = now() - interval '31 days' WHERE user_id = $1`,
		f.userID.String()); err != nil {
		t.Fatal(err)
	}
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), true, string(placement.Mixed), 3)

	// Dismissing again refreshes the time (an upsert, not a second row).
	dismiss(f, areas["Updates"], rsSender)
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), false, store.RuleReasonDismissed, 3)
	if n := f.scalar(t, `SELECT count(*) FROM app.rule_suggestion_dismissals WHERE user_id = $1`, f.userID.String()); n != 1 {
		t.Errorf("dismissal rows = %d, want 1", n)
	}

	// A dismissal of another sender, or in another area, does not silence this one.
	g := newFixture(t)
	gAreas := g.rsMixedSeed(t, 3)
	g.rsWant(t, g.rsAsk(t, gAreas["Updates"], rsSender), true, string(placement.Mixed), 3)
	dismiss(g, gAreas["Updates"], "bob@example.com")
	dismiss(g, gAreas["Forums"], rsSender)
	g.rsWant(t, g.rsAsk(t, gAreas["Updates"], rsSender), true, string(placement.Mixed), 3)

	// RLS: user G cannot dismiss against F's area, and F's dismissal is
	// unchanged by the attempt; asking about F's area is the same answer as for
	// an area that does not exist.
	before := f.scalar(t, `SELECT count(*) FROM app.rule_suggestion_dismissals WHERE area_id = $1`, areas["Updates"].ID.String())
	if dismiss(g, areas["Updates"], rsSender) {
		t.Error("user G's dismissal against user F's area was accepted")
	}
	if after := f.scalar(t, `SELECT count(*) FROM app.rule_suggestion_dismissals WHERE area_id = $1`, areas["Updates"].ID.String()); after != before {
		t.Errorf("F's area dismissals changed from %d to %d", before, after)
	}
	if got := g.rsAsk(t, areas["Updates"], rsSender); got.Suggest || got.Reason != store.RuleReasonNoArea {
		t.Errorf("G asking about F's area = %+v, want noArea", got)
	}
}

// TestRuleSuggestionNotWhenLearnedPlacementWillDoIt: when the evidence is
// consistent and the sender authentic, learned placement files the next message
// into the area itself, so no rule is offered; each way it would NOT is
// reported as the reason for offering one.
func TestRuleSuggestionNotWhenLearnedPlacementWillDoIt(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, rsSender, 3, areas, "Updates")
	f.lpSwitch(t, true)
	f.lpWantFilings(t, rsSender, areas["Updates"], 3)

	// Control for the whole test: the real ingest path DOES place the next
	// message, so "learned would act" is a fact about this fixture.
	next := f.lpIngest(t, rsSender)
	if n := f.lpLabelBy(t, next, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Fatalf("fixture: learned placement did not place the sender's next message (%d)", n)
	}
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), false, store.RuleReasonLearned, 3)

	// Switched off: learned placement will not, so the offer is made.
	f.lpSwitch(t, false)
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), true, store.RuleReasonOff, 3)
	f.lpSwitch(t, true)
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), false, store.RuleReasonLearned, 3)

	// The area's correction gate stopped.
	f.lpSeedStoppedGate(t, areas["Updates"])
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender), true, store.RuleReasonGated, 3)
}

// TestRuleSuggestionSenderChecks: learned placement refuses a sender whose
// latest message does not authenticate, so the user still gets the offer.
func TestRuleSuggestionSenderChecks(t *testing.T) {
	f := newFixture(t)
	f.lpWithTrustedAuthserv(t, "mx.trusted.example")
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, rsSender, 3, areas, "Updates")
	f.lpSwitch(t, true)
	f.lpWantFilings(t, rsSender, areas["Updates"], 3)

	// With trusted authserv-ids configured, the filed messages carry no trusted
	// pass, so the sender is unauthenticated.
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender, "mx.trusted.example"), true, string(placement.Unauthenticated), 3)

	// A newer message with a trusted, aligned DMARC pass authenticates the
	// sender: learned placement would act, so no offer. (The message is placed
	// by learned placement at ingest, which writes no user filing.)
	f.lpIngest(t, rsSender,
		"Authentication-Results: mx.trusted.example; spf=pass; dkim=pass header.d=example.com; dmarc=pass header.from=example.com")
	f.rsWant(t, f.rsAsk(t, areas["Updates"], rsSender, "mx.trusted.example"), false, store.RuleReasonLearned, 3)

	// And an unreadable sender source never produces an offer on a guess.
	var got store.RuleSuggestion
	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var err error
		got, err = tx.RuleSuggestion(ctx, areas["Updates"].ID, rsSender, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.rsWant(t, got, false, store.RuleReasonUnverified, 3)
}

// TestRuleSuggestionEarlierCount: `earlier` is the live messages from the
// sender that are not in the area: not trashed, not junk, not a draft.
func TestRuleSuggestionEarlierCount(t *testing.T) {
	f := newFixture(t)
	areas := f.rsMixedSeed(t, 3)
	// rsMixedSeed filed one message into Promotions: that one is "earlier".
	f.lpSwitch(t, false)
	f.lpIngest(t, rsSender)
	trashed := f.lpIngest(t, rsSender)
	junk := f.lpIngest(t, rsSender)
	f.lpIngest(t, "bob@example.com") // someone else's
	f.lpSwitch(t, true)
	if _, err := f.super.Exec(context.Background(), `UPDATE app.messages SET trashed_at = now() WHERE id = $1`, trashed.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.super.Exec(context.Background(), `UPDATE app.messages SET is_junk = true WHERE id = $1`, junk.String()); err != nil {
		t.Fatal(err)
	}
	got := f.rsAsk(t, areas["Updates"], rsSender)
	f.rsWant(t, got, true, string(placement.Mixed), 3)
	if got.Earlier != 2 {
		t.Errorf("earlier = %d, want 2 (the Promotions message and the plain one; not the trashed, junk or other sender's)", got.Earlier)
	}
	// From the Promotions side, the three Updates messages are the earlier ones.
	if got := f.rsAsk(t, areas["Promotions"], rsSender); got.Earlier != 4 {
		t.Errorf("earlier (Promotions) = %d, want 4", got.Earlier)
	}
}
