// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// This file holds the JMAP Email surface (RFC 8621 §4): reads (Email/get,
// Email/query) and the Email/set mutation path — the daily-driver actions
// (mark read/unread, star, add/remove label, archive/trash).
//
// Load-bearing invariant (CLAUDE.md, ADR-003): every Email/set that changes a
// JMAP-visible property appends EXACTLY ONE Email change_log row in the SAME
// UserTx as the canonical mutation, and (spec §4a) enqueues a per-live-account
// writeback intent so the change mirrors back to IMAP. ApplyEmailUpdate is the
// single method that upholds all three at once.

// The Trash dwell time before the purge job hard-deletes (spec §4 / ADR-009:
// 30-day default, configurable via MAILROOM_TRASH_RETENTION) is carried on the
// UserTx (tx.trashRetention), stamped onto purge_after here and in the reaper's
// vanish path. It is no longer a package constant so the operator can widen or
// narrow the restore window; see store.SetTrashRetention.

// System keyword tokens (RFC 8621 §4.1.1) ↔ the canonical flag columns.
const (
	KeywordSeen     = "$seen"
	KeywordFlagged  = "$flagged"
	KeywordAnswered = "$answered"
	KeywordDraft    = "$draft"
	KeywordJunk     = "$junk"
)

// ErrInvalidKeyword refuses a keyword that is not a keyword: an IMAP system
// flag spelled as one (`\Draft`), or anything else carrying a backslash (#49
// review). A keyword intent becomes `UID STORE +FLAGS (<keyword>)`, so a
// keyword of `\Draft` sets the IMAP draft flag, the next sync reads it back as
// is_draft, and the draft guard in ApplyEmailUpdate never saw it happen. The
// system flags have their own EmailUpdate fields and their own checks; a
// keyword is never a way round them.
var ErrInvalidKeyword = errors.New("store: an IMAP system flag cannot be written as a keyword")

// KeywordWritable reports whether kw may be written back to IMAP as a keyword.
// It is the store's backstop, deliberately narrow: no empty keyword, and no
// backslash anywhere (RFC 3501 forbids one in an atom; RFC 8621 §4.1.1 forbids
// one in a keyword; a leading one names a system flag). The JMAP layer applies
// the full RFC 8621 keyword syntax before a patch gets here.
func KeywordWritable(kw string) bool {
	return kw != "" && !strings.ContainsRune(kw, '\\')
}

// KeywordReserved reports whether kw, compared case-insensitively (RFC 8621
// §4.1.1), is a keyword with meaning of its own: a JMAP system keyword whose
// state lives in a flag column ($seen, $flagged, $answered, $draft, $junk), or
// a junk-verdict token that ingest maps to is_junk (JunkKeywords,
// NotJunkKeywords). None of them may be written as a RAW keyword (#49 review):
// `$JUNK` or `junk` written back to the server would come back from the next
// sync as is_junk, around the refusal to set junk, and `$Draft` would skip the
// draft guard. The JMAP layer routes the case variants to their own fields;
// this is the store's backstop for AddKeywords/RemoveKeywords.
//
// ⚠ It is NOT applied in enqueueEmailIntent: the junk verdict's own intents
// legitimately remove the provider's `$Junk` and add `$NotJunk`
// (enqueueJunkVerdictIntents). Raw keywords from a patch never reach an intent
// without passing applyEmailUpdate's check first.
func KeywordReserved(kw string) bool {
	l := strings.ToLower(kw)
	switch l {
	case KeywordSeen, KeywordFlagged, KeywordAnswered, KeywordDraft, KeywordJunk:
		return true
	}
	return slices.Contains(JunkKeywords, l) || slices.Contains(NotJunkKeywords, l)
}

// IMAP system-flag names for writeback intents (spec §4a deltas).
const (
	flagSeen     = `\Seen`
	flagFlagged  = `\Flagged`
	flagAnswered = `\Answered`
	flagDraft    = `\Draft`
)

// EmailView is a JMAP Email projection assembled from the canonical message row
// plus its live mailbox membership (provenance) and label keywords. Keywords
// merges the system flags, the raw messages.keywords[], and the label keyword
// atoms (labels are keywords, spec §13) into the single JMAP keyword set.
type EmailView struct {
	ID            ulid.ULID
	ThreadID      ulid.ULID
	ContentHash   []byte
	MailboxIDs    []ulid.ULID
	Keywords      []string
	Size          int64
	ReceivedAt    time.Time
	SentAt        *time.Time
	Subject       string
	Preview       string
	FromName      string
	FromAddress   string
	HasAttachment bool
	Trashed       bool

	// DeliveredTo is the alias this message was delivered to — the address a
	// reply defaults to sending FROM (spec §5). AddressedIdentityID is the
	// verified identity it resolved to, zero when none did. Both are Mailroom
	// extension properties on the JMAP Email object (urn:mailroom:compose).
	DeliveredTo         string
	AddressedIdentityID ulid.ULID

	// MessageIDHint is the RFC 5322 Message-ID header (a threading hint, never
	// identity — ADR-001). Reply/In-Reply-To needs it.
	MessageIDHint string
	// InReplyTo is the message's own In-Reply-To header id; References is its
	// References chain.
	InReplyTo  string
	References []string

	// PinnedAt is when the user pinned this message, or nil if it is not pinned
	// (spec §6, #51). A timestamp rather than a bool so a client can order
	// pinned-first by recency of pinning; api.emailObject renders it as the
	// isPinned/pinnedAt pair under urn:mailroom:organize.
	PinnedAt *time.Time

	// SnoozedUntil is when this message returns, or nil if it is not snoozed
	// (spec §6, #51). It is the WHOLE snooze state: a message is snoozed exactly
	// while this is set and still in the future, so a wake is one UPDATE to NULL.
	// api.emailObject renders it as the isSnoozed/snoozedUntil pair under
	// urn:mailroom:organize, deriving the boolean against the current time —
	// which is what makes a message reappear at its wake time even if the wake
	// job runs late.
	SnoozedUntil *time.Time

	// SnoozeWokeAt is when a snooze last returned this message, or nil if one
	// never has (spec §6, #152). It is what "effective arrival" ordering sorts by,
	// and what lets a client say WHY a week-old message is at the top of the
	// inbox — without it, that is indistinguishable from a bug.
	SnoozeWokeAt *time.Time

	// FlaggedAt is when is_flagged last went false->true, or nil when the
	// message is not flagged (#444): the in-tray's "waiting since". It rides
	// IMAP \Flagged (no Mailroom-only keyword, decided on #444), so it is
	// read-only from the JMAP side — the flag itself is set through the
	// $flagged keyword, and whichever write path flips is_flagged stamps this
	// alongside it in the same statement (ingest.go, this file's
	// ApplyEmailUpdate, rules.go's applyRuleAction), so the two columns can
	// never disagree about whether a message is flagged.
	FlaggedAt *time.Time

	// PriorityScore is the T1 triage priority in [0,1] (app.triage_scores), or
	// nil when the message has not been scored (no row — e.g. mail ingested
	// before scoring was wired, or a scoring failure that was swallowed). It is a
	// Mailroom extension property on the JMAP Email object (urn:mailroom:triage);
	// isImportant is derived from it at render time (api.emailObject).
	PriorityScore *float64

	// TopFeatures is the RAW app.triage_scores.top_features JSON (the #34
	// explainability payload): a machine-readable []ml.FeatureContribution array
	// of the top per-feature contributions behind the score. It is nil when the
	// message is unscored (no triage row), and the empty array []byte("[]") for a
	// score whose model could not explain itself. api.emailObject decodes it and
	// maps each key through ml.FeatureLabel to render priorityReasons.
	TopFeatures []byte

	// ListID is the message's List-Id header value, or "" when it is not list
	// mail (app.messages.list_id, projected onto the row by migration 00016).
	//
	// It is exposed because it is the most PRECISE thing a bulk message carries:
	// it names the actual list rather than the domain that happens to send it,
	// which is what lets a rule catch a retailer's newsletter without also
	// catching that retailer's receipts (#155's "create a rule from this
	// message", and the discriminator #156 needs).
	ListID string

	// ListUnsubscribe is the RAW List-Unsubscribe header value (RFC 2369), or ""
	// when the message carries none (app.messages.list_unsubscribe, projected onto
	// the row by migration 00016).
	//
	// Raw on purpose: the store's job is to hand back what arrived. Turning a header
	// written by a stranger into something a client may put in an href is a policy
	// decision, and it lives in one place — internal/api's parseListUnsubscribe
	// (#58) — rather than being half-done here and half-done in the browser.
	ListUnsubscribe string

	// SuggestedLabels are the classifier's open questions about this message
	// (#169): labels it considered and was not confident enough to apply. Empty
	// for the overwhelming majority of mail — the model either decided or had
	// nothing to say.
	SuggestedLabels []LabelSuggestion

	// LabelProvenance says who put each label on this message (#155): the user,
	// or an automation — and for a rule, WHICH rule, by name. It is what makes a
	// surprising label traceable to its cause instead of a mystery the user has
	// to reverse-engineer. Empty when the message carries no labels.
	LabelProvenance []LabelProvenance

	// LabelIDs are the ids of EVERY label on this message (#704), mirrored and
	// local-only alike, straight from app.message_labels. Keywords carries only
	// the mirrored ones (as atoms); a local-only label — every area's own label
	// (ADR-031) — has no atom and is visible ONLY here. Order is unspecified.
	LabelIDs []ulid.ULID

	// LocallyAuthored is true when Mailroom wrote this message — a draft, or a
	// copy of something the user sent — rather than receiving it from a server.
	// It is `local_mailbox_id IS NOT NULL` (db/schema.sql: "NULL for anything
	// that came from the server").
	//
	// ⚠ IT IS A DURABLE MARKER, NOT A TRANSIENT ONE, and the sender-auth display
	// depends on that. The column's own comment reads "until the APPEND
	// round-trips", which invites the reading that it is cleared afterwards. It is
	// not: nothing in this repo ever sets it back to NULL — when provenance
	// arrives, content-hash dedup attaches it to THIS row and the column stays.
	// So it still identifies our own mail long after the round trip, which is
	// exactly what a display must not get wrong.
	//
	// WHY THE API NEEDS IT (#55 increment 1): locally authored mail never crosses
	// an authenticating hop, so it carries no Authentication-Results BY
	// CONSTRUCTION. In practice a large share of the messages with
	// no such header are ours. Reporting those as "not authenticated" would be
	// actively wrong, so authResults is null for them rather than empty — "this
	// question does not apply" and "nobody answered it" are different claims.
	LocallyAuthored bool
}

// LabelProvenance is one label on a message, plus who applied it.
type LabelProvenance struct {
	// KeywordAtom is how the label appears in Email.keywords, so a client can
	// join this to what it already renders without a second lookup.
	KeywordAtom string
	// AppliedBy is one of the LabelBy* constants.
	AppliedBy string
	// RuleID and RuleName identify the rule, when AppliedBy is LabelByRule and
	// the rule still exists. A rule deleted after it labelled something leaves
	// AppliedBy == "rule" with no id (ON DELETE SET NULL) — "an automation did
	// this, and it is gone", which is the truth and better than naming a rule
	// that no longer exists.
	RuleID   string
	RuleName string
}

// GetEmails returns Email projections for the requested ids (RLS-scoped). Ids
// not returned were not found or belong to another user — indistinguishable by
// design (the isolation property). Result order is unspecified; the caller
// re-associates by id.
func (tx *UserTx) GetEmails(ctx context.Context, ids []ulid.ULID) ([]EmailView, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	idStrs := ulidStrings(ids)

	rows, err := tx.tx.Query(ctx, `
		SELECT id, COALESCE(thread_id, ''), content_hash,
		       size_bytes, received_at, sent_at,
		       subject, snippet, from_name, COALESCE(from_address, ''),
		       has_attachments,
		       is_seen, is_flagged, is_answered, is_draft, is_junk,
		       keywords, (trashed_at IS NOT NULL), pinned_at, snoozed_until, snooze_woke_at, flagged_at,
		       COALESCE(addressed_address, ''), COALESCE(addressed_identity_id, ''),
		       COALESCE(message_id_hint, ''), COALESCE(in_reply_to, ''), references_ids,
		       COALESCE(local_mailbox_id, ''), COALESCE(moved_mailbox_id, ''),
		       COALESCE(list_id, ''), COALESCE(list_unsubscribe, ''),
		       (SELECT ts.score::double precision FROM app.triage_scores ts
		         WHERE ts.user_id = messages.user_id AND ts.message_id = messages.id),
		       (SELECT ts.top_features FROM app.triage_scores ts
		         WHERE ts.user_id = messages.user_id AND ts.message_id = messages.id)
		  FROM app.messages
		 WHERE user_id = $1 AND id = ANY($2)`,
		tx.userID.String(), idStrs)
	if err != nil {
		return nil, fmt.Errorf("store: get emails: %w", err)
	}
	defer rows.Close()

	views := make(map[ulid.ULID]*EmailView)
	// fromProvenance are the ids whose membership still comes from where IMAP
	// says the message physically is. A message the USER has moved is not one of
	// them: its placement is canonical and overrides provenance until (and
	// unless) IMAP catches up. See EmailView.MailboxIDs.
	fromProvenance := make([]string, 0, len(ids))
	order := make([]ulid.ULID, 0, len(ids))
	for rows.Next() {
		var (
			ev                             EmailView
			id, threadID                   string
			seen, flagged, answered, draft bool
			junk                           bool
			rawKeywords                    []string
			identityID, localMailbox       string
			movedMailbox                   string
			priorityScore                  *float64
			topFeatures                    []byte
		)
		if err := rows.Scan(&id, &threadID, &ev.ContentHash,
			&ev.Size, &ev.ReceivedAt, &ev.SentAt,
			&ev.Subject, &ev.Preview, &ev.FromName, &ev.FromAddress,
			&ev.HasAttachment,
			&seen, &flagged, &answered, &draft, &junk,
			&rawKeywords, &ev.Trashed, &ev.PinnedAt, &ev.SnoozedUntil, &ev.SnoozeWokeAt, &ev.FlaggedAt,
			&ev.DeliveredTo, &identityID,
			&ev.MessageIDHint, &ev.InReplyTo, &ev.References,
			&localMailbox, &movedMailbox, &ev.ListID, &ev.ListUnsubscribe,
			&priorityScore, &topFeatures); err != nil {
			return nil, fmt.Errorf("store: scan email: %w", err)
		}
		ev.PriorityScore = priorityScore
		ev.TopFeatures = topFeatures
		// Read straight off the column that was already selected for placement —
		// no extra column, no extra query. See EmailView.LocallyAuthored.
		ev.LocallyAuthored = localMailbox != ""
		if ev.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse email id %q: %w", id, err)
		}
		if threadID != "" {
			if ev.ThreadID, err = ulid.Parse(threadID); err != nil {
				return nil, fmt.Errorf("store: parse thread id %q: %w", threadID, err)
			}
		}
		if identityID != "" {
			if ev.AddressedIdentityID, err = ulid.Parse(identityID); err != nil {
				return nil, fmt.Errorf("store: parse addressed identity id %q: %w", identityID, err)
			}
		}
		switch {
		case movedMailbox != "":
			// The user moved this message (archive/trash/move). That placement is
			// the canonical answer to "where is this message" from the instant the
			// Email/set commits — it does NOT wait for the IMAP round-trip, and it
			// SUPPRESSES the source folder's provenance, which still (correctly)
			// records where the bytes physically sit until writeback lands.
			u, err := ulid.Parse(movedMailbox)
			if err != nil {
				return nil, fmt.Errorf("store: parse moved mailbox id %q: %w", movedMailbox, err)
			}
			ev.MailboxIDs = append(ev.MailboxIDs, u)
		default:
			// A message Mailroom composed but has not yet APPENDed lives in its
			// local mailbox: without this, a fresh draft would be in NO mailbox
			// until the next sync pass round-tripped it (db/schema.sql
			// app.messages.local_mailbox_id).
			if localMailbox != "" {
				u, err := ulid.Parse(localMailbox)
				if err != nil {
					return nil, fmt.Errorf("store: parse local mailbox id %q: %w", localMailbox, err)
				}
				ev.MailboxIDs = append(ev.MailboxIDs, u)
			}
			fromProvenance = append(fromProvenance, id)
		}
		ev.Keywords = systemKeywords(seen, flagged, answered, draft, junk)
		ev.Keywords = append(ev.Keywords, rawKeywords...)
		views[ev.ID] = &ev
		order = append(order, ev.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: get emails: %w", err)
	}

	if err := tx.attachMailboxIDs(ctx, fromProvenance, views); err != nil {
		return nil, err
	}
	if err := tx.attachLabelKeywords(ctx, idStrs, views); err != nil {
		return nil, err
	}
	// The classifier's open questions (#169). One query for the whole batch, and
	// almost always empty — the model either decided or had nothing to say.
	if suggestions, err := tx.SuggestionsForMessages(ctx, order); err != nil {
		return nil, err
	} else if len(suggestions) > 0 {
		for id, list := range suggestions {
			if ev, ok := views[id]; ok {
				ev.SuggestedLabels = list
			}
		}
	}

	out := make([]EmailView, 0, len(order))
	for _, id := range order {
		out = append(out, *views[id])
	}
	return out, nil
}

// systemKeywords renders the JMAP system keyword tokens present on a message.
func systemKeywords(seen, flagged, answered, draft, junk bool) []string {
	var out []string
	if seen {
		out = append(out, KeywordSeen)
	}
	if flagged {
		out = append(out, KeywordFlagged)
	}
	if answered {
		out = append(out, KeywordAnswered)
	}
	if draft {
		out = append(out, KeywordDraft)
	}
	if junk {
		out = append(out, KeywordJunk)
	}
	return out
}

// attachMailboxIDs fills in each view's live mailbox membership from provenance.
// idStrs holds only the messages whose membership provenance still decides — a
// message with a canonical placement (moved_mailbox_id) is not passed here.
func (tx *UserTx) attachMailboxIDs(ctx context.Context, idStrs []string, views map[ulid.ULID]*EmailView) error {
	if len(idStrs) == 0 {
		return nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, array_agg(DISTINCT mailbox_id)
		  FROM app.message_provenance
		 WHERE user_id = $1 AND message_id = ANY($2) AND state = 'live'
		 GROUP BY message_id`,
		tx.userID.String(), idStrs)
	if err != nil {
		return fmt.Errorf("store: get emails (mailboxes): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		var mailboxIDs []string
		if err := rows.Scan(&messageID, &mailboxIDs); err != nil {
			return fmt.Errorf("store: scan email mailboxes: %w", err)
		}
		mid, err := ulid.Parse(messageID)
		if err != nil {
			return fmt.Errorf("store: parse message id %q: %w", messageID, err)
		}
		ev, ok := views[mid]
		if !ok {
			continue
		}
		// The view may already carry the local placement (a draft we appended
		// and have now synced back); membership is a set, so de-dup.
		have := make(map[ulid.ULID]bool, len(ev.MailboxIDs))
		for _, id := range ev.MailboxIDs {
			have[id] = true
		}
		for _, m := range mailboxIDs {
			u, err := ulid.Parse(m)
			if err != nil {
				return fmt.Errorf("store: parse mailbox id %q: %w", m, err)
			}
			if have[u] {
				continue
			}
			have[u] = true
			ev.MailboxIDs = append(ev.MailboxIDs, u)
		}
	}
	return rows.Err()
}

// attachLabelKeywords appends each message's label keyword atoms to its keyword
// set (labels are keywords, spec §13; the atom is the persisted mapping),
// records who applied each one (#155), and collects the ids of EVERY label on
// the message into LabelIDs (#704) — mirrored and local-only alike.
//
// ⚠ THE JOIN ON label_keyword_map IS A LEFT JOIN, AND ONLY THE ATOM SIDE IS
// FILTERED (#704). Until then it was an INNER JOIN, which made a local-only
// label (no atom — every area's own label, ADR-031) invisible on Email/get: the
// row was in message_labels, the area resolver counted it, and no client could
// see or toggle it. A NULL atom now contributes its id to LabelIDs and nothing
// else, so Keywords and LabelProvenance are byte-for-byte what they were for
// mirrored labels — and a local label never appears as a keyword, which is the
// whole point of it being local.
//
// Both come from ONE row-per-label query rather than the previous aggregate plus
// a second pass: the provenance is per-label, so it cannot be array_agg'd
// alongside the atoms without a composite, and a second query would double the
// round trips on the hottest read path in the server for a property that is
// almost free to carry here.
//
// The LEFT JOIN on app.rules is what makes "labelled by rule X" say X. It is a
// left join because ON DELETE SET NULL means the rule may be gone; the label
// then reports applied_by='rule' with no id, which is exactly what happened.
func (tx *UserTx) attachLabelKeywords(ctx context.Context, idStrs []string, views map[ulid.ULID]*EmailView) error {
	rows, err := tx.tx.Query(ctx, `
		SELECT ml.message_id, ml.label_id, lkm.keyword_atom, ml.applied_by,
		       COALESCE(ml.applied_by_rule_id::text, ''), COALESCE(r.name, '')
		  FROM app.message_labels ml
		  LEFT JOIN app.label_keyword_map lkm
		    ON lkm.user_id = ml.user_id AND lkm.label_id = ml.label_id
		  LEFT JOIN app.rules r
		    ON r.user_id = ml.user_id AND r.id = ml.applied_by_rule_id
		 WHERE ml.user_id = $1 AND ml.message_id = ANY($2)`,
		tx.userID.String(), idStrs)
	if err != nil {
		return fmt.Errorf("store: get emails (labels): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID, labelID string
		var atom *string
		var p LabelProvenance
		if err := rows.Scan(&messageID, &labelID, &atom, &p.AppliedBy, &p.RuleID, &p.RuleName); err != nil {
			return fmt.Errorf("store: scan email labels: %w", err)
		}
		mid, err := ulid.Parse(messageID)
		if err != nil {
			return fmt.Errorf("store: parse message id %q: %w", messageID, err)
		}
		lid, err := ulid.Parse(labelID)
		if err != nil {
			return fmt.Errorf("store: parse label id %q: %w", labelID, err)
		}
		if ev, ok := views[mid]; ok {
			ev.LabelIDs = append(ev.LabelIDs, lid)
			if atom != nil {
				p.KeywordAtom = *atom
				ev.Keywords = append(ev.Keywords, p.KeywordAtom)
				ev.LabelProvenance = append(ev.LabelProvenance, p)
			}
		}
	}
	return rows.Err()
}

// inMailboxSQL returns the predicate "message m is EFFECTIVELY in mailbox
// mailboxID", for the caller's `m` alias. Membership, in priority order:
//
//  1. the CANONICAL placement (moved_mailbox_id) — where the user put this
//     message. It overrides provenance outright, because the source folder's
//     provenance row still says "INBOX" until the IMAP move lands (and, since
//     there is no VANISHED/expunge reconciliation yet, possibly after it does).
//     Without this the message the user just archived reappears in the inbox
//     seconds later.
//  2. live provenance — where IMAP says the bytes physically are.
//  3. the local placement of a message we composed and have not APPENDed yet
//     (drafts, spec §5), which has no provenance at all.
//
// It is the per-message form of effectiveMembershipSQL below (which produces
// the whole relation), and it is ONE function because two callers now need the
// same answer: Email/query's `inMailbox` filter, and day-one setup's
// bringToDesk step, which must count and perform a move ONLY for a message
// whose effective mailbox is not already the account's INBOX (#446). A second
// hand-copy would let the preview promise a move the apply then declines to
// make, or the reverse — the count and the write disagreeing about the same
// message.
//
// ⚠ IT IS THREE-VALUED-LOGIC-FREE, AND THAT IS LOAD-BEARING FOR THE SECOND
// CALLER. `m.local_mailbox_id = $n` is NULL — not false — for the ordinary
// server-delivered message whose local_mailbox_id is NULL, and `false OR NULL`
// is NULL, so the whole expression used to yield NULL for exactly the common
// case. In a WHERE clause NULL and false behave alike, so Email/query never
// noticed; `NOT (NULL)` is NULL, so day-one's "this message is NOT already in
// the inbox" counted ZERO MOVES and its chunk query returned a NULL boolean.
// MEASURED as a red test, not inferred. Hence the explicit IS NOT NULL guard:
// the expression is now strictly true or false for every row.
//
// Parenthesised as one expression so a caller can safely wrap it in `NOT (...)`.
//
// userID is the caller's own user (tx.userID): the provenance subquery is
// scoped to it explicitly — see mailboxMembershipSQL.
func inMailboxSQL(bind func(any) string, userID ulid.ULID, mailboxID ulid.ULID) string {
	return mailboxMembershipSQL(bind(userID.String()), "= "+bind(mailboxID.String()))
}

// inAnyMailboxSQL is inMailboxSQL over a SET of mailboxes: the message is in at
// least one of them. An empty set matches nothing (`= ANY('{}')` is false).
func inAnyMailboxSQL(bind func(any) string, userID ulid.ULID, mailboxIDs []ulid.ULID) string {
	return mailboxMembershipSQL(bind(userID.String()), "= ANY("+bind(ulidStrings(mailboxIDs))+")")
}

// mailboxMembershipSQL is the body inMailboxSQL and inAnyMailboxSQL share, so
// the membership derivation is written once. userPH is the caller's
// placeholder for its own user id; match is the comparison applied to each
// candidate column, placeholder included.
//
// ⚠ THE PROVENANCE ARM IS A NON-CORRELATED `m.id IN (SELECT …)`, NOT AN
// EXISTS ON THE MESSAGE (#660). Email/query's default trash exclusion wraps
// this in NOT for EVERY message of a list, so a correlated EXISTS was
// evaluated once per message — tens of thousands of times on a large Inbox —
// and each evaluation re-ran the RLS policy per provenance row, because the
// user was the outer message's column rather than a constant. As written the
// subquery is built once, as a hash over the user's live provenance in the
// matched mailboxes, and probed per message. Scoped to userPH explicitly, so
// it never contains another user's rows whatever the caller can see; the
// caller's `m` is its own user's messages.
//
// Still three-valued-logic-free (see inMailboxSQL): m.id is a primary key and
// p.message_id is NOT NULL, so the IN is strictly true or false.
func mailboxMembershipSQL(userPH, match string) string {
	return `(CASE WHEN m.moved_mailbox_id IS NOT NULL
		 THEN m.moved_mailbox_id ` + match + `
		 ELSE (m.id IN (SELECT p.message_id FROM app.message_provenance p
		        WHERE p.user_id = ` + userPH + `
		          AND p.state = 'live' AND p.mailbox_id ` + match + `)
		       OR (m.local_mailbox_id IS NOT NULL AND m.local_mailbox_id ` + match + `)) END)`
}

// effectiveMembershipSQL is THE derivation of "which mailboxes is a message in",
// as one relation of (message_id, mailbox_id, is_seen, trashed_at). It binds one
// argument, $1 = the user id.
//
// Three sources, in strict priority order — the same order GetEmails and
// QueryEmails apply, stated once here so the JMAP membership a client reads and
// the counts it sees on the Mailbox object can never disagree:
//
//  1. moved_mailbox_id — the CANONICAL placement, where the user moved the
//     message. It overrides provenance from the moment the Email/set commits.
//  2. local_mailbox_id — a message Mailroom authored that has not been APPENDed
//     yet (drafts, spec §5) and therefore has no provenance at all.
//  3. live provenance — where IMAP says the bytes physically are.
const effectiveMembershipSQL = `
	SELECT m.id AS message_id, m.moved_mailbox_id AS mailbox_id,
	       m.is_seen, m.trashed_at
	  FROM app.messages m
	 WHERE m.user_id = $1 AND m.moved_mailbox_id IS NOT NULL
	UNION ALL
	SELECT m.id, m.local_mailbox_id, m.is_seen, m.trashed_at
	  FROM app.messages m
	 WHERE m.user_id = $1 AND m.moved_mailbox_id IS NULL
	   AND m.local_mailbox_id IS NOT NULL
	UNION ALL
	SELECT p.message_id, p.mailbox_id, m.is_seen, m.trashed_at
	  FROM app.message_provenance p
	  JOIN app.messages m ON m.id = p.message_id AND m.user_id = p.user_id
	 WHERE p.user_id = $1 AND p.state = 'live' AND m.moved_mailbox_id IS NULL`

// EmailQuery is the (subset of) filter/sort/window Email/query supports in 0.5.
// A nil filter field is "no constraint". Sort is by received_at; SortAscending
// flips the default newest-first order.
type EmailQuery struct {
	InMailbox  *ulid.ULID
	InThread   *ulid.ULID
	InAccount  *ulid.ULID
	HasKeyword string
	NotKeyword string

	// RestrictAccounts, when non-nil, confines the query to messages in these
	// accounts — the account restriction of an API key (#49, ADR-042 §5),
	// applied HERE, in the one WHERE every Email/query path builds, rather than
	// by filtering ids afterwards (which would break position/total and miss a
	// path). A non-nil EMPTY slice matches nothing. The predicate is
	// keyAccountsSQL, the same one MessagesInAccounts answers for Email/get and
	// Email/set, so a key can never find by query what it cannot fetch.
	RestrictAccounts []ulid.ULID

	// RestrictIDs, when non-nil, restricts the query to these message ids — the
	// candidate set a full-text search produced (spec §7, internal/search). The
	// index answers only "which messages match this text?"; Postgres stays the
	// authority on the structured predicates, the ORDER BY, the total, and the
	// window, so a text-filtered Email/query keeps exactly the JMAP semantics of
	// an unfiltered one.
	//
	// A non-nil but EMPTY slice means "the search matched nothing" and returns
	// nothing — distinct from nil, which means "no text filter was applied".
	RestrictIDs []ulid.ULID

	// Before/After bound received_at (RFC 8621 §4.4.1: after is inclusive,
	// before is exclusive). They are enforced HERE, in Postgres, rather than in
	// the search index: JMAP's bounds carry a timestamp, and the index's date
	// filter is day-granular by construction (the engine binding exposes no
	// numeric fields — see internal/search/daterange.go). Postgres keeps the
	// API's contract exact.
	After  *time.Time
	Before *time.Time

	// Snoozed, when set, keeps only snoozed messages (true) or only messages
	// that are not snoozed (false) — spec §6's Snoozed view, and the predicate
	// that takes a snoozed message OUT of the inbox view (#51).
	//
	// "Snoozed" is evaluated against the CURRENT TIME, not merely against the
	// column being non-null. The two differ for a message whose wake time has
	// passed but whose wake job has not run yet, and the clock is the honest
	// answer there: the user asked for the message back at 9am, so at 9am it is
	// back in the inbox view whether or not the timer has fired. The job then
	// makes it durable (clears the column, appends the change_log row) — it is
	// not what makes it TRUE.
	Snoozed *bool

	Position int
	Limit    int
	// LimitSet is true when the client sent a limit, including zero — Limit
	// alone cannot say, because 0 is also the zero value. RFC 8620 §5.5: limit
	// absent/null means the server chooses a page size; limit: 0 is a
	// legitimate request for zero ids (the way to ask only for `total` and
	// `queryState`). Without this flag the two are indistinguishable and an
	// explicit zero silently reads as unbounded.
	LimitSet      bool
	SortAscending bool

	// Important, when set, keeps only messages the T1 model scored at or above
	// ImportantThreshold (true) or below it (false) — spec §8's priority inbox
	// (#154). A message with NO score is NOT important: a model that has not
	// spoken must not promote mail.
	Important *bool

	// NeedsCategory selects mail NOTHING HAS CATEGORISED (#169/#176) — the review
	// queue. True means: an open suggestion about it, OR no label on it at all,
	// and in neither case a dismissal saying it does not need one. False is the
	// exact complement.
	//
	// It is not "the classifier abstained". That was the original reading and it
	// describes an empty set on a mailbox with no trained model — which is every
	// mailbox on its first day, and the day supervision is worth the most. #176
	// lists three sources for this queue and the abstention is only one of them.
	//
	// A message that carries any label — the user's, a rule's, the classifier's —
	// has a category and is not waiting on anybody.
	NeedsCategory *bool

	// InArea selects mail whose resolved area is this one (#442); Unsorted
	// selects mail no area claims (Needs sorting). Both use AreaPredicate —
	// the SQL twin of internal/areas.Resolve (areas_resolve.go). Mutually
	// exclusive; the api refuses both.
	InArea   *ulid.ULID
	Unsorted bool

	// OnDesk, when set, keeps only messages that are (true) or are not (false)
	// "on the desk" (#375): live in INBOX or a feeds_desk mailbox and not junk,
	// trashed, vanished or draft — the SAME predicate AreaSummary counts over
	// (deskMessageSQL), so a tab's queries and its counts cannot disagree.
	OnDesk *bool
	// OnSurface, when set, splits mail at the desk's line (#375): true keeps
	// everything on the surface OR unjudged; false keeps what is in the drawer.
	// The side is lineSideSQL's — the user's own placement (#464) if they made
	// one, else the score against LineThreshold — so this mirrors AreaSummary's
	// surface+unscored / drawer split exactly, by construction.
	OnSurface *bool
	// LineThreshold is the desk's line (DeskPreferences.LineThreshold), supplied by
	// the API from the same transaction; only read when OnSurface is set or
	// Search uses SearchAboveLine.
	LineThreshold float32

	// DeskArchived, when set, keeps only mail that IS (true) or is NOT (false)
	// "archived in Mailroom" — day-one setup took it off the desk without
	// moving anything on the server (#446, app.messages.desk_archived_at).
	//
	// It exists because that mail is otherwise unreachable as a set. It is not
	// in a folder anybody moved it to, it carries no keyword, and `onDesk:false`
	// answers a much broader question (it also matches every message in Sent,
	// in Trash, in a folder that does not feed the desk). The client's All-mail
	// view needs to be able to say "here is what day-one filed away", and that
	// requires a filter that reads the column itself.
	DeskArchived *bool

	// InTab keeps only mail whose resolved category tab is one of these (#228,
	// ADR-022). Empty means no tab filter.
	//
	// IT IS THE EXPANDED SET, not the tab the user clicked, and the caller does
	// the expanding. tabs.Fold sends mail from a disabled tab to Primary, so
	// asking for Primary means "primary, plus every tab that is switched off",
	// while asking for an enabled tab means just that tab. Doing the fold in the
	// caller keeps ONE implementation of it (in Go) instead of a second one in
	// SQL — the same reason the precedence order is bound in as a parameter
	// rather than defined in the database.
	InTab []Tab

	// ImportantThreshold is the score cutoff Important and SortByPriority use.
	// It is supplied by the caller rather than hardcoded here because it is a
	// PRESENTATION decision (api.ImportantThreshold), and spec §8 says it is
	// per-user adjustable — the store has no business deciding what "important"
	// means.
	ImportantThreshold float64

	// SortByPriority groups the result into the two buckets spec §6 describes —
	// important first, then everything else — with each bucket ordered by
	// effective arrival. That is what makes a Priority view PAGE correctly: the
	// grouping happens in SQL, so page 2 continues the same order instead of
	// re-sorting a window the client happens to hold.
	//
	// It does not order by raw score. Within "important", newest-first is what a
	// reader wants; ranking a three-week-old 0.9 above this morning's 0.7 would be
	// a scoreboard, not an inbox.
	SortByPriority bool

	// SortBySuggestionConfidence orders by the message's HIGHEST open suggestion
	// confidence, descending, before the date key — the needs-a-category queue's
	// own order (#176).
	//
	// It exists because "most-confident first" cannot be done client-side. The
	// client can only reorder the page it was given, and a page ordered by
	// received_at is the NEWEST N questions, not the most confident ones — which
	// on a mailbox with hundreds of open questions is a different set entirely.
	// This is what migration 00018's label_suggestions_by_confidence index was
	// created for; until now nothing read it.
	//
	// Most-confident-first is not a scoreboard, it is where a human's answer is
	// worth the most: the messages the model came CLOSEST to being sure about are
	// the ones an answer teaches it most about, and the ones where a person can
	// answer fastest because the guess in front of them is nearly right.
	//
	// Messages with no suggestions sort LAST rather than vanishing (NULLS LAST),
	// so the sort stays meaningful on a query that did not filter on
	// needsCategory — a sort must not silently act as a filter.
	SortBySuggestionConfidence bool

	// SortByEffectiveArrival orders by COALESCE(snooze_woke_at, received_at)
	// instead of received_at (spec §6, #152): a message that came back from a
	// snooze sorts by WHEN IT CAME BACK, so it returns to the top of the list
	// rather than to the position its age would give it — which, for anything
	// snoozed more than a few hours, is far enough down to defeat the snooze.
	//
	// Off by default, and never applied to an RFC 8621 `receivedAt` sort, because
	// it is NOT that sort: a client that asked to order by receivedAt must get
	// receivedAt. api exposes it as its own Mailroom sort property.
	//
	// The key is MUTABLE — a wake changes it — unlike received_at/sent_at, so a
	// query using it can never be delta-stable. See emailQueryPlan.
	SortByEffectiveArrival bool

	// CollapseThreads keeps only the FIRST message of each thread in this
	// query's own sort order (RFC 8621 §4.4.4), for both the page and the
	// total.
	CollapseThreads bool

	// SortByRank orders by each message's POSITION IN RestrictIDs — the search
	// index's relevance order — before the date key (#724, the "relevance" sort
	// of urn:mailroom:search). The API hands RestrictIDs over in
	// search.RankHits order, which is total (score, then newest, then id), so
	// the rank key is unique per row and the ORDER BY is a total order.
	//
	// WHY PAGING STAYS CORRECT. A page is still Postgres's OFFSET/LIMIT over
	// one ORDER BY, the same mechanism as every other sort; what has to hold is
	// that page 2's request sees the same sequence page 1's did. Each request
	// re-runs the search, and the candidate sequence is a pure function of the
	// index's contents: the engine's top-N by score, re-sorted by RankHits so
	// an engine's arbitrary tie order cannot leak in. It moves only when the
	// index does — new mail indexed between the two requests — which is the
	// same exposure a newest-first list has to mail arriving between pages, and
	// the same remedy: the client re-queries from position 0 when the Email
	// state moves (the web client grows its window rather than fetching
	// disjoint pages, so it never stitches two sequences together).
	//
	// No candidates (RestrictIDs nil: the query had nothing for the index to
	// rank, e.g. `area:Work` alone) means there is no relevance to order by,
	// and the order is the date order the rest of the terms give.
	SortByRank bool

	// OnlyTrashed keeps only deleted mail, across every account: mail in a
	// Trash folder, whoever put it there, plus anything Mailroom itself trashed
	// wherever it is filed. It is how a search for `is:trashed` finds anything
	// (#623). Before it, the index matched the trashed messages and the trash
	// predicate below removed every one of them again, because a search is not
	// scoped to the Trash mailbox.
	//
	// It is the exact complement of the default predicate in QueryEmails — the
	// ONE definition of deleted (#631): trashed_at set, or effectively in a
	// trash-role mailbox — save the vanished carve-out below, which neither
	// side lists.
	//
	// THE TRASH FOLDER COUNTS, NOT ONLY trashed_at. Mail another client moved
	// to Trash carries no trashed_at (only Mailroom's own delete and the reaper
	// set it), yet the Trash view shows it, and a search for trash that missed
	// it would be a second, smaller Trash. The trashed_at arm matters for the
	// opposite case: mail trashed in an account with no Trash folder, and mail
	// archived out of Trash before a move out of Trash restored it (#628),
	// which is still due for purge and has no other view that lists it.
	//
	// Mail the reaper soft-deleted because it vanished from the server is
	// excluded: it is in no folder, the Trash view does not list it, and there
	// is no server copy for a restore to move.
	//
	// Postgres decides it, never the index: the index's trashed flag is
	// trashed_at alone, and it can lag behind a restore.
	OnlyTrashed bool

	// IncludeDeleted drops the default "deleted mail is gone" predicate without
	// restricting TO deleted mail: `in:anywhere`, or a search that names trash
	// as something to include inside an OR (#723). Mail the reaper soft-deleted
	// because it vanished from the server stays out, as it does for
	// OnlyTrashed. Ignored when OnlyTrashed is set or the query is the Trash
	// view.
	IncludeDeleted bool

	// OnlyJunk keeps only junk mail; ExcludeJunk drops it (#723). Junk is the
	// provider's verdict OR a junk-role folder (junkSQL) — the Junk view's own
	// membership plus the $junk keyword. OnlyJunk is how `is:junk` / `in:junk`
	// find mail another client filed in Junk, as OnlyTrashed does for Trash;
	// ExcludeJunk is a search's default, set by the API only for a query the
	// user typed. Neither is set by anything else, so a plain mailbox view —
	// the Junk view included — is unaffected.
	OnlyJunk    bool
	ExcludeJunk bool

	// Search is the store half of a search query (#722/#723): a boolean tree of
	// predicates the index cannot answer (area:, is:snoozed, is:muted,
	// is:pinned, is:important), ANDed with every other condition. nil means
	// none. See search_predicate.go.
	Search *SearchPredicate
}

// QueryEmails runs an Email/query and returns the matching ids (already
// windowed) and the total match count, both from one statement (the total
// is a window count over the same rows; only an empty page needs a second
// count statement). All predicates are RLS-scoped, so a
// query can only ever see the caller's own messages.
func (tx *UserTx) QueryEmails(ctx context.Context, q EmailQuery) (ids []ulid.ULID, total int, err error) {
	args := []any{tx.userID.String()}

	// bind appends an argument and returns its placeholder. (A placeholder may
	// be referenced more than once; the argument is bound only the first time.)
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	from, where, err := tx.emailQueryWhere(ctx, q, bind, nil)
	if err != nil {
		return nil, 0, err
	}

	var countSQL, pageSQL string
	if q.CollapseThreads {
		// RFC 8621 §4.4.4: "Emails in the same Thread as a previous Email in
		// the list (given the filter and sort order) will be removed from the
		// list" — for the returned page AND for `total`.
		//
		// The terms have to exist BEFORE the count query here (unlike the
		// uncollapsed branch below), because collapsing ranks every candidate
		// row by this exact order first: `total` is "how many threads have a
		// rank-1 row", which needs the window computed.
		body, outerOrder := collapsedRankSQL(from, where, emailOrderTerms(q, bind))
		ranked := `WITH ranked AS (` + body + `) `
		countSQL = ranked + `SELECT count(*) FROM ranked WHERE rn = 1`
		// One pass: count(*) OVER () is evaluated before LIMIT/OFFSET, so every
		// returned row carries the full thread count. The ranked CTE runs once
		// instead of twice (#665).
		pageSQL = ranked + `SELECT id, count(*) OVER () AS total FROM ranked WHERE rn = 1 ORDER BY ` + outerOrder
	} else {
		countSQL = `SELECT count(*) FROM ` + from + ` WHERE ` + where

		terms := emailOrderTerms(q, bind)
		orderByParts := make([]string, len(terms))
		for i, t := range terms {
			orderByParts[i] = t.render(t.expr)
		}
		orderBy := strings.Join(orderByParts, ", ")
		pageSQL = `SELECT m.id, count(*) OVER () AS total FROM ` + from + ` WHERE ` + where + ` ORDER BY ` + orderBy
	}
	// countSQL's placeholders are exactly the ones bound so far; LIMIT and
	// OFFSET below are bound for the page statement only. Capping the slice
	// keeps the fallback count from ever being sent those extra arguments (pgx
	// rejects a mismatch).
	countArgs := args[:len(args):len(args)]
	if q.LimitSet || q.Limit > 0 {
		// An explicit limit: 0 (LimitSet, Limit == 0) must still bind and emit
		// LIMIT 0 — RFC 8620 §5.5 makes a client-requested zero-length page a
		// legitimate way to ask only for `total` and `queryState`. LIMIT 0
		// returns no rows, so no window total arrives; the empty-page fallback
		// below runs countSQL, so total and queryState are unaffected.
		pageSQL += " LIMIT " + bind(q.Limit)
	}
	if q.Position > 0 {
		pageSQL += " OFFSET " + bind(q.Position)
	}

	// ⚠ A CUSTOM PLAN FOR AN AREA OR DESK FILTER (#697), for the reason
	// DeskSnapshot gives in full: the statement text repeats on every load, so
	// after five executions Postgres may settle on a GENERIC plan, planned
	// without knowing whose mail it is. With other users' rows in the tables
	// that plan stalled a per-section desk query past 20–60 s from its sixth
	// execution, against ~0.3 s custom. Drawers, and any client without
	// Area/getDesk, still send these per section. Scoped to this call and
	// restored after it, so the rest of the caller's transaction plans as it
	// would have.
	restorePlan := ""
	if q.InArea != nil || q.Unsorted || q.OnDesk != nil || q.OnSurface != nil || q.Search != nil {
		if err := tx.tx.QueryRow(ctx,
			`SELECT current_setting('plan_cache_mode'), set_config('plan_cache_mode', 'force_custom_plan', true)`,
		).Scan(&restorePlan, new(string)); err != nil {
			return nil, 0, fmt.Errorf("store: query emails (plan mode): %w", err)
		}
	}

	rows, err := tx.tx.Query(ctx, pageSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: query emails: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var rowTotal int
		if err := rows.Scan(&id, &rowTotal); err != nil {
			return nil, 0, fmt.Errorf("store: scan query id: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return nil, 0, fmt.Errorf("store: parse query id %q: %w", id, err)
		}
		ids = append(ids, u)
		total = rowTotal // identical on every row: count(*) OVER () precedes LIMIT/OFFSET
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: query emails: %w", err)
	}
	rows.Close()
	if len(ids) == 0 {
		// An empty page (no matches, a position at or past the end, or limit 0)
		// carries no window total, and 0 would be wrong for a non-empty result
		// paged past its end. Ask the count statement instead.
		if err := tx.tx.QueryRow(ctx, countSQL, countArgs...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("store: query emails (count): %w", err)
		}
	}
	if restorePlan != "" {
		if _, err := tx.tx.Exec(ctx, `SELECT set_config('plan_cache_mode', $1, true)`, restorePlan); err != nil {
			return nil, 0, fmt.Errorf("store: query emails (plan mode): %w", err)
		}
	}
	return ids, total, nil
}

// emailQueryWhere compiles every FILTER of an Email/query into SQL over the
// message alias `m`: the FROM clause (app.messages m plus any at-most-one-row
// LEFT JOIN a filter needs) and the WHERE conjunction. The caller's $1 MUST be
// its own user id (tx.userID) — the first condition is `m.user_id = $1`, and
// AreaPredicate scopes its subquery to the same placeholder.
//
// It is the ONE compiler of Email/query's predicates. QueryEmails runs it once
// per call; DeskSnapshot (desk_snapshot.go, #665) runs it once per desk section
// inside a single statement, so a section's rows there cannot be chosen by a
// different predicate than the per-section Email/query the client used to send.
//
// areaCond, when non-nil, replaces AreaPredicate for InArea/Unsorted with an
// equivalent condition over an area resolution the caller has ALREADY computed
// (DeskSnapshot's materialised desk CTE) — the only thing it may change is
// WHERE the resolved area is read from, never what it is. Nil means the
// ordinary, self-contained AreaPredicate.
func (tx *UserTx) emailQueryWhere(ctx context.Context, q EmailQuery, bind func(any) string,
	areaCond func(areaID *ulid.ULID) string) (from, where string, err error) {
	conds := []string{"m.user_id = $1"}
	// from is the FROM clause every statement shares. It only ever gains LEFT
	// JOINs that match at most one row per message (a primary-key join), so it
	// never changes which messages match or how many times.
	from = "app.messages m"
	customArea := areaCond != nil
	if areaCond == nil {
		areaCond = func(areaID *ulid.ULID) string { return AreaPredicate(areaID, bind) }
	}
	if q.InThread != nil {
		conds = append(conds, "m.thread_id = "+bind(q.InThread.String()))
	}
	if len(q.InTab) > 0 {
		// The twin of store.resolveOne — see tab_resolve.go, which holds both and
		// explains why a differential test rather than shared code.
		conds = append(conds, tabFilterSQL(q.InTab, bind))
	}
	// deskPred is the on-desk predicate, built once so the area resolver can be
	// scoped to it (#706) and the OnDesk filter below reuses the same text.
	// deskMessageSQL(bind, tx.userID, q.InAccount) is the SAME predicate
	// AreaSummary counts over (areas_resolve.go): when the filter also carries
	// InAccount, the desk-live provenance row it looks for must belong to
	// that account, exactly as AreaSummary's own DeskScope.AccountID does.
	var deskPred string
	if q.OnDesk != nil {
		deskPred = deskMessageSQL(bind, tx.userID, q.InAccount)
	}
	if q.InArea != nil {
		if !customArea && q.OnDesk != nil && *q.OnDesk {
			// Resolve only the desk's messages, not the area's candidates: the
			// outer onDesk condition below keeps the answer identical (#706).
			conds = append(conds, AreaPredicateOnDesk(*q.InArea, bind, deskPred))
		} else {
			conds = append(conds, areaCond(q.InArea))
		}
	} else if q.Unsorted {
		conds = append(conds, areaCond(nil))
	}
	if q.OnDesk != nil {
		desk := deskPred
		if *q.OnDesk {
			conds = append(conds, desk)
		} else {
			conds = append(conds, "NOT "+desk)
		}
	}
	if q.DeskArchived != nil {
		// The plain column, not a re-derivation through deskLiveSQL: this filter
		// asks about the MARK, and deskLiveSQL answers the broader "is it on the
		// desk" (which also depends on junk/trash/draft state and on which
		// mailbox holds the bytes). Conflating them would make
		// `deskArchived:true` silently exclude a day-one archived message the
		// user later trashed, which is exactly the message an "undo, where did
		// my mail go" question is about.
		if *q.DeskArchived {
			conds = append(conds, "m.desk_archived_at IS NOT NULL")
		} else {
			conds = append(conds, "m.desk_archived_at IS NULL")
		}
	}
	if q.OnSurface != nil {
		// The score comes from a LEFT JOIN on triage_scores' primary key, not a
		// scalar subquery per message (#660): the side test below uses the score
		// up to four times, and each use of a correlated `(SELECT ts.score …)` was
		// its own per-message SubPlan — four index probes per message, on every
		// desk list. The join reads each score once, set-based; it matches at
		// most one row, so no message is duplicated or dropped (unscored mail
		// reads a NULL score, exactly as the scalar subquery did).
		//
		// It is handed to lineSideSQL rather than compared here, so this filter
		// and AreaSummary's counts split on ONE expression (#464) — including
		// the user's own placement, which outranks the score.
		from += " LEFT JOIN app.triage_scores side_ts ON side_ts.user_id = m.user_id AND side_ts.message_id = m.id"
		side := lineSideSQL("side_ts.score", bind(q.LineThreshold))
		if *q.OnSurface {
			// Unjudged mail is never hidden in a drawer (#375): no side at all
			// counts as surface, exactly like AreaSummary's `unscored` bucket
			// joining `surface` in a tab's query.
			conds = append(conds, "("+side+" IS NULL OR "+side+" = 'surface')")
		} else {
			conds = append(conds, "("+side+" = 'drawer')")
		}
	}
	if q.InMailbox != nil {
		conds = append(conds, inMailboxSQL(bind, tx.userID, *q.InMailbox))
	}
	if q.RestrictAccounts != nil {
		conds = append(conds, keyAccountsSQL("m", bind(ulidStrings(q.RestrictAccounts))))
	}
	if q.InAccount != nil {
		// "Lives in the account" is EXACTLY liveAccountIDs' definition — a LIVE
		// provenance row for this message in that account — so an index-independent
		// per-account view (spec §7) matches the writeback fan-out set. It reads
		// provenance directly and deliberately ignores moved_mailbox_id: a message
		// the user moved still physically belongs to the account whose folder holds
		// the bytes, which is the account the mutation writes back to.
		//
		// RLS-safe by construction: app.message_provenance is RLS-scoped to the
		// user (and we bind p.user_id = m.user_id = tx.userID), so a foreign or
		// other-user account id matches ZERO rows — fail-closed, with no separate
		// ownership check that could error (500) on a valid request.
		a := bind(q.InAccount.String())
		conds = append(conds, `EXISTS (SELECT 1 FROM app.message_provenance p
			 WHERE p.user_id = m.user_id AND p.message_id = m.id
			   AND p.state = 'live' AND p.account_id = `+a+`)`)
	}
	// DELETED MAIL HAS ONE DEFINITION (#631): trashed_at is set, OR the message
	// is effectively in a trash-role mailbox. A deleted message is visible only
	// when the query asks for it — the Trash view (InMailbox on a trash-role
	// mailbox) or a search for `is:trashed` (OnlyTrashed, #623), which reaches
	// deleted mail in every account at once. Everywhere else it is gone:
	// Email/set reported Mailroom's own deletes as `destroyed` (soft-delete,
	// ADR-009), and a client told a message is destroyed must not keep finding
	// it in queries.
	//
	// THE FOLDER ARM IS WHAT #631 ADDED. trashed_at is written only by
	// Mailroom's own delete and by the reaper, so mail another client (a phone,
	// webmail) moved to Trash arrives as live provenance in Trash with
	// trashed_at NULL. With trashed_at alone it showed in All mail, Unread,
	// label views and plain search while the Trash view listed it too — one
	// message, deleted and not deleted at once. It is deliberately NOT fixed by
	// setting trashed_at at ingest: purge_after rides on trashed_at, so that
	// would make Mailroom hard-delete mail it never deleted.
	//
	// Membership is the effective one (mailboxMembershipSQL), so a message the
	// user restored in Mailroom (moved_mailbox_id out of Trash) is NOT deleted
	// even while its provider copy is still live in Trash — writeback pending,
	// or disabled on an ingest-only account (ADR-028). The move outranks
	// provenance, exactly as the Trash view itself decides.
	//
	// With no trash-role mailbox at all the clause is left out rather than
	// bound to an empty array: `= ANY('{}')` would be correct (false), but
	// costs a subquery over the user's provenance to learn nothing.
	trashQuery, junkQuery := false, false
	if q.InMailbox != nil {
		role, _, err := tx.MailboxRole(ctx, *q.InMailbox)
		if err != nil {
			return "", "", err
		}
		trashQuery = role == "trash"
		junkQuery = role == "junk"
	}
	switch {
	case q.OnlyTrashed:
		trash, err := tx.trashRoleMailboxIDs(ctx)
		if err != nil {
			return "", "", err
		}
		conds = append(conds, "((m.trashed_at IS NOT NULL AND m.vanished_at IS NULL) OR "+
			inAnyMailboxSQL(bind, tx.userID, trash)+")")
	case q.IncludeDeleted && !trashQuery:
		// Deleted mail is welcome; mail that vanished from the server is not
		// (it is in no folder and nothing could restore it — see OnlyTrashed).
		conds = append(conds, "NOT (m.trashed_at IS NOT NULL AND m.vanished_at IS NOT NULL)")
	case !trashQuery:
		conds = append(conds, "m.trashed_at IS NULL")
		trash, err := tx.trashRoleMailboxIDs(ctx)
		if err != nil {
			return "", "", err
		}
		if len(trash) > 0 {
			// inAnyMailboxSQL is three-valued-logic-free (see inMailboxSQL), so
			// NOT is a true complement: no row escapes both halves as NULL.
			conds = append(conds, "NOT "+inAnyMailboxSQL(bind, tx.userID, trash))
		}
	}
	if q.HasKeyword != "" {
		cond, err := tx.keywordPredicate(ctx, "m", q.HasKeyword, true, bind)
		if err != nil {
			return "", "", err
		}
		conds = append(conds, cond)
	}
	if q.NotKeyword != "" {
		cond, err := tx.keywordPredicate(ctx, "m", q.NotKeyword, false, bind)
		if err != nil {
			return "", "", err
		}
		conds = append(conds, cond)
	}
	if q.RestrictIDs != nil {
		// An empty candidate set is a real answer ("the text matched nothing"),
		// not an absent filter — `id = ANY('{}')` is false for every row, which
		// is exactly right.
		conds = append(conds, "m.id = ANY("+bind(ulidStrings(q.RestrictIDs))+")")
	}
	if q.Important != nil {
		// NOT EXISTS rather than a score comparison for the negative case, so an
		// UNSCORED message reads as not-important instead of vanishing from both
		// halves — mail ingested before scoring was wired, or whose scoring failed,
		// must still appear somewhere.
		scored := `EXISTS (SELECT 1 FROM app.triage_scores ts
			 WHERE ts.user_id = m.user_id AND ts.message_id = m.id
			   AND ts.score >= ` + bind(q.ImportantThreshold) + `)`
		if *q.Important {
			conds = append(conds, scored)
		} else {
			conds = append(conds, "NOT "+scored)
		}
	}
	if q.Snoozed != nil {
		// now() is the transaction timestamp, so every predicate in a single
		// Email/query agrees on what "still snoozed" means.
		if *q.Snoozed {
			conds = append(conds, snoozedSQL)
		} else {
			conds = append(conds, "(m.snoozed_until IS NULL OR m.snoozed_until <= now())")
		}
	}
	// A search inside the Junk view is a search OF junk: the default
	// exclusion would empty it.
	if q.OnlyJunk || (q.ExcludeJunk && !junkQuery) {
		junk, err := tx.junkSQL(ctx, bind)
		if err != nil {
			return "", "", err
		}
		if q.OnlyJunk {
			conds = append(conds, junk)
		} else {
			conds = append(conds, "NOT "+junk)
		}
	}
	if q.Search != nil {
		if q.Search.Uses(SearchAboveLine) {
			from += searchScoreJoin
		}
		cond, err := searchPredicateSQL(*q.Search, bind, q.LineThreshold)
		if err != nil {
			return "", "", err
		}
		conds = append(conds, cond)
	}
	if q.NeedsCategory != nil {
		// "NOTHING HAS CATEGORISED THIS MESSAGE" — an open question about it, or
		// no label on it at all, and no dismissal saying it does not need one.
		//
		// The second arm is what makes the surface exist before a classifier does
		// (#176's second listed source, "mail no rule and no kind-binding
		// covers"). When this was written there were no
		// suggestions, no model rows, and many unlabelled inbox messages. Abstentions
		// alone would have shipped a permanently empty page.
		//
		// EXISTS throughout rather than joins: a message can carry several open
		// questions and several labels, and a join would return it once per row —
		// silently inflating both the page and the total.
		//
		// The label arm is deliberately ANY label, whoever applied it. A rule's
		// label is a category; the classifier's is a category; the user's is a
		// category. What the queue is about is the absence of one, not who would
		// have supplied it.
		//
		// DRAFTS AND JUNK ARE NEVER IN IT, whatever the caller asks. They are
		// unlabelled and untrashed and so satisfy the arms above, and both are
		// actively harmful here: the queue offers batch labelling, so junk in it
		// means one keystroke files spam into a real category — corpus poisoning
		// through the surface built to grow the corpus — and a draft in it means
		// being asked to categorise something you are still writing. A client can
		// narrow further (the web client scopes the view to the inbox); it cannot
		// widen past this.
		needs := `((EXISTS (SELECT 1 FROM app.label_suggestions ls
			 WHERE ls.user_id = m.user_id AND ls.message_id = m.id)
		    OR NOT EXISTS (SELECT 1 FROM app.message_labels mlb
			 WHERE mlb.user_id = m.user_id AND mlb.message_id = m.id))
		  AND NOT EXISTS (SELECT 1 FROM app.category_dismissals cd
			 WHERE cd.user_id = m.user_id AND cd.message_id = m.id)
		  AND NOT m.is_draft AND NOT m.is_junk)`
		if *q.NeedsCategory {
			conds = append(conds, needs)
		} else {
			conds = append(conds, "NOT "+needs)
		}
	}
	if q.After != nil {
		conds = append(conds, "m.received_at >= "+bind(q.After.UTC()))
	}
	if q.Before != nil {
		conds = append(conds, "m.received_at < "+bind(q.Before.UTC()))
	}

	return from, strings.Join(conds, " AND "), nil
}

// orderTerm is one ORDER BY key: an expression plus its OWN direction and
// NULLS placement (the priority bucket's DESC and the confidence bucket's
// NULLS LAST do not simply follow the query's direction). Kept as a slice,
// built ONCE by emailOrderTerms, rather than assembled as one string: when
// q.CollapseThreads is set, the window that picks each thread's
// representative and the outer SELECT that returns the page both have to
// order by EXACTLY this sequence of keys, and driving both off the same slice
// is what makes that true by construction instead of by two people
// remembering to keep two hand-written ORDER BY strings in sync.
//
// ⚠ THE CRITICAL CORRECTNESS CONSTRAINT (RFC 8621 §4.4.4): if the window's
// ordering and the outer ordering ever drifted, the row collapseThreads keeps
// for a thread would not be "the first Email in the list given the filter and
// sort order" — the result would be silently wrong in a way no test that only
// counts rows per thread would catch.
type orderTerm struct {
	expr      string
	dir       string
	nullsLast bool
}

// render turns the term into "<name> DIR [NULLS LAST]". `name` is either the
// term's own expression (a window or ORDER BY that runs inside the same SELECT
// as `m` and can reference it directly) or the term's outer alias (the ranked
// CTE's outer ORDER BY, which cannot) — same direction and NULLS clause either
// way.
func (t orderTerm) render(name string) string {
	s := name + " " + t.dir
	if t.nullsLast {
		s += " NULLS LAST"
	}
	return s
}

// emailOrderTerms is Email/query's ORDER BY, as terms. It binds through `bind`
// (the priority bucket's threshold), so it must be called exactly once per
// statement it is rendered into.
func emailOrderTerms(q EmailQuery, bind func(any) string) []orderTerm {
	dir := "DESC"
	if q.SortAscending {
		dir = "ASC"
	}
	sortKey := "m.received_at"
	if q.SortByEffectiveArrival {
		sortKey = "COALESCE(m.snooze_woke_at, m.received_at)"
	}
	var terms []orderTerm
	// Relevance (#724) leads when asked for: rank 1 is the best match, so the
	// natural direction is ASCENDING position, and isAscending:true (which flips
	// every other sort) asks for the worst match first. array_position over the
	// candidate list is at most search.MaxLimit elements per row, and every
	// row has a position because RestrictIDs is also the WHERE clause's id set.
	if q.SortByRank && len(q.RestrictIDs) > 0 {
		rankDir := "ASC"
		if q.SortAscending {
			rankDir = "DESC"
		}
		terms = append(terms, orderTerm{
			expr: "array_position(" + bind(ulidStrings(q.RestrictIDs)) + "::text[], m.id::text)",
			dir:  rankDir,
		})
	}
	// The priority bucket leads the ORDER BY when asked for: important
	// first, then the date key within each bucket. DESC on the boolean
	// puts true first, always — the bucket order does not flip with `dir`.
	if q.SortByPriority {
		terms = append(terms, orderTerm{
			expr: `(EXISTS (SELECT 1 FROM app.triage_scores ts
				 WHERE ts.user_id = m.user_id AND ts.message_id = m.id
				   AND ts.score >= ` + bind(q.ImportantThreshold) + `))`,
			dir: "DESC",
		})
	}
	// The needs-a-category order (#176): highest open-suggestion confidence
	// first, then the date key to break ties. A correlated max() rather
	// than a join, for the reason the NeedsCategory predicate uses EXISTS —
	// a message with three open questions must appear once, ordered by its
	// BEST one.
	//
	// NULLS LAST keeps unsuggested mail at the end instead of at the front,
	// which is where a plain DESC would put it in Postgres. `dir`, not a
	// hardcoded DESC: an ascending request must flip the WHOLE order, not
	// just the date tie-break, or isAscending:true would return a list
	// ordered by neither key coherently.
	if q.SortBySuggestionConfidence {
		terms = append(terms, orderTerm{
			expr: `(SELECT max(ls.confidence) FROM app.label_suggestions ls
				 WHERE ls.user_id = m.user_id AND ls.message_id = m.id)`,
			dir:       dir,
			nullsLast: true,
		})
	}
	terms = append(terms, orderTerm{expr: sortKey, dir: dir})
	terms = append(terms, orderTerm{expr: "m.id", dir: dir})
	return terms
}

// collapsedRankSQL is the body of a thread-collapsing ranked CTE (RFC 8621
// §4.4.4): every matching message with its rank inside its thread (`rn`, 1 for
// the thread's representative) and each ordering key aliased k0, k1, … — plus
// the ORDER BY over those aliases that a SELECT from the CTE uses to return
// representatives in the query's own order. Both orderings come from the same
// terms, so they cannot drift (see orderTerm).
//
// The ranked CTE only projects what it selects, so the outer query cannot
// reference m.received_at etc. directly — that is what the k-aliases are for:
// each ordering key, aliased, so the outer ORDER BY uses the SAME expressions
// under the SAME directions and NULLS clauses as the window that picked the
// representative.
//
// m.thread_id is NULLABLE, and PARTITION BY groups every NULL together — which
// would collapse EVERY unthreaded message in the result into a SINGLE row.
// COALESCE'ing to the message's own id when there is no thread makes an
// unthreaded message a partition of one: it is never collapsed with another
// unthreaded message.
func collapsedRankSQL(from, where string, terms []orderTerm) (body, outerOrder string) {
	orderByParts := make([]string, len(terms))
	keyCols := make([]string, len(terms))
	outerParts := make([]string, len(terms))
	for i, t := range terms {
		orderByParts[i] = t.render(t.expr)
		alias := "k" + strconv.Itoa(i)
		keyCols[i] = t.expr + " AS " + alias
		outerParts[i] = t.render(alias)
	}
	body = `
			SELECT m.id AS id,
			       row_number() OVER (
			           PARTITION BY COALESCE(m.thread_id::text, 'msg:' || m.id::text)
			           ORDER BY ` + strings.Join(orderByParts, ", ") + `
			       ) AS rn,
			       ` + strings.Join(keyCols, ",\n\t\t       ") + `
			  FROM ` + from + ` WHERE ` + where + `
		`
	return body, strings.Join(outerParts, ", ")
}

// keywordPredicate builds a WHERE fragment testing whether a keyword is present
// (want=true) or absent (want=false) on the message.
//
// A JMAP keyword is ONE atom with THREE canonical homes (spec §13, "labels are
// keywords"), and this predicate has to see all three or the read path and the
// write path disagree about what a keyword is:
//
//   - a system keyword ($seen, $flagged, ...) is a flag COLUMN;
//   - a LABEL's keyword atom is a row in message_labels, joined through
//     label_keyword_map — which is where Email/set puts it, and where
//     Email/get's keyword set reads it from (attachLabelKeywords);
//   - anything else is a raw IMAP keyword in the messages.keywords[] array.
//
// The label branch resolves the atom to its label id ONCE, here, so the row
// predicate is a primary-key probe of message_labels (user_id, message_id,
// label_id) rather than a per-row join through label_keyword_map. Both sides of
// the OR are also kept, deliberately: an atom can be BOTH a known label and a
// raw keyword synced off the IMAP server before the label existed, and a query
// that matched only one of them would be wrong for exactly the messages the user
// most recently touched.
func (tx *UserTx) keywordPredicate(ctx context.Context, alias, keyword string, want bool, bind func(any) string) (string, error) {
	col := ""
	switch keyword {
	case KeywordSeen:
		col = alias + ".is_seen"
	case KeywordFlagged:
		col = alias + ".is_flagged"
	case KeywordAnswered:
		col = alias + ".is_answered"
	case KeywordDraft:
		col = alias + ".is_draft"
	case KeywordJunk:
		col = alias + ".is_junk"
	}
	if col != "" {
		if want {
			return col, nil
		}
		return "NOT " + col, nil
	}

	pred := bind(keyword) + " = ANY(" + alias + ".keywords)"
	labelID, isLabel, err := tx.LabelIDByKeywordAtom(ctx, keyword)
	if err != nil {
		return "", err
	}
	if isLabel {
		pred = "(" + pred + ` OR EXISTS (SELECT 1 FROM app.message_labels ml
			 WHERE ml.user_id = ` + alias + `.user_id AND ml.message_id = ` + alias + `.id
			   AND ml.label_id = ` + bind(labelID.String()) + "))"
	}
	if !want {
		pred = "NOT (" + pred + ")"
	}
	return pred, nil
}

// LabelIDByKeywordAtom resolves a JMAP keyword atom back to the label it maps
// to (label_keyword_map), so Email/set can route a keyword patch to the right
// canonical store (a label vs. a raw keyword). found is false for a keyword
// that is not a known label atom.
func (tx *UserTx) LabelIDByKeywordAtom(ctx context.Context, atom string) (ulid.ULID, bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx,
		`SELECT label_id FROM app.label_keyword_map
		  WHERE user_id = $1 AND keyword_atom = $2`,
		tx.userID.String(), atom).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return ulid.ULID{}, false, nil
		}
		return ulid.ULID{}, false, fmt.Errorf("store: label by keyword atom: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse label id %q: %w", id, err)
	}
	return u, true, nil
}

// EmailUpdate is the parsed, canonical form of a single Email/set update patch.
// A nil flag pointer means "leave unchanged"; the keyword/label slices are
// deltas. Trash/Restore drive the deletion lifecycle (ADR-009); MoveToRole /
// MoveToMailboxID are a folder move (e.g. archive), applied canonically AND
// mirrored to IMAP as a writeback intent. A folder move of a trashed message
// also restores it: nothing that has left Trash is still deleted.
type EmailUpdate struct {
	Seen     *bool
	Flagged  *bool
	Answered *bool
	Draft    *bool

	// Junk is the provider's spam verdict, mirrored into app.messages.is_junk by
	// ingest (spec §6: "Junk is read-only with one exception"). The exception is
	// "Not junk" — clearing it — which the API layer pairs with a move to INBOX,
	// because the move IS the ham signal to the provider. Setting it is refused
	// there: Mailroom does not decide what is spam.
	//
	// ⚠ It is a COLUMN, not a member of the keywords array (#351). $junk is
	// derived from this column on read, exactly like $seen/$flagged/$answered/
	// $draft — which is why routing it through AddKeywords/RemoveKeywords, as
	// Email/set did until now, removed it from an array it had never been in.
	// That reported success, appended a change row, enqueued a writeback, and
	// left the message junk.
	Junk *bool
	// Pinned is the local-only pin (spec §6, #51). Unlike the flags above it
	// generates NO writeback intent — there is no \Pinned IMAP flag, so §6's
	// "with writeback where representable" simply does not reach it. It still
	// appends the Email change_log row, because it IS JMAP-visible state that
	// other devices must see.
	Pinned *bool

	// SnoozeUntil sets the wake time; Unsnooze clears it (spec §6, #51). Two
	// fields rather than one nullable pointer, matching Trash/Restore just below:
	// "set this value" and "clear whatever is there" are different instructions,
	// and a *time.Time cannot carry the second without overloading nil, which
	// already means "the patch did not mention the snooze".
	//
	// Local-only, like the pin: migration 00013 records why a snooze deliberately
	// does NOT become an IMAP move.
	SnoozeUntil *time.Time
	Unsnooze    bool

	AddKeywords    []string
	RemoveKeywords []string
	AddLabelIDs    []ulid.ULID
	RemoveLabelIDs []ulid.ULID

	// LabelSource attributes the labels this update ADDS (#155). Zero value means
	// the user did it by hand, which is the honest default for the JMAP path:
	// Email/set is a client asking on a person's behalf. Automation sets it so a
	// surprising label can be traced to its cause.
	//
	// It applies to AddLabelIDs only. A REMOVAL has no lasting row to attribute,
	// and a user pulling a label off a message is the same act whoever put it
	// there.
	//
	// ONE EXCEPTION: Kind LabelByAgent (an API key, #49) names the ACTOR of
	// the whole update, removals and moves included, because what a key does
	// must never become the user's evidence: see LabelByAgent and the three
	// places ApplyEmailUpdate reads byAgent. Email/set sets it for every key
	// call; nothing a client sends can (parseEmailPatch never fills it).
	LabelSource LabelSource

	// AddLabelSources overrides LabelSource for individual labels in
	// AddLabelIDs (#708). It exists for ONE caller: undoing an area-filing
	// backfill, which in a single update takes the rule's label off and puts
	// back the desk/setup labels the backfill removed — each with the
	// provenance it had, so a restored label is not mistaken for the user's
	// hand-filing (induction evidence) or the rule's. One update rather than
	// one per source keeps it at exactly one Email change_log row.
	//
	// The suggestion clearing below follows the label's EFFECTIVE source: only
	// a label the user applied answers the message's open questions.
	AddLabelSources map[ulid.ULID]LabelSource

	// RemovalIsUndo marks a removal as UNDOING AN OPERATION rather than
	// disagreeing with the automation that performed it (#191).
	//
	// The distinction is the whole reason this field exists. Taking a rule's
	// label off one message says "this rule is wrong about this message", and
	// #170 records it so the rule can be narrowed. Undoing a backfill that
	// labelled four hundred messages says "I did not want that operation" — and
	// routing it through the same path would manufacture four hundred
	// corrections and make a perfectly good rule look catastrophic, which is
	// worse than recording nothing at all.
	//
	// It suppresses the correction record ONLY. The rows still go, the change_log
	// rows are still appended and the writeback still happens: what the user sees
	// is identical either way, and only the evidence differs.
	RemovalIsUndo bool

	Trash   bool
	Restore bool
	// MoveToRole is the destination as a JMAP/RFC 6154 role ("archive",
	// "trash", "inbox", ...). It is what the writeback intent carries, because
	// mailbox NAMES are per-account and a message can live in several accounts.
	// Empty when the client moved the message into a folder that has no role.
	MoveToRole string
	// MoveToMailboxID is the exact mailbox the client named, when it named one.
	// It is the canonical placement (app.messages.moved_mailbox_id) and, for the
	// account that owns it, the mailbox the IMAP move actually targets — so a
	// move into a roleless folder ("Receipts") goes THERE and not, as it did
	// before, into whatever mailbox happened to hold the \Archive flag.
	MoveToMailboxID *ulid.ULID
}

// ApplyEmailUpdate applies one Email/set update to the canonical message, mirrors
// it back via writeback intents, and appends exactly one Email change_log row —
// all in this UserTx (the load-bearing invariant). It returns whether the
// message existed and the resulting Email state (the change sequence). When
// nothing actually changed, no change_log row is written and the current state
// is returned, so a no-op patch is a true no-op.
func (tx *UserTx) ApplyEmailUpdate(ctx context.Context, id ulid.ULID, up EmailUpdate) (found bool, newState int64, err error) {
	return tx.applyEmailUpdate(ctx, id, up, true)
}

// applyEmailUpdate is ApplyEmailUpdate with the Email change_log row made
// optional, for exactly ONE caller: a blocked sender's new mail moved at
// ingest (MoveBlockedArrival, #59), whose move rides the message's own
// `created` row appended by ingest immediately afterwards — the same rule
// every ingest-time write follows (skipInboxForNewMessage, learned placement).
// Everything else — the canonical move, trashed_at/purge_after, the
// writeback intents — is identical, which is the point: a block's move goes
// through THE move path, not a copy of it. emitChange is true everywhere else.
func (tx *UserTx) applyEmailUpdate(ctx context.Context, id ulid.ULID, up EmailUpdate, emitChange bool) (found bool, newState int64, err error) {
	// 1. Lock and read the current canonical state.
	var (
		seen, flagged, answered, draft bool
		keywords                       []string
		junk                           bool
		trashed                        bool
		movedMailbox                   string
	)
	err = tx.tx.QueryRow(ctx, `
		SELECT is_seen, is_flagged, is_answered, is_draft, keywords, is_junk,
		       (trashed_at IS NOT NULL), COALESCE(moved_mailbox_id, '')
		  FROM app.messages
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`,
		tx.userID.String(), id.String()).
		Scan(&seen, &flagged, &answered, &draft, &keywords, &junk, &trashed, &movedMailbox)
	if err != nil {
		if isNoRows(err) {
			state, serr := tx.ChangeState(ctx, ObjectEmail)
			return false, state, serr
		}
		return false, 0, fmt.Errorf("store: apply email update (read): %w", err)
	}

	// 2. Compute the new canonical values.
	nSeen, nFlagged, nAnswered, nDraft := seen, flagged, answered, draft
	if up.Seen != nil {
		nSeen = *up.Seen
	}
	if up.Flagged != nil {
		nFlagged = *up.Flagged
	}
	if up.Answered != nil {
		nAnswered = *up.Answered
	}
	if up.Draft != nil {
		nDraft = *up.Draft
	}
	// NOTHING BECOMES A DRAFT BY PATCH (#49 review). A draft is a message
	// Mailroom composed (SaveDraft) or that arrived draft-flagged in a Drafts
	// folder; a draft is what a send sends and what a destroy may retire for
	// good. Letting `keywords/$draft: true` flip a received or Sent message would
	// arm both: the next ordinary delete would erase it, and a send could
	// re-send old mail. Clearing the flag stays allowed. Refused before any
	// write, so the transaction remains usable for the next object.
	if nDraft && !draft {
		return false, 0, ErrCannotMakeDraft
	}
	// A raw keyword is never a system flag (#49 review, ErrInvalidKeyword).
	// Also refused before any write, for the same reason.
	// Nor one with meaning of its own in any casing (KeywordReserved).
	for _, kw := range up.AddKeywords {
		if !KeywordWritable(kw) || KeywordReserved(kw) {
			return false, 0, ErrInvalidKeyword
		}
	}
	for _, kw := range up.RemoveKeywords {
		if !KeywordWritable(kw) || KeywordReserved(kw) {
			return false, 0, ErrInvalidKeyword
		}
	}
	// AN API KEY NEVER TAKES OFF A FILING THE USER MADE BY HAND (#49).
	// The user's own area filings ARE learned placement's evidence
	// (placementFilings), so a key removing one would retrain placement by
	// subtraction: re-filing the hand-filed half of a sender's mail would
	// flip the majority without a single key row ever counting. Same rule as
	// a rule's area filing (area_filing.go: a filing on the user's behalf
	// never removes the user's own). Refused before any write, so the
	// transaction stays usable for the next object.
	byAgent := up.LabelSource.byAgent()
	if byAgent && len(up.RemoveLabelIDs) > 0 {
		protected, err := tx.userFiledAreaLabels(ctx, id, up.RemoveLabelIDs)
		if err != nil {
			return false, 0, err
		}
		if len(protected) > 0 {
			return false, 0, &UserFilingProtectedError{LabelIDs: protected}
		}
	}
	// Junk mirrors the provider verdict and is written by ingest (spec §6). The one
	// update that may clear it is "Not junk", which the API layer pairs with a move
	// to INBOX. The move is the ham signal on a provider that judges by placement;
	// on one that ALSO tags the verdict as a keyword, the keyword has to come off
	// too, or the move carries it into the inbox and the next sync re-junks the
	// message (#576) — see enqueueJunkVerdictIntents.
	nJunk := junk
	if up.Junk != nil {
		nJunk = *up.Junk
	}
	nKeywords := applyKeywordDelta(keywords, up.AddKeywords, up.RemoveKeywords)

	nTrashed := trashed
	if up.Trash {
		nTrashed = true
	}
	// A MOVE INTO ANY OTHER FOLDER TAKES A MESSAGE OUT OF TRASH, exactly as a
	// Restore does (#628). Before this, only a Restore cleared trashed_at, so
	// archiving or dragging a message out of the Trash view filed it in its new
	// folder while it stayed trashed: hidden from that folder by the trash
	// predicate, gone from Trash because it was no longer there, and destroyed
	// by the purge when its 30 days ran out. The one exception is a patch that
	// ALSO names Trash (up.Trash), where Trash wins, as it does for the
	// placement below.
	if up.Restore || (!up.Trash && (up.MoveToRole != "" || up.MoveToMailboxID != nil)) {
		nTrashed = false
	}

	changed := false

	// 3a. Flags + keywords + trashed_at, in one guarded UPDATE. purge_after is
	// stamped alongside trashed_at when newly trashing (spec §4/ADR-009: 30-day
	// window) and both are cleared on restore; an unchanged trash state leaves
	// the timestamps untouched.
	setTrash := ""
	// restored is "this call is taking the message OUT of Trash", which is the
	// moment a queued purge EXPUNGE for it stops being something the user wants
	// (#555). Computed here rather than from up.Restore because the transition
	// is what matters: a Restore of a message that was not trashed is a no-op
	// and must not cancel anything.
	restored := false
	var trashArgs []any
	switch {
	case nTrashed && !trashed:
		now := time.Now().UTC()
		setTrash = ", trashed_at = $9, purge_after = $10"
		trashArgs = []any{now, now.Add(tx.trashRetention)}
	case !nTrashed && trashed:
		setTrash = ", trashed_at = NULL, purge_after = NULL"
		restored = true
	}
	var touched string
	// flagged_at rides this UPDATE in SQL, not a follow-up Go write (#444): it
	// becomes now() the instant is_flagged goes false->true, NULL the instant it
	// goes true->false, and is otherwise left alone — computed off the OLD row's
	// is_flagged (Postgres evaluates a SET clause's right-hand sides against the
	// pre-update row), so the two columns can never disagree about whether the
	// message is flagged. It is not part of the guard: an update that changes
	// some OTHER column here re-asserts the same flagged_at, which is a no-op.
	query := `
		UPDATE app.messages
		   SET is_seen = $3, is_flagged = $4, is_answered = $5, is_draft = $6,
		       keywords = $7, is_junk = $8,
		       flagged_at = CASE WHEN $4 AND NOT is_flagged THEN now()
		                         WHEN NOT $4 THEN NULL
		                         ELSE flagged_at END` + setTrash + `
		 WHERE user_id = $1 AND id = $2
		   AND (is_seen, is_flagged, is_answered, is_draft, keywords, is_junk, (trashed_at IS NOT NULL))
		       IS DISTINCT FROM ($3, $4, $5, $6, $7, $8, ` + boolLit(nTrashed) + `)
		RETURNING id`
	args := []any{tx.userID.String(), id.String(), nSeen, nFlagged, nAnswered, nDraft, nKeywords, nJunk}
	args = append(args, trashArgs...)
	if err := tx.tx.QueryRow(ctx, query, args...).Scan(&touched); err != nil {
		if !isNoRows(err) {
			return false, 0, fmt.Errorf("store: apply email update (write): %w", err)
		}
	} else {
		changed = true
	}

	// 3a-ter. TAKING A MESSAGE OUT OF TRASH WITHDRAWS ITS QUEUED PURGE EXPUNGE
	// (#555). The purge sweep may already have decided this message was due and
	// enqueued an EXPUNGE against every account holding a copy; that decision
	// was made when it was true and nothing re-examined it. Without this, the
	// next drain destroys the only server copy of a message the user has just
	// asked to keep — reproduced, not theorised.
	//
	// It runs in the SAME transaction as the clearing of trashed_at/purge_after,
	// so there is no window in which the message is restored while its
	// destruction is still queued.
	//
	// ⚠ THIS IS THE CHEAP HALF AND IT IS NOT SUFFICIENT ON ITS OWN. It only
	// catches a restore that goes through Mailroom. A copy can leave Trash with
	// no local transaction to hook onto at all — another client moves it, and
	// Mailroom finds out at the next sync pass — and nothing here can see that.
	// The engine's apply-time re-check (internal/writeback, keyed on
	// source='purge') is what closes the general case; this exists because the
	// common path should not depend on a guard firing at the last moment, and
	// because an intent withdrawn here never reaches the engine at all.
	if restored {
		if _, err := tx.WithdrawPurgeExpunges(ctx, id); err != nil {
			return false, 0, err
		}
	}

	// 3a-bis. Pin. A SEPARATE guarded UPDATE rather than another column on 3a's
	// statement: 3a already renumbers its arguments around the optional trash
	// clause, and threading a second optional column through that is how off-by-
	// one argument bugs are born. It runs only when the patch actually mentions
	// the pin, so the common path pays nothing.
	//
	// Guarded the same way as everything else here — re-pinning an already-
	// pinned message must not churn the change log, because every appended row
	// wakes the indexer and every client cursor.
	if up.Pinned != nil {
		var pinnedAt *time.Time
		if *up.Pinned {
			now := time.Now().UTC()
			pinnedAt = &now
		}
		var pinTouched string
		err := tx.tx.QueryRow(ctx, `
			UPDATE app.messages
			   SET pinned_at = $3
			 WHERE user_id = $1 AND id = $2
			   AND (pinned_at IS NOT NULL) IS DISTINCT FROM $4
			RETURNING id`,
			tx.userID.String(), id.String(), pinnedAt, *up.Pinned).Scan(&pinTouched)
		if err != nil {
			if !isNoRows(err) {
				return false, 0, fmt.Errorf("store: apply email update (pin): %w", err)
			}
		} else {
			changed = true
		}
	}

	// 3a-ter. Snooze — the same shape as the pin above, and here for the same
	// reason: it belongs to THIS update, so it must append THIS update's single
	// Email change_log row rather than a second one of its own.
	//
	// The wake TIMER is enqueued in this transaction too, which is the property
	// that matters: a snooze whose state committed without its timer would be a
	// message that never comes back. Committing both together makes that
	// unrepresentable — either the user's mail is snoozed AND a wake is pending,
	// or neither happened.
	//
	// It is enqueued whenever a future wake is requested, not only when the row
	// changed. Re-snoozing to a time that is already set is a no-op for the DATA,
	// but it is also the natural moment to repair a timer that was somehow lost,
	// and EnsureSnoozeWakeJob's idempotency key makes the common case free.
	if up.SnoozeUntil != nil || up.Unsnooze {
		var wake *time.Time
		if up.SnoozeUntil != nil {
			w := up.SnoozeUntil.UTC()
			wake = &w
		}
		// snooze_woke_at (#152) moves with the state:
		//
		//   snoozing   -> cleared. The message is away again, so it is no longer
		//                 "recently returned" — it must not keep a return cue, nor
		//                 sort by a return that has been superseded.
		//   un-snoozing -> stamped NOW. "Bring it back" means bring it back into
		//                 view, and a manual return that landed three days down the
		//                 list would be the same disappointment as a timed one.
		var wokeAt *time.Time
		if up.Unsnooze {
			n := time.Now().UTC()
			wokeAt = &n
		}
		var snoozeTouched string
		err := tx.tx.QueryRow(ctx, `
			UPDATE app.messages
			   SET snoozed_until = $3, snooze_woke_at = $4
			 WHERE user_id = $1 AND id = $2
			   AND snoozed_until IS DISTINCT FROM $3
			RETURNING id`,
			tx.userID.String(), id.String(), wake, wokeAt).Scan(&snoozeTouched)
		if err != nil {
			if !isNoRows(err) {
				return false, 0, fmt.Errorf("store: apply email update (snooze): %w", err)
			}
		} else {
			changed = true
		}
		if wake != nil {
			if _, err := tx.EnsureSnoozeWakeJob(ctx, *wake); err != nil {
				return false, 0, err
			}
		}
	}

	// 3b. Labels (message_labels), tracking which actually changed for writeback.
	//
	// DO NOTHING on conflict deliberately keeps the ORIGINAL attribution: if the
	// user labelled a message by hand and a rule later matches it, the label was
	// the user's and stays theirs. Re-attributing it to the rule would tell them
	// an automation did something they remember doing.
	var addedLabels, removedLabels []ulid.ULID
	userAdded := false
	for _, lid := range up.AddLabelIDs {
		src := up.LabelSource
		if s, ok := up.AddLabelSources[lid]; ok {
			src = s
		}
		appliedBy, ruleID := src.columns()
		if appliedBy == LabelByUser {
			userAdded = true
		}
		tag, err := tx.tx.Exec(ctx, `
			INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by, applied_by_rule_id)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_id, message_id, label_id) DO NOTHING`,
			tx.userID.String(), id.String(), lid.String(), appliedBy, ruleID)
		if err != nil {
			return false, 0, fmt.Errorf("store: apply email update (add label): %w", err)
		}
		if tag.RowsAffected() > 0 {
			changed = true
			addedLabels = append(addedLabels, lid)
		}
	}
	// A HUMAN answering closes the question (#169). Once the user has filed this
	// message themselves, every open suggestion about it is stale — including the
	// ones for other labels, because the answer they gave is the answer. Leaving
	// them queued would show the review surface a message the user has already
	// dealt with, which is the fastest way to teach someone to ignore it.
	//
	// Only for the user's own labelling: a label the CLASSIFIER applied must not
	// clear its own outstanding questions.
	//
	// ⚠ CLEARING THEM COUNTS AS A CHANGE (#299). The precondition is v1.34.1's
	// documented "it does not sweep" limit: a rule labels a message after a
	// suggestion already exists, the user then accepts that same label, the INSERT
	// above conflicts and reports nothing added, and if nothing else in this patch
	// changed either then `changed` stays false — so the early return below fires,
	// the suggestions are gone, and no Email `updated` row is ever appended. The
	// user's own client re-renders from its response; every OTHER client keeps the
	// answered question in its review queue forever, because /changes never
	// mentions the message again.
	if userAdded {
		cleared, err := tx.ClearLabelSuggestions(ctx, id)
		if err != nil {
			return false, 0, err
		}
		if cleared {
			changed = true
		}
	}
	// Rules whose label the user just took back (#170), deduped so one message
	// losing several of a rule's labels appends one change row.
	correctedRules := map[ulid.ULID]bool{}
	// A learned placement the user just undid (#714): the learned label came
	// off, by Re-file's swap or by hand.
	learnedCorrected := false
	// A learned placement an API key took the label off (#49).
	learnedWithdrawn := false
	for _, lid := range up.RemoveLabelIDs {
		// RETURNING the attribution, because the DELETE is the moment it stops
		// existing (#170). A rule-applied label that a human takes off is the only
		// evidence that the rule was wrong, and until now it was discarded with
		// the row — so the rule kept firing on every future match and nothing
		// recorded the disagreement.
		var wasBy string
		var byRule *string
		err := tx.tx.QueryRow(ctx, `
			DELETE FROM app.message_labels
			 WHERE user_id = $1 AND message_id = $2 AND label_id = $3
			RETURNING applied_by, applied_by_rule_id`,
			tx.userID.String(), id.String(), lid.String()).Scan(&wasBy, &byRule)
		if err != nil {
			if isNoRows(err) {
				continue // the label was not on this message
			}
			return false, 0, fmt.Errorf("store: apply email update (remove label): %w", err)
		}
		changed = true
		removedLabels = append(removedLabels, lid)

		// Only a RULE's label is worth recording, and only when a HUMAN took it
		// off. The classifier does not apply labels at all (#169), and a rule
		// removing its own label is not a correction. `up.LabelSource` describes
		// ADDS, so a removal arriving through the JMAP path is the user — which
		// is exactly the actor whose disagreement means something — unless the
		// update is an API key's (byAgent, below).
		//
		// A rule deleted since it labelled leaves applied_by='rule' with a NULL id
		// (ON DELETE SET NULL): the disagreement is real but there is no longer a
		// rule to offer to narrow, so there is nothing to record.
		//
		// RemovalIsUndo opts out entirely: see its doc — reversing a bulk apply is
		// not a hundred separate disagreements.
		//
		// Nor when an API KEY took it off (#49): an agent's removal is not
		// the user disagreeing with the rule, and counting it would let a key
		// narrow the user's rules.
		if wasBy == LabelByRule && byRule != nil && !up.RemovalIsUndo && !byAgent {
			ruleID, perr := ulid.Parse(*byRule)
			if perr == nil {
				if err := tx.recordRuleCorrection(ctx, ruleID, id, lid); err != nil {
					return false, 0, err
				}
				correctedRules[ruleID] = true
			}
		}
		// A LEARNED label taken off by the user is learned placement's
		// correction (#714): the placement is marked corrected, which blocks
		// its key until the user files on it again (placement.Judge). The same
		// RemovalIsUndo opt-out as a rule's: reversing a bulk operation is not
		// a disagreement with the placement.
		//
		// An API KEY taking it off is neither (#49): not a correction, or
		// the key could block a sender's placement, and not left open either,
		// or it would age into "kept" (effectiveOutcomeSQL) and pad the gate
		// with a placement nobody kept. It is WITHDRAWN: counted as neither.
		switch {
		case wasBy == LabelByLearned && byAgent:
			learnedWithdrawn = true
		case wasBy == LabelByLearned && !up.RemovalIsUndo:
			learnedCorrected = true
		case wasBy == LabelByAgent && !byAgent && !up.RemovalIsUndo:
			// A KEY'S RE-ADD MUST NOT BLIND THE USER (#49 K2 review). A key
			// that takes a learned label off withdraws the placement and
			// deletes the 'learned' row; re-adding the same label writes an
			// 'agent' row in its place. Without this case the user's later
			// removal of that label matched neither branch above, so the
			// key had frozen placement's feedback for the message. When the
			// label the user took off is the area learned placement put this
			// message in, and that placement is still withdrawn or open,
			// it is the user's correction exactly as if the learned row had
			// survived. A label a key added to a message with no learned
			// placement, or another area's label, records nothing.
			if err := tx.recordLearnedCorrectionOfAgentLabel(ctx, id, lid); err != nil {
				return false, 0, err
			}
		}
	}
	if learnedCorrected {
		if err := tx.recordLearnedCorrection(ctx, id); err != nil {
			return false, 0, err
		}
	}
	if learnedWithdrawn {
		if err := tx.recordLearnedWithdrawal(ctx, id); err != nil {
			return false, 0, err
		}
	}
	// A rule's correction count is part of what a client renders for it, so a
	// correction is a Rule change and owes a change_log row — otherwise the
	// rules list keeps showing "0 misfiled" until something unrelated happens to
	// touch that rule. Deduped: one message losing three labels from the same
	// rule is one rule that changed, not three.
	for ruleID := range correctedRules {
		if _, err := tx.AppendChange(ctx, ObjectRule, ruleID, OpUpdated); err != nil {
			return false, 0, err
		}
	}

	// 3c. The MOVE — canonically, here, now.
	//
	// A folder move used to be writeback-only: the intent went to the queue and
	// nothing in Postgres changed, so mailbox membership (which is read from
	// provenance) still said INBOX. The message the user archived therefore
	// reappeared in the inbox on the next poll and stayed there — provenance only
	// ever grows, since there is no VANISHED/expunge reconciliation, so the
	// source row never went away even after the IMAP move succeeded.
	//
	// moved_mailbox_id is the canonical answer instead: it is the mailbox the
	// USER put the message in, it takes effect in this transaction, and it
	// OVERRIDES provenance for JMAP membership (GetEmails / QueryEmails).
	// Provenance is deliberately left alone — it is the (mailbox, uidvalidity,
	// uid) coordinate the writeback engine needs to find the message on the IMAP
	// server, and rewriting or deleting it here would make the move unappliable
	// (MessageProvenanceTargetsForAccount would come back empty, which the
	// writeback engine reads as "no live provenance" — a conflict — and drops the
	// move).
	accounts, err := tx.liveAccountIDs(ctx, id)
	if err != nil {
		return false, 0, err
	}
	moveRole := up.MoveToRole
	moveTarget := up.MoveToMailboxID
	switch {
	case up.Trash:
		moveRole, moveTarget = "trash", nil
	case up.Restore && moveRole == "" && moveTarget == nil:
		// A restore with no destination named goes to Archive — the message came
		// out of Trash and has to land somewhere.
		moveRole = "archive"
	}
	if moveRole != "" || moveTarget != nil {
		placement, err := tx.movePlacement(ctx, accounts, moveRole, moveTarget)
		if err != nil {
			return false, 0, err
		}
		switch {
		case placement != nil && placement.String() == movedMailbox:
			// Already canonically there: a re-asserted mailboxIds patch is a
			// no-op, not another IMAP move.
			moveRole, moveTarget = "", nil
		case placement != nil:
			if _, err := tx.tx.Exec(ctx, `
				UPDATE app.messages SET moved_mailbox_id = $3
				 WHERE user_id = $1 AND id = $2`,
				tx.userID.String(), id.String(), placement.String()); err != nil {
				return false, 0, fmt.Errorf("store: apply email update (move): %w", err)
			}
			moveTarget = placement
			changed = true
		default:
			// No mailbox with that role in any of the message's accounts (or the
			// message has no account at all — a draft we have not APPENDed). The
			// intent is still enqueued below so the failure is SURFACED in account
			// health rather than silently swallowed (spec §4a).
			changed = true
		}
	}
	// ARCHIVED WHERE IT WAS PUT (#714): the user dealt with a learned
	// placement in the area it was placed in, so it is KEPT now rather than
	// after placement.KeptAfter. Not when this same update took the learned
	// label off: that is a correction, recorded above.
	//
	// Not when an API key archived it (#49): the user did not act on the
	// placement, so it ages like any other the user has not looked at.
	// Nor when day-one's server archive moved it (#934): machinery, same reason.
	if moveRole == "archive" && !learnedCorrected && !byAgent && up.LabelSource.Kind != LabelBySetup {
		if err := tx.keepLearnedOnArchive(ctx, id); err != nil {
			return false, 0, err
		}
	}

	if !changed {
		state, serr := tx.ChangeState(ctx, ObjectEmail)
		return true, state, serr
	}

	// 4. Writeback intents (spec §4a) — one delta per live account. All ops are
	// deltas, so replay is safe; a fresh action id keeps each Email/set's
	// intents distinct (data-model §4 idempotency_key = hash(op,obj,payload,
	// origin_action_id)).
	action := NewULID()
	// What asked for these intents (#554 item 3, migration 00057). It travels
	// with `action` because it has exactly the same scope: one Email/set call is
	// one antecedent, and every intent it enqueues shares both.
	//
	// ⚠ THE ATTRIBUTION IS THE CALL'S, NOT THE SUB-MUTATION'S. A rule that
	// applies a label and incidentally moves the message produces 'rule' for
	// both, because one actor made one request; the model here is already
	// per-call (one action id, one change_log row), and splitting attribution
	// finer than the request that caused it would invent a distinction the
	// caller never made.
	//
	// ⚠ AND THE ZERO VALUE IS 'user' HERE, WHICH IS RIGHT ONLY ON THIS PATH.
	// LabelSource's zero value means "a person acting through a client", and
	// that is exactly what ApplyEmailUpdate is when nobody says otherwise. The
	// machinery paths do not come through here and set their own source
	// explicitly; see IntentRow.Source for why there is no global default.
	intentSource, _ := up.LabelSource.columns()
	enqueueFlag := func(add bool, flag string) error {
		return tx.enqueueEmailIntent(ctx, action, intentSource, id, accounts, flagOp(add), map[string]string{"flag": flag})
	}
	if up.Seen != nil && nSeen != seen {
		if err := enqueueFlag(nSeen, flagSeen); err != nil {
			return false, 0, err
		}
	}
	if up.Flagged != nil && nFlagged != flagged {
		if err := enqueueFlag(nFlagged, flagFlagged); err != nil {
			return false, 0, err
		}
	}
	if up.Answered != nil && nAnswered != answered {
		if err := enqueueFlag(nAnswered, flagAnswered); err != nil {
			return false, 0, err
		}
	}
	if up.Draft != nil && nDraft != draft {
		if err := enqueueFlag(nDraft, flagDraft); err != nil {
			return false, 0, err
		}
	}
	// The junk verdict's keywords (#576). BEFORE the move below, and that
	// placement is the point: the engine applies one action's intents in
	// enqueue order (ClaimPendingIntents), and a keyword STORE must reach the
	// copy in the folder it is leaving, because the MOVE carries whatever
	// keywords that copy still has.
	if err := tx.enqueueJunkVerdictIntents(ctx, action, intentSource, id, up, moveRole); err != nil {
		return false, 0, err
	}
	for _, kw := range up.AddKeywords {
		if err := tx.enqueueEmailIntent(ctx, action, intentSource, id, accounts, IntentAddKeyword, map[string]string{"keyword": kw}); err != nil {
			return false, 0, err
		}
	}
	for _, kw := range up.RemoveKeywords {
		if err := tx.enqueueEmailIntent(ctx, action, intentSource, id, accounts, IntentRemoveKeyword, map[string]string{"keyword": kw}); err != nil {
			return false, 0, err
		}
	}
	// A label's intent carries the label's NAME beside its keyword atom
	// (#52, ADR-047 §3): a Gmail account writes a mirrored label back as
	// X-GM-LABELS by name, every other account as the keyword. Carried, not
	// looked up at apply time, so a label renamed or deleted in between still
	// writes what the user did.
	for _, lid := range addedLabels {
		if atom, name, ok, err := tx.labelKeywordAtom(ctx, lid); err != nil {
			return false, 0, err
		} else if ok {
			if err := tx.enqueueEmailIntent(ctx, action, intentSource, id, accounts, IntentAddKeyword, map[string]string{"keyword": atom, "label": name}); err != nil {
				return false, 0, err
			}
		}
	}
	for _, lid := range removedLabels {
		if atom, name, ok, err := tx.labelKeywordAtom(ctx, lid); err != nil {
			return false, 0, err
		} else if ok {
			if err := tx.enqueueEmailIntent(ctx, action, intentSource, id, accounts, IntentRemoveKeyword, map[string]string{"keyword": atom, "label": name}); err != nil {
				return false, 0, err
			}
		}
	}
	if moveRole != "" || moveTarget != nil {
		if err := tx.enqueueMoveIntents(ctx, action, intentSource, id, accounts, moveRole, moveTarget); err != nil {
			return false, 0, err
		}
	}

	// 5. The one Email change_log row for this update (the invariant) — unless
	// the caller is ingest, whose own `created` row reports the final state.
	if !emitChange {
		state, serr := tx.ChangeState(ctx, ObjectEmail)
		return true, state, serr
	}
	newState, err = tx.AppendChange(ctx, ObjectEmail, id, OpUpdated)
	if err != nil {
		return false, 0, err
	}
	return true, newState, nil
}

// boolLit renders a Go bool as a SQL boolean literal for a static predicate.
func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// flagOp maps add/remove to the corresponding IMAP flag intent op.
func flagOp(add bool) IntentOp {
	if add {
		return IntentAddFlag
	}
	return IntentRemoveFlag
}

// liveAccountIDs returns the distinct accounts a message currently lives in —
// the set of writeback targets for a mutation. The order is stable (by id) so
// that resolving a move's canonical destination across several accounts is
// deterministic rather than dependent on Postgres's row order.
func (tx *UserTx) liveAccountIDs(ctx context.Context, messageID ulid.ULID) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT account_id FROM app.message_provenance
		 WHERE user_id = $1 AND message_id = $2 AND state = 'live'
		 ORDER BY account_id`,
		tx.userID.String(), messageID.String())
	if err != nil {
		return nil, fmt.Errorf("store: live account ids: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan account id: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse account id %q: %w", id, err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// labelKeywordAtom returns the IMAP keyword atom a label mirrors to, if any
// (a local-only label has no mapping and mirrors nowhere — spec §6), and the
// label's name (what a Gmail account writes as X-GM-LABELS, ADR-047 §3).
func (tx *UserTx) labelKeywordAtom(ctx context.Context, labelID ulid.ULID) (atom, name string, ok bool, err error) {
	err = tx.tx.QueryRow(ctx,
		`SELECT k.keyword_atom, l.name
		   FROM app.label_keyword_map k
		   JOIN app.labels l ON l.id = k.label_id AND l.user_id = k.user_id
		  WHERE k.user_id = $1 AND k.label_id = $2`,
		tx.userID.String(), labelID.String()).Scan(&atom, &name)
	if err != nil {
		if isNoRows(err) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("store: label keyword atom: %w", err)
	}
	return atom, name, true, nil
}

// movePlacement resolves a move to the ONE mailbox that becomes the message's
// canonical placement (app.messages.moved_mailbox_id).
//
// When the client named a mailbox, that is the answer — it is what the user
// clicked. Otherwise the role is resolved against the message's own accounts, in
// account order, so a trash/archive with no mailbox named lands in that
// account's Trash/Archive. nil means the move has no canonical destination (the
// account has no mailbox with that role, or the message lives in no account
// yet); the caller still enqueues the intent so the failure is surfaced.
func (tx *UserTx) movePlacement(ctx context.Context, accounts []ulid.ULID, role string, target *ulid.ULID) (*ulid.ULID, error) {
	if target != nil {
		return target, nil
	}
	if role == "" {
		return nil, nil
	}
	for _, acct := range accounts {
		mb, found, err := tx.MailboxForRole(ctx, acct, role)
		if err != nil {
			return nil, err
		}
		if found {
			id := mb.ID
			return &id, nil
		}
	}
	return nil, nil
}

// enqueueMoveIntents enqueues the per-account `move` intents for an archive /
// trash / restore / folder move.
//
// A move must name a DESTINATION MAILBOX, and mailbox names are per-account
// (one account's Archive may be "Archive", another's "[Gmail]/All Mail"), so the
// destination is resolved to each account's actual IMAP name HERE, at enqueue
// time, where a UserTx can see app.mailboxes. The engine applies bytes, not
// roles: its payload contract is {"mailbox": <raw IMAP name>}.
//
// target, when set, is the exact mailbox the client named. For the account that
// OWNS that mailbox it wins outright — that is how a move into a folder with no
// SPECIAL-USE role ("Receipts") reaches the right folder instead of being
// rounded off to Archive. Other accounts fall back to the role.
//
// When an account can resolve neither, the intent is still enqueued — carrying
// the role and no mailbox — so the engine dead-letters it with a SURFACED
// reason. A user's "archive" is never silently dropped (spec §4a); an account
// that cannot represent the move must SAY so, in the account-health view,
// rather than quietly losing it.
func (tx *UserTx) enqueueMoveIntents(ctx context.Context, action ulid.ULID, source string, messageID ulid.ULID, accounts []ulid.ULID, role string, target *ulid.ULID) error {
	mid := messageID
	var named MailboxTarget
	var namedOK bool
	if target != nil {
		var err error
		named, namedOK, err = tx.MailboxByID(ctx, *target)
		if err != nil {
			return err
		}
	}
	for _, acct := range accounts {
		payload := map[string]string{}
		if role != "" {
			payload["target_role"] = role
		}
		switch {
		case namedOK && named.AccountID == acct:
			payload["mailbox"] = named.IMAPName
		case role != "":
			mb, found, err := tx.MailboxForRole(ctx, acct, role)
			if err != nil {
				return err
			}
			if found {
				payload["mailbox"] = mb.IMAPName
			}
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("store: marshal move intent payload: %w", err)
		}
		key := strings.Join([]string{action.String(), string(IntentMove),
			messageID.String(), acct.String(), role, payload["mailbox"]}, ":")
		if _, err := tx.EnqueueIntent(ctx, IntentRow{
			MessageID:      &mid,
			AccountID:      acct,
			Op:             IntentMove,
			Payload:        body,
			IdempotencyKey: key,
			Source:         source,
		}); err != nil {
			return err
		}
	}
	return nil
}

// enqueueEmailIntent enqueues one writeback intent per live account for an
// Email mutation, with a deterministic idempotency key scoped to the action.
func (tx *UserTx) enqueueEmailIntent(ctx context.Context, action ulid.ULID, source string, messageID ulid.ULID, accounts []ulid.ULID, op IntentOp, payload map[string]string) error {
	// Every keyword intent passes here (patch keywords, label atoms, the junk
	// verdict's tokens), so this is where a system flag spelled as a keyword
	// is stopped whichever path produced it (#49 review).
	if (op == IntentAddKeyword || op == IntentRemoveKeyword) && !KeywordWritable(payload["keyword"]) {
		return ErrInvalidKeyword
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("store: marshal intent payload: %w", err)
	}
	// The key's argument is the op's own (flag or keyword), read by name: a
	// payload can carry more than one field (a label's name beside its atom),
	// and Go's map order would otherwise pick one at random.
	//
	// ⚠ THE KIND IS PART OF THE KEY (#911 review F8). A raw keyword patch
	// equal to a label's atom, in the same Email/set, would otherwise give
	// the keyword's intent and the label's intent the same key, and the
	// second would be swallowed by the first: on Gmail one is a keyword and
	// the other an X-GM-LABELS write, so dropping either loses a change.
	var arg string
	switch {
	case payload["flag"] != "":
		arg = "flag:" + payload["flag"]
	case payload["label"] != "":
		arg = "label:" + payload["keyword"]
	case payload["keyword"] != "":
		arg = "kw:" + payload["keyword"]
	default:
		for _, v := range payload {
			arg = v
			break
		}
	}
	mid := messageID
	for _, acct := range accounts {
		key := strings.Join([]string{action.String(), string(op), messageID.String(), acct.String(), arg}, ":")
		if _, err := tx.EnqueueIntent(ctx, IntentRow{
			MessageID:      &mid,
			AccountID:      acct,
			Op:             op,
			Payload:        body,
			IdempotencyKey: key,
			Source:         source,
		}); err != nil {
			return err
		}
	}
	return nil
}
