// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// #859: a collapse carries WHO applied each label, and the report/repair for the
// collapses that ran before it did.
//
// The failure these guard is silent by construction: a learned label arriving
// on the keeper as 'user' renders exactly the same label in every client. Only
// the learners notice — by learning from a filing nobody made.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

type provFixture struct {
	t      *testing.T
	h      *harness
	u      fixtureUser
	userID ulid.ULID
}

func newProvFixture(t *testing.T) *provFixture {
	h := newHarness(t)
	u := seedUser(t, h.super, "prov-"+NewULID().String())
	return &provFixture{t: t, h: h, u: u, userID: mustULID(t, u.ID)}
}

func (f *provFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.h.super.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("seed: %v\n%s", err, sql)
	}
}

func (f *provFixture) label(name string) string {
	id := NewULID().String()
	f.exec(`INSERT INTO app.labels (id, user_id, name) VALUES ($1,$2,$3)`, id, f.u.ID, name+"-"+id)
	return id
}

func (f *provFixture) rule() string {
	id := NewULID().String()
	f.exec(`INSERT INTO app.rules (id, user_id, name, conditions, actions) VALUES ($1,$2,$3,'{}','[]')`,
		id, f.u.ID, "rule-"+id)
	return id
}

// put writes a label row with an explicit attribution and timestamp.
func (f *provFixture) put(msg ulid.ULID, label, by string, ruleID *string, at time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by, applied_by_rule_id, created_at)
	        VALUES ($1,$2,$3,$4,$5,$6)`, f.u.ID, msg.String(), label, by, ruleID, at)
}

type labelRow struct {
	by     string
	ruleID *string
	at     time.Time
}

func (f *provFixture) row(msg ulid.ULID, label string) (labelRow, bool) {
	f.t.Helper()
	var r labelRow
	err := f.h.super.QueryRow(context.Background(),
		`SELECT applied_by, applied_by_rule_id, created_at FROM app.message_labels
		  WHERE user_id=$1 AND message_id=$2 AND label_id=$3`,
		f.u.ID, msg.String(), label).Scan(&r.by, &r.ruleID, &r.at)
	if err != nil {
		if isNoRows(err) {
			return r, false
		}
		f.t.Fatal(err)
	}
	return r, true
}

func (f *provFixture) collapse(keeper ulid.ULID, losers ...ulid.ULID) CollapseResult {
	f.t.Helper()
	var res CollapseResult
	var ok bool
	if err := f.h.appStore().UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var e error
		res, ok, e = tx.CollapseDuplicateGroup(context.Background(), keeper, losers)
		return e
	}); err != nil {
		f.t.Fatalf("collapse: %v", err)
	}
	if !ok {
		f.t.Fatal("collapse refused a group with nothing blocking it")
	}
	return res
}

// past is a fixed, distinctive timestamp: the carried created_at must be the
// LOSER's, not the collapse's now().
var past = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)

func TestCollapseCarriesALearnedLabelAsLearned(t *testing.T) {
	f := newProvFixture(t)
	keeper, loser := dupPair(t, f.h, f.u, "prov-learned")
	lid := f.label("area")
	f.put(loser, lid, LabelByLearned, nil, past)

	res := f.collapse(keeper, loser)

	r, ok := f.row(keeper, lid)
	if !ok {
		t.Fatal("the label was lost")
	}
	if r.by != LabelByLearned {
		t.Errorf("keeper applied_by = %q, want %q — a learned placement became the user's filing (#859)", r.by, LabelByLearned)
	}
	if !r.at.Equal(past) {
		t.Errorf("keeper created_at = %s, want the loser's %s", r.at, past)
	}
	if res.LabelsUnioned != 1 || res.LabelsUpgraded != 0 {
		t.Errorf("unioned/upgraded = %d/%d, want 1/0", res.LabelsUnioned, res.LabelsUpgraded)
	}
}

// Positive control: carrying provenance must not demote a real filing.
func TestCollapseCarriesAUserLabelAsUser(t *testing.T) {
	f := newProvFixture(t)
	keeper, loser := dupPair(t, f.h, f.u, "prov-user")
	lid := f.label("filed")
	f.put(loser, lid, LabelByUser, nil, past)

	f.collapse(keeper, loser)

	if r, ok := f.row(keeper, lid); !ok || r.by != LabelByUser {
		t.Errorf("keeper row = %+v (present %v), want applied_by user", r, ok)
	}
}

func TestCollapseCarriesARulesLabelWithItsRule(t *testing.T) {
	f := newProvFixture(t)
	keeper, loser := dupPair(t, f.h, f.u, "prov-rule")
	lid := f.label("ruled")
	rid := f.rule()
	f.put(loser, lid, LabelByRule, &rid, past)

	f.collapse(keeper, loser)

	r, ok := f.row(keeper, lid)
	if !ok || r.by != LabelByRule || r.ruleID == nil || *r.ruleID != rid {
		t.Errorf("keeper row = %+v (present %v), want rule %s", r, ok, rid)
	}
}

// Conflict: the keeper's automation row loses to the copy's real filing.
func TestCollapseUserFilingOnTheCopyBeatsKeepersLearnedRow(t *testing.T) {
	f := newProvFixture(t)
	keeper, loser := dupPair(t, f.h, f.u, "prov-up")
	lid := f.label("area")
	f.put(keeper, lid, LabelByLearned, nil, time.Now().UTC())
	f.put(loser, lid, LabelByUser, nil, past)

	res := f.collapse(keeper, loser)

	r, _ := f.row(keeper, lid)
	if r.by != LabelByUser {
		t.Errorf("keeper applied_by = %q, want user — the person's filing was dropped by subtraction", r.by)
	}
	if !r.at.Equal(past) {
		t.Errorf("created_at = %s, want the filing's own time %s", r.at, past)
	}
	if res.LabelsUnioned != 0 || res.LabelsUpgraded != 1 {
		t.Errorf("unioned/upgraded = %d/%d, want 0/1", res.LabelsUnioned, res.LabelsUpgraded)
	}
}

// Conflict the other way: the keeper's filing is never downgraded.
func TestCollapseKeepersUserFilingBeatsCopysRuleRow(t *testing.T) {
	f := newProvFixture(t)
	keeper, loser := dupPair(t, f.h, f.u, "prov-keep")
	lid := f.label("filed")
	rid := f.rule()
	keptAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f.put(keeper, lid, LabelByUser, nil, keptAt)
	f.put(loser, lid, LabelByRule, &rid, past)

	res := f.collapse(keeper, loser)

	r, _ := f.row(keeper, lid)
	if r.by != LabelByUser || r.ruleID != nil || !r.at.Equal(keptAt) {
		t.Errorf("keeper row = %+v, want the untouched user row at %s", r, keptAt)
	}
	if res.LabelsUpgraded != 0 {
		t.Errorf("LabelsUpgraded = %d, want 0", res.LabelsUpgraded)
	}
}

// Two copies carrying the same label: one statement must pick ONE candidate
// (ON CONFLICT DO UPDATE refuses to touch a row twice), and the strongest wins.
func TestCollapseTwoLosersSameLabelStrongestWins(t *testing.T) {
	f := newProvFixture(t)
	keeper, loser := dupPair(t, f.h, f.u, "prov-two")
	// A third copy of the same message, the same way dupPair builds them.
	var dedup []byte
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT dedup_hash FROM app.messages WHERE id=$1`, keeper.String()).Scan(&dedup); err != nil {
		t.Fatal(err)
	}
	third := NewULID()
	hash := []byte("prov-two-third-hash-0000000000000")[:32]
	f.exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1,$2,1,1,$3)`,
		f.u.ID, hash, NewULID().String())
	f.exec(`INSERT INTO app.messages (id, user_id, content_hash, subject, snippet, from_name, from_address,
	          received_at, size_bytes, message_id_hint, dedup_hash)
	        VALUES ($1,$2,$3,'dup','p','S','s@example.com',now(),1,'<prov-two@example.com>',$4)`,
		third.String(), f.u.ID, hash, dedup)

	lid := f.label("area")
	f.put(loser, lid, LabelByDesk, nil, past)
	f.put(third, lid, LabelByUser, nil, past.Add(time.Hour))

	res := f.collapse(keeper, loser, third)

	if r, _ := f.row(keeper, lid); r.by != LabelByUser {
		t.Errorf("keeper applied_by = %q, want user", r.by)
	}
	if res.LabelsUnioned != 1 {
		t.Errorf("LabelsUnioned = %d, want 1", res.LabelsUnioned)
	}
}

// The dry run's count of upgrades must agree with what the collapse does.
func TestDuplicateCollapseCandidatesCountsUpgrades(t *testing.T) {
	f := newProvFixture(t)
	keeper, loser := dupPair(t, f.h, f.u, "prov-cand")
	up, same, down := f.label("up"), f.label("same"), f.label("down")
	f.put(keeper, up, LabelByDesk, nil, past)
	f.put(loser, up, LabelByUser, nil, past)
	f.put(keeper, same, LabelByRule, nil, past)
	f.put(loser, same, LabelByRule, nil, past)
	f.put(keeper, down, LabelByUser, nil, past)
	f.put(loser, down, LabelByLearned, nil, past)

	var groups []CollapseCandidate
	if err := f.h.appStore().UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var e error
		groups, e = tx.DuplicateCollapseCandidates(context.Background(), 10)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Losers) != 1 {
		t.Fatalf("groups = %+v, want one group of one loser", groups)
	}
	if got := groups[0].Losers[0].LabelsToUpgrade; got != 1 {
		t.Errorf("LabelsToUpgrade = %d, want 1", got)
	}
	if res := f.collapse(keeper, loser); res.LabelsUpgraded != 1 {
		t.Errorf("LabelsUpgraded = %d, want 1 (the dry run said 1)", res.LabelsUpgraded)
	}
}

// ---- report and repair -------------------------------------------------------

// plantPreFixCollapse reproduces, row for row, what a pre-#859 collapse left
// behind: the keeper's unioned label rows stamped with the transaction time T
// and applied_by 'user' (the old INSERT carried only label_id and the rule id),
// plus the change_log pair — the loser's Email destroyed and the keeper's Email
// updated — at the same T. The loser itself no longer exists.
func (f *provFixture) plantPreFixCollapse(keeper ulid.ULID, at time.Time, seq int64) {
	f.t.Helper()
	gone := NewULID().String()
	f.exec(`INSERT INTO app.change_log (user_id, object_type, seq, object_id, op, created_at)
	        VALUES ($1,'Email',$2,$3,'destroyed',$5), ($1,'Email',$2+1,$4,'updated',$5)`,
		f.u.ID, seq, gone, keeper.String(), at)
}

func (f *provFixture) report() CollapseProvenanceReport {
	f.t.Helper()
	var r CollapseProvenanceReport
	if err := f.h.appStore().UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var e error
		r, e = tx.CollapseProvenanceReport(context.Background())
		return e
	}); err != nil {
		f.t.Fatalf("report: %v", err)
	}
	return r
}

func (f *provFixture) repair() (int, int) {
	f.t.Helper()
	var labels, msgs int
	if err := f.h.appStore().UserTx(context.Background(), f.userID, func(tx *UserTx) error {
		var e error
		labels, msgs, e = tx.RepairCollapseRuleProvenance(context.Background())
		return e
	}); err != nil {
		f.t.Fatalf("repair: %v", err)
	}
	return labels, msgs
}

func TestCollapseProvenanceReportAndRepair(t *testing.T) {
	f := newProvFixture(t)
	keeper, other := dupPair(t, f.h, f.u, "prov-rep")
	T := time.Date(2025, 6, 7, 8, 9, 10, 123456000, time.UTC)
	f.plantPreFixCollapse(keeper, T, 900001)

	// seedUser may already have filed something; the bound counts on top of it.
	baseUser := scalar(t, f.h, `SELECT count(*) FROM app.message_labels
	                             WHERE user_id=$1 AND applied_by='user' AND applied_by_rule_id IS NULL`, f.u.ID)

	rid := f.rule()
	ruled, filedInCollapse, filedByHand, genuineRule, anomaly :=
		f.label("ruled"), f.label("fic"), f.label("hand"), f.label("rule"), f.label("anom")
	// The planted mis-attribution: a rule's label a collapse turned into 'user'.
	f.put(keeper, ruled, LabelByUser, &rid, T)
	// Written by the same collapse with no rule id: may have been a real filing on
	// the deleted copy. Counted under evidence 2, never repaired.
	f.put(keeper, filedInCollapse, LabelByUser, nil, T)
	// A genuine filing with no collapse anywhere near it.
	f.put(other, filedByHand, LabelByUser, nil, past)
	// A correct rule row.
	f.put(other, genuineRule, LabelByRule, &rid, past)
	// A rule id under an attribution no collapse produces: reported, left alone.
	f.put(other, anomaly, LabelByLearned, &rid, past)

	r := f.report()
	if r.CollapseTxs != 1 || r.CollapseKeepers != 1 {
		t.Errorf("collapse txs/keepers = %d/%d, want 1/1", r.CollapseTxs, r.CollapseKeepers)
	}
	if r.FirstCollapseTx == nil || !r.FirstCollapseTx.Equal(T) {
		t.Errorf("FirstCollapseTx = %v, want %s", r.FirstCollapseTx, T)
	}
	if got := r.CollapseLabelsByBy[LabelByUser]; got != 2 || len(r.CollapseLabelsByBy) != 1 {
		t.Errorf("CollapseLabelsByBy = %v, want {user:2}", r.CollapseLabelsByBy)
	}
	if r.RuleIDByAppliedBy[LabelByUser] != 1 || r.RuleIDByAppliedBy[LabelByLearned] != 1 || len(r.RuleIDByAppliedBy) != 2 {
		t.Errorf("RuleIDByAppliedBy = %v, want {user:1, learned:1}", r.RuleIDByAppliedBy)
	}
	if len(r.Repairable) != 1 || r.Repairable[0].LabelID != ruled || !r.Repairable[0].InCollapseTx {
		t.Fatalf("Repairable = %+v, want exactly the planted row, matched to the collapse", r.Repairable)
	}
	if r.OldestEmailChange == nil {
		t.Error("OldestEmailChange is nil with change_log rows present — the horizon would be misreported")
	}
	// The bound: both no-rule 'user' rows, and only the one older than the
	// oldest change row sits outside evidence 2's reach.
	if r.UserRowsNoRule != baseUser+2 || r.UserRowsNoRuleBeforeHorizon != 1 {
		t.Errorf("bound = %d (before horizon %d), want %d (1)", r.UserRowsNoRule, r.UserRowsNoRuleBeforeHorizon, baseUser+2)
	}

	changes := func() int {
		return scalar(t, f.h, `SELECT count(*) FROM app.change_log
		                        WHERE user_id=$1 AND object_type='Email' AND op='updated' AND object_id=$2`,
			f.u.ID, keeper.String())
	}
	before := changes()

	labels, msgs := f.repair()
	if labels != 1 || msgs != 1 {
		t.Errorf("repair = %d labels / %d messages, want 1/1", labels, msgs)
	}
	if got := changes() - before; got != 1 {
		t.Errorf("Email updated rows for the keeper = %d, want 1 — the change_log invariant", got)
	}
	if row, _ := f.row(keeper, ruled); row.by != LabelByRule || row.ruleID == nil || *row.ruleID != rid {
		t.Errorf("repaired row = %+v, want rule %s", row, rid)
	}
	for _, c := range []struct {
		msg   ulid.ULID
		label string
		want  string
	}{
		{keeper, filedInCollapse, LabelByUser},
		{other, filedByHand, LabelByUser},
		{other, genuineRule, LabelByRule},
		{other, anomaly, LabelByLearned},
	} {
		if row, _ := f.row(c.msg, c.label); row.by != c.want {
			t.Errorf("label %s: applied_by %q, want %q untouched", c.label, row.by, c.want)
		}
	}

	// A rerun changes nothing and appends nothing.
	before = changes()
	if labels, msgs := f.repair(); labels != 0 || msgs != 0 {
		t.Errorf("rerun repaired %d/%d, want 0/0", labels, msgs)
	}
	if got := changes() - before; got != 0 {
		t.Errorf("rerun appended %d change rows, want 0", got)
	}
	if r := f.report(); len(r.Repairable) != 0 || r.RuleIDByAppliedBy[LabelByUser] != 0 {
		t.Errorf("report after repair: repairable %d, user+rule %d, want 0/0",
			len(r.Repairable), r.RuleIDByAppliedBy[LabelByUser])
	}
}

// After #859 a collapse cannot be mistaken for a pre-fix one: the carried rows
// keep their own created_at, so evidence 2 matches nothing, and no rule-id row
// says 'user'.
func TestCollapseProvenanceReportSeesNothingAfterAFixedCollapse(t *testing.T) {
	f := newProvFixture(t)
	keeper, loser := dupPair(t, f.h, f.u, "prov-clean")
	rid := f.rule()
	l1, l2 := f.label("a"), f.label("b")
	f.put(loser, l1, LabelByRule, &rid, past)
	f.put(loser, l2, LabelByLearned, nil, past)
	f.collapse(keeper, loser)

	r := f.report()
	if r.CollapseTxs != 1 {
		t.Errorf("CollapseTxs = %d, want 1 — the collapse's change rows should be visible", r.CollapseTxs)
	}
	if len(r.CollapseLabelsByBy) != 0 || len(r.Repairable) != 0 || len(r.RuleIDByAppliedBy) != 0 {
		t.Errorf("report = %+v, want nothing attributed to the fixed collapse", r)
	}
}
