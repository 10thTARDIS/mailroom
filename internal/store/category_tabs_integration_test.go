// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Category tabs (#219, migration 00028) — the per-user state layer.
//
// Four properties are worth pinning, and only one of them is about tabs:
//
//  1. MATERIALISATION IS EXACTLY-ONCE. The five rows are created on first read
//     (there is nowhere earlier to seed them — see the migration), and each
//     carries a change_log row. A repeat read that emitted more would make every
//     client resync forever; one that emitted none the first time would leave the
//     rows invisible to /changes.
//  2. THE TWO VOCABULARIES AGREE. tabs.All and the app.category_tab enum are two
//     copies of one list, in two languages. If they drift, either the classifier
//     can name a tab the database refuses, or the database holds one the binary
//     cannot read.
//  3. PRIMARY CANNOT BE DISABLED, at BOTH layers — the Go check for the message,
//     the CHECK constraint as the thing that actually holds.
//  4. THE MAPPING IS A JOIN. Re-pointing one label moves its mail on the next
//     read, with no backfill and nothing stale. That is the whole reason a
//     message's tab is not a column.

import (
	"context"
	"crypto/sha256"
	"errors"
	"sort"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/tabs"
)

func TestCategoryTabsMaterialiseExactlyOnce(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "ct-"+NewULID().String())
	userID := mustULID(t, u.ID)

	// The fixture seeds ONE row (promotions) so the RLS inventory has one. That is
	// also the sharpest starting point for this test: materialisation must fill in
	// the other four and leave the seeded one alone, which is what ON CONFLICT DO
	// NOTHING is for. A test starting from zero rows would not exercise it at all.
	var (
		first     []CategoryTabView
		stateOne  int64
		stateTwo  int64
		stateZero int64
	)
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		if stateZero, e = tx.ChangeState(ctx, ObjectCategoryTab); e != nil {
			return e
		}
		if first, e = tx.ListCategoryTabs(ctx); e != nil {
			return e
		}
		stateOne, e = tx.ChangeState(ctx, ObjectCategoryTab)
		return e
	}); err != nil {
		t.Fatalf("first list: %v", err)
	}

	if len(first) != len(tabs.All) {
		t.Fatalf("first list returned %d tabs, want %d", len(first), len(tabs.All))
	}
	// Four created, not five: the seeded promotions row was already there.
	if got := stateOne - stateZero; got != 4 {
		t.Errorf("first read appended %d change_log rows, want 4 (five tabs, one pre-seeded)", got)
	}

	var second []CategoryTabView
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		if second, e = tx.ListCategoryTabs(ctx); e != nil {
			return e
		}
		stateTwo, e = tx.ChangeState(ctx, ObjectCategoryTab)
		return e
	}); err != nil {
		t.Fatalf("second list: %v", err)
	}
	if stateTwo != stateOne {
		t.Errorf("a repeat read moved the state %d -> %d; materialisation must be idempotent",
			stateOne, stateTwo)
	}
	// Ids are stable across reads, or a client's synced object would change
	// identity under it.
	for i := range first {
		if first[i].ID != second[i].ID || first[i].Tab != second[i].Tab {
			t.Errorf("tab %d changed identity between reads: %v then %v", i, first[i], second[i])
		}
	}

	// The seeded row survived with ITS OWN position (2), rather than being reset to
	// the default arrangement — a materialisation that overwrote existing rows would
	// silently undo the user's arrangement every time they opened the app.
	for _, v := range first {
		if v.Tab == tabs.Promotions && v.Position != 2 {
			t.Errorf("promotions position = %d, want the pre-existing 2 (materialisation overwrote it)",
				v.Position)
		}
	}
}

// TestCategoryTabVocabularyMatchesTheEnum is the drift guard between Go and SQL.
// It reads the enum out of the live database rather than restating it, so this
// fails when either side gains a value the other lacks.
func TestCategoryTabVocabularyMatchesTheEnum(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var inDB []string
	rows, err := h.super.Query(ctx,
		`SELECT unnest(enum_range(NULL::app.category_tab))::text`)
	if err != nil {
		t.Fatalf("read enum: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan enum: %v", err)
		}
		inDB = append(inDB, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read enum: %v", err)
	}

	inGo := make([]string, 0, len(tabs.All))
	for _, tab := range tabs.All {
		inGo = append(inGo, string(tab))
	}
	sort.Strings(inDB)
	sort.Strings(inGo)
	if len(inDB) != len(inGo) {
		t.Fatalf("app.category_tab has %v, tabs.All has %v", inDB, inGo)
	}
	for i := range inDB {
		if inDB[i] != inGo[i] {
			t.Errorf("vocabulary drift: app.category_tab %v vs tabs.All %v", inDB, inGo)
		}
	}
}

func TestCategoryTabUpdateAndChangeLog(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "ctu-"+NewULID().String())
	userID := mustULID(t, u.ID)

	byTab := map[tabs.Tab]CategoryTabView{}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.ListCategoryTabs(ctx)
		if e != nil {
			return e
		}
		for _, v := range views {
			byTab[v.Tab] = v
		}
		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}

	// Disabling a non-Primary tab is ordinary, and appends exactly one change row.
	off := false
	pos := 4
	var before, after int64
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		if before, e = tx.ChangeState(ctx, ObjectCategoryTab); e != nil {
			return e
		}
		found, e := tx.UpdateCategoryTab(ctx, byTab[tabs.Forums].ID,
			CategoryTabPatch{Enabled: &off, Position: &pos})
		if e != nil {
			return e
		}
		if !found {
			t.Error("forums tab not found")
		}
		after, e = tx.ChangeState(ctx, ObjectCategoryTab)
		return e
	}); err != nil {
		t.Fatalf("disable forums: %v", err)
	}
	if after-before != 1 {
		t.Errorf("disabling a tab appended %d change_log rows, want exactly 1", after-before)
	}

	// A no-op patch is a TRUE no-op: same values in, no change row out. Without
	// this, a client that re-sent its current state on every render would drive an
	// endless /changes loop across every other device.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		start, e := tx.ChangeState(ctx, ObjectCategoryTab)
		if e != nil {
			return e
		}
		found, e := tx.UpdateCategoryTab(ctx, byTab[tabs.Forums].ID,
			CategoryTabPatch{Enabled: &off, Position: &pos})
		if e != nil {
			return e
		}
		if !found {
			t.Error("forums tab not found on the no-op patch")
		}
		end, e := tx.ChangeState(ctx, ObjectCategoryTab)
		if e != nil {
			return e
		}
		if end != start {
			t.Errorf("a no-op patch appended %d change_log rows, want 0", end-start)
		}
		return nil
	}); err != nil {
		t.Fatalf("no-op patch: %v", err)
	}

	// The fold map reflects it.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		enabled, e := tx.EnabledTabs(ctx)
		if e != nil {
			return e
		}
		if enabled[tabs.Forums] {
			t.Error("forums still reads as enabled after being disabled")
		}
		if !enabled[tabs.Primary] {
			t.Error("primary must always read as enabled")
		}
		if got := tabs.Fold(tabs.Forums, enabled); got != tabs.Primary {
			t.Errorf("mail in a disabled tab folded to %q, want primary", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("enabled tabs: %v", err)
	}
}

// TestPrimaryTabCannotBeDisabled proves it at BOTH layers. The Go check is the
// message a client gets; the CHECK constraint is what actually holds — and the
// second half of this test is the one that matters, because a Go-only guard is
// exactly the kind that a later caller bypasses without noticing.
func TestPrimaryTabCannotBeDisabled(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "ctp-"+NewULID().String())
	userID := mustULID(t, u.ID)

	var primaryID ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.ListCategoryTabs(ctx)
		if e != nil {
			return e
		}
		for _, v := range views {
			if v.Tab == tabs.Primary {
				primaryID = v.ID
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}

	off := false
	err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.UpdateCategoryTab(ctx, primaryID, CategoryTabPatch{Enabled: &off})
		return e
	})
	if !errors.Is(err, ErrPrimaryTabRequired) {
		t.Errorf("disabling primary through the store: %v, want ErrPrimaryTabRequired", err)
	}

	// Negative control on the constraint itself, as the superuser so RLS and the
	// store layer are both out of the way: if this write SUCCEEDS, the Go check
	// above is the only thing standing between a caller and an unrenderable
	// mailbox, and the test that "proves" the rule proves nothing about the data.
	if _, err := h.super.Exec(ctx,
		`UPDATE app.category_tabs SET enabled = false WHERE id = $1`, primaryID.String()); err == nil {
		t.Error("the database accepted a disabled primary tab: category_tabs_primary_always_enabled is not holding")
	}
}

// TestMessageTabMappingIsAJoin is the property the whole design rests on:
// re-pointing a label moves its mail with no backfill. It also pins the
// distinction the report depends on — unlabelled mail and labelled-but-unmapped
// mail are different populations with different fixes.
func TestMessageTabMappingIsAJoin(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "ctm-"+NewULID().String())
	userID := mustULID(t, u.ID)
	messageID := mustULID(t, u.MessageID)

	// seedUser already puts a label on this message, and that is useful rather than
	// in the way: it is a SECOND, UNMAPPED label riding along, so every assertion
	// below about the mapped one is also an assertion that an unmapped sibling
	// contributes nothing.
	var baseline int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.message_labels WHERE user_id = $1 AND message_id = $2`,
		u.ID, u.MessageID).Scan(&baseline); err != nil {
		t.Fatalf("count seeded labels: %v", err)
	}

	var labelID ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		id, e := tx.CreateLabel(ctx, LabelRow{Name: "Shopping", MirrorToIMAP: true})
		if e != nil {
			return e
		}
		labelID = id
		return nil
	}); err != nil {
		t.Fatalf("create label: %v", err)
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
		u.ID, u.MessageID, labelID.String()); err != nil {
		t.Fatalf("attach label: %v", err)
	}

	mappingFor := func(t *testing.T, id ulid.ULID) MessageTabMapping {
		t.Helper()
		var out MessageTabMapping
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			page, e := tx.MessageTabMappingsAfter(ctx, ulid.ULID{}, 100)
			if e != nil {
				return e
			}
			for _, m := range page {
				if m.ID == id {
					out = m
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("scan mappings: %v", err)
		}
		return out
	}

	// LABELLED, BUT THE LABEL IS UNMAPPED. Not the same as unlabelled: this one is
	// one mapping edit away, not a classifier's problem, and the report has to be
	// able to tell them apart.
	got := mappingFor(t, messageID)
	if got.Labels != baseline+1 {
		t.Errorf("label count = %d, want %d", got.Labels, baseline+1)
	}
	if len(got.Mapped) != 0 {
		t.Errorf("mapped tabs = %v, want none while the label is unmapped", got.Mapped)
	}
	if _, _, ok := tabs.FromLabels(got.Mapped); ok {
		t.Error("an unmapped label decided a tab")
	}

	// Map the label. NOTHING touches the message, and no backfill runs.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, e := tx.UpdateLabel(ctx, labelID, LabelPatch{SetTab: true, Tab: tabs.Updates})
		if e != nil {
			return e
		}
		if !found {
			t.Error("label not found")
		}
		return nil
	}); err != nil {
		t.Fatalf("map label: %v", err)
	}

	got = mappingFor(t, messageID)
	winner, contested, ok := tabs.FromLabels(got.Mapped)
	if !ok || winner != tabs.Updates {
		t.Errorf("after mapping the label, message resolves to (%q, %v), want updates", winner, ok)
	}
	if len(contested) != 0 {
		t.Errorf("contested = %v, want none: the message has one label", contested)
	}

	// Re-point it. Same message row, different tab, still no backfill — this is
	// the "I can rearrange later" requirement, exercised.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.UpdateLabel(ctx, labelID, LabelPatch{SetTab: true, Tab: tabs.Promotions})
		return e
	}); err != nil {
		t.Fatalf("re-map label: %v", err)
	}
	if winner, _, _ := tabs.FromLabels(mappingFor(t, messageID).Mapped); winner != tabs.Promotions {
		t.Errorf("after re-pointing the label the message resolves to %q, want promotions", winner)
	}

	// Unmap it: back to undecided, not to Primary.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.UpdateLabel(ctx, labelID, LabelPatch{SetTab: true, Tab: ""})
		return e
	}); err != nil {
		t.Fatalf("unmap label: %v", err)
	}
	if _, _, ok := tabs.FromLabels(mappingFor(t, messageID).Mapped); ok {
		t.Error("unmapping the label left the message decided")
	}
}

// TestMessageTabFactsReadsTheFunctionSignalColumns is the guard against the
// quietest possible failure in this feature.
//
// THE ABSENCE OF AN ERROR PROVES NOTHING HERE. If MessageTabFactsAfter forgot a
// column, or COALESCEd one to the wrong side, the classifier would receive empty
// facts for every message, file the entire mailbox as Primary via
// `no_bulk_markers`, and NOTHING would error — not the scan, not the classify,
// not the report, which would print a clean table of confident zeroes. That is
// the #159/#155 shape exactly: a write path that fails while the read path
// renders perfectly.
//
// So this asks the question directly: what would I observe if the projection
// were broken? Answer: a Primary verdict. The test therefore inserts a message
// carrying migration 00027's signals and asserts the classifier reaches them
// THROUGH THE STORE — not that the columns scan, but that they arrive with
// enough fidelity to change the verdict.
func TestMessageTabFactsReadsTheFunctionSignalColumns(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "ctf-"+NewULID().String())
	userID := mustULID(t, u.ID)

	msgID := NewULID()
	sum := sha256.Sum256([]byte("tabfacts-" + u.Tag))
	hash := sum[:] // app.sha256 is a CHECKed domain: 32 bytes, not any []byte
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		 VALUES ($1, $2, 10, 20, $3)`,
		u.ID, hash, NewULID().String()); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.messages (id, user_id, content_hash, subject, from_address,
		    received_at, size_bytes, list_id, list_unsubscribe, list_unsubscribe_post,
		    auto_submitted, precedence, feedback_id, dkim_domains, return_path,
		    recipient_count, in_reply_to)
		VALUES ($1, $2, $3, 'A note from us', 'news@e.brand.example', now(), 10,
		        '<discuss.brand.example>', '<https://brand.example/u>',
		        'List-Unsubscribe=One-Click', 'auto-generated', 'bulk', 'a:b:c',
		        ARRAY['mcsv.net'], '<bounce-abc123def456@bounces.brand.example>', 1,
		        '<parent@example.com>')`,
		msgID.String(), u.ID, hash); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	var got MessageTabFacts
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		page, e := tx.MessageTabFactsAfter(ctx, ulid.ULID{}, 200)
		if e != nil {
			return e
		}
		for _, m := range page {
			if m.ID == msgID {
				got = m
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read tab facts: %v", err)
	}
	if got.ID != msgID {
		t.Fatal("the seeded message did not come back from MessageTabFactsAfter")
	}

	// Every signal migration 00027 stores, checked individually — a single
	// "facts are non-empty" assertion would pass with five of six missing.
	for _, c := range []struct{ name, got, want string }{
		{"subject", got.Facts.Subject, "A note from us"},
		{"from_address", got.Facts.FromAddress, "news@e.brand.example"},
		{"from_domain", got.Facts.FromDomain, "e.brand.example"},
		{"list_id", got.Facts.ListID, "<discuss.brand.example>"},
		{"list_unsubscribe", got.Facts.ListUnsubscribe, "<https://brand.example/u>"},
		{"list_unsubscribe_post", got.Facts.ListUnsubscribePost, "List-Unsubscribe=One-Click"},
		{"auto_submitted", got.Facts.AutoSubmitted, "auto-generated"},
		{"precedence", got.Facts.Precedence, "bulk"},
		{"feedback_id", got.Facts.FeedbackID, "a:b:c"},
		{"return_path", got.Facts.ReturnPath, "<bounce-abc123def456@bounces.brand.example>"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if len(got.Facts.DKIMDomains) != 1 || got.Facts.DKIMDomains[0] != "mcsv.net" {
		t.Errorf("dkim_domains = %v, want [mcsv.net]", got.Facts.DKIMDomains)
	}
	if !got.Facts.IsReply {
		t.Error("in_reply_to is set but IsReply is false")
	}
	// HasCorresponded is never populated — a tab decided by the user's own
	// behaviour would forfeit sender-independence, which is the whole premise.
	if got.Facts.HasCorresponded {
		t.Error("HasCorresponded was populated; tabs are decided from public facts only")
	}

	// AND THE VERDICT ACTUALLY MOVES. This is the assertion that a broken
	// projection cannot survive: with the signals present the message is Forums
	// (it carries List-Id); with them absent it would be Primary.
	if v := tabs.Classify(got.Facts); v.Tab != tabs.Forums || v.Rule != tabs.RuleListID {
		t.Errorf("classified as %q via %q, want forums via %q — the facts did not survive the read",
			v.Tab, v.Rule, tabs.RuleListID)
	}
}

// TestLabelTabRejectsUnknownValues: refused, not coerced. A mapping the operator
// set and the server quietly changed would move mail without saying so.
func TestLabelTabRejectsUnknownValues(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "ctl-"+NewULID().String())
	userID := mustULID(t, u.ID)

	err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.CreateLabel(ctx, LabelRow{Name: "Bogus", MirrorToIMAP: true, Tab: tabs.Tab("inbox")})
		return e
	})
	if !errors.Is(err, ErrLabelTabInvalid) {
		t.Errorf("create with an unknown tab: %v, want ErrLabelTabInvalid", err)
	}

	var labelID ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		id, e := tx.CreateLabel(ctx, LabelRow{Name: "Real", MirrorToIMAP: true, Tab: tabs.Social})
		labelID = id
		return e
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	err = s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.UpdateLabel(ctx, labelID, LabelPatch{SetTab: true, Tab: tabs.Tab("PRIMARY")})
		return e
	})
	if !errors.Is(err, ErrLabelTabInvalid) {
		t.Errorf("update with an unknown tab: %v, want ErrLabelTabInvalid", err)
	}

	// And the good value survived the rejected one.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		views, e := tx.GetLabels(ctx, []ulid.ULID{labelID})
		if e != nil {
			return e
		}
		if len(views) != 1 || views[0].Tab != tabs.Social {
			t.Errorf("label tab = %v, want social", views)
		}
		return nil
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
}
