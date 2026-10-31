// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package drawer

// The negative control for the drawer sweep's login-level master switch
// (migration 00048, #443): without this test, DeskPreferences'
// DrawerSweepEnabled would be a column nobody actually checks.
// internal/store's own drawer suite (drawer_sweep_integration_test.go)
// covers every SQL-level predicate DrawerCandidates and ArchiveByDrawer
// apply; this file is the one place that runs the REAL worker (Sweep, not a
// re-implementation of its logic) against a true candidate with the switch
// at its default OFF, then flips it on and proves the identical candidate
// is archived without being re-seeded.
//
// The fixture below is a trimmed version of internal/store's own
// drawerFixture — that one is unexported to package store, and an internal
// test file in package store cannot import this package (it imports store),
// so the minimal object graph Sweep needs is rebuilt here directly against
// the exported store surface, the same way internal/api's own integration
// harness does rather than reaching into internal/store's private fixture.

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// sweepFixture is one user with an account carrying an INBOX and an Archive
// mailbox, one area whose drawer auto-archives, and one message old enough
// and scored low enough to be a true drawer candidate.
type sweepFixture struct {
	super  *pgxpool.Pool
	st     *store.Store
	userID ulid.ULID

	accountID ulid.ULID
	inbox     ulid.ULID
	archive   ulid.ULID
}

func newSweepFixture(t *testing.T) *sweepFixture {
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

	f := &sweepFixture{
		super:     super,
		st:        store.NewFromPool(app),
		userID:    store.NewULID(),
		accountID: store.NewULID(),
		inbox:     store.NewULID(),
		archive:   store.NewULID(),
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+f.userID.String(), f.userID.String()+"@example.com")
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'primary', 'mail.example.com', 'smtp.example.com', '\x00', $3)`,
		f.accountID.String(), f.userID.String(), store.NewULID().String())
	for _, m := range []struct {
		id   ulid.ULID
		name string
		role string
	}{
		{f.inbox, "INBOX", ""},
		{f.archive, "Archive", `\Archive`},
	} {
		exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use, feeds_desk)
		      VALUES ($1, $2, $3, $4, $4, NULLIF($5, ''), false)`,
			m.id.String(), f.userID.String(), f.accountID.String(), m.name, m.role)
	}
	return f
}

// createArea creates an area whose drawer auto-archives with the given
// period and returns its label id (what a message needs to carry to resolve
// into the area).
func (f *sweepFixture) createArea(t *testing.T, name string, days int) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	var areaID ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		on := true
		var e error
		areaID, e = tx.CreateArea(ctx, store.AreaRow{Name: name, AutoArchive: &on, AutoArchiveDays: &days})
		return e
	}); err != nil {
		t.Fatalf("create area %s: %v", name, err)
	}
	var views []store.AreaView
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		var e error
		views, e = tx.GetAreas(ctx, []ulid.ULID{areaID})
		return e
	}); err != nil {
		t.Fatalf("get area %s: %v", name, err)
	}
	if len(views) != 1 {
		t.Fatalf("get area %s: got %d views, want 1", name, len(views))
	}
	return views[0].LabelID
}

// seedCandidate inserts one message old enough and scored low enough (0.1,
// against the 0.5 default threshold) to be a true drawer candidate: labelled
// into areaLabel, unflagged, unpinned, unsnoozed, sitting in INBOX.
func (f *sweepFixture) seedCandidate(t *testing.T, subject string, daysOld int, areaLabel ulid.ULID) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := store.NewULID()
	hash := sha256.Sum256([]byte(subject + id.String()))
	threadID := store.NewULID()
	receivedAt := time.Now().UTC().AddDate(0, 0, -daysOld)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed message %s: %v", subject, err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 10, 20, $3)`, f.userID.String(), hash[:], store.NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
		threadID.String(), f.userID.String(), subject)
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes)
	      VALUES ($1, $2, $3, $4, $5, $6, 10)`,
		id.String(), f.userID.String(), hash[:], threadID.String(), subject, receivedAt)
	exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
	      VALUES ($1, $2, $3, $4, $5, 1, 1, 'live')`,
		store.NewULID().String(), f.userID.String(), id.String(), f.accountID.String(), f.inbox.String())
	exec(`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
	      VALUES ($1, $2, $3, 'user')`,
		f.userID.String(), id.String(), areaLabel.String())

	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		return tx.UpsertTriageScore(ctx, id, 0.1, "test-model", nil)
	}); err != nil {
		t.Fatalf("seed score for %s: %v", subject, err)
	}
	return id
}

func (f *sweepFixture) movedMailbox(t *testing.T, msg ulid.ULID) string {
	t.Helper()
	var m string
	if err := f.super.QueryRow(context.Background(),
		`SELECT COALESCE(moved_mailbox_id, '') FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), msg.String()).Scan(&m); err != nil {
		t.Fatalf("moved_mailbox_id: %v", err)
	}
	return m
}

// TestSweepRespectsTheMasterSwitch is the negative control for the whole
// login-level switch: it runs the real Worker.Sweep against a true
// candidate with DrawerSweepEnabled at its default false and asserts
// nothing moved, then flips the switch on and asserts the SAME candidate is
// archived on the very next run, with no re-seeding in between.
func TestSweepRespectsTheMasterSwitch(t *testing.T) {
	f := newSweepFixture(t)
	ctx := context.Background()

	areaLabel := f.createArea(t, "Sweeps", 30)
	msg := f.seedCandidate(t, "candidate", 31, areaLabel)

	w := NewWorker(f.st, time.Hour, nil)

	// The switch defaults to OFF (migration 00048): Sweep must archive
	// NOTHING, even though a true candidate is sitting right there.
	res, err := w.Sweep(ctx, f.userID)
	if err != nil {
		t.Fatalf("sweep with the switch off: %v", err)
	}
	if !res.Disabled {
		t.Error("Sweep did not report Disabled with the switch at its default OFF")
	}
	if res.Candidates != 0 || res.Archived != 0 {
		t.Errorf("Sweep with the switch off = %+v, want Candidates=0 Archived=0", res)
	}
	if got := f.movedMailbox(t, msg); got != "" {
		t.Errorf("moved_mailbox_id = %q with the switch off, want empty — the switch must have kept the message in place", got)
	}

	// Flip the switch on: the SAME candidate, still sitting there, is now
	// archived without needing to be re-seeded or re-scanned by hand.
	on := true
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		return tx.UpdateDeskPreferences(ctx, store.DeskPreferencesPatch{DrawerSweepEnabled: &on})
	}); err != nil {
		t.Fatalf("enable drawer sweep: %v", err)
	}
	res, err = w.Sweep(ctx, f.userID)
	if err != nil {
		t.Fatalf("sweep with the switch on: %v", err)
	}
	if res.Disabled {
		t.Error("Sweep reported Disabled with the switch on")
	}
	if res.Archived != 1 {
		t.Errorf("Sweep with the switch on archived = %d, want 1", res.Archived)
	}
	if got := f.movedMailbox(t, msg); got != f.archive.String() {
		t.Errorf("moved_mailbox_id = %q after the switch flipped on, want the Archive mailbox %q", got, f.archive.String())
	}
}
