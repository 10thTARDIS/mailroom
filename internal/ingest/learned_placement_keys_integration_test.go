// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// AN API KEY'S MOVES NEVER TRAIN LEARNED PLACEMENT (#49, ADR-042 §3a).
//
// The decision: a key may file mail into areas, and what it files
// is never the user's filing evidence. Every path where a person's move
// becomes evidence is exercised here through the real ingest path and the
// real Email/set write (store.ApplyEmailUpdate), once attributed to an API
// key (store.AgentLabelSource, what Email/set sets for every key call) and
// once to the user — the user half is the positive control that proves each
// fixture can observe the evidence the key half must not create:
//
//	filings     a key's area filings are not evidence: no placement, the
//	            report's filing count and verdicts unchanged
//	correction  a key taking a learned label off is no correction (the key is
//	            not blocked) and the placement is WITHDRAWN, never aging into
//	            kept; the user's removal of a label a key took off and
//	            put back is still the user's correction
//	kept        a key archiving a learned placement does not keep it
//	rule        a key taking a rule's label off records no rule correction
//	protection  a key cannot take off a filing the user made by hand
//	explain     a key's label is not read as the arrival's rule label
//
// The API half (that every key call IS attributed to the key) is in
// internal/api/agent_areas_integration_test.go; the send-and-archive half in
// internal/compose/send_archive_integration_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// lpRefileAs is lpRefile (the client's swap) attributed to src.
func (f *fixture) lpRefileAs(t *testing.T, msgID ulid.ULID, areas map[string]store.AreaView, target string, src store.LabelSource) {
	t.Helper()
	if err := f.lpTryRefileAs(msgID, areas, target, src); err != nil {
		t.Fatalf("re-file as %q: %v", src.Kind, err)
	}
}

func (f *fixture) lpTryRefileAs(msgID ulid.ULID, areas map[string]store.AreaView, target string, src store.LabelSource) error {
	ctx := context.Background()
	var remove []ulid.ULID
	for name, a := range areas {
		if name != target {
			remove = append(remove, a.LabelID) // absent labels are a no-op
		}
	}
	return f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, _, err := tx.ApplyEmailUpdate(ctx, msgID, store.EmailUpdate{
			AddLabelIDs: []ulid.ULID{areas[target].LabelID}, RemoveLabelIDs: remove, LabelSource: src})
		return err
	})
}

func (f *fixture) lpReport(t *testing.T) store.PlacementReport {
	t.Helper()
	ctx := context.Background()
	var rep store.PlacementReport
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		rep, e = tx.PlacementReport(ctx)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	return rep
}

func lpAreaCounts(rep store.PlacementReport, area store.AreaView) store.PlacementAreaCounts {
	for _, a := range rep.Areas {
		if a.AreaID == area.ID {
			return a.Counts
		}
	}
	return store.PlacementAreaCounts{}
}

var agent = store.AgentLabelSource()

// Filings. RED PROBE: count applied_by 'agent' in placementFilings (or make
// AgentLabelSource attribute 'user') and the key half places the arrival and
// moves the report's filing count.
func TestKeyFilingsAreNotPlacementEvidence(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	before := f.lpReport(t)

	var keyFiled []ulid.ULID
	for range 3 {
		m := f.lpIngest(t, "alice@example.com")
		f.lpRefileAs(t, m, areas, "Updates", agent)
		keyFiled = append(keyFiled, m)
	}
	for _, m := range keyFiled {
		if n := f.lpLabelBy(t, m, areas["Updates"], store.LabelByAgent); n != 1 {
			t.Fatalf("fixture: the key's filing is not on the message as the key's (%d rows)", n)
		}
	}
	// The key's filings are on the messages (above) and the evidence query
	// counts none of them: the zero is a measurement, not an empty fixture.
	f.lpWantFilings(t, "alice@example.com", areas["Updates"], 0)
	after := f.lpReport(t)
	if after.Filings != before.Filings {
		t.Errorf("report filings %d -> %d after three key filings, want unchanged", before.Filings, after.Filings)
	}
	if got := after.KeyReasons[placement.KeyAddress][placement.Confident]; got != before.KeyReasons[placement.KeyAddress][placement.Confident] {
		t.Errorf("confident address keys %d -> %d after key filings, want unchanged",
			before.KeyReasons[placement.KeyAddress][placement.Confident], got)
	}
	arrival, raw := f.lpIngestRaw(t, "alice@example.com")
	f.assertDeskPlaced(t, arrival, areas, "three key filings into Updates")
	if ex := f.lpExplainRaw(t, arrival, raw, nil, false); ex.Trace.Decision.Act {
		t.Errorf("explain: the key's filings were read as evidence (%+v)", ex.Trace.Evidence)
	}

	// Positive control: the very same moves by the user teach Updates. With
	// learning off while they are made, so the third is not learned into
	// Updates before the user files it (a filing onto a learned label writes
	// no row of its own).
	f.lpSwitch(t, false)
	for range 3 {
		f.lpRefileAs(t, f.lpIngest(t, "bob@example.com"), areas, "Updates", store.LabelSource{})
	}
	f.lpSwitch(t, true)
	f.lpWantFilings(t, "bob@example.com", areas["Updates"], 3)
	if got := f.lpReport(t).Filings; got != before.Filings+3 {
		t.Errorf("positive control: report filings = %d after three user filings, want %d", got, before.Filings+3)
	}
	if n := f.lpLabelBy(t, f.lpIngest(t, "bob@example.com"), areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("positive control: three user filings must teach Updates, got %d learned labels", n)
	}
}

// Correction and kept-by-age. RED PROBES: drop the byAgent case in
// ApplyEmailUpdate's learned-label switch and the key's removal is a
// correction (the next arrival is desk-placed, outcome corrected); record
// nothing for it instead (no recordLearnedWithdrawal) and the aged placement
// counts as kept.
func TestKeyRemovingALearnedLabelWithdrawsThePlacement(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	learned := f.lpIngest(t, "alice@example.com")
	if f.lpLabelBy(t, learned, areas["Updates"], store.LabelByLearned) != 1 {
		t.Fatal("fixture: alice's third message must be learned into Updates")
	}
	f.lpRefileAs(t, learned, areas, "Promotions", agent)
	if got := f.lpOutcome(t, learned); got != "withdrawn//false" {
		t.Errorf("placement after a key re-filed it = %s, want withdrawn//false", got)
	}
	next := f.lpIngest(t, "alice@example.com")
	if n := f.lpLabelBy(t, next, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("after a key's re-file the sender's key must still act (no correction): learned = %d, want 1", n)
	}
	// Aged past KeptAfter, the withdrawn placement is still not kept.
	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.learned_placements SET decided_at = now() - interval '30 days' WHERE message_id=$1`, learned.String()); err != nil {
		t.Fatal(err)
	}
	c := lpAreaCounts(f.lpReport(t), areas["Updates"])
	if c.Kept != 0 || c.Corrected != 0 || c.Withdrawn != 1 || c.Open != 1 {
		t.Errorf("Updates counts = kept %d corrected %d withdrawn %d open %d, want 0, 0, 1, 1 (the next arrival is open)",
			c.Kept, c.Corrected, c.Withdrawn, c.Open)
	}

	// Positive control: the user's own re-file is a correction that blocks.
	f.lpFile(t, "bob@example.com", 2, areas, "Updates")
	bob := f.lpIngest(t, "bob@example.com")
	if f.lpLabelBy(t, bob, areas["Updates"], store.LabelByLearned) != 1 {
		t.Fatal("fixture: bob's third message must be learned into Updates")
	}
	f.lpRefileAs(t, bob, areas, "Promotions", store.LabelSource{})
	if got := f.lpOutcome(t, bob); got != "corrected//true" {
		t.Errorf("positive control: the user's re-file = %s, want corrected//true", got)
	}
	f.assertDeskPlaced(t, f.lpIngest(t, "bob@example.com"), areas, "after the user's correction")
}

// Kept on archive. RED PROBE: drop !byAgent from the keepLearnedOnArchive
// condition and the key's archive keeps the placement.
func TestKeyArchiveDoesNotKeepALearnedPlacement(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	byKey := f.lpIngest(t, "alice@example.com")
	byUser := f.lpIngest(t, "alice@example.com")
	for _, m := range []ulid.ULID{byKey, byUser} {
		if f.lpLabelBy(t, m, areas["Updates"], store.LabelByLearned) != 1 {
			t.Fatal("fixture: both arrivals must be learned into Updates")
		}
	}
	f.lpUpdate(t, byKey, store.EmailUpdate{MoveToRole: "archive", LabelSource: agent})
	if got := f.lpOutcome(t, byKey); got != "open//false" {
		t.Errorf("after a key archived it = %s, want open//false (the user did not act on it)", got)
	}
	f.lpUpdate(t, byUser, store.EmailUpdate{MoveToRole: "archive"})
	if got := f.lpOutcome(t, byUser); got != "kept/archive/false" {
		t.Errorf("positive control: after the user archived it = %s, want kept/archive/false", got)
	}
}

// Rule corrections. RED PROBE: drop !byAgent from the recordRuleCorrection
// condition and the key's removal records a correction.
func TestKeyRemovingARuleLabelIsNotACorrection(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	areas := f.lpAreas(t)
	var ruleID ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var err error
		ruleID, err = tx.CreateRule(ctx, store.RuleRow{
			Name: "never-matches", Enabled: true,
			Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "nobody@nowhere.example"},
			Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: areas["Forums"].LabelID.String()}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	corrections := func() int {
		return f.scalar(t, `SELECT count(*) FROM app.rule_corrections WHERE rule_id=$1`, ruleID.String())
	}
	label := []ulid.ULID{areas["Forums"].LabelID}
	for _, c := range []struct {
		who  string
		src  store.LabelSource
		want int
	}{{"a key", agent, 0}, {"the user (positive control)", store.LabelSource{}, 1}} {
		m := f.lpIngest(t, "carol@example.com")
		f.lpUpdate(t, m, store.EmailUpdate{AddLabelIDs: label, LabelSource: store.RuleLabelSource(ruleID)})
		if f.lpLabelBy(t, m, areas["Forums"], store.LabelByRule) != 1 {
			t.Fatal("fixture: the rule's label is not on the message")
		}
		before := corrections()
		f.lpUpdate(t, m, store.EmailUpdate{RemoveLabelIDs: label, LabelSource: c.src})
		if got := corrections() - before; got != c.want {
			t.Errorf("%s took the rule's label off: %d corrections recorded, want %d", c.who, got, c.want)
		}
	}
}

// Protection. RED PROBE: drop the userFiledAreaLabels check in
// ApplyEmailUpdate and the key's re-file succeeds, deleting a filing.
func TestKeyCannotTakeOffAFilingTheUserMade(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	m := f.lpIngest(t, "alice@example.com")
	f.lpRefileAs(t, m, areas, "Promotions", store.LabelSource{})
	before := f.lpReport(t).Filings
	changes := f.changeCount(t, "Email", "updated")

	err := f.lpTryRefileAs(m, areas, "Updates", agent)
	var protected *store.UserFilingProtectedError
	if !errors.As(err, &protected) || len(protected.LabelIDs) != 1 || protected.LabelIDs[0] != areas["Promotions"].LabelID {
		t.Fatalf("a key re-filing the user's own filing: err = %v, want UserFilingProtectedError naming Promotions", err)
	}
	if f.lpLabelBy(t, m, areas["Promotions"], store.LabelByUser) != 1 || f.lpLabelBy(t, m, areas["Updates"], store.LabelByAgent) != 0 {
		t.Error("the refused re-file changed the message's labels")
	}
	if got := f.lpReport(t).Filings; got != before {
		t.Errorf("report filings %d -> %d after the refused re-file", before, got)
	}
	if got := f.changeCount(t, "Email", "updated"); got != changes {
		t.Errorf("the refused re-file appended %d change rows", got-changes)
	}

	// A key MAY take off what an automation put there (the desk's label):
	// the protection is the user's own filing, not area labels as such.
	d := f.lpIngest(t, "dave@example.com")
	if f.lpDeskLabels(t, d) == 0 {
		t.Fatal("fixture: the arrival carries no desk label")
	}
	f.lpRefileAs(t, d, areas, "Updates", agent)
	if f.lpDeskLabels(t, d) != 0 || f.lpLabelBy(t, d, areas["Updates"], store.LabelByAgent) != 1 {
		t.Error("control: a key's re-file of desk-placed mail did not swap the labels")
	}
	// And the user may re-file their own filing (the protection is the key's).
	f.lpRefileAs(t, m, areas, "Updates", store.LabelSource{})
	if f.lpLabelBy(t, m, areas["Updates"], store.LabelByUser) != 1 {
		t.Error("control: the user's own re-file was refused")
	}
}

// Explain. RED PROBE: take LabelByAgent out of postArrivalSources and the
// key's label is read as an arrival rule label (Current.ByLabel, "placed"
// becomes the already-placed refusal).
func TestPlacementExplainIgnoresAKeysLabel(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	f.lpSwitch(t, false) // the arrival is left to the desk, so the key's label is its only area label
	msg, raw := f.lpIngestRaw(t, "alice@example.com")
	f.lpRefileAs(t, msg, areas, "Promotions", agent)
	f.lpSwitch(t, true)
	ex := f.lpExplainRaw(t, msg, raw, nil, false)
	if ex.Trace.Current.ByLabel {
		t.Errorf("explain read the key's label as the arrival's: current %+v", ex.Trace.Current)
	}
	if got := ex.Trace.Result.Label(); got != "placed" {
		t.Errorf("explain = %s, want placed (two user filings into Updates)", got)
	}

	// Positive control: a RULE's label on the same kind of arrival is read
	// as the arrival's own, so the explain can see a label when it should.
	f.lpSwitch(t, false)
	ruled, rawRuled := f.lpIngestRaw(t, "alice@example.com")
	f.lpUpdate(t, ruled, store.EmailUpdate{AddLabelIDs: []ulid.ULID{areas["Promotions"].LabelID},
		LabelSource: store.LabelSource{Kind: store.LabelByRule}})
	f.lpSwitch(t, true)
	if ex := f.lpExplainRaw(t, ruled, rawRuled, nil, false); !ex.Trace.Current.ByLabel {
		t.Errorf("positive control: a rule's label is not read as the arrival's (current %+v)", ex.Trace.Current)
	}
}

// A key's re-add does not blind the user (#49 K2 review). A key takes a
// learned label off (the placement is withdrawn, the 'learned' row deleted),
// then puts the same label back (an 'agent' row in its place); the user's
// later removal of that label is still learned placement's correction.
// RED PROBE: drop the LabelByAgent case from ApplyEmailUpdate's learned-label
// switch and the placement stays withdrawn, the sender's key unblocked.
func TestUserCorrectionAfterAKeyReaddsALearnedLabel(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	updates := []ulid.ULID{areas["Updates"].LabelID}
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	learned := f.lpIngest(t, "alice@example.com")
	if f.lpLabelBy(t, learned, areas["Updates"], store.LabelByLearned) != 1 {
		t.Fatal("fixture: alice's third message must be learned into Updates")
	}
	f.lpUpdate(t, learned, store.EmailUpdate{RemoveLabelIDs: updates, LabelSource: agent})
	if got := f.lpOutcome(t, learned); got != "withdrawn//false" {
		t.Fatalf("fixture: after the key took the learned label off = %s, want withdrawn//false", got)
	}
	f.lpUpdate(t, learned, store.EmailUpdate{AddLabelIDs: updates, LabelSource: agent})
	if f.lpLabelBy(t, learned, areas["Updates"], store.LabelByAgent) != 1 {
		t.Fatal("fixture: the key's re-add is not on the message as the key's")
	}
	f.lpUpdate(t, learned, store.EmailUpdate{RemoveLabelIDs: updates})
	if got := f.lpOutcome(t, learned); got != "corrected//true" {
		t.Errorf("after the user took the key's re-added label off = %s, want corrected//true", got)
	}
	c := lpAreaCounts(f.lpReport(t), areas["Updates"])
	if c.Corrected != 1 || c.Withdrawn != 0 {
		t.Errorf("Updates counts = corrected %d withdrawn %d, want 1, 0 (the correction is counted)", c.Corrected, c.Withdrawn)
	}
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "after the user's correction of the key's re-add")

	// Control: a label a key added to a message with NO learned placement,
	// taken off by the user, records nothing.
	carol := f.lpIngest(t, "carol@elsewhere.example")
	if f.lpPlacements(t, carol) != 0 {
		t.Fatal("fixture: carol's arrival must have no learned placement")
	}
	f.lpUpdate(t, carol, store.EmailUpdate{AddLabelIDs: updates, LabelSource: agent})
	f.lpUpdate(t, carol, store.EmailUpdate{RemoveLabelIDs: updates})
	if n := f.lpPlacements(t, carol); n != 0 {
		t.Errorf("control: the user's removal of a key's label wrote %d placement rows, want 0", n)
	}

	// Control: a key's label for ANOTHER area, on a message with an open
	// learned placement, taken off by the user, leaves the placement open.
	f.lpFile(t, "erin@another.example", 2, areas, "Updates")
	erin := f.lpIngest(t, "erin@another.example")
	if f.lpLabelBy(t, erin, areas["Updates"], store.LabelByLearned) != 1 {
		t.Fatal("fixture: erin's third message must be learned into Updates")
	}
	forums := []ulid.ULID{areas["Forums"].LabelID}
	f.lpUpdate(t, erin, store.EmailUpdate{AddLabelIDs: forums, LabelSource: agent})
	f.lpUpdate(t, erin, store.EmailUpdate{RemoveLabelIDs: forums})
	if got := f.lpOutcome(t, erin); got != "open//false" {
		t.Errorf("control: the user took off a key's Forums label = %s, want open//false (not the placement's area)", got)
	}

	// Control: the plain path is unchanged. The user taking the learned
	// label itself off corrects the placement.
	f.lpUpdate(t, erin, store.EmailUpdate{RemoveLabelIDs: updates})
	if got := f.lpOutcome(t, erin); got != "corrected//true" {
		t.Errorf("control: the user took the learned label off = %s, want corrected//true", got)
	}
}
