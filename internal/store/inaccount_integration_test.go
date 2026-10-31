// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for the native inAccount filter on Email/query
// (mailroom#26, spec §7): an index-INDEPENDENT per-account view that matches a
// message with a LIVE provenance row in the named account — the exact
// definition liveAccountIDs uses, so a per-account view agrees with the
// writeback fan-out. Includes the required cross-user fail-closed probe
// (data-model §5): another user's account id must surface ZERO rows, never a
// 500. Runs against the throwaway Postgres; skips when the env is absent.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestQueryEmailsInAccount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.appStore()

	uid := NewULID()
	u := uid.String()
	mustExec(t, h, `INSERT INTO app.users (id, oidc_subject, email) VALUES ($1,$2,$3)`,
		u, "sub-"+u, u+"@example.com")

	acctA := NewULID().String()
	acctB := NewULID().String()
	mustExec(t, h, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1,$2,'A','mail.example.com','smtp.example.com',$3,$4)`,
		acctA, u, []byte{0x01}, NewULID().String())
	mustExec(t, h, `INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		VALUES ($1,$2,'B','mail.example.com','smtp.example.com',$3,$4)`,
		acctB, u, []byte{0x01}, NewULID().String())

	mbA := NewULID().String()
	mbB := NewULID().String()
	mustExec(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
		VALUES ($1,$2,$3,'INBOX','Inbox')`, mbA, u, acctA)
	mustExec(t, h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
		VALUES ($1,$2,$3,'INBOX','Inbox')`, mbB, u, acctB)

	// msg inserts one canonical message (with its own blob) and returns its id.
	msg := func(tagByte byte) string {
		hash := make([]byte, 32)
		hash[0] = tagByte
		mustExec(t, h, `INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
			VALUES ($1,$2,10,20,$3)`, u, hash, NewULID().String())
		id := NewULID().String()
		mustExec(t, h, `INSERT INTO app.messages (id, user_id, content_hash, subject, received_at, size_bytes)
			VALUES ($1,$2,$3,'m',now(),10)`, id, u, hash)
		return id
	}
	// prov attaches a LIVE provenance row (default state 'live') placing a message
	// in an account's mailbox with a per-mailbox-unique uid.
	prov := func(msgID, acct, mb string, uid int64) {
		mustExec(t, h, `INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
			VALUES ($1,$2,$3,$4,$5,$6,1)`, NewULID().String(), u, msgID, acct, mb, uid)
	}

	msgAOnly := msg(0xA1)
	prov(msgAOnly, acctA, mbA, 1)

	// The dedup case: ONE canonical message with a provenance row in EACH account
	// (content-hash dedup, ADR-001) — it lives in both A and B.
	msgShared := msg(0x5D)
	prov(msgShared, acctA, mbA, 2)
	prov(msgShared, acctB, mbB, 1)

	msgBOnly := msg(0xB1)
	prov(msgBOnly, acctB, mbB, 2)

	query := func(inAccount string) map[string]bool {
		t.Helper()
		var got map[string]bool
		if err := st.UserTx(ctx, uid, func(tx *UserTx) error {
			q := EmailQuery{}
			if inAccount != "" {
				id := ulid.MustParse(inAccount)
				q.InAccount = &id
			}
			ids, _, err := tx.QueryEmails(ctx, q)
			if err != nil {
				return err
			}
			got = make(map[string]bool, len(ids))
			for _, id := range ids {
				got[id.String()] = true
			}
			return nil
		}); err != nil {
			t.Fatalf("QueryEmails: %v", err)
		}
		return got
	}

	if all := query(""); len(all) != 3 {
		t.Fatalf("no filter: got %d ids, want all 3 (%v)", len(all), all)
	}
	if a := query(acctA); len(a) != 2 || !a[msgAOnly] || !a[msgShared] || a[msgBOnly] {
		t.Errorf("inAccount=A: got %v, want exactly {A-only, shared}", a)
	}
	if b := query(acctB); len(b) != 2 || !b[msgBOnly] || !b[msgShared] || b[msgAOnly] {
		t.Errorf("inAccount=B: got %v, want exactly {B-only, shared}", b)
	}
}

// The REQUIRED cross-user probe (data-model §5): inAccount is a leak surface
// because it takes a caller-supplied account id. A user querying with ANOTHER
// user's account id must get ZERO rows — the provenance rows for that account
// belong to the other user and are invisible under RLS, so the EXISTS predicate
// matches nothing. The positive control makes the probe meaningful: the same
// account id DOES surface a row for its owner, so a leak would be caught.
func TestQueryEmailsInAccountCrossUserFailClosed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.appStore()

	x := seedUser(t, h.super, "inacct-x-"+NewULID().String())
	y := seedUser(t, h.super, "inacct-y-"+NewULID().String())
	xID := ulid.MustParse(x.ID)
	yID := ulid.MustParse(y.ID)
	yAcct := ulid.MustParse(y.AccountID)

	// Positive control: Y, querying with Y's OWN account id, sees Y's message —
	// so the id is real and would surface a row if isolation leaked.
	if err := st.UserTx(ctx, yID, func(tx *UserTx) error {
		ids, _, err := tx.QueryEmails(ctx, EmailQuery{InAccount: &yAcct})
		if err != nil {
			return err
		}
		if len(ids) != 1 || ids[0].String() != y.MessageID {
			t.Fatalf("positive control: Y/inAccount=Y got %v, want [%s]", ids, y.MessageID)
		}
		return nil
	}); err != nil {
		t.Fatalf("positive control tx: %v", err)
	}

	// The probe: X, querying with Y's account id, gets nothing — fail-closed, and
	// no error (no separate ownership check that could 500).
	if err := st.UserTx(ctx, xID, func(tx *UserTx) error {
		ids, total, err := tx.QueryEmails(ctx, EmailQuery{InAccount: &yAcct})
		if err != nil {
			return err
		}
		if len(ids) != 0 || total != 0 {
			t.Fatalf("ISOLATION BREACH: X/inAccount=Y's account returned %d ids / total %d, want 0", len(ids), total)
		}
		return nil
	}); err != nil {
		t.Fatalf("probe tx: %v", err)
	}
}
