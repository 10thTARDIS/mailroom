//go:build integration

package api

import (
	"context"
	"strings"
	"testing"

	"mailroom/internal/store"
)

// ⚠ WHAT THIS FILE GUARDS, AND WHY A STORE-LEVEL TEST WOULD NOT (#351).
//
// Spec §6 is unambiguous: "Junk is read-only with one exception: 'Not junk'
// must write back (move to INBOX) so users can rescue false positives."
//
// It did not work, and the shape of the failure is why these tests go through
// the API rather than the store. $junk is DERIVED from app.messages.is_junk on
// read, exactly like $seen/$flagged/$answered/$draft. But setKeyword had
// explicit cases for those four and none for $junk, so $junk fell through to
// default:, failed the label lookup, and landed in RemoveKeywords — which edits
// the keywords ARRAY, where $junk has never lived.
//
// So Email/set {"keywords/$junk": false} returned success, appended a change_log
// row, and enqueued a writeback intent, while the message stayed junk. A test
// calling ApplyEmailUpdate directly would have passed throughout: the store was
// never the broken half.
//
// PROBE: delete the `case store.KeywordJunk:` arm in setKeyword and
// TestNotJunkActuallyClearsTheJunkVerdict fails on the surviving $junk keyword.

// markJunk sets the provider verdict the way ingest does — directly on the
// column. Nothing in the JMAP write path could set it before this change, which
// is the whole point.
func markJunk(t *testing.T, h *apiHarness, f apiFixture) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET is_junk = true WHERE user_id = $1 AND id = $2`,
		f.UserID.String(), f.MessageID.String()); err != nil {
		t.Fatalf("seed junk verdict: %v", err)
	}
}

func keywordsOf(t *testing.T, h *apiHarness, f apiFixture) map[string]any {
	t.Helper()
	res := call(t, h.api.emailGet, f.UserID, map[string]any{
		"ids": []string{f.MessageID.String()}, "properties": []string{"keywords"}})
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get returned %d messages, want 1", len(list))
	}
	msg, _ := list[0].(map[string]any)
	kw, _ := msg["keywords"].(map[string]any)
	return kw
}

func TestNotJunkActuallyClearsTheJunkVerdict(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	markJunk(t, h, a)

	// Positive control: the fixture really is junk before we touch it. Without
	// this the test would pass just as happily against a message that was never
	// junk in the first place.
	if _, ok := keywordsOf(t, h, a)[store.KeywordJunk]; !ok {
		t.Fatalf("fixture is not junk before the update; the test cannot observe the fix")
	}

	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"keywords/" + store.KeywordJunk: nil},
		},
	})
	if notUpdated, _ := res["notUpdated"].(map[string]any); len(notUpdated) != 0 {
		t.Fatalf("notUpdated = %v, want the removal to be accepted", notUpdated)
	}

	// THE ASSERTION THAT WAS FAILING SILENTLY.
	if _, still := keywordsOf(t, h, a)[store.KeywordJunk]; still {
		t.Error("$junk is still set after \"not junk\": Email/set reported success and changed nothing")
	}
}

// Spec §6 makes the move the ham signal ("otherwise the move itself is the
// signal"), so clearing junk without relocating the message would satisfy the
// letter and lose the point.
func TestNotJunkMovesTheMessageToTheInbox(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	markJunk(t, h, a)

	ctx := context.Background()
	if _, err := h.super.Exec(ctx,
		`UPDATE app.messages SET moved_mailbox_id = $3 WHERE user_id = $1 AND id = $2`,
		a.UserID.String(), a.MessageID.String(), a.TrashID.String()); err != nil {
		t.Fatalf("park the message outside the inbox: %v", err)
	}

	call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"keywords/" + store.KeywordJunk: nil},
		},
	})

	var moved string
	if err := h.super.QueryRow(ctx,
		`SELECT COALESCE(moved_mailbox_id, '') FROM app.messages WHERE user_id = $1 AND id = $2`,
		a.UserID.String(), a.MessageID.String()).Scan(&moved); err != nil {
		t.Fatalf("read placement: %v", err)
	}
	if moved == a.TrashID.String() {
		t.Error("the message did not move off its old placement; \"not junk\" must move it to the inbox (spec §6)")
	}
}

// The other half of §6: junk is READ-ONLY. The verdict belongs to the provider
// and arrives through ingest, so accepting a local "this is spam" would put
// Mailroom out of step with the provider at the next sync — which is precisely
// how the old fall-through behaved.
func TestSettingJunkIsRefusedRatherThanSilentlyAccepted(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"keywords/" + store.KeywordJunk: true},
		},
	})
	notUpdated, _ := res["notUpdated"].(map[string]any)
	if len(notUpdated) != 1 {
		t.Fatalf("notUpdated = %v, want the set to be refused", notUpdated)
	}
	// The refusal has to explain the way out, or a client author is stuck.
	entry, _ := notUpdated[a.MessageID.String()].(map[string]any)
	desc, _ := entry["description"].(string)
	if !strings.Contains(strings.ToLower(desc), "not junk") {
		t.Errorf("refusal = %q, want it to point at the supported operation", desc)
	}
	if _, nowJunk := keywordsOf(t, h, a)[store.KeywordJunk]; nowJunk {
		t.Error("the refused set still marked the message junk")
	}
}

// fileAsJunkLikeMigadu puts the fixture message where the provider this project
// runs against puts spam: in a \Junk FOLDER, with is_junk left false and no junk
// keyword. Observed in practice (#528): the Junk-folder messages
// all had is_junk = false — ingest derives it only from a Junk/$Junk IMAP keyword,
// which Migadu never sets. It returns the Junk mailbox and an Archive mailbox.
func fileAsJunkLikeMigadu(t *testing.T, h *apiHarness, f apiFixture) (junkID, archiveID string) {
	t.Helper()
	ctx := context.Background()
	junkID, archiveID = store.NewULID().String(), store.NewULID().String()
	for _, mb := range []struct{ id, name, use string }{
		{junkID, "Junk", `\Junk`},
		{archiveID, "Archive", `\Archive`},
	} {
		if _, err := h.super.Exec(ctx,
			`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
			 VALUES ($1,$2,$3,$4,$4,$5)`,
			mb.id, f.UserID.String(), f.AccountID.String(), mb.name, mb.use); err != nil {
			t.Fatalf("seed %s mailbox: %v", mb.name, err)
		}
	}
	if _, err := h.super.Exec(ctx,
		`UPDATE app.message_provenance SET mailbox_id = $3 WHERE user_id = $1 AND message_id = $2`,
		f.UserID.String(), f.MessageID.String(), junkID); err != nil {
		t.Fatalf("file the message into Junk: %v", err)
	}
	return junkID, archiveID
}

func mailboxIDsOf(t *testing.T, h *apiHarness, f apiFixture) map[string]any {
	t.Helper()
	res := call(t, h.api.emailGet, f.UserID, map[string]any{
		"ids": []string{f.MessageID.String()}, "properties": []string{"mailboxIds", "keywords"}})
	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get returned %d messages, want 1", len(list))
	}
	msg, _ := list[0].(map[string]any)
	ids, _ := msg["mailboxIds"].(map[string]any)
	return ids
}

// The web client's "Not junk" (#528) sends exactly this patch: remove the verdict
// AND name the inbox (web/src/core/junk.ts notJunkPatch — the mailbox key is what
// lets the offline queue roll a refusal back). This proves that patch rescues a
// message the way the real provider files spam, and that the provider-side move
// is enqueued — the move IS the ham signal (spec §6).
func TestNotJunkWithTheWebClientPatchRescuesAFolderFiledJunkMessage(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	junkID, _ := fileAsJunkLikeMigadu(t, h, a)

	// Positive control: in Junk by placement, and NOT by keyword — the shape a
	// keyword-gated control would never have offered to rescue.
	before := mailboxIDsOf(t, h, a)
	if _, ok := before[junkID]; !ok || len(before) != 1 {
		t.Fatalf("fixture mailboxIds = %v, want only the Junk mailbox %s", before, junkID)
	}
	if _, tagged := keywordsOf(t, h, a)[store.KeywordJunk]; tagged {
		t.Fatalf("fixture carries $junk; it must be placement-only to model the provider")
	}

	res := call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{
				"keywords/" + store.KeywordJunk:    nil,
				"mailboxIds/" + a.InboxID.String(): true,
			},
		},
	})
	if notUpdated, _ := res["notUpdated"].(map[string]any); len(notUpdated) != 0 {
		t.Fatalf("notUpdated = %v, want the rescue accepted", notUpdated)
	}

	after := mailboxIDsOf(t, h, a)
	if _, ok := after[a.InboxID.String()]; !ok || len(after) != 1 {
		t.Errorf("mailboxIds after \"Not junk\" = %v, want only the inbox %s", after, a.InboxID)
	}

	var moves int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.writeback_intents
		  WHERE user_id = $1 AND message_id = $2 AND op = 'move' AND payload->>'mailbox' = 'INBOX'`,
		a.UserID.String(), a.MessageID.String()).Scan(&moves); err != nil {
		t.Fatalf("read writeback intents: %v", err)
	}
	if moves != 1 {
		t.Errorf("move-to-INBOX intents = %d, want 1: without it the rescue never reaches the provider", moves)
	}
}

// WHY THE WEB CLIENT'S INBOX DROP NO LONGER SENDS `untrash` FOR EVERY MESSAGE (#528).
//
// A drop on the Inbox used to call untrash, whose patch is `mailboxIds/<trash>:
// false`. On a message that is NOT in Trash the server reads that as a Restore with
// no destination, and a Restore with no destination goes to Archive. So dragging a
// false positive out of Junk onto the Inbox filed it into Archive. This pins the
// server behaviour web/src/core/dragDrop.ts inboxDropPlan is built around; if it
// changes, that comment is stale and the drop plan may be simplifiable — it is not
// a behaviour anyone should be relying on.
func TestUntrashPatchOnAJunkMessageFilesItIntoArchive(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	_, archiveID := fileAsJunkLikeMigadu(t, h, a)

	call(t, h.api.emailSet, a.UserID, map[string]any{
		"update": map[string]any{
			a.MessageID.String(): map[string]any{"mailboxIds/" + a.TrashID.String(): false},
		},
	})

	after := mailboxIDsOf(t, h, a)
	if _, ok := after[archiveID]; !ok {
		t.Errorf("mailboxIds after the old drop patch = %v, want Archive %s — the server no longer "+
			"archives a destination-less Restore, so revisit inboxDropPlan's comment", after, archiveID)
	}
}
