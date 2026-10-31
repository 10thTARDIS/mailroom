// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// Integration tests for the untangle-labels mutation (mailroom#219), against a
// real Postgres with db/schema.sql applied (scripts/integration-test.sh).
//
// The parser tests (untanglelabels_test.go) pin what a plan file is allowed
// to look like. These pin what the command is allowed to DO to the database:
// it removes exactly the labels decide() computed, through
// store.ApplyEmailUpdate only, appending exactly one Email change_log row per
// message it actually changes (the load-bearing invariant) — and it leaves a
// message completely alone whenever any of the fail-closed checks fire,
// whether that is "fewer than two labels", "keep-label not present", or
// another user's message id (RLS makes the last one indistinguishable from
// "not found", which is exactly the point).

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// untangleHarness mirrors internal/api's apiHarness: a superuser pool for
// fixture seeding (bypasses RLS) and a Store backed by the mailroom_app pool
// — the exact production configuration the command runs through.
type untangleHarness struct {
	super *pgxpool.Pool
	store *store.Store
}

func newUntangleHarness(t *testing.T) *untangleHarness {
	t.Helper()
	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	t.Cleanup(app.Close)
	return &untangleHarness{super: super, store: store.NewFromPool(app)}
}

// seedUntangleUser inserts the minimal object graph untangle-labels needs: a
// user and nothing else — labels and messages are added per test by
// seedUntangleLabel / seedUntangleMessage so each test carries only the shape
// it is testing.
func seedUntangleUser(t *testing.T, super *pgxpool.Pool, tag string) ulid.ULID {
	t.Helper()
	id := store.NewULID()
	if _, err := super.Exec(context.Background(),
		`INSERT INTO app.users (id, oidc_subject, email, display_name) VALUES ($1,$2,$3,$4)`,
		id.String(), "oidc-"+tag, tag+"@example.com", "User "+tag); err != nil {
		t.Fatalf("seed user %s: %v", tag, err)
	}
	return id
}

// seedUntangleLabel creates a label (with its keyword mapping, matching how
// real labels are provisioned) and returns its id.
func seedUntangleLabel(t *testing.T, super *pgxpool.Pool, userID ulid.ULID, name string) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := store.NewULID()
	if _, err := super.Exec(ctx,
		`INSERT INTO app.labels (id, user_id, name) VALUES ($1,$2,$3)`,
		id.String(), userID.String(), name); err != nil {
		t.Fatalf("seed label %q: %v", name, err)
	}
	if _, err := super.Exec(ctx,
		`INSERT INTO app.label_keyword_map (user_id, label_id, keyword_atom) VALUES ($1,$2,$3)`,
		userID.String(), id.String(), "MailroomLabel-"+id.String()); err != nil {
		t.Fatalf("seed label keyword for %q: %v", name, err)
	}
	return id
}

// seedUntangleMessage inserts one message (and its backing blob row, the FK
// content_hash needs) for userID and returns its id.
func seedUntangleMessage(t *testing.T, super *pgxpool.Pool, userID ulid.ULID, subject string) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := store.NewULID()
	hash := sha256.Sum256([]byte("blob-" + id.String()))
	if _, err := super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		 VALUES ($1,$2,100,132,$3)`,
		userID.String(), hash[:], store.NewULID().String()); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := super.Exec(ctx,
		`INSERT INTO app.messages (id, user_id, content_hash, subject, from_name, from_address,
		    received_at, size_bytes, message_id_hint)
		 VALUES ($1,$2,$3,$4,'Friend','friend@example.com', now(), 100, $5)`,
		id.String(), userID.String(), hash[:], subject, "<"+id.String()+"@example.com>"); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	return id
}

// attachUntangleLabel attaches a label to a message with an explicit
// applied_by, bypassing ApplyEmailUpdate — fixture setup for "labels already
// on the message before this run", not the write path under test.
func attachUntangleLabel(t *testing.T, super *pgxpool.Pool, userID, msgID, labelID ulid.ULID) {
	t.Helper()
	if _, err := super.Exec(context.Background(),
		`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
		 VALUES ($1,$2,$3,$4)`,
		userID.String(), msgID.String(), labelID.String(), store.LabelByUser); err != nil {
		t.Fatalf("attach label: %v", err)
	}
}

// currentLabelNames reads a message's labels back through the store's own
// RLS-scoped path (LabelsOnMessages), resolves them to names, and returns a
// set — an independent check that the write did exactly what it claims,
// through the same reads the command itself uses.
func currentLabelNames(t *testing.T, st *store.Store, userID, msgID ulid.ULID) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if err := st.UserTx(context.Background(), userID, func(tx *store.UserTx) error {
		byMsg, err := tx.LabelsOnMessages(context.Background(), []ulid.ULID{msgID})
		if err != nil {
			return err
		}
		if len(byMsg[msgID]) == 0 {
			return nil
		}
		views, err := tx.GetLabels(context.Background(), byMsg[msgID])
		if err != nil {
			return err
		}
		for _, v := range views {
			out[v.Name] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("current label names: %v", err)
	}
	return out
}

// emailChangeState reads the Email change_log state for a user — the change
// count assertions compare this before and after a write.
func emailChangeState(t *testing.T, st *store.Store, userID ulid.ULID) int64 {
	t.Helper()
	var n int64
	if err := st.UserTx(context.Background(), userID, func(tx *store.UserTx) error {
		var err error
		n, err = tx.ChangeState(context.Background(), store.ObjectEmail)
		return err
	}); err != nil {
		t.Fatalf("email change state: %v", err)
	}
	return n
}

// A 3-label message reduced to 1 leaves EXACTLY the kept label, and appends
// EXACTLY ONE Email change_log row — not one per label removed. That count is
// the load-bearing invariant (CLAUDE.md): every mutation of a JMAP-visible
// object appends a change_log row in the SAME transaction as the mutation,
// and a multi-label removal is still one Email/set-shaped update, not
// several.
func TestUntangleReducesAThreeLabelMessageToOne(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	tag := "reduce-" + store.NewULID().String()
	userID := seedUntangleUser(t, h.super, tag)

	personal := seedUntangleLabel(t, h.super, userID, "Personal")
	receipts := seedUntangleLabel(t, h.super, userID, "Receipts")
	shopping := seedUntangleLabel(t, h.super, userID, "Shopping")
	msgID := seedUntangleMessage(t, h.super, userID, "Your order and a personal note")
	attachUntangleLabel(t, h.super, userID, msgID, personal)
	attachUntangleLabel(t, h.super, userID, msgID, receipts)
	attachUntangleLabel(t, h.super, userID, msgID, shopping)

	before := emailChangeState(t, h.store, userID)

	entries := []untanglePlanEntry{{Line: 1, MessageID: msgID, KeepLabel: "Receipts"}}
	r, err := runUntanglePlan(ctx, h.store, userID, entries, nil, true, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := r.refusedTotal(); n != 0 {
		t.Fatalf("refusedTotal = %d, want 0: %+v", n, r.results)
	}
	if r.results[0].outcome != outcomeChanged {
		t.Fatalf("outcome = %v, want outcomeChanged", r.results[0].outcome)
	}

	got := currentLabelNames(t, h.store, userID, msgID)
	if len(got) != 1 || !got["Receipts"] {
		t.Errorf("labels after = %v, want exactly {Receipts}", got)
	}

	after := emailChangeState(t, h.store, userID)
	if after != before+1 {
		t.Errorf("Email change state moved %d -> %d, want exactly +1 (one change_log row for the whole removal)",
			before, after)
	}
}

// A message with fewer than two labels is refused and left completely
// untouched: the command must be incapable of stripping a message to
// nothing, or of touching one that is already correctly labelled.
func TestUntangleRefusesASingleLabelledMessageUntouched(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	tag := "single-" + store.NewULID().String()
	userID := seedUntangleUser(t, h.super, tag)

	personal := seedUntangleLabel(t, h.super, userID, "Personal")
	msgID := seedUntangleMessage(t, h.super, userID, "Just one label")
	attachUntangleLabel(t, h.super, userID, msgID, personal)

	before := emailChangeState(t, h.store, userID)

	entries := []untanglePlanEntry{{Line: 1, MessageID: msgID, KeepLabel: "Personal"}}
	r, err := runUntanglePlan(ctx, h.store, userID, entries, nil, true, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.results[0].outcome != outcomeAlreadySingle {
		t.Fatalf("outcome = %v, want outcomeAlreadySingle", r.results[0].outcome)
	}
	if n := r.refusedTotal(); n != 1 {
		t.Errorf("refusedTotal = %d, want 1", n)
	}

	got := currentLabelNames(t, h.store, userID, msgID)
	if len(got) != 1 || !got["Personal"] {
		t.Errorf("labels after = %v, want the message untouched at {Personal}", got)
	}
	if after := emailChangeState(t, h.store, userID); after != before {
		t.Errorf("Email change state moved %d -> %d, want unchanged (nothing written)", before, after)
	}
}

// A keep-label the plan names but that is NOT currently on the message is
// refused and the message is left untouched — the plan may only ever choose
// among labels already present, never add one.
func TestUntangleRefusesWhenKeepLabelNotPresentUntouched(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	tag := "absent-" + store.NewULID().String()
	userID := seedUntangleUser(t, h.super, tag)

	personal := seedUntangleLabel(t, h.super, userID, "Personal")
	receipts := seedUntangleLabel(t, h.super, userID, "Receipts")
	seedUntangleLabel(t, h.super, userID, "Shopping") // exists for the user, just not on this message
	msgID := seedUntangleMessage(t, h.super, userID, "Two labels, wrong keep")
	attachUntangleLabel(t, h.super, userID, msgID, personal)
	attachUntangleLabel(t, h.super, userID, msgID, receipts)

	before := emailChangeState(t, h.store, userID)

	entries := []untanglePlanEntry{{Line: 1, MessageID: msgID, KeepLabel: "Shopping"}}
	r, err := runUntanglePlan(ctx, h.store, userID, entries, nil, true, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.results[0].outcome != outcomeRefused {
		t.Fatalf("outcome = %v, want outcomeRefused", r.results[0].outcome)
	}
	if n := r.refusedTotal(); n != 1 {
		t.Errorf("refusedTotal = %d, want 1", n)
	}

	got := currentLabelNames(t, h.store, userID, msgID)
	if len(got) != 2 || !got["Personal"] || !got["Receipts"] {
		t.Errorf("labels after = %v, want the message untouched at {Personal,Receipts}", got)
	}
	if after := emailChangeState(t, h.store, userID); after != before {
		t.Errorf("Email change state moved %d -> %d, want unchanged (nothing written)", before, after)
	}
}

// Another user's message id is refused exactly like a missing one — RLS
// (via store.GetEmails, scoped to the caller's own UserTx) makes "belongs to
// someone else" and "does not exist" indistinguishable by design, and that
// message must come out of this run completely untouched.
func TestUntangleRefusesAnotherUsersMessageID(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	tag := store.NewULID().String()
	userA := seedUntangleUser(t, h.super, "a-"+tag)
	userB := seedUntangleUser(t, h.super, "b-"+tag)

	// B's message, multi-labelled with B's own labels.
	bPersonal := seedUntangleLabel(t, h.super, userB, "Personal")
	bReceipts := seedUntangleLabel(t, h.super, userB, "Receipts")
	bMsgID := seedUntangleMessage(t, h.super, userB, "Belongs to B")
	attachUntangleLabel(t, h.super, userB, bMsgID, bPersonal)
	attachUntangleLabel(t, h.super, userB, bMsgID, bReceipts)

	// A plan run AS USER A naming B's label ("Receipts" also happens to exist
	// for A) and B's message id.
	seedUntangleLabel(t, h.super, userA, "Receipts")

	beforeA := emailChangeState(t, h.store, userA)
	beforeB := emailChangeState(t, h.store, userB)

	entries := []untanglePlanEntry{{Line: 1, MessageID: bMsgID, KeepLabel: "Receipts"}}
	r, err := runUntanglePlan(ctx, h.store, userA, entries, nil, true, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.results[0].outcome != outcomeRefused {
		t.Fatalf("outcome = %v, want outcomeRefused", r.results[0].outcome)
	}
	if r.results[0].reason != "message id not found for this user" {
		t.Errorf("reason = %q, want the not-found refusal", r.results[0].reason)
	}

	// B's message is completely untouched.
	got := currentLabelNames(t, h.store, userB, bMsgID)
	if len(got) != 2 || !got["Personal"] || !got["Receipts"] {
		t.Errorf("B's labels after = %v, want untouched at {Personal,Receipts}", got)
	}
	if afterA := emailChangeState(t, h.store, userA); afterA != beforeA {
		t.Errorf("A's Email change state moved %d -> %d, want unchanged", beforeA, afterA)
	}
	if afterB := emailChangeState(t, h.store, userB); afterB != beforeB {
		t.Errorf("B's Email change state moved %d -> %d, want unchanged", beforeB, afterB)
	}
}

// A dry run computes the identical decision an apply would — it just does
// not write. The label set and change state must be untouched afterward,
// and the outcome classification must already say "changed" so the report
// and the eventual --apply agree with each other.
func TestUntangleDryRunWritesNothing(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	tag := "dry-" + store.NewULID().String()
	userID := seedUntangleUser(t, h.super, tag)

	personal := seedUntangleLabel(t, h.super, userID, "Personal")
	receipts := seedUntangleLabel(t, h.super, userID, "Receipts")
	msgID := seedUntangleMessage(t, h.super, userID, "Dry run only")
	attachUntangleLabel(t, h.super, userID, msgID, personal)
	attachUntangleLabel(t, h.super, userID, msgID, receipts)

	before := emailChangeState(t, h.store, userID)

	entries := []untanglePlanEntry{{Line: 1, MessageID: msgID, KeepLabel: "Receipts"}}
	r, err := runUntanglePlan(ctx, h.store, userID, entries, nil, false /* apply */, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.results[0].outcome != outcomeChanged {
		t.Fatalf("outcome = %v, want outcomeChanged (would change)", r.results[0].outcome)
	}

	got := currentLabelNames(t, h.store, userID, msgID)
	if len(got) != 2 || !got["Personal"] || !got["Receipts"] {
		t.Errorf("labels after dry run = %v, want untouched at {Personal,Receipts}", got)
	}
	if after := emailChangeState(t, h.store, userID); after != before {
		t.Errorf("Email change state moved %d -> %d, want unchanged in dry run", before, after)
	}
}
