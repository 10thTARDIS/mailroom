// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

// End-to-end coverage for #442 increment 3's ingest hook: a LIVE-ingested
// message that classifies to a tab the default area plan feeds ends the
// ingest transaction carrying that area's own label, attributed to the desk,
// with NO extra change_log row beyond the Email `created` row ingest always
// appends — and a BULK ARCHIVE IMPORT (localMailbox set) gets no area label at
// all, because nobody has looked at a verdict on that mail yet (CLAUDE.md:
// build the report before the fix; mailroom-admin materialize-area-labels is
// that report for existing mail).

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// listIDMessage carries an RFC 2919 List-Id header and nothing else that would
// classify it — tabs.Classify's RuleListID fires on that header alone (and
// only that header), sending it to tabs.Forums, which the default area plan
// (areas.Default) feeds into an area named "Forums".
func listIDMessage(subject, msgID string) []byte {
	return []byte("From: list@example.com\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: <" + msgID + ">\r\n" +
		"List-Id: Announcements <announce.example.com>\r\n" +
		"Date: Mon, 12 Jul 2026 10:00:00 +0000\r\n\r\n" +
		"Body of " + subject + "\r\n")
}

// forumsOwnLabel materialises the caller's default area plan and returns the
// Forums area's own label id — the label ApplyAreaLabelForNewMessage is
// expected to write for a List-Id message.
func (f *fixture) forumsOwnLabel(t *testing.T) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	var views []store.AreaView
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		views, e = tx.ListAreas(ctx)
		return e
	}); err != nil {
		t.Fatalf("materialise default areas: %v", err)
	}
	for _, v := range views {
		if v.Name == "Forums" {
			return v.LabelID
		}
	}
	t.Fatalf("no Forums area in the default plan: %+v", views)
	return ulid.ULID{}
}

// TestIngestLiveMessageCarriesItsAreasOwnLabel is the LIVE-path half.
func TestIngestLiveMessageCarriesItsAreasOwnLabel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	forumsLabel := f.forumsOwnLabel(t)

	before := f.changeCount(t, "Email", "created")
	res, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		listIDMessage("Weekly digest", "area-live-1@example.com"), f.prov(f.mailboxA, 701, nil))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !res.Created {
		t.Fatal("expected a new canonical message")
	}

	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2 AND applied_by='desk'`,
		res.MessageID.String(), forumsLabel.String()); n != 1 {
		t.Errorf("message_labels (msg, Forums own label, applied_by=desk) = %d, want 1", n)
	}

	// ⚠ EXACTLY ONE Email `created` ROW, NOT TWO. The area label rides the SAME
	// row ApplyAreaLabelForNewMessage's own doc comment promises — a SECOND
	// Email change row here would mean the ingest hook silently grew its own
	// change_log write, which is exactly the load-bearing invariant #442
	// increment 3 must not touch.
	if got := f.changeCount(t, "Email", "created") - before; got != 1 {
		t.Errorf("Email created changes = %d, want 1 (the area label must not append its own)", got)
	}
}

// TestIngestArchiveImportGetsNoAreaLabel is the control: the identical facts,
// through the bulk-archive path (localMailbox set), must NOT be labelled —
// nobody has looked at this verdict, and a mass import must never label
// millions of messages by it.
func TestIngestArchiveImportGetsNoAreaLabel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	forumsLabel := f.forumsOwnLabel(t)

	res, err := f.ing.IngestArchive(ctx, f.userID, f.dek, f.dekID,
		listIDMessage("Weekly digest (archived)", "area-archive-1@example.com"),
		Archive{MailboxID: f.mailboxB})
	if err != nil {
		t.Fatalf("ingest archive: %v", err)
	}
	if !res.Created {
		t.Fatal("expected a new canonical message")
	}

	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2`,
		res.MessageID.String(), forumsLabel.String()); n != 0 {
		t.Errorf("archive-imported message carries the Forums own label (count=%d), want 0 — "+
			"bulk import must not label mail by a verdict nobody has reviewed", n)
	}
}

// TestIngestRuleFilingIntoAnAreaWinsOverTheDesk pins the ORDER the #708 item 3
// rule offer relies on for NEW mail: ingest runs the rules BEFORE the desk
// writes its placement label, and the desk labels only mail that resolved by
// TAB. So a rule "from alice -> Updates' own label" on a message whose tab
// verdict is Primary must leave it in Updates carrying NO desk label. Swap the
// two calls in ingest.go and the desk's Primary label lands first, Primary
// (position 0) wins the labels-first resolution, and the rule the user just
// accepted silently does nothing to the next message from that sender.
//
// The control message (another sender, same verdict, no rule) gets the desk's
// Primary label — without it "no desk label" could just mean the desk hook was
// not running in this fixture.
func TestIngestRuleFilingIntoAnAreaWinsOverTheDesk(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var primary, updates store.AreaView
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		views, err := tx.ListAreas(ctx)
		for _, v := range views {
			switch v.Name {
			case "Primary":
				primary = v
			case "Updates":
				updates = v
			}
		}
		if err != nil {
			return err
		}
		_, err = tx.CreateRule(ctx, store.RuleRow{
			Name: "alice is Updates", Enabled: true,
			Conditions: rules.Condition{Field: rules.FieldFrom, Op: rules.OpEquals, Value: "alice@example.com"},
			Actions:    []rules.Action{{Type: rules.ActionAddLabel, Label: updates.LabelID.String()}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if primary.Name == "" || updates.Name == "" || primary.Position >= updates.Position {
		t.Fatalf("premise: default plan with Primary before Updates, got %+v / %+v", primary, updates)
	}

	ruled, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		rawMessage("Hello from alice", "area-rule-1@example.com", nil), f.prov(f.mailboxA, 801, nil))
	if err != nil || !ruled.Created {
		t.Fatalf("ingest ruled message: created=%v err=%v", ruled.Created, err)
	}
	control, err := f.ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		[]byte("From: Carol <carol@example.com>\r\nTo: bob@example.com\r\nSubject: Hello from carol\r\n"+
			"Message-ID: <area-rule-control-1@example.com>\r\nDate: Mon, 12 Jul 2026 10:00:00 +0000\r\n\r\nHi\r\n"),
		f.prov(f.mailboxA, 802, nil))
	if err != nil || !control.Created {
		t.Fatalf("ingest control message: created=%v err=%v", control.Created, err)
	}

	// Premise: both carry a PRIMARY tab verdict, so the desk would place them
	// in Primary if nothing else decided.
	for _, id := range []ulid.ULID{ruled.MessageID, control.MessageID} {
		if n := f.scalar(t, `SELECT count(*) FROM app.messages WHERE id=$1 AND classified_tab='primary'`, id.String()); n != 1 {
			t.Fatalf("premise: message %s is not classified primary", id)
		}
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2 AND applied_by='desk'`,
		control.MessageID.String(), primary.LabelID.String()); n != 1 {
		t.Fatalf("control: the desk did not place the unruled message in Primary (count=%d)", n)
	}

	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND label_id=$2 AND applied_by='rule'`,
		ruled.MessageID.String(), updates.LabelID.String()); n != 1 {
		t.Errorf("the rule's Updates label is missing (count=%d)", n)
	}
	if n := f.scalar(t, `SELECT count(*) FROM app.message_labels WHERE message_id=$1 AND applied_by='desk'`,
		ruled.MessageID.String()); n != 0 {
		t.Errorf("the ruled message carries %d desk labels, want 0 — the desk wrote a placement over the rule's", n)
	}
	var area *ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		res, err := tx.ResolveAreas(ctx, []ulid.ULID{ruled.MessageID})
		area = res[ruled.MessageID].AreaID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if area == nil || *area != updates.ID {
		t.Errorf("the ruled message resolves to %v, want Updates %s", area, updates.ID)
	}
}
