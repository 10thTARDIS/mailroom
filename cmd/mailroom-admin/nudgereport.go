// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/nudge"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin nudge-report` — #58: how accurate would follow-up nudges be
// on this user's OWN history? READ-ONLY; there is no --apply, because what it
// decides (which rule, which delay, whether to build the nudge at all) is a
// judgement, not a side effect.
//
// The August audit on #58 found nothing to measure (3 sent, 0 \Answered).
// The provider's Sent folder is now synced, so the user's replies from every
// client are messages here, and "did the user reply?" has a real answer. This
// report uses it as ground truth:
//
//   - INBOUND "needs reply": for each candidate rule, of the messages it would
//     flag, how many did the user reply to within 14 days (a Sent message in
//     the thread or naming it in In-Reply-To, or \Answered)? Precision with
//     the bottom of its Wilson interval, recall, and the nudges a week a
//     "haven't replied in N days" nudge would have fired.
//   - OUTBOUND "no answer": of the user's sent mail, how much got a human
//     answer within 3/7/14 days, and how many nudges a week that leaves.
//
// Counts per signal and per rule, never one total. Message ids only with
// --examples; never subjects or addresses, so the output can be pasted into an
// issue (portability).
func runNudgeReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("nudge-report", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "user id (ULID)")
		sinceStr  = fs.String("since", "180d", "window to measure, ending now (e.g. 90d, 180d, 365d)")
		examples  = fs.Bool("examples", false, "list up to 10 message ids per bucket (ids only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	since, err := parseDays(*sinceStr)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("nudge-report: master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("nudge-report: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)
	read := func(ctx context.Context, hash []byte) ([]byte, error) {
		return readMailBlob(ctx, blobs, userID, dek, hash)
	}

	now := time.Now().UTC()
	cfg := nudge.DefaultConfig(now, since)
	if *examples {
		cfg.Examples = 10
	}
	col, err := collectNudgeMessages(ctx, d.store, userID, cfg.Since, read)
	if err != nil {
		return err
	}
	rep := nudge.Evaluate(col.msgs, col.me, cfg)
	printNudgeReport(os.Stdout, col, rep, *sinceStr)
	return nil
}

// parseDays reads "180d" as 180 days, or any Go duration ("2160h").
func parseDays(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%q: want a positive number of days such as 180d", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil || dur <= 0 {
		return 0, fmt.Errorf("%q: want a positive number of days such as 180d", s)
	}
	return dur, nil
}

// rawReader returns a message's raw RFC 5322 bytes by content hash. The real
// one decrypts from the blob store; the integration test passes fixtures, so
// the parsing it exercises is the real parser over real bytes.
type rawReader func(ctx context.Context, contentHash []byte) ([]byte, error)

// nudgeCollection is what the report read, with the counts that bound it.
type nudgeCollection struct {
	msgs       []nudge.Message
	me         nudge.Me
	identities store.NudgeIdentities
	parsed     int
	unreadable int
	// Where Asks read the body from, per parsed message: text/plain, the
	// HTML-only fallback, or nothing (no body text at all).
	bodyText, bodyHTML, bodyNone int
}

// collectNudgeMessages reads every non-draft message of the user's and parses
// the ones the measurement needs content for: everything the user sent (the
// recipients build the as-of "corresponded before" map, at any age) and
// every non-junk message in the window.
func collectNudgeMessages(ctx context.Context, st *store.Store, userID ulid.ULID, since time.Time, read rawReader) (*nudgeCollection, error) {
	col := &nudgeCollection{}
	if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		col.identities, e = tx.NudgeIdentities(ctx)
		return e
	}); err != nil {
		return nil, fmt.Errorf("nudge-report: identities: %w", err)
	}
	col.me = nudge.NewMe(col.identities.Addresses, col.identities.Domains)

	const batch = 1000
	var after ulid.ULID
	for {
		var page []store.NudgeMessage
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.NudgeMessagesAfter(ctx, after, batch)
			return e
		}); err != nil {
			return nil, fmt.Errorf("nudge-report: walk messages: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			after = r.ID
			m := nudge.Message{
				ID:            r.ID.String(),
				At:            r.ReceivedAt,
				From:          r.FromAddress,
				InSentFolder:  r.InSentMailbox,
				ViaSubmission: r.ViaSubmission,
				FromIdentity:  col.me.Is(r.FromAddress),
				MessageID:     r.MessageIDHint,
				InReplyTo:     r.InReplyTo,
				Answered:      r.IsAnswered,
				Junk:          r.IsJunk,
			}
			if r.SentAt != nil {
				m.Date = r.SentAt.UTC()
			}
			if r.ThreadID != (ulid.ULID{}) {
				m.Thread = r.ThreadID.String()
			}
			if m.Outbound() || (!m.Junk && !m.At.Before(since)) {
				raw, err := read(ctx, r.ContentHash)
				if err == nil {
					err = parseNudgeContent(raw, &m, col)
				}
				if err != nil {
					col.unreadable++
				} else {
					col.parsed++
				}
			}
			col.msgs = append(col.msgs, m)
		}
	}
	return col, nil
}

// parseNudgeContent fills m's parsed fields through nudge.ParseContent — the
// one parse the live nudge uses too — and tallies where the body came from.
func parseNudgeContent(raw []byte, m *nudge.Message, col *nudgeCollection) error {
	src, err := nudge.ParseContent(raw, m)
	if err != nil {
		return err
	}
	switch src {
	case nudge.BodyText:
		col.bodyText++
	case nudge.BodyHTML:
		col.bodyHTML++
	default:
		col.bodyNone++
	}
	return nil
}

func printNudgeReport(w io.Writer, col *nudgeCollection, rep nudge.Report, sinceStr string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	pct := func(v float64) string {
		if math.IsNaN(v) {
			return "n/a"
		}
		return fmt.Sprintf("%.1f%%", 100*v)
	}
	num := func(v float64) string {
		if math.IsNaN(v) {
			return "n/a"
		}
		return fmt.Sprintf("%.2f", v)
	}
	rate := func(r nudge.Rate) string {
		return fmt.Sprintf("%d/%d = %s (95%% LB %s)", r.Hits, r.N, pct(r.P()), pct(r.LB()))
	}

	p("=== nudge-report (#58): follow-up nudge accuracy on this user's own history ===\n")
	p("window: %s .. %s (--since %s); read-only\n", rep.Since.Format(time.RFC3339), rep.Now.Format(time.RFC3339), sinceStr)
	p("identities: %d fixed address(es), %d wildcard domain(s); sent-role mailboxes: %d\n",
		len(col.identities.Addresses), len(col.identities.Domains), col.identities.SentMailboxes)
	if col.me.Empty() {
		p("⚠ NO IDENTITIES: From can never be recognised as the user's, so direction rests on the Sent folder alone\n")
	}
	if col.identities.SentMailboxes == 0 {
		p("⚠ NO SENT-ROLE MAILBOX: replies sent from other clients are invisible; inbound precision is a LOWER bound\n")
	}
	p("messages (non-draft): %d — inbound %d, outbound %d\n", rep.Messages, rep.Inbound, rep.Outbound)
	p("content parsed: %d, unreadable %d (excluded below — every count is over what could be read)\n", col.parsed, col.unreadable)
	p("body read for question/request: text/plain %d, HTML-only fallback %d, no body text %d\n", col.bodyText, col.bodyHTML, col.bodyNone)

	a := rep.Audit
	p("\n--- 1. how sent mail is represented (per signal) ---\n")
	p("outbound by evidence: in a Sent-role mailbox %d; via a Mailroom submission %d; From is an identity %d\n",
		a.OutboundSentFolder, a.OutboundSubmission, a.OutboundIdentity)
	p("  in Sent but From is NOT a known identity: %d   (identity gap — add the identity, or these replies look inbound)\n", a.SentFolderNotIdentity)
	p("  From is an identity but NOT in Sent:      %d   (self-sent, or a send whose Sent copy is not synced)\n", a.IdentityNotSentFolder)
	p("clock check: outbound whose arrival time is >1 day from its Date header %d, with no Date %d (a large first number means timings are not trustworthy)\n",
		a.OutboundClockSkewOver1d, a.OutboundNoDate)
	p("outbound in window: %d — sharing a thread with inbound mail %d, unthreaded %d\n",
		a.OutboundInWindow, a.OutboundThreadedWithInbound, a.OutboundUnthreaded)
	p("  with In-Reply-To %d: parent in the SAME thread %d, parent in ANOTHER thread %d, parent not in the mailbox %d\n",
		a.OutboundWithInReplyTo, a.ParentSameThread, a.ParentOtherThread, a.ParentMissing)
	p("  replies found only through In-Reply-To (threading missed them): %d\n", a.RepliesByInReplyToOnly)

	p("\n--- 2. inbound \"needs reply\" (ground truth: the user replied within 14 days) ---\n")
	p("inbound in window %d: censored (younger than 14d, outcome unknown) %d, junk %d, unreadable %d → evaluable %d over %s weeks\n",
		rep.InboundInWindow, rep.InboundCensored, rep.InboundJunk, rep.InboundUnparsed, rep.InboundEvaluable, num(rep.InboundWeeks))
	p("\\Answered among evaluable: %d — with a reply message found %d, with none (reply time unknown) %d\n",
		a.AnsweredInbound, a.AnsweredWithReply, a.AnsweredNoReply)
	for _, v := range rep.InboundVariants {
		p("\n[%s] %s\n", v.Name, v.Rule)
		p("  precision %s\n", rate(v.Precision))
		p("    hits by reply message within 14d %d; by \\Answered only %d\n", v.TimedReplies, v.AnsweredOnly)
		p("  recall    %s\n", rate(v.Recall))
		p("  candidates/week %s\n", num(v.PerWeek))
		for _, n := range v.Nudges {
			p("  nudge after %dd: fired %d (%s/week), superseded %d — of fired: replied later %d, \\Answered untimed %d, never replied %d\n",
				n.AfterDays, n.Fired, num(n.PerWeek), n.Superseded, n.LateReplied, n.AnsweredUntimed, n.NoReply)
		}
		printExamples(p, v.Examples)
	}
	printSignals := func(title string, rows []nudge.SignalRow) {
		p("\n%s\n", title)
		p("  %-22s %-36s %s\n", "signal", "reply rate WITH", "reply rate WITHOUT")
		for _, r := range rows {
			p("  %-22s %-36s %s\n", r.Name, rate(r.With), rate(r.Without))
		}
	}
	printSignals("per signal, all evaluable inbound:", rep.SignalsAll)
	printSignals("per signal, within person+to-me:", rep.SignalsPerson)

	p("\n--- 3. outbound \"no one answered\" (ground truth: a human answer — not the user, not Auto-Submitted) ---\n")
	p("outbound in window %d, unreadable %d; answered ONLY by an automatic reply within 14d: %d (not counted as answered)\n",
		rep.OutboundInWindow, rep.OutboundUnparsed, rep.OutboundAutoOnly)
	for _, v := range rep.OutboundVariants {
		p("\n[%s] %s — %d sent in window\n", v.Name, v.Rule, v.InWindow)
		for _, od := range v.ByDays {
			p("  within %2dd: answered %s; nudges fired %d (%s/week), superseded by own follow-up %d; late answer (by 30d) %d of %d observable\n",
				od.Days, rate(od.Answered), od.Fired, num(od.PerWeek), od.Superseded, od.LateAnswered, od.LateObserved)
		}
		printExamples(p, v.Examples)
	}
}

func printExamples(p func(string, ...any), ex map[string][]string) {
	if len(ex) == 0 {
		return
	}
	keys := make([]string, 0, len(ex))
	for k := range ex {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p("    e.g. %s: %s\n", k, strings.Join(ex[k], " "))
	}
}
