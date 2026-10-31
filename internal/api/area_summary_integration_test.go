// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Area/getSummary over the wire (#442 increment 2), mirroring
// internal/store's TestAreaSummary (areas_resolve_integration_test.go) at the
// JMAP layer: the same shape of seeded mail (an Inbox message, a "feeds_desk"
// folder, a folder that does NOT feed the desk, and a second mail account),
// but driven through Area/set (to create the areas) and Area/getSummary (to
// read the counts) rather than the store API directly.

import (
	"context"
	"crypto/sha256"
	"testing"

	"mailroom/internal/store"
)

// mustExecAPI runs a seeding statement as the super user, failing the test on
// error — the api package's own version of the store package's mustExec.
func mustExecAPI(t *testing.T, h *apiHarness, sql string, args ...any) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestAreaGetSummary(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)

	// Two areas: Work (which will hold every test message below via its own
	// label) and Empty (which holds none — "a bounded/empty result must
	// still report its bound").
	res := call(t, h.api.areaSet, a.UserID, map[string]any{
		"create": map[string]any{
			"work":  map[string]any{"name": "Work"},
			"empty": map[string]any{"name": "Empty"},
		},
	})
	created, _ := res["created"].(map[string]any)
	work, ok := created["work"].(map[string]any)
	if !ok {
		t.Fatalf("Work area create failed: %#v", res)
	}
	workID, _ := work["id"].(string)
	workLabelID, _ := work["labelId"].(string)
	empty, ok := created["empty"].(map[string]any)
	if !ok {
		t.Fatalf("Empty area create failed: %#v", res)
	}
	emptyID, _ := empty["id"].(string)

	// A folder that feeds the desk like INBOX (#442/#446), and one that does
	// not.
	flaggedMailbox := store.NewULID().String()
	mustExecAPI(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, feeds_desk)
		VALUES ($1,$2,$3,'Flagged','Flagged',true)`, flaggedMailbox, a.UserID.String(), a.AccountID.String())
	unflaggedMailbox := store.NewULID().String()
	mustExecAPI(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, feeds_desk)
		VALUES ($1,$2,$3,'Unflagged','Unflagged',false)`, unflaggedMailbox, a.UserID.String(), a.AccountID.String())

	// A second mail account, so mailAccountId scoping has something to
	// exclude.
	otherAccount := store.NewULID().String()
	mustExecAPI(t, h, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1,$2,'other','mail.example.com','smtp.example.com',$3,$4)`,
		otherAccount, a.UserID.String(), []byte{0x01}, store.NewULID().String())
	otherMailbox := store.NewULID().String()
	mustExecAPI(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
		VALUES ($1,$2,$3,'INBOX','Inbox')`, otherMailbox, a.UserID.String(), otherAccount)

	seedMsg := func(tag string, score *float32) string {
		hash := sha256.Sum256([]byte("apisummary-" + a.UserID.String() + "-" + tag))
		id := store.NewULID().String()
		mustExecAPI(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,10,20,$3)`, a.UserID.String(), hash[:], store.NewULID().String())
		mustExecAPI(t, h, `INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes)
			VALUES ($1,$2,$3,$4,now(),10)`, id, a.UserID.String(), hash[:], tag)
		if score != nil {
			mustExecAPI(t, h, `INSERT INTO app.triage_scores (user_id, message_id, score, model_version)
				VALUES ($1,$2,$3,'v0')`, a.UserID.String(), id, *score)
		}
		mustExecAPI(t, h, `INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`,
			a.UserID.String(), id, workLabelID)
		return id
	}
	provenance := func(msg, account, mailbox string) {
		uid := testUID()
		mustExecAPI(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
			VALUES ($1,$2,$3,$4,$5,$6,1)`, store.NewULID().String(), a.UserID.String(), msg, account, mailbox, uid)
	}

	// Threshold stays at the default (0.5) for these: surfaceScore comfortably
	// above it, and ALSO comfortably below the 0.9 this test moves to later —
	// so the later threshold change visibly moves it from Surface to Drawer.
	surfaceScore := float32(0.7)
	drawerScore := float32(0.1)

	inInbox := seedMsg("in-inbox", &surfaceScore)
	provenance(inInbox, a.AccountID.String(), a.InboxID.String())

	inFlagged := seedMsg("in-flagged", &drawerScore)
	provenance(inFlagged, a.AccountID.String(), flaggedMailbox)

	inUnflagged := seedMsg("in-unflagged", &surfaceScore)
	provenance(inUnflagged, a.AccountID.String(), unflaggedMailbox)

	unscored := seedMsg("unscored", nil)
	provenance(unscored, a.AccountID.String(), a.InboxID.String())

	inOtherAccount := seedMsg("other-account", &surfaceScore)
	provenance(inOtherAccount, otherAccount, otherMailbox)

	find := func(list []any, id *string) map[string]any {
		for _, item := range list {
			obj, _ := item.(map[string]any)
			if id == nil {
				if obj["areaId"] == nil {
					return obj
				}
				continue
			}
			if got, _ := obj["areaId"].(string); got == *id {
				return obj
			}
		}
		return nil
	}

	// Default threshold (0.5, DeskPreferences' own default): Surface = the
	// Inbox message and the other-account message; Drawer = the flagged-
	// mailbox message; Unscored = the one unscored message. The unflagged-
	// mailbox message is EXCLUDED — its folder feeds neither INBOX nor the
	// desk.
	res = call(t, h.api.areaGetSummary, a.UserID, map[string]any{})
	list, _ := res["list"].([]any)
	work = find(list, &workID)
	if work == nil {
		t.Fatal("Work area missing from the summary")
	}
	if s, _ := work["surface"].(float64); s != 2 {
		t.Errorf("Work.surface = %v, want 2 (in-inbox and the other account's message; "+
			"the unflagged-mailbox message must be excluded)", work["surface"])
	}
	if d, _ := work["drawer"].(float64); d != 1 {
		t.Errorf("Work.drawer = %v, want 1 (in-flagged, via feeds_desk)", work["drawer"])
	}
	if u, _ := work["unscored"].(float64); u != 1 {
		t.Errorf("Work.unscored = %v, want 1", work["unscored"])
	}

	empty = find(list, &emptyID)
	if empty == nil {
		t.Fatal("Empty area (zero messages) missing from the summary — a bounded/empty result must still report its bound")
	}
	if s, _ := empty["surface"].(float64); s != 0 {
		t.Errorf("Empty.surface = %v, want 0", s)
	}
	if d, _ := empty["drawer"].(float64); d != 0 {
		t.Errorf("Empty.drawer = %v, want 0", d)
	}
	if u, _ := empty["unscored"].(float64); u != 0 {
		t.Errorf("Empty.unscored = %v, want 0", u)
	}

	// THE "areaId: null" ROW IS PRESENT: seedTwo's own fixture message
	// (a.MessageID) carries a's own seeded label, which feeds no area, so it
	// falls into Needs sorting.
	unsortedRow := find(list, nil)
	if unsortedRow == nil {
		t.Fatal("the Needs-sorting (areaId: null) row is missing from the summary")
	}
	if u, _ := unsortedRow["unscored"].(float64); u != 1 {
		t.Errorf("Needs-sorting.unscored = %v, want 1 (the fixture's own unlabelled-to-any-area message)", u)
	}

	if got, _ := res["threshold"].(float64); got != 0.5 {
		t.Errorf("default threshold = %v, want 0.5 (DeskPreferences' own default)", got)
	}

	// mailAccountId scoping: the other account's message drops out.
	res = call(t, h.api.areaGetSummary, a.UserID, map[string]any{"mailAccountId": a.AccountID.String()})
	list, _ = res["list"].([]any)
	scopedWork := find(list, &workID)
	if scopedWork == nil {
		t.Fatal("Work area missing from the account-scoped summary")
	}
	if s, _ := scopedWork["surface"].(float64); s != 1 {
		t.Errorf("account-scoped Work.surface = %v, want 1 (in-inbox only; "+
			"the other account's message must be excluded)", s)
	}

	// Changing the threshold to 0.9 via DeskPreferences/set MOVES both
	// surfaceScore (0.7) messages from Surface into Drawer.
	dp := call(t, h.api.deskPreferencesSet, a.UserID, map[string]any{
		"update": map[string]any{a.UserID.String(): map[string]any{"lineThreshold": 0.9}},
	})
	if _, ok := dp["updated"].(map[string]any)[a.UserID.String()]; !ok {
		t.Fatalf("setting lineThreshold was refused: %#v", dp)
	}

	res = call(t, h.api.areaGetSummary, a.UserID, map[string]any{})
	if got, _ := res["threshold"].(float64); got != 0.9 {
		t.Errorf("threshold after DeskPreferences/set = %v, want 0.9", got)
	}
	list, _ = res["list"].([]any)
	work = find(list, &workID)
	if work == nil {
		t.Fatal("Work area missing after the threshold change")
	}
	if s, _ := work["surface"].(float64); s != 0 {
		t.Errorf("Work.surface after raising the threshold to 0.9 = %v, want 0 (0.7 < 0.9)", s)
	}
	if d, _ := work["drawer"].(float64); d != 3 {
		t.Errorf("Work.drawer after raising the threshold to 0.9 = %v, want 3 "+
			"(both former-Surface messages moved down, joining the already-Drawer one)", d)
	}

	// An explicit threshold argument overrides DeskPreferences entirely.
	res = call(t, h.api.areaGetSummary, a.UserID, map[string]any{"threshold": 0.5})
	if got, _ := res["threshold"].(float64); got != 0.5 {
		t.Errorf("explicit threshold = %v, want 0.5 (the argument, not the 0.9 preference)", got)
	}
}
