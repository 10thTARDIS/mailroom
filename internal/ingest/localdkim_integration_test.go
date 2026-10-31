// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/dkimverify/dkimtest"
	"mailroom/internal/store"
)

// #776: the local DKIM verdict is written in the transaction that creates the
// message, for live IMAP mail only, and rides the Email 'created' change row.
func TestLocalDKIMIsWrittenWithTheMessage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := dkimtest.NewKey(t)
	dns := func(_ context.Context, name string) ([]string, error) {
		if name == dkimtest.Name("example.com", "s1") {
			return []string{key.Record()}, nil
		}
		return nil, errors.New("servfail")
	}
	var seen []string
	ing := New(f.st, f.ing.blobs, WithTrustedAuthservIDs([]string{"mx.provider.example"}),
		WithLocalDKIM(func(r string) { seen = append(seen, r) }), withDKIMLookup(dns))
	signed := func(id string) []byte {
		return []byte(key.Sign(t, string(rawFrom("Alice <alice@example.com>", "s-"+id, id+"@example.com")), "example.com", "s1"))
	}
	row := func(id string) (n int, result string, aligned bool) {
		_ = f.super.QueryRow(ctx, `SELECT count(*), coalesce(max(result),''), coalesce(bool_or(aligned),false) FROM app.message_local_auth WHERE message_id=$1`, id).Scan(&n, &result, &aligned)
		return
	}

	// 1. A live message with no trusted header: one aligned pass row.
	live, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID, signed("live"), f.prov(f.mailboxA, 901, nil))
	if err != nil || !live.Created {
		t.Fatalf("ingest: %+v %v", live, err)
	}
	if n, res, al := row(live.MessageID.String()); n != 1 || res != "pass" || !al {
		t.Fatalf("local auth row: n=%d result=%q aligned=%v", n, res, al)
	}
	// It adds NO change_log row of its own: the message's only row is the Email
	// 'created' one.
	if n := f.scalar(t, `SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_id=$2`, f.userID.String(), live.MessageID.String()); n != 1 {
		t.Errorf("%d change_log rows name the message, want exactly the Email created row", n)
	}
	if len(seen) != 1 || seen[0] != LocalDKIMPass {
		t.Errorf("metric labels %v, want [pass]", seen)
	}

	// 2. A dedup hit writes nothing new and is not counted.
	again, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID, signed("live"), f.prov(f.mailboxB, 902, nil))
	if err != nil || again.Created {
		t.Fatalf("redelivery: %+v %v", again, err)
	}
	if len(seen) != 1 {
		t.Errorf("a dedup hit was counted: %v", seen)
	}

	// 3. A trusted header: skipped, no row.
	trusted, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		[]byte("Authentication-Results: mx.provider.example; dkim=pass\r\n"+string(signed("trusted"))), f.prov(f.mailboxA, 903, nil))
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _ := row(trusted.MessageID.String()); n != 0 || seen[len(seen)-1] != LocalDKIMSkippedTrusted {
		t.Errorf("trusted header: rows=%d labels=%v", n, seen)
	}

	// 4. A DNS failure: the message is stored, no row, counted not_checked.
	noKey := dkimtest.NewKey(t)
	failing, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID,
		[]byte(noKey.Sign(t, string(rawFrom("Bob <bob@example.org>", "dns", "dns@example.org")), "example.org", "s9")), f.prov(f.mailboxA, 904, nil))
	if err != nil || !failing.Created {
		t.Fatalf("a DNS failure must not fail the ingest: %+v %v", failing, err)
	}
	if n, _, _ := row(failing.MessageID.String()); n != 0 || seen[len(seen)-1] != LocalDKIMNotChecked {
		t.Errorf("DNS failure: rows=%d labels=%v", n, seen)
	}

	// 5. An archive import is never checked (CONTROL: the same bytes live above
	// were).
	arc, err := ing.IngestArchive(ctx, f.userID, f.dek, f.dekID, signed("archive"), Archive{MailboxID: f.mailboxB})
	if err != nil || !arc.Created {
		t.Fatalf("archive: %+v %v", arc, err)
	}
	if n, _, _ := row(arc.MessageID.String()); n != 0 {
		t.Errorf("an archive import has a local DKIM row")
	}

	// 6. Check disabled (no option): nothing written, and the lookup is never made.
	off := New(f.st, f.ing.blobs, withDKIMLookup(func(context.Context, string) ([]string, error) {
		t.Error("the resolver was called with the check unconfigured")
		return nil, nil
	}))
	off.localDKIM = nil
	o, err := off.Ingest(ctx, f.userID, f.dek, f.dekID, signed("off"), f.prov(f.mailboxA, 905, nil))
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _ := row(o.MessageID.String()); n != 0 {
		t.Errorf("disabled check wrote a row")
	}

	// 7. The store's batched read sees the aligned pass and only it.
	if err := f.st.UserTx(ctx, f.userID, func(tx *store.UserTx) error {
		m, e := tx.LocalDKIMFor(ctx, []ulid.ULID{live.MessageID, arc.MessageID})
		if e != nil {
			return e
		}
		if v, ok := m[live.MessageID]; !ok || !v.LocalDKIMPass() || v.Domain != "example.com" {
			t.Errorf("LocalDKIMFor: %+v", m)
		}
		if _, ok := m[arc.MessageID]; ok {
			t.Errorf("LocalDKIMFor returned a row for a message with none")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
