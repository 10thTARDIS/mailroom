// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// THE DIFFERENTIAL TEST ADR-022 RESTS ON.
//
// A message's tab is resolved in two languages: the Go resolver (ResolveTabs)
// answers `Email.categoryTab`, and the SQL predicate (tabFilterSQL) answers
// `Email/query?inTab`. They cannot share code across that boundary, and #155
// established what a second implementation costs — "a preview that can disagree
// with the engine is worse than none". Here the disagreement would be between
// what a tab strip COUNTS and what its list SHOWS.
//
// So the remedy is not shared code, it is a red build: for every tab, the set of
// messages the SQL predicate selects must equal the set the Go resolver assigns
// to that tab. Over a fixture deliberately built to hit every branch — mapped
// labels, several mapped labels needing the tie-break, an unmapped label, a
// stored classifier verdict, and a message with neither.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/tabs"
)

// tabFixture seeds one message with the label/verdict combination described.
type tabFixture struct {
	name string
	// labelTabs are the tabs of the labels attached to this message; "" attaches
	// a label with no tab mapping.
	labelTabs []tabs.Tab
	// classified is the stored classifier verdict; "" means none.
	classified tabs.Tab
	// want is the tab the message should resolve to with every tab enabled.
	want tabs.Tab
}

func TestTabResolverAgreesWithTheQueryPredicate(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "tabres-"+NewULID().String())
	userID := mustULID(t, u.ID)

	fixtures := []tabFixture{
		{name: "mapped label", labelTabs: []tabs.Tab{tabs.Promotions}, want: tabs.Promotions},
		{name: "mapped label beats a stored verdict",
			labelTabs: []tabs.Tab{tabs.Social}, classified: tabs.Updates, want: tabs.Social},
		{name: "tie-break: forums outranks updates",
			labelTabs: []tabs.Tab{tabs.Updates, tabs.Forums}, want: tabs.Forums},
		{name: "tie-break: social outranks promotions",
			labelTabs: []tabs.Tab{tabs.Promotions, tabs.Social}, want: tabs.Social},
		{name: "tie-break: any positive claim outranks primary",
			labelTabs: []tabs.Tab{tabs.Primary, tabs.Updates}, want: tabs.Updates},
		{name: "unmapped label falls through to the verdict",
			labelTabs: []tabs.Tab{""}, classified: tabs.Promotions, want: tabs.Promotions},
		{name: "no label, verdict only", classified: tabs.Forums, want: tabs.Forums},
		// Neither. Resolves to Primary by fold, and is INVISIBLE to a tab filter —
		// the honest cost of a nullable verdict column, asserted rather than
		// discovered.
		{name: "neither: no label, no verdict", want: tabs.Primary},
	}

	// Labels are shared across fixtures by tab, so a tie-break case attaches two
	// real labels rather than needing two label rows per message.
	labelFor := map[tabs.Tab]ulid.ULID{}
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		for _, tab := range append(append([]tabs.Tab{}, tabs.All...), "") {
			name := "L-" + string(tab)
			if tab == "" {
				name = "L-unmapped"
			}
			id, e := tx.CreateLabel(ctx, LabelRow{Name: name, MirrorToIMAP: false, Tab: tab})
			if e != nil {
				return e
			}
			labelFor[tab] = id
		}
		return nil
	}); err != nil {
		t.Fatalf("create labels: %v", err)
	}

	byID := map[ulid.ULID]tabFixture{}
	for i, f := range fixtures {
		msgID := NewULID()
		sum := sha256.Sum256([]byte(fmt.Sprintf("tabres-%s-%d", u.Tag, i)))
		if _, err := h.super.Exec(ctx,
			`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			 VALUES ($1,$2,10,20,$3)`,
			u.ID, sum[:], NewULID().String()); err != nil {
			t.Fatalf("seed blob: %v", err)
		}
		var classified any
		if f.classified != "" {
			classified = string(f.classified)
		}
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.messages (id, user_id, content_hash, subject, from_address,
			    received_at, size_bytes, classified_tab, classifier_version)
			VALUES ($1,$2,$3,$4,'sender@example.com',now(),10,$5::app.category_tab,$6)`,
			msgID.String(), u.ID, sum[:], f.name, classified, tabs.ClassifierVersion,
		); err != nil {
			t.Fatalf("seed message %q: %v", f.name, err)
		}
		for _, lt := range f.labelTabs {
			if _, err := h.super.Exec(ctx,
				`INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
				u.ID, msgID.String(), labelFor[lt].String()); err != nil {
				t.Fatalf("attach label to %q: %v", f.name, err)
			}
		}
		byID[msgID] = f
	}

	enabled := map[tabs.Tab]bool{}
	for _, tab := range tabs.All {
		enabled[tab] = true
	}

	// 1. The Go resolver produces the expected tab for every fixture.
	ids := make([]ulid.ULID, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	var resolved map[ulid.ULID]ResolvedTab
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		resolved, e = tx.ResolveTabs(ctx, ids, enabled)
		return e
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for id, f := range byID {
		got, ok := resolved[id]
		if !ok {
			t.Errorf("%s: no resolution returned", f.name)
			continue
		}
		if got.Tab != f.want {
			t.Errorf("%s: Go resolver = %q, want %q", f.name, got.Tab, f.want)
		}
		wantClassified := len(f.labelTabs) > 0 && f.labelTabs[0] != "" || f.classified != ""
		if got.Classified != wantClassified {
			t.Errorf("%s: Classified = %v, want %v", f.name, got.Classified, wantClassified)
		}
	}

	// 2. THE ACTUAL DIFFERENTIAL: for every tab, the SQL predicate must select
	// exactly the messages the Go resolver put there.
	for _, tab := range tabs.All {
		var goSet []string
		for id, r := range resolved {
			// A message nothing has classified resolves to Primary in Go but is
			// invisible to the SQL predicate — the documented consequence of a
			// nullable verdict (ADR-022). Excluded here and asserted separately
			// below, so the difference is stated rather than papered over.
			if !r.Classified {
				continue
			}
			if r.Tab == tab {
				goSet = append(goSet, id.String())
			}
		}
		var sqlSet []string
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			got, _, e := tx.QueryEmails(ctx, EmailQuery{InTab: []Tab{tab}, Limit: 100})
			if e != nil {
				return e
			}
			for _, id := range got {
				sqlSet = append(sqlSet, id.String())
			}
			return nil
		}); err != nil {
			t.Fatalf("query inTab=%s: %v", tab, err)
		}
		sort.Strings(goSet)
		sort.Strings(sqlSet)
		if fmt.Sprint(goSet) != fmt.Sprint(sqlSet) {
			t.Errorf("tab %q DISAGREEMENT:\n  Go resolver: %v\n  SQL filter : %v", tab, goSet, sqlSet)
		}
	}

	// 3. The unclassified message really is invisible to every tab filter. This is
	// the cost ADR-022 accepts, and a test that did not assert it would let a
	// future "helpful" COALESCE-to-primary slip in unnoticed and quietly put
	// un-backfilled mail in Primary.
	var unclassified ulid.ULID
	for id, f := range byID {
		if f.name == "neither: no label, no verdict" {
			unclassified = id
		}
	}
	for _, tab := range tabs.All {
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			got, _, e := tx.QueryEmails(ctx, EmailQuery{InTab: []Tab{tab}, Limit: 100})
			if e != nil {
				return e
			}
			for _, id := range got {
				if id == unclassified {
					t.Errorf("the unclassified message was selected by inTab=%s; "+
						"it has no verdict and no mapped label, so no tab can claim it", tab)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("query inTab=%s: %v", tab, err)
		}
	}

	// And the counter that makes that population visible to an operator.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		stale, missing, e := tx.MessagesNeedingClassification(ctx, tabs.ClassifierVersion)
		if e != nil {
			return e
		}
		if stale != 0 {
			t.Errorf("stale = %d, want 0 — every seeded row carries the current version", stale)
		}
		// The fixture message with no verdict, plus seedUser's own message.
		if missing < 1 {
			t.Errorf("missing = %d, want at least 1 — the unclassified fixture must be counted", missing)
		}
		return nil
	}); err != nil {
		t.Fatalf("count needing classification: %v", err)
	}
}
