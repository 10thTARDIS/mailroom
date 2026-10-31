// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// LEARNED PLACEMENT end to end (#714, ADR-032): the user's own filings teach
// Mailroom where a sender's mail goes, and a new message from that sender is
// filed there at ingest, with every safeguard the "act
// immediately" choice made mandatory. Each test drives the real ingest path
// and the real Re-file write (store.ApplyEmailUpdate, the path Email/set
// takes), never a hand-written label row where the behaviour under test is
// the one that writes it.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/placement"
	"mailroom/internal/rules"
	"mailroom/internal/store"
)

var lpUID atomic.Int64

func init() { lpUID.Store(70_000) }

// lpAreas materialises the default area plan and returns name -> area.
func (f *fixture) lpAreas(t *testing.T) map[string]store.AreaView {
	t.Helper()
	ctx := context.Background()
	out := map[string]store.AreaView{}
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		views, err := tx.ListAreas(ctx)
		for _, v := range views {
			out[v.Name] = v
		}
		return err
	}); err != nil {
		t.Fatalf("list areas: %v", err)
	}
	for _, n := range []string{"Primary", "Updates", "Promotions", "Forums"} {
		if _, ok := out[n]; !ok {
			t.Fatalf("default plan has no %s area: %v", n, out)
		}
	}
	return out
}

// lpIngest live-ingests a plain message from `from` into INBOX, with any
// extra header lines (Authentication-Results, for the authentication tests).
func (f *fixture) lpIngest(t *testing.T, from string, extra ...string) ulid.ULID {
	t.Helper()
	uid := lpUID.Add(1)
	res, err := f.ing.Ingest(context.Background(), f.userID, f.dek, f.dekID,
		rawFrom(from, fmt.Sprintf("Note %d", uid), fmt.Sprintf("lp-%d-%s@example.com", uid, f.userID), extra...),
		f.prov(f.mailboxA, uid, nil))
	if err != nil {
		t.Fatalf("ingest from %s: %v", from, err)
	}
	if !res.Created {
		t.Fatalf("ingest from %s: not a new message", from)
	}
	return res.MessageID
}

// lpRefile is the client's Re-file (web/src/core/refile.ts) through the
// store's Email/set write: remove every OTHER area's own label the message
// carries, add the target's, attributed to the user.
func (f *fixture) lpRefile(t *testing.T, msgID ulid.ULID, areas map[string]store.AreaView, target string) {
	t.Helper()
	f.lpRefileChecked(t, msgID, areas, target, true)
}

// lpRefileChecked is lpRefile; when check is set it also asserts the filing
// was RECORDED, as the evidence query counts it (#848), unless the target is
// Primary (see below). check is false only for lpFileHeld, whose claim step
// asserts the count instead.
func (f *fixture) lpRefileChecked(t *testing.T, msgID ulid.ULID, areas map[string]store.AreaView, target string, check bool) {
	t.Helper()
	var remove []ulid.ULID
	for name, a := range areas {
		if name == target {
			continue
		}
		if f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
			msgID.String(), a.LabelID.String()) > 0 {
			remove = append(remove, a.LabelID)
		}
	}
	// #848: a Re-file that recorded no filing is evidence the test does not
	// have. The one legitimate zero is a Re-file into the area the message
	// already resolves to by the desk (Primary), which writes no row of its
	// own: those callers claim the labels (lpClaim), which asserts the count.
	var from string
	if err := f.super.QueryRow(context.Background(),
		`SELECT COALESCE(from_address::text, '') FROM app.messages WHERE id=$1`, msgID.String()).Scan(&from); err != nil {
		t.Fatalf("re-file: read From: %v", err)
	}
	_, _, keyed := placement.KeysFor(from)
	keyed = keyed && check
	var before int
	if keyed {
		before = f.lpFilingsOf(t, from)[areas[target].ID]
	}
	f.lpUpdate(t, msgID, store.EmailUpdate{AddLabelIDs: []ulid.ULID{areas[target].LabelID}, RemoveLabelIDs: remove})
	if keyed && target != "Primary" {
		if got := f.lpFilingsOf(t, from)[areas[target].ID]; got != before+1 {
			t.Fatalf("fixture: re-filing into %s moved the evidence for %s from %d to %d filings, want +1 (the filing was not recorded)",
				target, from, before, got)
		}
	}
}

func (f *fixture) lpUpdate(t *testing.T, msgID ulid.ULID, up store.EmailUpdate) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		found, _, err := tx.ApplyEmailUpdate(ctx, msgID, up)
		if err == nil && !found {
			err = fmt.Errorf("message %s not found", msgID)
		}
		return err
	}); err != nil {
		t.Fatalf("email update: %v", err)
	}
}

// lpFile ingests n messages from `from` and Re-files each into target.
func (f *fixture) lpFile(t *testing.T, from string, n int, areas map[string]store.AreaView, target string) {
	t.Helper()
	for i := 0; i < n; i++ {
		f.lpRefile(t, f.lpIngest(t, from), areas, target)
	}
}

// lpFileHeld is lpFile for a sender whose mail the desk ALREADY places in
// target: a Re-file into the area a message is already in writes no row of its
// own (#848: the fixture sender "filed into Updates" recorded no evidence), so
// the filings are claimed for the user (lpClaim), which asserts the evidence
// query now counts them.
func (f *fixture) lpFileHeld(t *testing.T, from string, n int, areas map[string]store.AreaView, target string) {
	t.Helper()
	for i := 0; i < n; i++ {
		f.lpRefileChecked(t, f.lpIngest(t, from), areas, target, false)
	}
	f.lpClaim(t, from, areas[target])
}

// lpLabelBy counts msgID's rows for area's own label applied by `by`.
func (f *fixture) lpLabelBy(t *testing.T, msgID ulid.ULID, area store.AreaView, by string) int {
	t.Helper()
	return f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2 AND applied_by=$3`,
		msgID.String(), area.LabelID.String(), by)
}

// lpDeskLabels counts every desk-applied label on msgID.
func (f *fixture) lpDeskLabels(t *testing.T, msgID ulid.ULID) int {
	t.Helper()
	return f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND applied_by='desk'`, msgID.String())
}

func (f *fixture) lpPlacements(t *testing.T, msgID ulid.ULID) int {
	t.Helper()
	return f.scalar(t, `SELECT count(*) FROM app.learned_placements WHERE message_id=$1`, msgID.String())
}

func (f *fixture) lpOutcome(t *testing.T, msgID ulid.ULID) string {
	t.Helper()
	var s string
	if err := f.super.QueryRow(context.Background(),
		`SELECT outcome || '/' || COALESCE(kept_by, '') || '/' || (corrected_at IS NOT NULL)::text
		   FROM app.learned_placements WHERE message_id=$1`, msgID.String()).Scan(&s); err != nil {
		t.Fatalf("placement outcome: %v", err)
	}
	return s
}

// assertDeskPlaced: the desk placed msgID in Primary by its tab, and learned
// placement did nothing.
func (f *fixture) assertDeskPlaced(t *testing.T, msgID ulid.ULID, areas map[string]store.AreaView, why string) {
	t.Helper()
	if n := f.lpLabelBy(t, msgID, areas["Primary"], "desk"); n != 1 {
		t.Errorf("%s: Primary desk label = %d, want 1 (the desk places it)", why, n)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND applied_by='learned'`,
		msgID.String()); n != 0 {
		t.Errorf("%s: learned labels = %d, want 0", why, n)
	}
	if n := f.lpPlacements(t, msgID); n != 0 {
		t.Errorf("%s: placement log rows = %d, want 0", why, n)
	}
}

// TestLearnedPlacementFilesAKnownSender is the feature: two filings of alice
// into Updates, and her next message, which the tab verdict puts in Primary,
// is filed into Updates as `learned`, with no desk label, a placement log row,
// exactly the one Email `created` change row, and no writeback.
func TestLearnedPlacementFilesAKnownSender(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)

	// Control: with no evidence, alice's mail goes to Primary by the desk.
	// Without this the assertions below could pass on a message the tab
	// verdict already put in Updates.
	control := f.lpIngest(t, "alice@example.com")
	f.assertDeskPlaced(t, control, areas, "control before any filing")
	f.lpRefile(t, control, areas, "Updates")
	f.lpFile(t, "alice@example.com", 1, areas, "Updates")
	f.lpWantFilings(t, "alice@example.com", areas["Updates"], 2)

	beforeCreated := f.changeCount(t, "Email", "created")
	beforeUpdated := f.changeCount(t, "Email", "updated")
	msg := f.lpIngest(t, "Alice@Example.com")

	if n := f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("Updates label applied_by=learned = %d, want 1", n)
	}
	if n := f.lpDeskLabels(t, msg); n != 0 {
		t.Errorf("desk labels on the learned message = %d, want 0 (it resolves by label)", n)
	}
	var kind, value, outcome string
	if err := f.super.QueryRow(context.Background(), `
		SELECT key_kind, key_value, outcome FROM app.learned_placements
		 WHERE user_id=$1 AND message_id=$2 AND area_id=$3`,
		f.userID.String(), msg.String(), areas["Updates"].ID.String()).Scan(&kind, &value, &outcome); err != nil {
		t.Fatalf("placement log row: %v", err)
	}
	if kind != "address" || value != "alice@example.com" || outcome != store.PlacementOpen {
		t.Errorf("placement = (%s, %s, %s), want (address, alice@example.com, open)", kind, value, outcome)
	}
	// CHANGE_LOG: the label rides the Email `created` row; nothing else.
	if got := f.changeCount(t, "Email", "created") - beforeCreated; got != 1 {
		t.Errorf("Email created rows = %d, want 1", got)
	}
	if got := f.changeCount(t, "Email", "updated") - beforeUpdated; got != 0 {
		t.Errorf("Email updated rows = %d, want 0 (an ingest-time placement owns no row of its own)", got)
	}
	// NO WRITEBACK: area labels are local-only (ADR-031) and the placement
	// is made before the message has a live account.
	if n := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1 AND message_id=$2`,
		f.userID.String(), msg.String()); n != 0 {
		t.Errorf("writeback intents mentioning the learned message = %d, want 0", n)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.writeback_intents WHERE user_id=$1 AND source='learned'`,
		f.userID.String()); n != 0 {
		t.Errorf("writeback intents attributed to learned placement = %d, want 0", n)
	}
	// And the provenance the client reads says so.
	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		prov, err := tx.AreaLabelProvenance(ctx, []ulid.ULID{msg})
		if err != nil {
			return err
		}
		if got := prov[msg][areas["Updates"].ID]; got != store.LabelByLearned {
			return fmt.Errorf("area label provenance = %q, want learned", got)
		}
		return nil
	}); err != nil {
		t.Error(err)
	}
}

// TestLearnedPlacementThinEvidenceAbstains: one filing is not a pattern.
func TestLearnedPlacementThinEvidenceAbstains(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 1, areas, "Updates")
	f.lpWantFilings(t, "alice@example.com", areas["Updates"], 1)
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "one filing")
}

// TestLearnedPlacementMixedEvidenceAbstains: filed into two areas, so no
// answer.
//
// Seeded with learning switched OFF: with it on, the third filing would be a
// Re-file of a LEARNED message, i.e. a correction, and the contradiction rule
// would abstain on its own and hide whether mixed evidence does (a probe
// that made mixed evidence act stayed green until this was changed).
func TestLearnedPlacementMixedEvidenceAbstains(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	f.lpFile(t, "alice@example.com", 1, areas, "Promotions")
	f.lpSwitch(t, true)
	f.lpWantFilings(t, "alice@example.com", areas["Updates"], 2)
	f.lpWantFilings(t, "alice@example.com", areas["Promotions"], 1)
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "mixed filings")
}

// lpSwitch sets learned placement's off switch.
func (f *fixture) lpSwitch(t *testing.T, on bool) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		return tx.UpdateDeskPreferences(ctx, store.DeskPreferencesPatch{LearnedPlacementEnabled: &on})
	}); err != nil {
		t.Fatal(err)
	}
}

// TestLearnedPlacementDomainFallback: an address with no evidence follows its
// domain when the domain's filings agree across at least two addresses, and
// not when they all come from one address.
func TestLearnedPlacementDomainFallback(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)

	// One address only: the domain is that address's evidence, so no fallback.
	f.lpFile(t, "bob@shop.example", 2, areas, "Promotions")
	f.assertDeskPlaced(t, f.lpIngest(t, "eve@shop.example"), areas, "domain over one address")

	// A second agreeing address: the domain acts for a third.
	f.lpFile(t, "carol@shop.example", 1, areas, "Promotions")
	msg := f.lpIngest(t, "dave@shop.example")
	if n := f.lpLabelBy(t, msg, areas["Promotions"], store.LabelByLearned); n != 1 {
		t.Fatalf("dave (no evidence, confident domain): learned Promotions label = %d, want 1", n)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.learned_placements WHERE message_id=$1 AND key_kind='domain' AND key_value='shop.example'`,
		msg.String()); n != 1 {
		t.Errorf("placement key: want (domain, shop.example)")
	}
}

// TestLearnedPlacementAddressBeatsDomain: the domain fallback never overrides
// an address that has evidence of its own, even thin evidence.
//
// ⚠ THE FIXTURE MUST LET THE DOMAIN BE CONFIDENT WHILE THE ADDRESS IS NOT.
// An address's filings are also its domain's, so an address filed into a
// DIFFERENT area just makes the domain mixed, and a test built that way
// passes whether or not the address takes precedence (the first version of
// this test did, and a probe removing the precedence stayed green). The
// cases that can tell are an address whose evidence AGREES with the domain
// but is thin, and an address whose own placement was corrected.
func TestLearnedPlacementAddressBeatsDomain(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	// Seeded with learning off, so no seeding ingest is itself a placement.
	f.lpSwitch(t, false)
	f.lpFile(t, "bob@shop.example", 2, areas, "Promotions")
	f.lpFile(t, "carol@shop.example", 2, areas, "Promotions")
	// dave has ONE filing, into the domain's own area: thin on his address.
	f.lpFile(t, "dave@shop.example", 1, areas, "Promotions")
	// erin has two, so her address is confident on its own.
	f.lpFile(t, "erin@shop.example", 2, areas, "Promotions")
	f.lpSwitch(t, true)

	// Thin address, confident domain agreeing with it: the address decides,
	// and it abstains.
	f.assertDeskPlaced(t, f.lpIngest(t, "dave@shop.example"), areas, "thin address in a confident domain")

	// A corrected address: erin's learned placement is taken back without a
	// re-file, so her evidence is consistent but contradicted. The domain is
	// still confident, and must not decide for her.
	erinLearned := f.lpIngest(t, "erin@shop.example")
	if f.lpLabelBy(t, erinLearned, areas["Promotions"], store.LabelByLearned) != 1 {
		t.Fatal("fixture: erin's confident address must be learned into Promotions")
	}
	f.lpUpdate(t, erinLearned, store.EmailUpdate{RemoveLabelIDs: []ulid.ULID{areas["Promotions"].LabelID}})
	f.assertDeskPlaced(t, f.lpIngest(t, "erin@shop.example"), areas, "corrected address in a confident domain")

	// dave filed into Updates now: the domain becomes mixed, so what follows
	// is the address deciding on its own.
	f.lpSwitch(t, false)
	f.lpFile(t, "dave@shop.example", 2, areas, "Updates")
	f.lpSwitch(t, true)
	f.assertDeskPlaced(t, f.lpIngest(t, "dave@shop.example"), areas, "mixed address")

	// frank, new to the domain, files twice into Updates: his own address is
	// confident and decides, against a domain that (counting dave) is mixed.
	f.lpSwitch(t, false)
	f.lpFile(t, "frank@shop.example", 2, areas, "Updates")
	f.lpSwitch(t, true)
	msg := f.lpIngest(t, "frank@shop.example")
	if n := f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("frank confident on his own address: learned Updates = %d, want 1", n)
	}
}

// TestLearnedPlacementNeverOverARule: a rule's area label at ingest wins, and
// learned placement neither adds its own nor logs a placement.
func TestLearnedPlacementNeverOverARule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	f.lpWantFilings(t, "alice@example.com", areas["Updates"], 2)
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err := tx.CreateRule(ctx, store.RuleRow{
			Name: "alice-to-forums", Enabled: true,
			Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "alice@example.com"},
			Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: areas["Forums"].LabelID.String()}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	msg := f.lpIngest(t, "alice@example.com")
	if n := f.lpLabelBy(t, msg, areas["Forums"], store.LabelByRule); n != 1 {
		t.Fatalf("rule's Forums label = %d, want 1 (fixture: the rule must fire)", n)
	}
	if n := f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned); n != 0 {
		t.Errorf("learned label over a rule's placement = %d, want 0", n)
	}
	if n := f.lpPlacements(t, msg); n != 0 {
		t.Errorf("placement rows over a rule's placement = %d, want 0", n)
	}
}

// TestLearnedPlacementNeverLearnsFromItsOwnOutput is the #169 negative
// control: rows applied by desk, setup, rule, ml or learned are NOT evidence.
// The positive half re-attributes the very same rows to the user and
// requires the placement to happen, so the test can see the mechanism it
// claims to exclude.
func TestLearnedPlacementNeverLearnsFromItsOwnOutput(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	var planted []ulid.ULID
	for _, by := range []string{"desk", "setup", "rule", "ml", "learned"} {
		for i := 0; i < 2; i++ {
			m := f.lpIngest(t, "alice@example.com")
			if _, err := f.super.Exec(context.Background(), `DELETE FROM app.message_labels WHERE message_id=$1`, m.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := f.super.Exec(context.Background(), `
				INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by) VALUES ($1,$2,$3,$4)`,
				f.userID.String(), m.String(), areas["Updates"].LabelID.String(), by); err != nil {
				t.Fatal(err)
			}
			planted = append(planted, m)
		}
	}
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "ten automation rows into Updates")

	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.message_labels SET applied_by='user' WHERE message_id = ANY($1)`, idStrings(planted)); err != nil {
		t.Fatal(err)
	}
	msg := f.lpIngest(t, "alice@example.com")
	if n := f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("positive control: the same rows as user filings must teach Updates, got %d learned labels", n)
	}
}

// TestLearnedPlacementCorrection: Re-filing a learned message records the
// correction, and the key abstains afterwards. Then the contradiction rule on
// its own: a learned label removed without re-filing leaves the evidence
// consistent, and the key still abstains until the user files again.
func TestLearnedPlacementCorrection(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	learned := f.lpIngest(t, "alice@example.com")
	if f.lpLabelBy(t, learned, areas["Updates"], store.LabelByLearned) != 1 {
		t.Fatal("fixture: alice's third message must be learned into Updates")
	}

	beforeUpdated := f.changeCount(t, "Email", "updated")
	f.lpRefile(t, learned, areas, "Promotions")
	if got := f.lpOutcome(t, learned); got != "corrected//true" {
		t.Errorf("placement after Re-file = %s, want corrected//true", got)
	}
	if got := f.changeCount(t, "Email", "updated") - beforeUpdated; got != 1 {
		t.Errorf("Email updated rows for the correction = %d, want 1", got)
	}
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "after a correction (mixed and contradicted)")

	// The contradiction alone, on a second sender.
	f.lpFile(t, "bob@example.com", 2, areas, "Updates")
	bobLearned := f.lpIngest(t, "bob@example.com")
	if f.lpLabelBy(t, bobLearned, areas["Updates"], store.LabelByLearned) != 1 {
		t.Fatal("fixture: bob's third message must be learned into Updates")
	}
	f.lpUpdate(t, bobLearned, store.EmailUpdate{RemoveLabelIDs: []ulid.ULID{areas["Updates"].LabelID}})
	if got := f.lpOutcome(t, bobLearned); got != "corrected//true" {
		t.Errorf("placement after removing the learned label = %s, want corrected//true", got)
	}
	f.assertDeskPlaced(t, f.lpIngest(t, "bob@example.com"), areas, "contradicted, evidence still consistent")

	// A filing strictly after the correction lifts it.
	time.Sleep(10 * time.Millisecond)
	f.lpFile(t, "bob@example.com", 1, areas, "Updates")
	again := f.lpIngest(t, "bob@example.com")
	if n := f.lpLabelBy(t, again, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("after a newer filing the key acts again: learned = %d, want 1", n)
	}
}

// TestLearnedPlacementKept: archived where it was placed resolves kept at
// once; an untouched placement is kept after placement.KeptAfter, and a
// correction after that does not reopen it.
func TestLearnedPlacementKept(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")

	archived := f.lpIngest(t, "alice@example.com")
	f.lpUpdate(t, archived, store.EmailUpdate{Seen: ptrBool(true)})
	if got := f.lpOutcome(t, archived); got != "open//false" {
		t.Errorf("after reading only = %s, want open (reading is not keeping)", got)
	}
	f.lpUpdate(t, archived, store.EmailUpdate{MoveToRole: "archive"})
	if got := f.lpOutcome(t, archived); got != "kept/archive/false" {
		t.Errorf("after archiving = %s, want kept/archive/false", got)
	}

	aged := f.lpIngest(t, "alice@example.com")
	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.learned_placements SET decided_at = now() - interval '8 days' WHERE message_id=$1`, aged.String()); err != nil {
		t.Fatal(err)
	}
	// A late move does not reopen it ...
	f.lpRefile(t, aged, areas, "Promotions")
	// ... and the next decision writes the age resolution down.
	f.lpFile(t, "carol@example.com", 2, areas, "Updates")
	f.lpIngest(t, "carol@example.com")
	if got := f.lpOutcome(t, aged); got != "kept/age/false" {
		t.Errorf("an 8-day-old placement = %s, want kept/age/false", got)
	}
}

// TestLearnedPlacementGateStopsAnArea: an area driven below the Wilson floor
// stops acting, the report says so, and an operator reset re-enables it.
func TestLearnedPlacementGateStopsAnArea(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	areas := f.lpAreas(t)
	upd := areas["Updates"]

	// 20 resolved placements in Updates, 19 kept and 1 corrected: Wilson
	// lower bound 0.764 < 0.80. Seeded as log rows on real messages: the
	// path that writes them is covered by the tests above, and this one is
	// about what the gate does with them.
	for i := 0; i < placement.GateMinResolved; i++ {
		m := f.lpIngest(t, fmt.Sprintf("seed%d@other.example", i))
		outcome, keptAt, keptBy, correctedAt := "kept", "now()", "'age'", "NULL"
		if i == 0 {
			outcome, keptAt, keptBy, correctedAt = "corrected", "NULL", "NULL", "now()"
		}
		if _, err := f.super.Exec(ctx, `
			INSERT INTO app.learned_placements (user_id, message_id, key_kind, key_value, area_id, decided_at, outcome, kept_at, kept_by, corrected_at)
			VALUES ($1, $2, 'address', $3, $4, now() - interval '9 days', $5, `+keptAt+`, `+keptBy+`, `+correctedAt+`)`,
			f.userID.String(), m.String(), fmt.Sprintf("seed%d@other.example", i), upd.ID.String(), outcome); err != nil {
			t.Fatal(err)
		}
	}
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "gated area")

	var rep store.PlacementReport
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		rep, e = tx.PlacementReport(ctx)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	stopped := false
	for _, a := range rep.Areas {
		if a.AreaID == upd.ID {
			stopped = a.Gate.Stopped
			if a.Counts.Kept != 19 || a.Counts.Corrected != 1 {
				t.Errorf("report counts for Updates = %+v, want 19 kept / 1 corrected", a.Counts)
			}
		}
	}
	if !stopped {
		t.Error("the report must say the Updates gate is STOPPED")
	}

	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		found, err := tx.ResetPlacementGate(ctx, upd.ID)
		if err == nil && !found {
			err = fmt.Errorf("reset: area not found")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	msg := f.lpIngest(t, "alice@example.com")
	if n := f.lpLabelBy(t, msg, upd, store.LabelByLearned); n != 1 {
		t.Errorf("after an operator reset the area acts again: learned = %d, want 1", n)
	}
}

// TestLearnedPlacementOffSwitch: switched off, nothing is placed, and the
// switch is the store's, not the client's.
func TestLearnedPlacementOffSwitch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	f.lpWantFilings(t, "alice@example.com", areas["Updates"], 2)
	off := false
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		return tx.UpdateDeskPreferences(ctx, store.DeskPreferencesPatch{LearnedPlacementEnabled: &off})
	}); err != nil {
		t.Fatal(err)
	}
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "switched off")
}

// TestLearnedPlacementIsPerUser: user B's filings never teach user A. RLS
// hides B's rows from A's transaction; this proves the evidence query has no
// path around it.
func TestLearnedPlacementIsPerUser(t *testing.T) {
	a := newFixture(t)
	b := newFixture(t)
	areasA := a.lpAreas(t)
	areasB := b.lpAreas(t)
	// Two filings teach B (a third would re-file B's own learned message into
	// the area it is already in, which records no filing, #848).
	b.lpFile(t, "alice@example.com", 2, areasB, "Updates")
	b.lpWantFilings(t, "alice@example.com", areasB["Updates"], 2)
	// Control: B itself has learned.
	if n := b.lpLabelBy(t, b.lpIngest(t, "alice@example.com"), areasB["Updates"], store.LabelByLearned); n != 1 {
		t.Fatalf("control: B's own next message from alice must be learned, got %d", n)
	}
	a.assertDeskPlaced(t, a.lpIngest(t, "alice@example.com"), areasA, "another user's filings")
}

// TestPlacementReportCounts: the report's counts on a known fixture,
// including the backfill simulation (counts only, nothing written).
func TestPlacementReportCounts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	areas := f.lpAreas(t)
	off, on := false, true
	setSwitch := func(v *bool) {
		if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
			return tx.UpdateDeskPreferences(ctx, store.DeskPreferencesPatch{LearnedPlacementEnabled: v})
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Existing mail, ingested while learning is OFF: three from alice (two
	// get filed into Updates, one stays in Primary) and one from bob (filed
	// once: thin), plus mixed carol.
	setSwitch(&off)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	f.lpIngest(t, "alice@example.com")
	f.lpFile(t, "bob@example.com", 1, areas, "Promotions")
	f.lpIngest(t, "bob@example.com")
	f.lpFile(t, "carol@example.com", 1, areas, "Promotions")
	f.lpFile(t, "carol@example.com", 1, areas, "Updates")
	f.lpIngest(t, "carol@example.com")
	setSwitch(&on)

	beforeLabels := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE user_id=$1`, f.userID.String())
	var rep store.PlacementReport
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		rep, e = tx.PlacementReport(ctx)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if after := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE user_id=$1`, f.userID.String()); after != beforeLabels {
		t.Errorf("the report wrote labels: %d -> %d", beforeLabels, after)
	}
	if rep.Filings != 5 {
		t.Errorf("filings = %d, want 5", rep.Filings)
	}
	r := rep.KeyReasons[placement.KeyAddress]
	if r[placement.Confident] != 1 || r[placement.Thin] != 1 || r[placement.Mixed] != 1 {
		t.Errorf("address keys = %v, want confident 1, thin 1, mixed 1", r)
	}
	for _, a := range rep.Areas {
		if a.AreaID == areas["Updates"].ID && a.ConfidentAddresses != 1 {
			t.Errorf("Updates confident addresses = %d, want 1", a.ConfidentAddresses)
		}
	}
	move := store.PlacementMove{From: areas["Primary"].ID, To: areas["Updates"].ID}
	if rep.Moves[move] != 1 || len(rep.Moves) != 1 {
		t.Errorf("backfill moves = %v, want exactly Primary->Updates: 1", rep.Moves)
	}
	if rep.Held != 5 {
		t.Errorf("held (user-filed) = %d, want 5", rep.Held)
	}
	if rep.Abstain[placement.Thin] != 1 || rep.Abstain[placement.Mixed] != 1 {
		t.Errorf("backfill abstentions = %v, want thin 1 (bob), mixed 1 (carol)", rep.Abstain)
	}
}

func ptrBool(b bool) *bool { return &b }

func idStrings(ids []ulid.ULID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// lpWithTrustedAuthserv rebuilds the fixture's Ingestor believing ids, the
// way cmd/mailroom does from MAILROOM_AUTHSERV_IDS.
func (f *fixture) lpWithTrustedAuthserv(t *testing.T, ids ...string) {
	t.Helper()
	blobs, err := blob.NewFSBlobStore(t.TempDir(), randomKey(t))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	f.ing = New(f.st, blobs, WithTrustedAuthservIDs(ids))
}

// lpAddIdentity gives the fixture user an enabled identity at address.
func (f *fixture) lpAddIdentity(t *testing.T, address string) ulid.ULID {
	t.Helper()
	id := ulid.Make()
	if _, err := f.super.Exec(context.Background(), `
		INSERT INTO app.identities (id, user_id, account_id, kind, address) VALUES ($1, $2, $3, 'fixed', $4)`,
		id.String(), f.userID.String(), f.accountID.String(), address); err != nil {
		t.Fatalf("identity: %v", err)
	}
	return id
}

// TestLearnedPlacementDomainFallbackBlockedForFreemail: two gmail.com
// addresses filed into Updates must not teach the whole of gmail.com, while
// a gmail.com ADDRESS still learns, and a corporate domain with the same
// evidence still acts (the positive control).
func TestLearnedPlacementDomainFallbackBlockedForFreemail(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpSwitch(t, false)
	f.lpFile(t, "ann@gmail.com", 1, areas, "Updates")
	f.lpFile(t, "ben@gmail.com", 1, areas, "Updates")
	f.lpFile(t, "ann@corp.example", 1, areas, "Updates")
	f.lpFile(t, "ben@corp.example", 1, areas, "Updates")
	f.lpSwitch(t, true)

	f.assertDeskPlaced(t, f.lpIngest(t, "new.friend@gmail.com"), areas, "new sender at a freemail domain")
	if msg := f.lpIngest(t, "carl@corp.example"); f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned) != 1 {
		t.Error("positive control: a corporate domain with the same evidence must still place a new address")
	}
	// The address-level evidence of a gmail.com sender is untouched.
	f.lpSwitch(t, false)
	f.lpFile(t, "ann@gmail.com", 1, areas, "Updates")
	f.lpSwitch(t, true)
	if msg := f.lpIngest(t, "ann@gmail.com"); f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned) != 1 {
		t.Error("ann@gmail.com with two filings of her own must still be learned")
	}
}

// TestLearnedPlacementDomainFallbackBlockedForOwnDomain: the domains of the
// user's enabled identities never act as a fallback. Run with a trusted
// authserv-id and a trusted DMARC pass on the new message, so neither the
// authentication gate nor the selfSpoof warning is what holds it back: only
// the block can. Disabling the identity is the positive control.
func TestLearnedPlacementDomainFallbackBlockedForOwnDomain(t *testing.T) {
	f := newFixture(t)
	f.lpWithTrustedAuthserv(t, "mx.trusted.example")
	areas := f.lpAreas(t)
	idID := f.lpAddIdentity(t, "me@mine.example")
	pass := "Authentication-Results: mx.trusted.example; dkim=pass header.d=mine.example; dmarc=pass header.from=mine.example"
	f.lpFile(t, "ann@mine.example", 1, areas, "Updates")
	f.lpFile(t, "ben@mine.example", 1, areas, "Updates")

	f.assertDeskPlaced(t, f.lpIngest(t, "carl@mine.example", pass), areas, "new sender at the user's own domain")

	if _, err := f.super.Exec(context.Background(),
		`UPDATE app.identities SET enabled = false WHERE id = $1`, idID.String()); err != nil {
		t.Fatal(err)
	}
	if msg := f.lpIngest(t, "dora@mine.example", pass); f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned) != 1 {
		t.Error("positive control: with the identity disabled, the domain places a new address")
	}
}

// TestLearnedPlacementRequiresTrustedDMARC: with a trusted authserv-id
// configured, a known sender's mail is placed only on a DMARC pass stamped by
// that id.
func TestLearnedPlacementRequiresTrustedDMARC(t *testing.T) {
	f := newFixture(t)
	f.lpWithTrustedAuthserv(t, "mx.trusted.example")
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")

	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com",
		"Authentication-Results: mx.trusted.example; spf=pass smtp.mailfrom=evil.example; dmarc=fail header.from=example.com"),
		areas, "spoofed: trusted dmarc=fail")
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com"), areas, "no Authentication-Results at all")
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com",
		"Authentication-Results: evil.example; dkim=pass; dmarc=pass header.from=example.com"),
		areas, "a pass claimed by an UNtrusted authserv-id")
	f.assertDeskPlaced(t, f.lpIngest(t, "alice@example.com",
		"Authentication-Results: mx.trusted.example; dkim=pass header.d=esp.example; dmarc=none"),
		areas, "a trusted DKIM pass without DMARC")

	msg := f.lpIngest(t, "alice@example.com",
		"Authentication-Results: mx.trusted.example; spf=pass; dkim=pass header.d=example.com; dmarc=pass header.from=example.com")
	if n := f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("positive control: a trusted dmarc=pass must be placed, learned = %d", n)
	}
}

// TestLearnedPlacementUnconfiguredAuthFloor: with no trusted authserv-id,
// mail whose authentication clearly failed is not placed, and mail that did
// not fail is.
func TestLearnedPlacementUnconfiguredAuthFloor(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "alice@example.com", 2, areas, "Updates")
	failed := f.lpIngest(t, "alice@example.com", "Authentication-Results: mx.any.example; dmarc=fail header.from=example.com")
	if f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND auth_fail`, failed.String()) != 1 {
		t.Fatal("fixture: the message must be stored with auth_fail = true")
	}
	f.assertDeskPlaced(t, failed, areas, "auth_fail true")
	msg := f.lpIngest(t, "alice@example.com")
	if n := f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("auth_fail false must be placed, learned = %d", n)
	}
}

// TestLearnedPlacementNeverOverASenderWarning: a message the spoofing or
// look-alike warnings would flag is never placed, however confident its key.
func TestLearnedPlacementNeverOverASenderWarning(t *testing.T) {
	f := newFixture(t)
	f.lpWithTrustedAuthserv(t, "mx.trusted.example")
	areas := f.lpAreas(t)
	f.lpAddIdentity(t, "me@mine.example")
	pass := "Authentication-Results: mx.trusted.example; dkim=pass header.d=mine.example; dmarc=pass header.from=mine.example"
	f.lpFile(t, "colleague@mine.example", 2, areas, "Updates")
	f.lpFile(t, "shop@xn--pple-43d.com", 2, areas, "Updates")

	// Punycode sending domain: flagged by senderwarn, never placed even with
	// a trusted DMARC pass.
	f.assertDeskPlaced(t, f.lpIngest(t, "shop@xn--pple-43d.com",
		"Authentication-Results: mx.trusted.example; dmarc=pass header.from=xn--pple-43d.com"),
		areas, "punycode domain warning")

	// The user's own domain with a trusted pass: no selfSpoof, so placed.
	// This is the control that the warning check, not the domain, decided.
	msg := f.lpIngest(t, "colleague@mine.example", pass)
	if n := f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("own-domain colleague with a trusted pass: learned = %d, want 1", n)
	}

	// Unconfigured, a bare own-domain message passes the auth floor, but the
	// selfSpoof warning fires on it, so it is not placed.
	f.lpWithTrustedAuthserv(t)
	f.assertDeskPlaced(t, f.lpIngest(t, "colleague@mine.example"), areas, "selfSpoof warning")
}

// TestLearnedPlacementBindsDMARCToTheKeyedFrom: a trusted pass counts only
// for the exact From domain the placement keys on, and a From that is not one
// mailbox in one header is never acted on. The authenticated identity and the
// acted-on identity must be the same value.
func TestLearnedPlacementBindsDMARCToTheKeyedFrom(t *testing.T) {
	f := newFixture(t)
	f.lpWithTrustedAuthserv(t, "mx.trusted.example")
	areas := f.lpAreas(t)
	const bank = "alerts@bank.example"
	passBank := "Authentication-Results: mx.trusted.example; spf=pass smtp.mailfrom=bank.example; dkim=pass header.d=bank.example; dmarc=pass header.from=bank.example"
	f.lpFile(t, bank, 2, areas, "Updates")

	f.assertDeskPlaced(t, f.lpIngest(t, bank,
		"Authentication-Results: mx.trusted.example; dkim=pass header.d=attacker.example; dmarc=pass header.from=attacker.example"),
		areas, "trusted pass for attacker.example, From bank.example")
	f.assertDeskPlaced(t, f.lpIngest(t, bank,
		"Authentication-Results: mx.trusted.example; dkim=pass header.d=bank.example; dmarc=pass"),
		areas, "trusted pass with no header.from")
	f.assertDeskPlaced(t, f.lpIngest(t, "x@attacker.example",
		"From: "+bank,
		"Authentication-Results: mx.trusted.example; dmarc=pass header.from=attacker.example"),
		areas, "two From headers, the learned sender second, the pass for the first")
	f.assertDeskPlaced(t, f.lpIngest(t, bank, "From: x@attacker.example", passBank),
		areas, "two From headers, the learned sender first, the pass for it")
	f.assertDeskPlaced(t, f.lpIngest(t, bank+", x@attacker.example", passBank),
		areas, "one From header holding two mailboxes")
	f.assertDeskPlaced(t, f.lpIngest(t, "Bank: "+bank+";", passBank),
		areas, "group syntax From")
	// A trusted-id header the SENDER wrote sits below the provider's own,
	// which is topmost. The provider found no DMARC policy; the forgery
	// claims a pass. Only the topmost trusted header is read.
	f.assertDeskPlaced(t, f.lpIngest(t, bank,
		"Authentication-Results: mx.trusted.example; spf=pass smtp.mailfrom=bank.example; dmarc=none header.from=bank.example",
		"Authentication-Results: mx.trusted.example; dmarc=pass header.from=bank.example"),
		areas, "forged trusted-id pass below the provider's own header")

	msg := f.lpIngest(t, bank, passBank)
	if n := f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned); n != 1 {
		t.Errorf("positive control: a trusted pass for bank.example with a single From must be placed, learned = %d", n)
	}
	var keyValue string
	if err := f.super.QueryRow(context.Background(),
		`SELECT key_value FROM app.learned_placements WHERE message_id=$1`, msg.String()).Scan(&keyValue); err != nil || keyValue != bank {
		t.Errorf("placement log key_value = %q (%v), want the authenticated From %q", keyValue, err, bank)
	}
}

// TestLearnedPlacementRelaxedDMARCForASubdomainSender (#846): through the
// real ingest path, a subdomain sender with two filings whose provider stamps
// a trusted DMARC pass for the ORGANISATIONAL domain (the production header
// shape, placeholders for names) is placed; the same sender with a pass for
// anything else is not. Every other sender check is unchanged.
//
// Each refusal asserts the REASON the live arrival reported (the #840
// counter), not only that nothing was placed, so a case cannot pass for want
// of evidence. The filings go to Forums and are claimed as the user's (as the
// lpCases subdomain case does), so they count whatever area the desk itself
// would pick for these senders.
//
// NEGATIVE CONTROL: with LearnedSender passing ml.DMARCStrict, all three
// positive cases go red; with DMARCHeaderFromMatch's `org != h` check and its
// H-is-its-own-org-domain check both removed, the intermediate-parent, TLD
// and co.uk cases go red.
func TestLearnedPlacementRelaxedDMARCForASubdomainSender(t *testing.T) {
	f := newFixture(t)
	rec := &lpRecorder{}
	f.lpObserve(t, rec, "mx.trusted.example")
	areas := f.lpAreas(t)
	ar := func(dmarc string) string {
		return "Authentication-Results: mx.trusted.example;\r\n" +
			"\tdkim=pass header.d=bank.example header.s=s1 header.b=AbCdEf12;\r\n" +
			"\tspf=pass (mx.trusted.example: domain of bounce@mail.bank.example designates 192.0.2.10 as permitted sender) smtp.mailfrom=bounce@mail.bank.example;\r\n" +
			"\t" + dmarc
	}
	const (
		sender = "alerts@mail.bank.example"
		deep   = "alerts@a.mail.bank.example"
		uk     = "alerts@mail.example.co.uk"
		parent = "alerts@bank.example"
	)
	for _, who := range []string{sender, deep, uk, parent} {
		f.lpFile(t, who, 2, areas, "Forums")
		f.lpClaim(t, who, areas["Forums"])
	}
	arrive := func(from string, extra ...string) (ulid.ULID, string) {
		t.Helper()
		rec.reset()
		msg := f.lpIngest(t, from, extra...)
		got := rec.all()
		if len(got) != 1 {
			t.Fatalf("counter saw %v for one arrival", got)
		}
		return msg, got[0]
	}
	refused := func(msg ulid.ULID, reason, want, why string) {
		t.Helper()
		if reason != want {
			t.Errorf("%s: arrival reason = %s, want %s", why, reason, want)
		}
		if n := f.lpLabelBy(t, msg, areas["Forums"], store.LabelByLearned); n != 0 || f.lpPlacements(t, msg) != 0 {
			t.Errorf("%s: placed (learned label %d) despite the refusal", why, n)
		}
	}

	for _, c := range []struct{ from, dmarc, why string }{
		{sender, "dmarc=pass (policy=reject) header.from=other.example", "pass for an unrelated domain"},
		{sender, "dmarc=pass (policy=reject) header.from=shop.bank.example", "pass for a sibling subdomain"},
		{sender, "dmarc=pass (policy=reject) header.from=example", "pass for the TLD (a public suffix)"},
		{parent, "dmarc=pass (policy=reject) header.from=mail.bank.example", "subdomain pass for the parent (reverse)"},
		{deep, "dmarc=pass (policy=reject) header.from=mail.bank.example", "intermediate parent, not the organisational domain"},
		{uk, "dmarc=pass (policy=reject) header.from=co.uk", "multi-level public suffix"},
		{sender, "dmarc=pass header.from=bank.example header.from=bank.example", "two header.from values"},
		{sender, "dmarc=pass header.from=bank.example; dmarc=fail header.from=bank.example", "an org-domain pass beside a fail"},
	} {
		msg, reason := arrive(c.from, ar(c.dmarc))
		refused(msg, reason, "unauthenticated", c.why)
	}
	// The sender wrote the org-domain pass BELOW the provider's own header.
	msg, reason := arrive(sender,
		"Authentication-Results: mx.trusted.example; dmarc=none header.from=mail.bank.example",
		ar("dmarc=pass (policy=reject) header.from=bank.example"))
	refused(msg, reason, "unauthenticated", "forged org-domain pass below the topmost trusted header")
	// Two From headers: the MTA's From domain is unknowable, so no binding.
	msg, reason = arrive(sender, "From: x@attacker.example", ar("dmarc=pass (policy=reject) header.from=bank.example"))
	refused(msg, reason, "ambiguous", "ambiguous From with an org-domain pass")

	for _, c := range []struct{ from, dmarc, why string }{
		{sender, "dmarc=pass (policy=reject) header.from=bank.example", "subdomain sender, org-domain pass"},
		{deep, "dmarc=pass (policy=reject) header.from=bank.example", "deeper subdomain, org-domain pass"},
		{uk, "dmarc=pass (policy=reject) header.from=example.co.uk", "multi-level suffix, org-domain pass"},
	} {
		msg, reason := arrive(c.from, ar(c.dmarc))
		if reason != "placed" {
			t.Errorf("%s: arrival reason = %s, want placed", c.why, reason)
		}
		if n := f.lpLabelBy(t, msg, areas["Forums"], store.LabelByLearned); n != 1 {
			t.Errorf("%s: learned = %d, want 1 (placed)", c.why, n)
		}
		var keyValue string
		if err := f.super.QueryRow(context.Background(),
			`SELECT key_value FROM app.learned_placements WHERE message_id=$1`, msg.String()).Scan(&keyValue); err != nil || keyValue != c.from {
			t.Errorf("%s: placement log key_value = %q (%v), want the subdomain address %q", c.why, keyValue, err, c.from)
		}
	}
}

// TestLearnedPlacementUnconfiguredRefusesAnAmbiguousFrom: the floor cannot
// bind to a domain, so the ambiguous-From check is all it has.
func TestLearnedPlacementUnconfiguredRefusesAnAmbiguousFrom(t *testing.T) {
	f := newFixture(t)
	areas := f.lpAreas(t)
	f.lpFile(t, "alerts@bank.example", 2, areas, "Updates")
	f.assertDeskPlaced(t, f.lpIngest(t, "alerts@bank.example", "From: x@attacker.example"),
		areas, "unconfigured, two From headers")
	if msg := f.lpIngest(t, "alerts@bank.example"); f.lpLabelBy(t, msg, areas["Updates"], store.LabelByLearned) != 1 {
		t.Error("positive control: a single From is placed in unconfigured mode")
	}
}
