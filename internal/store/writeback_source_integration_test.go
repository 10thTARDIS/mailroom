// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// What asked for each intent (ADR-028 amendment A2).
// Shares ledgerFx with writeback_ledger_integration_test.go.

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// TestEveryMutationClassRecordsItsSource drives the real enqueue paths and
// asserts that the source the report breaks down by is the right one, not merely
// present: a flag, a keyword and a move through Email/set are 'user' by default
// and 'rule' when a rule asked; a draft APPEND is 'draft'; a purge EXPUNGE is
// 'purge'. The source scan (TestEveryIntentRowLiteralSetsASource) proves every
// site SAYS something; this proves what it says.
//
// NEGATIVE CONTROL: have enqueueEmailIntent ignore its source argument and the
// keyword rows come out NULL; the count of NULLs is asserted to be zero.
func TestEveryMutationClassRecordsItsSource(t *testing.T) {
	f := newLedgerFx(t)
	ctx := context.Background()
	seen := true
	ruleID := NewULID()

	drafts := seedMailbox(t, ctx, f.h, f.a, "Drafts", `\Drafts`)
	if err := f.h.appStore().UserTx(ctx, f.uid, func(tx *UserTx) error {
		if _, _, err := tx.ApplyEmailUpdate(ctx, f.msg, EmailUpdate{
			Seen: &seen, AddKeywords: []string{"byhand"}, MoveToRole: "archive",
		}); err != nil {
			return err
		}
		if _, _, err := tx.ApplyEmailUpdate(ctx, f.msg, EmailUpdate{
			AddKeywords: []string{"byrule"}, LabelSource: RuleLabelSource(ruleID),
		}); err != nil {
			return err
		}
		id, hash := makeDraft(t, ctx, tx, f.a, drafts, "a draft body")
		if err := tx.EnqueueAppendIntent(ctx, id, f.acct, "Drafts", []string{`\Draft`}, nil, hash, "d@example.com"); err != nil {
			return err
		}
		_, err := tx.EnqueuePurgeExpungeIntents(ctx, f.msg, []ulid.ULID{f.acct})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	got := map[string]int{}
	rows, err := f.h.super.Query(ctx, `
		SELECT op || '/' || COALESCE(source, '<NULL>'), count(*)
		  FROM app.writeback_intents WHERE user_id = $1 GROUP BY 1`, f.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatal(err)
		}
		got[k] = n
	}
	for k := range got {
		if strings.Contains(k, "<NULL>") {
			t.Errorf("an intent was enqueued with no source: %s (%d)", k, got[k])
		}
	}
	for _, want := range []string{
		"+flag/user", "+kw/user", "move/user", // the by-hand update
		"+kw/rule",      // the rule's keyword
		"append/draft",  // the draft mirror
		"expunge/purge", // the purge sweep
	} {
		if got[want] == 0 {
			t.Errorf("no %s intent among %v", want, got)
		}
	}
}
