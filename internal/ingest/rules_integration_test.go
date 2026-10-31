// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// End-to-end coverage for the T0 rules engine at ingest (mailroom#32, spec §8):
// a rule "from alice@example.com → addLabel Y" attaches label Y to a message
// ingested from alice, and a "skipInbox" rule keeps an arriving message out of
// the inbox (canonically diverted to Archive) WHILE the message stays stored —
// never dropped. Runs against the throwaway Postgres provisioned by
// scripts/integration-test.sh; skips when the env is absent.

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// markMailboxRoles sets SPECIAL-USE on the fixture mailboxes so INBOX resolves to
// role "inbox" and Archive to role "archive" (the fixture seeds them without a
// special_use). skipInbox needs both to resolve.
func (f *fixture) markMailboxRoles(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for id, su := range map[ulid.ULID]string{f.mailboxA: `\Inbox`, f.mailboxB: `\Archive`} {
		if _, err := f.super.Exec(ctx,
			`UPDATE app.mailboxes SET special_use = $2 WHERE id = $1`, id.String(), su); err != nil {
			t.Fatalf("set special_use: %v", err)
		}
	}
}

func TestIngestAppliesAddLabelRule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A label Y and a rule "from alice@example.com → addLabel Y".
	var labelID ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var err error
		labelID, err = tx.CreateLabel(ctx, store.LabelRow{Name: "Y-" + store.NewULID().String()})
		if err != nil {
			return err
		}
		_, err = tx.CreateRule(ctx, store.RuleRow{
			Name: "from-alice", Enabled: true,
			Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "alice@example.com"},
			Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: labelID.String()}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	raw := rawMessage("Hello labelled", "rule-label-1@example.com", nil)
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 501, nil))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !res.Created {
		t.Fatal("expected a new canonical message")
	}

	// The rule attached label Y to the freshly-ingested message.
	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
		res.MessageID.String(), labelID.String()); n != 1 {
		t.Errorf("message_labels for (msg,Y) = %d, want 1 (rule addLabel did not fire)", n)
	}
	// The message still exists and is not trashed — a rule never loses mail.
	if n := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND trashed_at IS NULL`,
		res.MessageID.String()); n != 1 {
		t.Errorf("message present-and-untrashed = %d, want 1", n)
	}
}

// A rule's ActionFlag stamps flagged_at in the same ingest transaction as the
// flag itself (#444): the message arrives unflagged (no \Flagged in its
// remote flags), so flagged_at must read the moment the RULE flagged it, not
// the message's (much older, fixed) received_at.
func TestIngestAppliesFlagRule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err := tx.CreateRule(ctx, store.RuleRow{
			Name: "flag-from-alice", Enabled: true,
			Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "alice@example.com"},
			Actions:    []rules.Action{{Type: rules.ActionFlag}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	raw := rawMessage("Flag me", "rule-flag-1@example.com", nil)
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 701, nil))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !res.Created {
		t.Fatal("expected a new canonical message")
	}

	if n := f.scalar(t, `SELECT is_flagged::int FROM app.messages WHERE id=$1`, res.MessageID.String()); n != 1 {
		t.Fatal("rule ActionFlag did not set is_flagged")
	}
	var flaggedAt, receivedAt time.Time
	if err := f.super.QueryRow(ctx,
		`SELECT flagged_at, received_at FROM app.messages WHERE id=$1`,
		res.MessageID.String()).Scan(&flaggedAt, &receivedAt); err != nil {
		t.Fatalf("read flagged_at/received_at: %v", err)
	}
	if flaggedAt.IsZero() {
		t.Fatal("flagged_at is NULL after a rule flagged the message on ingest")
	}
	if d := time.Since(flaggedAt); d < 0 || d > 10*time.Second {
		t.Errorf("flagged_at = %v, want ~now (the moment the rule fired), off by %v", flaggedAt, d)
	}
	// received_at is the fixture's fixed 2023 InternalDate — a rule flagging a
	// message it just ingested must not be confused with a message that arrived
	// already flagged (InsertMessage's own received_at lower bound).
	if !flaggedAt.After(receivedAt.Add(time.Hour)) {
		t.Errorf("flagged_at (%v) is not clearly after received_at (%v) — "+
			"looks like the ingest-arrival lower bound leaked into the rule path", flaggedAt, receivedAt)
	}
}

func TestIngestSkipInboxRuleDivertsButKeepsMessage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.markMailboxRoles(t)

	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		_, err := tx.CreateRule(ctx, store.RuleRow{
			Name: "skip-from-alice", Enabled: true,
			Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "alice@example.com"},
			Actions:    []rules.Action{{Type: rules.ActionSkipInbox}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	raw := rawMessage("Skip me", "rule-skip-1@example.com", nil)
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, 601, nil))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// The message is stored and NOT trashed (skipInbox never deletes).
	if n := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND trashed_at IS NULL`,
		res.MessageID.String()); n != 1 {
		t.Fatalf("message present-and-untrashed = %d, want 1", n)
	}
	// Its canonical placement was diverted to Archive (out of the inbox).
	var moved string
	if err := f.super.QueryRow(ctx,
		`SELECT COALESCE(moved_mailbox_id::text,'') FROM app.messages WHERE id=$1`,
		res.MessageID.String()).Scan(&moved); err != nil {
		t.Fatal(err)
	}
	if moved != f.mailboxB.String() {
		t.Errorf("moved_mailbox_id = %q, want Archive %q", moved, f.mailboxB.String())
	}

	// JMAP membership confirms it: not in the inbox, but present in Archive.
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		inboxIDs, _, err := tx.QueryEmails(ctx, store.EmailQuery{InMailbox: &f.mailboxA})
		if err != nil {
			return err
		}
		for _, id := range inboxIDs {
			if id == res.MessageID {
				t.Error("skipInbox message is still in the INBOX")
			}
		}
		archiveIDs, _, err := tx.QueryEmails(ctx, store.EmailQuery{InMailbox: &f.mailboxB})
		if err != nil {
			return err
		}
		found := false
		for _, id := range archiveIDs {
			if id == res.MessageID {
				found = true
			}
		}
		if !found {
			t.Error("skipInbox message is not visible in Archive")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
