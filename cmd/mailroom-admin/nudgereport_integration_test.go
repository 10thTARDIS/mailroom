// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// nudge-report (#58) end to end against a real Postgres: a seeded mailbox
// whose ground truth is known exactly, read through the store's RLS-scoped
// walk and the real MIME parser (the blob bytes come from fixtures instead of
// the encrypted store — that is the only substitution), then measured by
// nudge.Evaluate. Every count below is derived by hand from the corpus.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/nudge"
	"mailroom/internal/store"
)

type nudgeSeed struct {
	t                    *testing.T
	super                *pgxpool.Pool
	user, account, ident string
	inbox, sent          string
	raw                  map[string][]byte // content hash (as string) -> bytes
	now                  time.Time
}

func newNudgeSeed(t *testing.T, h *untangleHarness, tag string) *nudgeSeed {
	t.Helper()
	s := &nudgeSeed{t: t, super: h.super, raw: map[string][]byte{}, now: time.Now().UTC()}
	s.user = seedUntangleUser(t, h.super, tag).String()
	s.account, s.ident, s.inbox, s.sent = store.NewULID().String(), store.NewULID().String(), store.NewULID().String(), store.NewULID().String()
	s.exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
	        VALUES ($1,$2,'primary','mail.example.com','smtp.example.com',$3,$4)`,
		s.account, s.user, []byte{1}, store.NewULID().String())
	s.exec(`INSERT INTO app.identities (id, user_id, account_id, kind, address) VALUES ($1,$2,$3,'fixed',$4)`,
		s.ident, s.user, s.account, "me-"+tag+"@example.com")
	s.exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name) VALUES ($1,$2,$3,'INBOX','Inbox')`,
		s.inbox, s.user, s.account)
	s.exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use) VALUES ($1,$2,$3,'Sent','Sent','\Sent')`,
		s.sent, s.user, s.account)
	return s
}

func (s *nudgeSeed) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.super.Exec(context.Background(), sql, args...); err != nil {
		s.t.Fatalf("seed: %s: %v", strings.SplitN(strings.TrimSpace(sql), "\n", 2)[0], err)
	}
}

func (s *nudgeSeed) thread() string {
	id := store.NewULID().String()
	s.exec(`INSERT INTO app.threads (id, user_id) VALUES ($1,$2)`, id, s.user)
	return id
}

// nmsg is one seeded message.
type nmsg struct {
	thread, mailbox  string // mailbox "" = no provenance
	daysAgo          float64
	from, to, cc     string
	subject, body    string
	msgID, inReplyTo string
	extraHeaders     string
	answered, junk   bool
	draft            bool
}

func (s *nudgeSeed) add(m nmsg) string {
	s.t.Helper()
	id := store.NewULID().String()
	at := s.now.Add(-time.Duration(m.daysAgo * float64(24*time.Hour)))
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\n", m.from, m.to)
	if m.cc != "" {
		fmt.Fprintf(&b, "Cc: %s\r\n", m.cc)
	}
	fmt.Fprintf(&b, "Subject: %s\r\nMessage-ID: %s\r\nDate: %s\r\n", m.subject, m.msgID, at.Format(time.RFC1123Z))
	if m.inReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", m.inReplyTo)
	}
	b.WriteString(m.extraHeaders)
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", m.body)
	raw := []byte(b.String())
	hash := sha256.Sum256(raw)
	s.raw[string(hash[:])] = raw
	s.exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1,$2,100,132,$3)`,
		s.user, hash[:], store.NewULID().String())
	var irt any
	if m.inReplyTo != "" {
		irt = m.inReplyTo
	}
	s.exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, from_address,
	          received_at, sent_at, size_bytes, message_id_hint, in_reply_to, is_answered, is_junk, is_draft)
	        VALUES ($1,$2,$3,$4,$5,$6,$7,$7,100,$8,$9,$10,$11,$12)`,
		id, s.user, hash[:], m.thread, m.subject, m.from, at, m.msgID, irt, m.answered, m.junk, m.draft)
	if m.mailbox != "" {
		s.exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity)
		        VALUES ($1,$2,$3,$4,$5,$6,1)`,
			store.NewULID().String(), s.user, id, s.account, m.mailbox, time.Now().UnixNano()%1_000_000_000)
	}
	return id
}

func (s *nudgeSeed) reader() rawReader {
	return func(_ context.Context, hash []byte) ([]byte, error) {
		if r, ok := s.raw[string(hash)]; ok {
			return r, nil
		}
		return nil, fmt.Errorf("no fixture for hash")
	}
}

// TestNudgeReportSeededMailbox: the corpus and its exact ground truth.
//
//	I1  alice → me, "Lunch?"        replied by R1 (Sent, same thread, 1d later)
//	I2  bob → me                    never replied
//	I3  newsletter (List-Id) → me   never replied
//	I4  carol → other, Cc me        \Answered, no reply message synced
//	I5  dan → me                    replied by R5, sent through Mailroom (submission, no Sent copy)
//	R6  sent from old@ (in Sent, NOT an identity) to erin; E6 erin's answer 2d later
//	J1  junk; D1 a draft (never read)
func TestNudgeReportSeededMailbox(t *testing.T) {
	h := newUntangleHarness(t)
	tag := "nudge-" + strings.ToLower(store.NewULID().String())
	s := newNudgeSeed(t, h, tag)
	me := "me-" + tag + "@example.com"

	t1, t2, t3, t4, t5, t6, t7 := s.thread(), s.thread(), s.thread(), s.thread(), s.thread(), s.thread(), s.thread()
	i1 := s.add(nmsg{thread: t1, mailbox: s.inbox, daysAgo: 60, from: "alice@example.org", to: me, subject: "Lunch?", body: "Free Tuesday", msgID: "<i1@x>"})
	s.add(nmsg{thread: t1, mailbox: s.sent, daysAgo: 59, from: me, to: "alice@example.org", subject: "Re: Lunch?", body: "Yes.", msgID: "<r1@x>", inReplyTo: "<i1@x>"})
	s.add(nmsg{thread: t2, mailbox: s.inbox, daysAgo: 50, from: "bob@example.org", to: me, subject: "Hello", body: "Thanks.", msgID: "<i2@x>"})
	s.add(nmsg{thread: t3, mailbox: s.inbox, daysAgo: 45, from: "news@example.net", to: me, subject: "Weekly", body: "News.", msgID: "<i3@x>",
		extraHeaders: "List-Id: <weekly.example.net>\r\nList-Unsubscribe: <mailto:u@example.net>\r\n"})
	s.add(nmsg{thread: t4, mailbox: s.inbox, daysAgo: 40, from: "carol@example.org", to: "other@example.org", cc: me, subject: "FYI", body: "Notes.", msgID: "<i4@x>", answered: true})
	s.add(nmsg{thread: t5, mailbox: s.inbox, daysAgo: 30, from: "dan@example.org", to: me, subject: "Plan", body: "Draft attached.", msgID: "<i5@x>"})
	r5 := s.add(nmsg{thread: t5, daysAgo: 29, from: me, to: "dan@example.org", subject: "Re: Plan", body: "Looks good.", msgID: "<r5@x>", inReplyTo: "<i5@x>"})
	s.exec(`INSERT INTO app.submissions (id, user_id, account_id, identity_id, message_id, from_address, envelope_to, state, sent_at)
	        VALUES ($1,$2,$3,$4,$5,$6,ARRAY['dan@example.org']::citext[],'sent',now())`,
		store.NewULID().String(), s.user, s.account, s.ident, r5, me)
	s.add(nmsg{thread: t6, mailbox: s.sent, daysAgo: 20, from: "old@example.com", to: "erin@example.org", subject: "Hi", body: "Long time.", msgID: "<r6@x>"})
	s.add(nmsg{thread: t6, mailbox: s.inbox, daysAgo: 18, from: "erin@example.org", to: "old@example.com", subject: "Re: Hi", body: "Indeed.", msgID: "<e6@x>", inReplyTo: "<r6@x>"})
	s.add(nmsg{thread: t7, mailbox: s.inbox, daysAgo: 30, from: "spam@example.net", to: me, subject: "Win", body: "Prize?", msgID: "<j1@x>", junk: true})
	s.add(nmsg{thread: t7, daysAgo: 25, from: me, to: "x@example.org", subject: "draft", body: "?", msgID: "<d1@x>", draft: true})

	ctx := context.Background()
	uid := ulid.MustParse(s.user)
	cfg := nudge.DefaultConfig(s.now, 180*24*time.Hour)
	cfg.Examples = 10
	col, err := collectNudgeMessages(ctx, h.store, uid, cfg.Since, s.reader())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(col.msgs) != 10 {
		t.Fatalf("collected %d messages, want 10 (the draft is never read)", len(col.msgs))
	}
	if col.unreadable != 0 || col.parsed != 9 {
		t.Fatalf("parsed %d unreadable %d, want 9/0 (junk inbound is never parsed)", col.parsed, col.unreadable)
	}
	rep := nudge.Evaluate(col.msgs, col.me, cfg)

	a := rep.Audit
	if a.OutboundSentFolder != 2 || a.OutboundSubmission != 1 || a.OutboundIdentity != 2 ||
		a.SentFolderNotIdentity != 1 || a.IdentityNotSentFolder != 1 || a.OutboundThreadedWithInbound != 3 ||
		a.OutboundClockSkewOver1d != 0 || a.OutboundNoDate != 0 {
		t.Errorf("sent representation audit = %+v", a)
	}
	if rep.InboundEvaluable != 6 || rep.InboundJunk != 1 {
		t.Fatalf("inbound evaluable %d junk %d, want 6/1", rep.InboundEvaluable, rep.InboundJunk)
	}
	want := map[string][2]int{
		"all":               {3, 6},
		"person":            {3, 5}, // the newsletter is out
		"person+to-me":      {2, 3}, // I1 I2 I5: carol's Cc and erin→old@ are out
		"person+to-me+asks": {1, 1}, // "Lunch?"
	}
	for _, v := range rep.InboundVariants {
		if w, ok := want[v.Name]; ok && (v.Precision.Hits != w[0] || v.Precision.N != w[1]) {
			t.Errorf("%s precision = %d/%d, want %d/%d", v.Name, v.Precision.Hits, v.Precision.N, w[0], w[1])
		}
		if v.Name == "all" {
			if v.TimedReplies != 2 || v.AnsweredOnly != 1 {
				t.Errorf("all: timed %d answered-only %d, want 2/1", v.TimedReplies, v.AnsweredOnly)
			}
			if got := v.Examples["flagged, replied"]; len(got) != 3 || got[0] != i1 {
				t.Errorf("examples %v", got)
			}
		}
	}
	for _, v := range rep.OutboundVariants {
		if v.Name == "all" {
			if v.InWindow != 3 || v.ByDays[0].Answered.Hits != 1 || v.ByDays[0].Answered.N != 3 {
				t.Errorf("outbound all = %d in window, @3d %+v; want 3 and 1/3", v.InWindow, v.ByDays[0].Answered)
			}
		}
	}

	// The printed report carries ids, never addresses or subjects.
	var buf bytes.Buffer
	printNudgeReport(&buf, col, rep, "180d")
	outText := buf.String()
	for _, leak := range []string{"example.org", "example.net", "Lunch", "alice", me} {
		if strings.Contains(outText, leak) {
			t.Fatalf("report output leaks %q:\n%s", leak, outText)
		}
	}
	if !strings.Contains(outText, i1) {
		t.Fatalf("--examples output does not list message ids:\n%s", outText)
	}

	// RLS: another user's walk sees none of this user's mail.
	other := seedUntangleUser(t, h.super, tag+"-other")
	ocol, err := collectNudgeMessages(ctx, h.store, other, cfg.Since, s.reader())
	if err != nil {
		t.Fatalf("collect other: %v", err)
	}
	if len(ocol.msgs) != 0 {
		t.Fatalf("another user's walk returned %d of this user's messages", len(ocol.msgs))
	}
}
