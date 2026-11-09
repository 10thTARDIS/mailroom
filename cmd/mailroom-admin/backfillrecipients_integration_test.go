// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// backfill-recipients (ADR-055): the report counts per signal and writes nothing;
// --apply fills exactly the candidates it counted; a second --apply changes
// nothing; an unreadable message is counted and left NULL, not fatal; another
// user's mail is never touched; and no change_log row is appended.
//
//	m1  to Alice Example, cc Bob                      (filled by the run)
//	m2  205 recipients in To                          (truncated)
//	m3  to carol, Bcc dan, List-Id <Plans.Example.net> (filled; bcc and list counted)
//	m4  bytes the reader cannot produce               (unreadable, stays NULL)
//	m5  already filled with a sentinel                (skipped, never rewritten)
func TestBackfillRecipientsReportApplyRerun(t *testing.T) {
	h := newUntangleHarness(t)
	tag := "rcptbf-" + strings.ToLower(store.NewULID().String())
	s := newNudgeSeed(t, h, tag)
	other := newNudgeSeed(t, h, tag+"-2")
	th := s.thread()

	var many []string
	for i := 0; i < 205; i++ {
		many = append(many, fmt.Sprintf("r%03d@example.org", i))
	}
	m1 := s.add(nmsg{thread: th, from: "sender@example.net", to: `"Alice Example" <alice@example.com>`, cc: "Bob <bob@example.com>", subject: "one", body: "a", msgID: "<r1@x>"})
	m2 := s.add(nmsg{thread: th, from: "sender@example.net", to: strings.Join(many, ", "), subject: "two", body: "b", msgID: "<r2@x>"})
	m3 := s.add(nmsg{thread: th, from: "sender@example.net", to: "carol@example.com", subject: "three", body: "c", msgID: "<r3@x>",
		extraHeaders: "Bcc: dan@example.com\r\nList-Id: Plans <Plans.Example.net>\r\n"})
	m4 := s.add(nmsg{thread: th, from: "sender@example.net", to: "gone@example.com", subject: "four", body: "d", msgID: "<r4@x>"})
	m5 := s.add(nmsg{thread: th, from: "sender@example.net", to: "kept@example.com", subject: "five", body: "e", msgID: "<r5@x>"})
	o1 := other.add(nmsg{thread: other.thread(), from: "sender@example.net", to: "alice@example.com", subject: "x", body: "x", msgID: "<o1@x>"})

	ctx := context.Background()
	uid := ulid.MustParse(s.user)
	// m4's bytes are gone; m5 is already filled with a value no parse would produce.
	var m4hash []byte
	if err := h.super.QueryRow(ctx, `SELECT content_hash FROM app.messages WHERE id=$1`, m4).Scan(&m4hash); err != nil {
		t.Fatal(err)
	}
	delete(s.raw, string(m4hash))
	s.exec(`UPDATE app.messages SET rcpt_to='sentinel', rcpt_cc='', rcpt_bcc='', rcpt_to_sort='', list_id_key='' WHERE id=$1`, m5)

	changeRows := func() int {
		t.Helper()
		var n int
		if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = ANY($1)`, []string{s.user, other.user}).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	filled := func() int {
		t.Helper()
		var n int
		if err := h.super.QueryRow(ctx, `SELECT count(rcpt_to) FROM app.messages WHERE user_id = ANY($1)`, []string{s.user, other.user}).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	run := func(apply bool, batch int) (recipientBackfillAgg, string) {
		t.Helper()
		agg, err := backfillRecipientsForUser(ctx, h.store, uid, s.reader(), apply, batch)
		if err != nil {
			t.Fatalf("backfill(apply=%v): %v", apply, err)
		}
		var out bytes.Buffer
		printRecipientBackfill(&out, agg, apply)
		return agg, out.String()
	}
	col := func(id, c string) string {
		t.Helper()
		var v *string
		if err := h.super.QueryRow(ctx, `SELECT `+c+` FROM app.messages WHERE id=$1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return "<NULL>"
		}
		return *v
	}

	changesBefore := changeRows()
	if filled() != 1 {
		t.Fatalf("fixture: %d filled rows, want only the sentinel", filled())
	}

	// REPORT: counts per signal, nothing written.
	rep, text := run(false, 2)
	if rep.total != 5 || rep.alreadyFilled != 1 || rep.candidates != 4 || rep.parsed != 3 || rep.unreadable != 1 ||
		rep.withTo != 3 || rep.withCc != 1 || rep.withBcc != 1 || rep.withList != 1 || rep.truncated != 1 || rep.written != 0 {
		t.Fatalf("report counts = %+v\n%s", rep, text)
	}
	if !strings.Contains(text, "REPORT ONLY") || !strings.Contains(text, "would fill") {
		t.Errorf("report text does not say it is a report:\n%s", text)
	}
	if filled() != 1 || changeRows() != changesBefore {
		t.Fatalf("the report wrote: %d filled rows (want 1), change rows %d -> %d", filled(), changesBefore, changeRows())
	}

	// APPLY (batch 2: three batches over four candidates).
	ap, text := run(true, 2)
	if ap.written != 3 || ap.lostRace != 0 || ap.unreadable != 1 || ap.candidates != 4 {
		t.Fatalf("apply counts = %+v\n%s", ap, text)
	}
	if filled() != 4 {
		t.Fatalf("after apply %d filled rows, want 4 (3 + the sentinel)", filled())
	}
	if got := col(m1, "rcpt_to"); got != "alice example\nalice@example.com" {
		t.Errorf("m1 rcpt_to = %q", got)
	}
	if got := col(m1, "rcpt_cc"); got != "bob\nbob@example.com" {
		t.Errorf("m1 rcpt_cc = %q", got)
	}
	if got := col(m1, "rcpt_to_sort"); got != "Alice Example" {
		t.Errorf("m1 rcpt_to_sort = %q (the key keeps its case)", got)
	}
	if got := col(m1, "list_id_key"); got != "" {
		t.Errorf("m1 list_id_key = %q, want filled-empty", got)
	}
	if got := col(m3, "rcpt_bcc"); got != "dan@example.com" {
		t.Errorf("m3 rcpt_bcc = %q", got)
	}
	if got := col(m3, "list_id_key"); got != "plans.example.net" {
		t.Errorf("m3 list_id_key = %q", got)
	}
	if got := col(m2, "rcpt_to"); strings.Count(got, "\n")+1 != 200 {
		t.Errorf("m2 stored %d lines, want the 200-recipient bound", strings.Count(got, "\n")+1)
	}
	if got := col(m4, "rcpt_to"); got != "<NULL>" {
		t.Errorf("unreadable m4 rcpt_to = %q, want NULL", got)
	}
	if got := col(m5, "rcpt_to"); got != "sentinel" {
		t.Errorf("already-filled m5 was rewritten: %q", got)
	}
	if got := col(o1, "rcpt_to"); got != "<NULL>" {
		t.Errorf("another user's message was touched: %q", got)
	}
	if changeRows() != changesBefore {
		t.Errorf("the backfill appended change_log rows: %d -> %d", changesBefore, changeRows())
	}

	// A second APPLY changes nothing: only the unreadable message is still a candidate.
	again, text := run(true, 2)
	if again.candidates != 1 || again.written != 0 || again.unreadable != 1 || again.alreadyFilled != 4 {
		t.Fatalf("second apply = %+v\n%s", again, text)
	}
	if filled() != 4 || col(m1, "rcpt_to") != "alice example\nalice@example.com" || col(m5, "rcpt_to") != "sentinel" {
		t.Errorf("the second apply changed data")
	}

	// RLS: a run for the other user fills only that user's mail.
	oagg, err := backfillRecipientsForUser(ctx, h.store, ulid.MustParse(other.user), other.reader(), true, 10)
	if err != nil || oagg.written != 1 || col(o1, "rcpt_to") != "alice@example.com" {
		t.Errorf("other user: %+v %v rcpt_to=%q", oagg, err, col(o1, "rcpt_to"))
	}
	if col(m4, "rcpt_to") != "<NULL>" {
		t.Error("the other user's run touched this user's unreadable message")
	}
}
