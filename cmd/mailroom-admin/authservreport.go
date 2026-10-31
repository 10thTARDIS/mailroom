// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/authserv"
	"mailroom/internal/blob"
	"mailroom/internal/config"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// `mailroom-admin auth-report --authserv` — WHO wrote the Authentication-Results
// headers, and whether Mailroom believes them (v1.90.1, #754 context).
//
// WHY IT EXISTS. On a real instance in v1.90.0 the reading pane's "unauthenticated"
// notice fired on a large share of messages, against almost none before.
// "Unauthenticated" means no header bearing a TRUSTED authserv-id
// (MAILROOM_AUTHSERV_IDS) carried a method result. Two causes would produce the
// same total and need opposite fixes:
//
//   - mail imported from a previous provider, whose headers name THAT
//     provider's authserv-id (nothing to trust — the notice should not apply);
//   - the current provider stamping from receiving hosts other than the one
//     trusted id (the fix is a longer trusted list).
//
// So it counts, per authserv-id: messages carrying it anywhere, messages where
// it is the TOPMOST header (the one the receiving MTA wrote last — what
// TopmostTrustedAuthResults keys on), and whether it is trusted; and, per
// RECEIVED MONTH, how many messages had no header at all, a trusted one, a
// trusted one on top, or only untrusted ones — plus each month's most common
// topmost ids. A migration shows up as a block of months with a foreign id on
// top; an MX change as a recent month with an untrusted sibling hostname.
//
// IT USES THE SHIPPED PARSER: ml.AuthservIDOf is the same extraction
// TrustedAuthResults filters by, and ml.IsTrustedAuthservID the same
// normalisation. A second regex here would measure itself.
//
// PRIVACY: authserv-ids are provider infrastructure hostnames, not personal
// data, and are printed. No address, subject or header body is.
//
// READ-ONLY.

// trustedAuthservEntries reads MAILROOM_AUTHSERV_IDS for an admin command and
// refuses a malformed entry (#766): the server refuses to start on one, and a
// report or backfill that silently dropped it would run under a narrower rule
// than the server applies. The result is the raw ENTRIES (`id` or
// `id<YYYY-MM-DD`); resolve them per message with ml.TrustedAt.
func trustedAuthservEntries() ([]string, error) {
	if err := config.ValidateAuthservIDs(); err != nil {
		return nil, err
	}
	return config.TrustedAuthservIDs(), nil
}

// maxAuthservRows bounds the per-id table; the remainder is summarised, never
// dropped silently (a bounded list reports its bound).
const maxAuthservRows = 40

// unreadableID is how a header whose authserv-id cannot be read is listed.
const unreadableID = "(unreadable authserv-id)"

type authservID struct {
	anywhere int // messages with at least one header bearing it
	topmost  int // messages whose topmost header bears it
	// firstTop and lastTop are the earliest and latest received_at (UTC) of the
	// messages it is TOPMOST on: the dates an operator picks a
	// MAILROOM_AUTHSERV_IDS bound from (#766). Zero until it is topmost once.
	firstTop, lastTop time.Time
}

type authservMonth struct {
	messages      int
	noHeader      int // no Authentication-Results at all
	noHeaderLocal int // ... of which Mailroom wrote
	trustedAny    int // some header bears a trusted id
	trustedTop    int // the topmost header bears a trusted id
	untrustedOnly int // headers present, none trusted
	topIDs        map[string]int
}

// noticeOrder is the order the notice categories print in, and is the full
// list: every message lands in exactly one, so they sum to the readable count.
var noticeOrder = []ml.AuthNotice{
	ml.NoticeTrusted, ml.NoticeLocalDKIM, ml.NoticeUnauthenticated, ml.NoticeSelf, ml.NoticeUnconfigured,
}

type authservAggregate struct {
	trusted    []string
	scanned    int
	unreadable int
	ids        map[string]*authservID
	months     map[string]*authservMonth
	// notices counts messages per ml.AuthNotice under the CONFIGURED rule,
	// judged at each message's received time (#766).
	notices map[ml.AuthNotice]int
	// noAR splits the messages with no Authentication-Results at all, each in
	// exactly one row (checked in this order).
	noAR map[string]int
	// untrustedOnly is REPORT-ONLY: messages with Authentication-Results headers
	// but no trusted one. They change nothing about the notice (#766); this is how
	// many still show it (or are self) despite carrying a header, and why.
	untrustedOnly int
	// localBlocked is REPORT-ONLY: messages whose local DKIM pass verified but
	// is not displayed because a signed header is duplicated (#776, ADR-036).
	localBlocked         int // duplicate_header:*
	localBlockedUnsigned int // unsigned_header:*
	// noar is the `--no-ar` breakdown (#776); nil unless asked for. Fed by the
	// same walk, so it costs no second pass over the blobs.
	noar *noARReport
}

// authClass says which `--no-ar` population a message belongs to. add decides it
// once, so the breakdown reuses the classification above rather than re-deriving it.
type authClass struct {
	other   bool // NOAR: inbound, no Authentication-Results at all (the "other inbound" row)
	control bool // CONTROL: inbound with a header bearing a trusted id under the configured rule
}

const (
	noARLocal      = "locally authored"
	noARSentDrafts = "in Sent or Drafts"
	noARFromOwn    = "From one of your identities, not Sent/Drafts"
	noAROther      = "other inbound"
)

var noAROrder = []string{noARLocal, noARSentDrafts, noARFromOwn, noAROther}

func newAuthservAggregate(trusted []string) *authservAggregate {
	return &authservAggregate{trusted: trusted, ids: map[string]*authservID{}, months: map[string]*authservMonth{},
		notices: map[ml.AuthNotice]int{}, noAR: map[string]int{}}
}

// add tallies one message. self is store.AuthSelfFacts.IsSelf for it (live in a
// Sent or Drafts mailbox); fromOwn is whether its From is one of the user's
// identity addresses, which is REPORT-ONLY: the notice does not treat it as
// self, because the sender writes From (#766).
//
// ⚠ `a.trusted` holds the raw ENTRIES; ids is what they resolve to at THIS
// message's received_at, and every trust test below reads ids, so a
// date-bounded entry counts only for the mail it covers.
//
// local is Mailroom's own DKIM verdict for the message, read from
// app.message_local_auth (#776); nil when none was recorded. Only an aligned
// pass changes the category (ml.NoticeLocalDKIM, the "trusted via local DKIM"
// row), through the same ml.AuthNoticeFor the Email property uses.
func (a *authservAggregate) add(ref store.MessageAuthRef, headers []string, self, fromOwn bool, local *store.LocalDKIM) (cl authClass) {
	ids := ml.TrustedAt(a.trusted, ref.TrustTime())
	a.notices[ml.AuthNoticeFor(len(a.trusted) > 0, ml.TrustedAuthResults(headers, ids).Present,
		ref.LocallyAuthored, self, local.LocalDKIMPass())]++
	if local != nil {
		switch {
		case strings.HasPrefix(local.DisplayBlocked, "duplicate_header:"):
			a.localBlocked++
		case strings.HasPrefix(local.DisplayBlocked, "unsigned_header:"):
			a.localBlockedUnsigned++
		}
	}
	if len(headers) > 0 && !ml.TrustedAuthResults(headers, ids).Present {
		a.untrustedOnly++
	}
	key := ref.ReceivedAt.UTC().Format("2006-01")
	m := a.months[key]
	if m == nil {
		m = &authservMonth{topIDs: map[string]int{}}
		a.months[key] = m
	}
	m.messages++
	if len(headers) == 0 {
		switch {
		case ref.LocallyAuthored:
			a.noAR[noARLocal]++
		case self:
			a.noAR[noARSentDrafts]++
		case fromOwn:
			a.noAR[noARFromOwn]++
		default:
			a.noAR[noAROther]++
			cl.other = true
		}
		m.noHeader++
		if ref.LocallyAuthored {
			m.noHeaderLocal++
		}
		return cl
	}
	seen := map[string]bool{}
	trustedAny := false
	for _, h := range headers {
		id := ml.AuthservIDOf(h)
		if id == "" {
			id = unreadableID
		}
		if !seen[id] {
			seen[id] = true
			a.id(id).anywhere++
		}
		if ml.IsTrustedAuthservID(id, ids) {
			trustedAny = true
		}
	}
	cl.control = trustedAny && !ref.LocallyAuthored && !self
	top := ml.AuthservIDOf(headers[0])
	if top == "" {
		top = unreadableID
	}
	tr := a.id(top)
	tr.topmost++
	if r := ref.ReceivedAt.UTC(); tr.firstTop.IsZero() || r.Before(tr.firstTop) {
		tr.firstTop = r
	}
	if r := ref.ReceivedAt.UTC(); r.After(tr.lastTop) {
		tr.lastTop = r
	}
	m.topIDs[top]++
	switch {
	case ml.IsTrustedAuthservID(top, ids):
		m.trustedAny++
		m.trustedTop++
	case trustedAny:
		m.trustedAny++
	default:
		m.untrustedOnly++
	}
	return cl
}

func (a *authservAggregate) id(id string) *authservID {
	r := a.ids[id]
	if r == nil {
		r = &authservID{}
		a.ids[id] = r
	}
	return r
}

func scanCorpusAuthserv(ctx context.Context, d *deps, blobs blob.BlobStore, userID ulid.ULID,
	dek []byte, batch int, trusted []string, noAR bool, examples int, dk *dkimConfig) (*authservAggregate, error) {
	agg := newAuthservAggregate(trusted)
	var (
		accountNames map[ulid.ULID]string
		labels       map[ulid.ULID]store.MailboxLabel
	)
	if noAR {
		agg.noar = newNoARReport(examples)
		if dk != nil {
			agg.noar.dkim = newNoARDKIM(*dk)
		}
	}
	after := ulid.ULID{}
	var (
		localDKIM map[ulid.ULID]store.LocalDKIM
		facts     store.AuthSelfFacts
		own       map[string]bool
		boxes     map[ulid.ULID][]ulid.ULID
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if facts, e = tx.AuthSelfFacts(ctx); e != nil {
			return e
		}
		if own, e = tx.OwnIdentityAddresses(ctx); e != nil || !noAR {
			return e
		}
		if labels, e = tx.MailboxLabels(ctx); e != nil {
			return e
		}
		accs, e := tx.ListAccounts(ctx)
		accountNames = make(map[ulid.ULID]string, len(accs))
		for _, ac := range accs {
			accountNames[ac.ID] = ac.Name
		}
		return e
	}); err != nil {
		return nil, fmt.Errorf("auth-report: self facts: %w", err)
	}
	for {
		var page []store.MessageAuthRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			if page, e = tx.MessageAuthRefsAfter(ctx, after, batch); e != nil {
				return e
			}
			pageIDs := make([]ulid.ULID, len(page))
			for i, r := range page {
				pageIDs[i] = r.ID
			}
			if boxes, e = tx.LiveMailboxIDs(ctx, pageIDs); e != nil {
				return e
			}
			localDKIM, e = tx.LocalDKIMFor(ctx, pageIDs)
			return e
		}); err != nil {
			return nil, fmt.Errorf("auth-report: walk messages: %w", err)
		}
		for _, ref := range page {
			after = ref.ID
			agg.scanned++
			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				agg.unreadable++
				continue
			}
			msg, err := mime.ParseBytes(raw)
			if err != nil || msg == nil {
				agg.unreadable++
				continue
			}
			var local *store.LocalDKIM
			if v, ok := localDKIM[ref.ID]; ok {
				local = &v
			}
			cl := agg.add(ref, msg.Header.Values("Authentication-Results"), facts.IsSelf(boxes[ref.ID]), own[store.NormalizeAddress(ref.FromAddress)], local)
			if agg.noar != nil && (cl.other || cl.control) {
				agg.observeNoAR(ctx, newNoARMsg(ref, raw, boxes[ref.ID], labels, accountNames), ref,
					msg.Header.Values("Authentication-Results"), cl)
			}
		}
		if len(page) < batch {
			break
		}
	}
	return agg, nil
}

// observeNoAR feeds one NOAR or CONTROL message to the --no-ar tables and, with
// --dkim, to the local verifier (the same walk, the same blob).
func (a *authservAggregate) observeNoAR(ctx context.Context, m noARMsg, ref store.MessageAuthRef,
	headers []string, cl authClass) {
	a.noar.observe(m, cl.other)
	if a.noar.dkim != nil {
		a.noar.dkim.observe(ctx, m, cl.other, func() string {
			return providerDKIMVerdict(headers, a.trusted, ref)
		})
	}
}

func (a *authservAggregate) print() {
	rp("scanned                 : %d messages\n", a.scanned)
	rp("unreadable              : %d  (blob or MIME failure — in no row below)\n", a.unreadable)
	if len(a.trusted) > 0 {
		rp("trusted authserv-ids    : %s  (entries; `id<date` = only for mail received before that date, 00:00 UTC)\n", strings.Join(a.trusted, ","))
	} else {
		rp("trusted authserv-ids    : NONE configured (MAILROOM_AUTHSERV_IDS) — nothing below can be trusted\n")
	}
	noHeader, noHeaderLocal := 0, 0
	for _, m := range a.months {
		noHeader += m.noHeader
		noHeaderLocal += m.noHeaderLocal
	}
	readable := a.scanned - a.unreadable
	rp("no Authentication-Results at all : %d of %d readable (%s), of which Mailroom wrote %d\n",
		noHeader, readable, sharePct(noHeader, readable), noHeaderLocal)

	rpln()
	rpln("AUTHSERV-IDS (messages carrying a header with it / messages where it is the TOPMOST header)")
	ids := make([]string, 0, len(a.ids))
	for id := range a.ids {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if a.ids[ids[i]].anywhere != a.ids[ids[j]].anywhere {
			return a.ids[ids[i]].anywhere > a.ids[ids[j]].anywhere
		}
		return ids[i] < ids[j]
	})
	rpln("  FIRST-TOP / LAST-TOP = earliest / latest received date (UTC) of the messages it is TOPMOST on")
	rp("  %-45s %9s %9s  %-10s %-10s  %s\n", "AUTHSERV-ID", "ANYWHERE", "TOPMOST", "FIRST-TOP", "LAST-TOP", "TRUSTED")
	shown := ids
	if len(shown) > maxAuthservRows {
		shown = shown[:maxAuthservRows]
	}
	for _, id := range shown {
		r := a.ids[id]
		rp("  %-45s %9d %9d  %-10s %-10s  %s\n", id, r.anywhere, r.topmost,
			topDate(r.firstTop), topDate(r.lastTop), trustLabel(id, a.trusted))
	}
	if rest := len(ids) - len(shown); rest > 0 {
		restTop := 0
		for _, id := range ids[len(shown):] {
			restTop += a.ids[id].topmost
		}
		rp("  … %d more authserv-ids not shown (topmost on %d messages)\n", rest, restTop)
	}

	rpln()
	rpln("NOTICE UNDER THE CONFIGURED RULE (one message in exactly one row; rows sum to the readable count)")
	readableN := a.scanned - a.unreadable
	sum := 0
	for _, k := range noticeOrder {
		n := a.notices[k]
		sum += n
		rp("  %-16s %8d  %7s  %s\n", k, n, sharePct(n, readableN), noticeNote[k])
	}
	rp("  %-16s %8d  (readable: %d)\n", "total", sum, readableN)
	rp("  report-only: %d message(s) carry Authentication-Results but none from a trusted id; they are in the\n", a.untrustedOnly)
	rpln("  rows above like any other message with no trusted header (an untrusted header does not silence the notice).")
	rp("  report-only: %d message(s) have a local DKIM pass that is NOT displayed because a signed header is duplicated\n", a.localBlocked)
	rpln("  (header-prepend guard, ADR-036); they read \"unauthenticated\".")
	rp("  report-only: %d message(s) have a local DKIM pass that is NOT displayed because a displayed header (Reply-To,\n", a.localBlockedUnsigned)
	rpln("  Cc, ...) is not covered by the signature (header-add guard, ADR-036): what legitimate mail the guard costs.")

	rpln()
	rpln("NO AUTHENTICATION-RESULTS AT ALL, split (rows sum to the NO-AR count above)")
	for _, k := range noAROrder {
		rp("  %-46s %8d\n", k, a.noAR[k])
	}
	rpln("  The first two are the user's own mail (no notice). The third is REPORT-ONLY: a From that")
	rpln("  matches an identity is not treated as the user's own, because the sender writes From, so")
	rpln("  it still reads \"unauthenticated\". Its count is what that choice costs.")

	rpln()
	rpln("BY RECEIVED MONTH")
	rp("  %-7s %8s %8s %8s %8s %9s  %s\n", "MONTH", "MSGS", "NO-AR", "TRUSTED", "TRU-TOP", "UNTRUSTED", "MOST COMMON TOPMOST IDS")
	months := make([]string, 0, len(a.months))
	for k := range a.months {
		months = append(months, k)
	}
	sort.Strings(months)
	for _, k := range months {
		m := a.months[k]
		rp("  %-7s %8d %8d %8d %8d %9d  %s\n", k, m.messages, m.noHeader, m.trustedAny, m.trustedTop,
			m.untrustedOnly, topN(m.topIDs, 3))
	}
	rpln()
	rpln("  NO-AR     = no Authentication-Results header at all")
	rpln("  TRUSTED   = some header bears a trusted authserv-id")
	rpln("  TRU-TOP   = the TOPMOST header bears a trusted id (what learned placement and calendar verification read)")
	rpln("  UNTRUSTED = headers present, none bearing a trusted id (the notice is unchanged by them, #766)")
	if a.noar != nil {
		a.noar.print()
	}
}

// noticeNote explains each notice category in the report.
var noticeNote = map[ml.AuthNotice]string{
	ml.NoticeTrusted:         "a header bears a trusted id (received before its bound, if bounded): its verdicts are shown",
	ml.NoticeLocalDKIM:       "no trusted header, but Mailroom's own DKIM check at ingest passed, aligned with From (#776): \"Verified by Mailroom\"",
	ml.NoticeUnauthenticated: "no Authentication-Results at all, inbound and not the user's own: \"unauthenticated\"",
	ml.NoticeSelf:            "no trusted header; locally authored or in Sent/Drafts (the user's own mail): no notice",
	ml.NoticeUnconfigured:    "MAILROOM_AUTHSERV_IDS names no id: no notice for any message",
}

// topDate renders a received date (UTC), or "-" when the id is never topmost.
func topDate(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02")
}

// trustLabel says how the configured entries treat an id: YES (unbounded),
// "before DATE" (date-bounded, #766), or no.
func trustLabel(id string, entries []string) string {
	label := "no"
	for _, raw := range entries {
		e, ok, err := authserv.Parse(raw)
		if err != nil || !ok || e.ID != strings.ToLower(strings.TrimSpace(id)) {
			continue
		}
		if !e.Bounded {
			return "YES"
		}
		label = "before " + e.Bound.Format("2006-01-02")
	}
	return label
}

func topN(counts map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range counts {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	var parts []string
	for i, e := range all {
		if i == n {
			parts = append(parts, fmt.Sprintf("+%d more", len(all)-n))
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%d", e.k, e.v))
	}
	return strings.Join(parts, " ")
}

func sharePct(n, of int) string {
	if of == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(of))
}
