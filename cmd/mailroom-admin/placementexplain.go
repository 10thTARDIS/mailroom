// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/ingest"
	"mailroom/internal/ml"
	"mailroom/internal/placement"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin placement-explain` — WHY did learned placement hold a
// message back (#840)? READ-ONLY, and safe on production: it runs in a
// read-only transaction that is rolled back, never committed.
//
// It re-runs the arrival decision for a message with the SAME functions ingest
// uses (ingest.LearnedSender for the sender, store.ExplainLearnedPlacement for
// the decision, which is the live path's own learnedArrival) and prints the
// trace in the order the decision looked: the sender parse, the keys, the
// evidence per key, the decision, then every sender check with its inputs, and
// finally the outcome and the first step that stopped it. The raw headers
// come from the blob store, through the reader placement-backfill uses.
//
// TIMING. The evidence is evaluated NOW, not at the message's arrival: a
// filing made since counts. --as-of narrows filings and corrections to those
// created before the message was ingested (messages.created_at), which is
// what makes explaining a PAST arrival honest. Even with --as-of the
// correction gate, the user's own domains, the sender-warning index and the
// tab verdict are read as of now; the output says so.
//
// An operator-only tool, so it prints addresses and domains. It never prints a
// subject or a body.
func runPlacementExplain(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("placement-explain", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "user id (ULID)")
		messageID = fs.String("message-id", "", "message id (ULID) to explain")
		sender    = fs.String("sender", "", "explain the latest arrivals from this From address instead")
		limit     = fs.Int("limit", 10, "with --sender: how many of the latest arrivals")
		asOf      = fs.Bool("as-of", false, "count only filings and corrections created before the message was ingested")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if (*messageID == "") == (*sender == "") {
		return errors.New("placement-explain: give exactly one of --message-id or --sender")
	}
	if *limit < 1 {
		return fmt.Errorf("--limit %d: must be at least 1", *limit)
	}
	var msgID *ulid.ULID
	if *messageID != "" {
		id, err := ulid.Parse(*messageID)
		if err != nil {
			return fmt.Errorf("--message-id %q: not a valid ULID: %w", *messageID, err)
		}
		msgID = &id
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
		return fmt.Errorf("placement-explain: master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("placement-explain: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)
	trusted, err := trustedAuthservEntries()
	if err != nil {
		return err
	}

	var names map[ulid.ULID]string
	var cands []store.ExplainCandidate
	var exps []explainedMessage
	if err := d.store.UserReadTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if names, e = tx.AreaNames(ctx); e != nil {
			return e
		}
		if cands, e = tx.ExplainCandidates(ctx, msgID, *sender, *limit); e != nil {
			return e
		}
		for _, c := range cands {
			var at time.Time
			if *asOf {
				at = c.CreatedAt
			}
			ex, e := ingest.ExplainLearned(ctx, tx, blobs, userID, dek, trusted, c, at)
			m := explainedMessage{Cand: c, AsOf: at}
			if e != nil {
				if !errors.Is(e, ingest.ErrUnreadableMessage) {
					return e
				}
				m.Err = e
			} else {
				m.Exp = ex
			}
			exps = append(exps, m)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("placement-explain: %w", err)
	}
	if len(cands) == 0 {
		return errors.New("placement-explain: no such message for this user (--sender matches the stored From address exactly, case-insensitively)")
	}
	printPlacementExplain(os.Stdout, exps, names)
	return nil
}

// explainedMessage is one message's explanation, or why it could not be made.
type explainedMessage struct {
	Cand store.ExplainCandidate
	AsOf time.Time // zero = evidence as of now
	Exp  ingest.LearnedExplanation
	Err  error
}

func printPlacementExplain(w io.Writer, msgs []explainedMessage, names map[ulid.ULID]string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("=== placement-explain (#840): why learned placement did or did not place a message ===\n")
	p("READ ONLY: nothing was written. Evidence is evaluated NOW unless --as-of; the correction gate, the user's own\n")
	p("domains, the sender-warning index and the tab verdict are always read as of now.\n")
	for i, m := range msgs {
		p("\n--- message %d of %d: %s (ingested %s) ---\n", i+1, len(msgs), m.Cand.MessageID,
			m.Cand.CreatedAt.UTC().Format("2006-01-02 15:04:05Z"))
		if m.Err != nil {
			p("cannot explain: %v\n", m.Err)
			continue
		}
		if m.AsOf.IsZero() {
			p("evidence: as of NOW (a filing made since this message arrived counts; use --as-of to exclude them)\n")
		} else {
			p("evidence: as of %s (this message's ingest time): only filings and corrections created before it\n", m.AsOf.UTC().Format("2006-01-02 15:04:05Z"))
		}
		printExplanation(w, m.Exp, names)
	}
}

// mq renders a value that originated in mail (or in anything a sender can
// influence) for the operator's terminal: Go-quoted with QuoteToGraphic, so
// ESC, BEL, CR, bidi overrides and every other non-graphic rune appear as
// visible \x / \u escapes and cannot rewrite the screen or reorder the
// explanation. Fixed labels print raw; every mail-derived value goes through this.
func mq(s string) string { return strconv.QuoteToGraphic(s) }

func mqAll(in []string) string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = mq(v)
	}
	return strings.Join(out, ", ")
}

func areaLabel(names map[ulid.ULID]string, id ulid.ULID) string {
	if n, ok := names[id]; ok {
		return n
	}
	return id.String()
}

func printExplanation(w io.Writer, ex ingest.LearnedExplanation, names map[ulid.ULID]string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	f, tr := ex.Facts, ex.Trace

	p("\n1. SENDER PARSE\n")
	p("  stored From address: %s\n", mq(f.StoredFrom))
	p("  From header fields: %d · non-empty values: %d · mailboxes parsed: %d (%s)\n",
		f.FromFields, len(f.FromValues), len(f.Mailboxes), mqAll(f.Mailboxes))
	if tr.KeysOK {
		p("  parsed address: %s · domain: %s\n", mq(tr.Address.Value), mq(tr.Domain.Value))
	} else {
		p("  no usable address or domain: the message has no key\n")
	}
	p("  ambiguous From (not exactly one mailbox in one header): %s\n", yesNo(tr.Ambiguous))
	p("  learned placement switch: %s\n", map[bool]string{true: "ON", false: "OFF"}[tr.Enabled])

	if !tr.KeysOK {
		printExplainOutcome(w, tr, names)
		return
	}
	p("\n2. KEYS\n")
	p("  address key: %s\n  domain key:  %s\n", mq(tr.Address.Value), mq(tr.Domain.Value))
	cur := "Needs sorting"
	if tr.Current.AreaID != nil {
		cur = areaLabel(names, *tr.Current.AreaID)
	}
	by := "the tab verdict"
	if tr.Current.ByLabel {
		by = "a rule's label (so learned placement would not act)"
	}
	p("  the message's area at arrival: %s, decided by %s\n", cur, by)

	p("\n3. EVIDENCE (the user's own filings into an area's own label)\n")
	printKeyEvidence(w, "address", mq(tr.Address.Value), tr.Evidence.Address, tr.Evidence.AddressVerdict, names)
	printKeyEvidence(w, "domain ", mq(tr.Domain.Value), tr.Evidence.Domain, tr.Evidence.DomainVerdict, names)
	if tr.Evidence.DomainFallbackBlocked {
		p("  the domain fallback is blocked for this domain (freemail or one of the user's own)\n")
	}
	if !tr.Decided {
		p("\n4. DECISION: not reached (an earlier step stopped it)\n")
	} else {
		d := tr.Decision
		p("\n4. DECISION\n")
		if d.Act {
			p("  act: %s key %s -> area %s (reason: %s)\n", d.Key.Kind, mq(d.Key.Value), areaLabel(names, d.Area), d.Reason)
		} else {
			p("  abstain: %s key %s (reason: %s)\n", d.Key.Kind, mq(d.Key.Value), d.Reason)
		}
	}

	p("\n5. SENDER CHECKS (store.senderRefusal's inputs; the live path asks them only when the decision acts)\n")
	if tr.Checks == nil {
		p("  not evaluated\n")
	} else {
		c := tr.Checks
		p("  a. ambiguous From: %s\n", yesNo(c.Ambiguous))
		p("  b. authentication: trust configured: %s · trusted DMARC pass bound to the key domain: %s · affirmative pass: %s · auth_fail (clear failure anywhere): %s\n",
			yesNo(c.Auth.TrustConfigured), dmarcPassLabel(c.Auth), yesNo(c.Auth.AffirmativePass), yesNo(c.Auth.AuthFail))
		p("     authenticated (%s): %s\n", map[bool]string{true: "needs the trusted DMARC pass", false: "no trust configured: needs no clear failure"}[c.Auth.TrustConfigured], yesNo(c.Authenticated))
		printAuthHeaders(w, f, tr.Domain.Value)
		if len(c.Warnings) == 0 {
			p("  c. sender warnings: none\n")
		} else {
			var ws []string
			for _, wn := range c.Warnings {
				ws = append(ws, wn.Kind)
			}
			p("  c. sender warnings: %s\n", strings.Join(ws, ", "))
		}
		refusal := string(c.Refusal)
		if refusal == "" {
			refusal = "none"
		}
		p("  first sender-check refusal: %s\n", refusal)
	}
	if tr.Decided && tr.Decision.Act {
		learned := areaLabel(names, tr.Decision.Area)
		p("  d. tab verdict agrees with the learned area (%s): %s\n", learned, yesNo(tr.AgreesWithDesk))
	}

	printExplainOutcome(w, tr, names)
}

func printKeyEvidence(w io.Writer, kind, value string, ev placement.Evidence, v placement.Verdict, names map[ulid.ULID]string) {
	var parts []string
	for a, n := range ev.Filings {
		parts = append(parts, fmt.Sprintf("%s %d", areaLabel(names, a), n))
	}
	sort.Strings(parts)
	filings := "no filings"
	if len(parts) > 0 {
		filings = strings.Join(parts, " · ")
	}
	_, _ = fmt.Fprintf(w, "  %s %s: %s · addresses %d · key verdict alone: %s", kind, value, filings, ev.Addresses, v.Reason)
	if !ev.LatestCorrection.IsZero() {
		_, _ = fmt.Fprintf(w, " · latest correction %s", ev.LatestCorrection.UTC().Format("2006-01-02 15:04Z"))
	}
	_, _ = fmt.Fprintln(w)
}

func printAuthHeaders(w io.Writer, f ingest.SenderFacts, keyDomain string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	if len(f.TrustedIDs) == 0 {
		p("     trusted authserv-ids: none configured\n")
	} else {
		p("     trusted authserv-ids for this message: %s\n", mqAll(f.TrustedIDs))
	}
	if len(f.AuthHeaders) == 0 {
		p("     Authentication-Results headers: none\n")
		return
	}
	for i, h := range f.AuthHeaders {
		id := mq(h.AuthservID)
		if h.AuthservID == "" {
			id = "(no acceptable authserv-id)"
		}
		role := "not trusted: ignored"
		switch {
		case h.Topmost:
			role = "TRUSTED, topmost: the one header read"
		case h.Trusted:
			role = "trusted but not topmost: ignored"
		}
		p("     header %d: authserv-id %s [%s]\n", i+1, id, role)
		if !h.Trusted {
			continue
		}
		for _, c := range h.Clauses {
			if c.Method != "dmarc" {
				p("       %s=%s\n", mq(c.Method), mq(c.Result))
				continue
			}
			hf := c.Props["header.from"]
			eq := "no header.from"
			switch len(hf) {
			case 0:
			case 1:
				// The rule learned placement acts on (relaxed, #846), not a
				// second copy of it.
				eq = fmt.Sprintf("header.from=%s matches the key domain: %s", mq(hf[0]),
					matchLabel(ml.DMARCHeaderFromMatch(hf[0], keyDomain, ml.DMARCRelaxed)))
			default:
				eq = fmt.Sprintf("header.from given %d times (%s): not a pass", len(hf), mqAll(hf))
			}
			_, _ = fmt.Fprintf(w, "       dmarc=%s · %s (key domain %s)\n", mq(c.Result), eq, mq(keyDomain))
		}
	}
}

// matchLabel names how a header.from matched the key domain (#846).
func matchLabel(m ml.DMARCMatch) string {
	switch m {
	case ml.DMARCExactMatch:
		return "yes (exact match)"
	case ml.DMARCOrgDomainMatch:
		return "yes (org-domain match: header.from is the key domain's organisational domain)"
	default:
		return "no"
	}
}

// dmarcPassLabel is the bound pass as the decision saw it, with how it matched.
func dmarcPassLabel(a placement.SenderAuth) string {
	if !a.DMARCPass {
		return "no"
	}
	switch ml.DMARCMatch(a.DMARCMatch) {
	case ml.DMARCExactMatch:
		return "yes (exact match)"
	case ml.DMARCOrgDomainMatch:
		return "yes (org-domain match)"
	default:
		return "yes"
	}
}

func printExplainOutcome(w io.Writer, tr store.LearnedTrace, names map[ulid.ULID]string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("\n6. OUTCOME\n")
	if g := tr.Result.Gate; g != nil {
		p("  correction gate for the area (as of now): resolved %d · stopped: %s\n", g.Resolved, yesNo(g.Stopped))
	}
	outcome := tr.Result.Label()
	if tr.Result.Placed && tr.Result.AreaID != nil {
		p("  outcome: %s (would be placed in %s)\n", outcome, areaLabel(names, *tr.Result.AreaID))
	} else {
		p("  outcome: %s (reason %q)\n", outcome, string(tr.Result.Reason))
	}
	if tr.FirstRefusal == "" {
		p("  first check that refused: none\n")
	} else {
		p("  first check that refused: %s\n", tr.FirstRefusal)
	}
}
