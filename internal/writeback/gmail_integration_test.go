// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package writeback

// THE GMAIL WRITEBACK SWITCH, THROUGH THE RUNNER AND THE REAL STORE (#52,
// ADR-047 §3, increment 5b).
//
// With store.GmailWritebackReady false (as shipped), a Gmail OAuth account
// is ingest-only twice over: EnqueueIntent bears every intent 'suppressed',
// and the runner's drain gate refuses to dial even for a row that is somehow
// pending. The positive control is the same account, the same row, with the
// hold lifted through WithGmailWritebackHold: the runner dials the fake
// Gmail, the label STORE lands, and the copy's gm_labels baseline moves by
// exactly that delta in PostgreSQL — after which a reading of the Gmail that
// Mailroom just wrote changes nothing (echo suppression, end to end).

import (
	"context"
	"reflect"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"
	"mailroom/internal/sync/gmailtest"
)

type gmailLive struct {
	*liveFixture
	gmail, allMail ulid.ULID
	msg, prov      ulid.ULID
	srv            *gmailtest.Server
	fakeUID        uint32
}

func newGmailLive(t *testing.T) *gmailLive {
	t.Helper()
	f := newLiveFixture(t)
	g := &gmailLive{liveFixture: f, gmail: store.NewULID(), allMail: store.NewULID(), prov: store.NewULID()}
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO app.accounts (id, user_id, name, provider_profile, auth_kind, imap_host, smtp_host,
	        credentials_ct, credentials_dek, writeback_enabled)
	      VALUES ($1, $2, 'gmail', 'gmail', 'oauth2', 'imap.gmail.com', 'smtp.gmail.com', '\x00', $3, true)`,
		g.gmail.String(), f.userID.String(), store.NewULID().String())
	// An active grant: without one the account is awaiting reconnect and the
	// runner holds its intents for that reason instead (ADR-046 §5).
	exec(`INSERT INTO app.oauth_grants (account_id, user_id, provider, subject, address)
	      VALUES ($1, $2, 'google', $3, 'you@example.com')`,
		g.gmail.String(), f.userID.String(), "sub-"+g.gmail.String())
	for _, m := range []struct{ id, name, su string }{
		{store.NewULID().String(), "INBOX", ""},
		{g.allMail.String(), "[Gmail]/All Mail", `\All`},
		{store.NewULID().String(), "[Gmail]/Trash", `\Trash`},
		{store.NewULID().String(), "[Gmail]/Spam", `\Junk`},
		{store.NewULID().String(), "[Gmail]/Drafts", `\Drafts`},
	} {
		exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
		      VALUES ($1, $2, $3, $4, $4, NULLIF($5, ''))`, m.id, f.userID.String(), g.gmail.String(), m.name, m.su)
	}
	g.srv = gmailtest.New(t)
	g.srv.StandardMailboxes()
	fm := g.srv.Append("[Gmail]/All Mail", &gmailtest.Message{MsgID: 4242, Labels: []string{`\Inbox`}, Body: []byte("x")})
	g.fakeUID = fm.UID
	g.msg = f.seedMessage(t, "gmail writeback")
	exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity,
	        state, gm_msgid, gm_labels)
	      VALUES ($1, $2, $3, $4, $5, $6, 1, 'live', '4242', ARRAY['\Inbox'])`,
		g.prov.String(), f.userID.String(), g.msg.String(), g.gmail.String(), g.allMail.String(), int64(fm.UID))
	return g
}

// queueLabel enqueues a label add the way Email/set does, and returns the id
// and the state it was born in.
func (g *gmailLive) queueLabel(t *testing.T) (ulid.ULID, string) {
	t.Helper()
	var id ulid.ULID
	if err := g.st.UserTx(context.Background(), g.userID, func(tx *store.UserTx) error {
		var err error
		id, err = tx.EnqueueIntent(context.Background(), store.IntentRow{
			MessageID: &g.msg, AccountID: g.gmail, Op: store.IntentAddKeyword,
			Payload: []byte(`{"keyword":"work","label":"Work"}`), IdempotencyKey: "gmail-label-" + store.NewULID().String(),
			Source: store.IntentByUser,
		})
		return err
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id, g.intentState(t, id)
}

// forcePending puts a row in the drainable state, as the superuser — the
// state no path may produce for a held Gmail account, which is what the drain
// gate is for.
func (g *gmailLive) forcePending(t *testing.T, id ulid.ULID) {
	t.Helper()
	if _, err := g.super.Exec(context.Background(),
		`UPDATE app.writeback_intents SET state = 'pending' WHERE id = $1`, id.String()); err != nil {
		t.Fatal(err)
	}
}

func (g *gmailLive) baseline(t *testing.T) []string {
	t.Helper()
	var got []string
	if err := g.super.QueryRow(context.Background(),
		`SELECT gm_labels FROM app.message_provenance WHERE id = $1`, g.prov.String()).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestGmailSwitchOffNeverDials: the shipped state.
func TestGmailSwitchOffNeverDials(t *testing.T) {
	if store.GmailWritebackReady {
		t.Skip("the switch is on; this test describes the held state")
	}
	g := newGmailLive(t)
	id, born := g.queueLabel(t)
	if born != "suppressed" {
		t.Errorf("a Gmail intent was born %q with the switch off, want suppressed", born)
	}
	g.forcePending(t, id)

	r := NewRunner(g.st, exploderDial(t), nil, nil)
	res, err := r.DrainAccount(context.Background(), g.userID, g.gmail, imapsync.Config{})
	if err != nil {
		t.Fatalf("DrainAccount: %v", err)
	}
	if res.Claimed != 0 || res.Applied != 0 {
		t.Errorf("held drain did work: %+v", res)
	}
	if got := g.intentState(t, id); got != "suppressed" {
		t.Errorf("state after the held drain = %q, want suppressed (the ledger)", got)
	}
	// Zero commands of any kind reached the fake: nothing dialled it.
	if c := g.srv.Commands(); len(c) != 0 {
		t.Errorf("commands reached the Gmail server with the switch off: %q", c)
	}
}

// TestGmailSwitchLiftedWritesAndMovesTheBaseline is the positive control.
func TestGmailSwitchLiftedWritesAndMovesTheBaseline(t *testing.T) {
	g := newGmailLive(t)
	id, _ := g.queueLabel(t)
	g.forcePending(t, id)
	cfg := g.srv.Listen(imapsync.TLSInsecure)
	dialed := 0
	dial := func(ctx context.Context, _ imapsync.Config) (imapsync.SyncSession, error) {
		dialed++
		return imapsync.Dial(ctx, cfg)
	}
	r := NewRunner(g.st, dial, nil, nil,
		WithGmailWritebackHold(func(string, string) bool { return false }))
	res, err := r.DrainAccount(context.Background(), g.userID, g.gmail, imapsync.Config{})
	if err != nil {
		t.Fatalf("DrainAccount: %v", err)
	}
	if dialed != 1 || res.Applied != 1 || g.intentState(t, id) != "applied" {
		t.Fatalf("dialed %d, result %+v, state %s", dialed, res, g.intentState(t, id))
	}
	if got := g.srv.MutatingCommands(); !reflect.DeepEqual(got, []string{`UID STORE 1 +X-GM-LABELS ("Work")`}) {
		t.Errorf("mutating commands = %q", got)
	}
	if got := g.baseline(t); !reflect.DeepEqual(got, []string{`\Inbox`, "Work"}) {
		t.Errorf("baseline = %q, want [\\Inbox Work]", got)
	}

	// The next sweep reads the Gmail Mailroom just wrote: nothing changes.
	labels := g.srv.Message("[Gmail]/All Mail", g.fakeUID).Labels
	var r2 store.GmailApplyResult
	if err := g.st.UserTx(context.Background(), g.userID, func(tx *store.UserTx) error {
		var err error
		r2, err = tx.ApplyGmailObservation(context.Background(), g.msg, store.GmailObservation{
			MailboxID: g.allMail, UIDValidity: 1, UID: int64(g.fakeUID), MsgID: 4242, Labels: labels})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r2.Changed || r2.LabelsAdded != 0 {
		t.Errorf("the reading of Mailroom's own write was applied as a Gmail change: %+v", r2)
	}
}
