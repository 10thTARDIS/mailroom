// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/placement"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin from-field-report` — how many messages carry a From header
// that is not exactly one plain mailbox, and what Mailroom did with them
// (#754, report first).
//
// WHY IT EXISTS. RFC 5322 allows exactly one From field. A message with two —
// or with an empty one beside a real one — is a known parser-differential
// spoof: DMARC may evaluate one From while a client displays or learns from
// the other. #725 made calendar verification require exactly one raw From
// field (mime.Header.FieldCount); two neighbours do not yet:
//
//   - learned placement counts Values("From"), which drops EMPTY fields, so an
//     empty From beside a real one passes as one;
//   - the sender-auth display and sender warnings count no From fields at all.
//
// Before either changes, this counts how often each shape actually occurs,
// and — per shape — how often learned placement acted on such a message and
// how often the reading pane showed an authentication notice or a trusted
// DMARC pass. A shape that never occurs is a finding too: say so, with the
// denominator.
//
// THE SHAPES are exclusive, decided in this order (first match wins):
//
//	none           no From field
//	multiEmptyPlus two or more From fields, at least one empty and one not
//	multiAllEmpty  two or more From fields, all empty
//	multi          two or more From fields, none empty
//	oneEmpty       one From field with nothing in it
//	group          one From field in RFC 5322 group syntax (placement.HasGroup)
//	multiMailbox   one From field holding more than one mailbox
//	unparsable     one From field from which no address could be parsed
//	one            exactly one From field holding exactly one mailbox
//
// ⚠ IT MEASURES THE SHIPPED CODE: the field count is mime.Header.FieldCount,
// the group rule is placement's own, the DMARC verdict is the trusted parse
// learned placement uses (ml.TopmostTrustedAuthResults + DMARCPassFor with
// ml.DMARCRelaxed, #846), and
// the notice is the web client's senderAuthNotice rule applied to the same
// trusted parse the server sends (authNoticeShown below says how).
//
// PRIVACY: it prints counts. --examples N adds up to N message IDS per unusual
// shape — never an address, a subject or a header value.
//
// READ-ONLY. There is no --apply: it writes nothing and proposes nothing.
func runFromFieldReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("from-field-report", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "user whose corpus to measure (required)")
	batch := fs.Int("batch", 500, "messages per page of the keyset walk")
	examples := fs.Int("examples", 0, "print up to this many message ids per unusual shape")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userFlag)
	if err != nil {
		return err
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
		return fmt.Errorf("from-field-report: master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("from-field-report: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	trusted, err := trustedAuthservEntries()
	if err != nil {
		return err
	}
	agg := newFromAggregate(trusted, *examples)
	after := ulid.ULID{}
	var (
		facts store.AuthSelfFacts
		boxes map[ulid.ULID][]ulid.ULID
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		facts, e = tx.AuthSelfFacts(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("from-field-report: self facts: %w", err)
	}
	for {
		var page []store.MessageFromRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			if page, e = tx.MessageFromRefsAfter(ctx, after, *batch); e != nil {
				return e
			}
			pageIDs := make([]ulid.ULID, len(page))
			for i, r := range page {
				pageIDs[i] = r.ID
			}
			boxes, e = tx.LiveMailboxIDs(ctx, pageIDs)
			return e
		}); err != nil {
			return fmt.Errorf("from-field-report: walk messages: %w", err)
		}
		for _, ref := range page {
			after = ref.ID
			agg.scanned++
			msg, ok := readAndParse(ctx, d, blobs, userID, dek, ref.ContentHash)
			if !ok {
				agg.unreadable++
				continue
			}
			agg.add(ref, msg, facts.IsSelf(boxes[ref.ID]))
		}
		if len(page) < *batch {
			break
		}
	}
	agg.print()
	return nil
}

func readAndParse(ctx context.Context, d *deps, blobs blob.BlobStore, userID ulid.ULID, dek, hash []byte) (*mime.Message, bool) {
	raw, err := readMailBlob(ctx, blobs, userID, dek, hash)
	if err != nil {
		d.log.Warn("from-field-report: read blob failed", "err", err)
		return nil, false
	}
	msg, err := mime.ParseBytes(raw)
	if err != nil || msg == nil {
		return nil, false
	}
	return msg, true
}

// fromShapes is the print order: the plain case first, then the rest.
var fromShapes = []string{
	"one", "none", "multiEmptyPlus", "multiAllEmpty", "multi",
	"oneEmpty", "group", "multiMailbox", "unparsable",
}

// classifyFrom decides a message's From shape. See the package comment above
// runFromFieldReport for the order and the definitions.
func classifyFrom(h mime.Header) string {
	n := h.FieldCount("From")
	// Values drops a field that is empty once sanitised; that is exactly how
	// the empty ones are told apart from the rest here.
	vals := h.Values("From")
	switch {
	case n == 0:
		return "none"
	case n >= 2 && len(vals) > 0 && len(vals) < n:
		return "multiEmptyPlus"
	case n >= 2 && len(vals) == 0:
		return "multiAllEmpty"
	case n >= 2:
		return "multi"
	case len(vals) == 0:
		return "oneEmpty"
	case placement.HasGroup(vals[0]):
		return "group"
	}
	addrs := h.From()
	parsed := 0
	for _, a := range addrs {
		if a.Address != "" {
			parsed++
		}
	}
	switch {
	case parsed > 1:
		return "multiMailbox"
	case parsed == 0:
		return "unparsable"
	}
	return "one"
}

// authNoticeShown is whether the reading pane shows a sender-authentication
// notice for this message, and why: web/src/core/senderAuth.ts
// senderAuthNotice applied to what internal/api sends (authNoticeObject, whose
// category rule is ml.AuthNoticeFor, shared with this report).
//
//   - no authserv-id configured: the server sends nothing → none;
//   - no trusted header, and the message is the user's own (locally authored, Sent or
//     Drafts, or From their address): null → none;
//   - no trusted header, inbound (untrusted headers change nothing, #766): "unauthenticated";
//   - a trusted header: a method with fail and no pass: "failed"; a method
//     with permerror/temperror: "unverifiable".
//
// ⚠ IT DOES NOT READ MAILROOM'S OWN DKIM VERDICT (#776, localPass false): this
// report asks about From shapes, and a local aligned pass would move a message
// from "unauthenticated" to no notice. Its "unauthenticated" count therefore
// includes mail the reading pane now marks "Verified by Mailroom"; auth-report
// --authserv has the exact split.
//
// ⚠ THE CATEGORY IS SHARED CODE, THE SEVERITY IS A MIRROR: the severity rule
// lives in TypeScript and this is its Go restatement for a report. If
// senderAuth.ts changes, this changes with it.
func authNoticeShown(res ml.AuthResults, locallyAuthored, self, trustConfigured bool) string {
	switch ml.AuthNoticeFor(trustConfigured, res.Present, locallyAuthored, self, false) {
	case ml.NoticeUnauthenticated:
		return "unauthenticated"
	case ml.NoticeTrusted:
	default:
		return ""
	}
	for _, m := range ml.AuthMethods {
		r := res.Results[m]
		if r["fail"] && !r["pass"] {
			return "failed"
		}
	}
	for _, m := range ml.AuthMethods {
		r := res.Results[m]
		if r["permerror"] || r["temperror"] {
			return "unverifiable"
		}
	}
	return ""
}

// fromRow is one shape's counts. Every column is its own measurement; none is
// obtained by subtracting two others.
type fromRow struct {
	messages      int
	local         int // Mailroom wrote it (draft / Sent copy)
	learnedPlaced int
	notice        map[string]int // severity → messages
	dmarcPass     int
	examples      []string
}

type fromAggregate struct {
	trusted    []string
	examples   int
	scanned    int
	unreadable int
	rows       map[string]*fromRow
}

func newFromAggregate(trusted []string, examples int) *fromAggregate {
	a := &fromAggregate{trusted: trusted, examples: examples, rows: map[string]*fromRow{}}
	for _, s := range fromShapes {
		a.rows[s] = &fromRow{notice: map[string]int{}}
	}
	return a
}

// add tallies one message; self is store.AuthSelfFacts.IsSelf for it (Sent or
// Drafts; the From address is deliberately not a signal, #766). Trust is
// judged at the message's received_at (#766, ml.TrustedAt).
func (a *fromAggregate) add(ref store.MessageFromRef, msg *mime.Message, self bool) {
	shape := classifyFrom(msg.Header)
	r := a.rows[shape]
	r.messages++
	if ref.LocallyAuthored {
		r.local++
	}
	if ref.LearnedPlaced {
		r.learnedPlaced++
	}
	authValues := msg.Header.Values("Authentication-Results")
	ids := ml.TrustedAt(a.trusted, ref.TrustTime())
	if sev := authNoticeShown(ml.TrustedAuthResults(authValues, ids), ref.LocallyAuthored, self, len(a.trusted) > 0); sev != "" {
		r.notice[sev]++
	}
	// The DMARC pass learned placement acts on, bound to the domain of the
	// FIRST parsed From mailbox — the one a single-From reader would take.
	if len(a.trusted) > 0 {
		for _, f := range msg.Header.From() {
			if i := strings.LastIndexByte(f.Address, '@'); i > 0 {
				// RELAXED, the mode learned placement acts on (#846).
				if ml.TopmostTrustedAuthResults(authValues, ids).DMARCPassFor(f.Address[i+1:], ml.DMARCRelaxed) {
					r.dmarcPass++
				}
				break
			}
		}
	}
	if shape != "one" && len(r.examples) < a.examples {
		r.examples = append(r.examples, ref.ID.String())
	}
}

func (a *fromAggregate) print() {
	rp("scanned                 : %d messages\n", a.scanned)
	rp("unreadable              : %d  (blob or MIME failure — in no shape below)\n", a.unreadable)
	if len(a.trusted) > 0 {
		rp("trusted authserv-ids    : %s\n", strings.Join(a.trusted, ","))
	} else {
		rp("trusted authserv-ids    : NONE configured — the notice and DMARC columns are 0 BY CONSTRUCTION\n")
		rp("                          (the server shows no notice and believes no pass); set MAILROOM_AUTHSERV_IDS\n")
	}
	rpln()
	rp("%-15s %9s %7s %9s %7s %7s %7s %7s\n", "SHAPE", "MESSAGES", "OURS", "LEARNED", "N-FAIL", "N-UNAU", "N-UNVF", "DMARC")
	unusual := 0
	for _, s := range fromShapes {
		r := a.rows[s]
		rp("%-15s %9d %7d %9d %7d %7d %7d %7d\n", s, r.messages, r.local, r.learnedPlaced,
			r.notice["failed"], r.notice["unauthenticated"], r.notice["unverifiable"], r.dmarcPass)
		if s != "one" {
			unusual += r.messages
		}
	}
	rpln()
	rp("not exactly one plain From : %d of %d readable\n", unusual, a.scanned-a.unreadable)
	rpln("  OURS    = Mailroom wrote it (drafts, Sent copies)")
	rpln("  LEARNED = learned placement applied a label (app.message_labels.applied_by = 'learned')")
	rpln("  N-FAIL / N-UNAU / N-UNVF = the reading pane's sender-auth notice: failed / unauthenticated / unverifiable")
	rpln("  DMARC   = a trusted DMARC pass aligned to the first parsed From domain, relaxed: header.from equal to it or")
	rpln("            exactly its organisational domain (the verdict placement acts on)")
	if a.examples > 0 {
		rpln()
		for _, s := range fromShapes {
			if r := a.rows[s]; len(r.examples) > 0 {
				rp("examples %-15s %s\n", s, strings.Join(r.examples, " "))
			}
		}
	}
}
