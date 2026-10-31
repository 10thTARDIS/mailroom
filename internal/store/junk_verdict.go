// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// The junk verdict's IMAP keywords (#576).
//
// is_junk is derived from keywords alone: ingest's mapFlags sets it for `junk`
// and `$junk` on ANY live copy, and the merge ORs it across copies. Being in a
// \Junk folder does not set it. Two kinds of provider therefore look different
// here:
//
//   - one that judges by PLACEMENT only (measured: Migadu) never carries the
//     keyword, so "Not junk" is its move to the inbox and nothing more;
//   - one that ALSO TAGS the verdict (Thunderbird-style filters and Apple Mail
//     write `Junk`/`$Junk`, and some servers' spam filters do) keeps the keyword
//     on the copy. IMAP MOVE, and the COPY+EXPUNGE fallback, carry a message's
//     keywords to the destination, so the rescued copy arrives in INBOX still
//     tagged and the next sync re-derives is_junk = true. The rescue lasted one
//     sync cycle, and since v1.88.0 the re-junked message also drops out of
//     search.
//
// So the rescue takes the keyword off before the move, and says `$NotJunk`,
// which is the conventional ham feedback spec §6 means by "where the provider
// offers a ham-feedback mechanism we use it".
//
// THE REVERSE IS DELIBERATELY ONLY HALF-SYMMETRIC. A move into Junk withdraws a
// `$NotJunk` we (or anyone) left on the copy, so the folder and the keyword do
// not contradict each other. It does NOT add `$Junk`:
//
//   - spec §6 makes the verdict the provider's, mirrored here — Email/set
//     refuses to set $junk for exactly that reason, and a move into Junk is not
//     a different door to the same decision;
//   - and it would manufacture this issue's failure on providers that never had
//     it. On a placement-only provider (Migadu accepts keywords), a $Junk added
//     on the way in rides every later ORDINARY move back out — a drag, a filter,
//     another client — and that message then sits in the inbox marked junk,
//     hidden from the desk and from search, with nothing ever having cleared it.
//
// Withdrawing `$NotJunk` has no such tail: no keyword it removes feeds is_junk.

// Keyword tokens a live copy can carry for each side of the verdict, lower-cased
// for matching; IMAP keywords are case-insensitive in practice and providers
// differ in the casing they write. JunkKeywords and NotJunkKeywords are THE
// list: ingest's flag mapping reads them, and so does KeywordReserved, so what
// sync gives meaning to and what a raw keyword may not be cannot drift apart.
var (
	JunkKeywords    = []string{"junk", "$junk"}
	NotJunkKeywords = []string{"nonjunk", "$notjunk"}

	junkVerdictTokens    = JunkKeywords
	notJunkVerdictTokens = NotJunkKeywords
)

// KeywordNotJunk is the ham-feedback keyword a rescue adds where the mailbox
// stores keywords.
const KeywordNotJunk = "$NotJunk"

// enqueueJunkVerdictIntents enqueues the keyword intents a verdict change owes
// the provider. It must be called BEFORE enqueueMoveIntents for the same action:
// the engine applies an action's intents in enqueue order, and these address the
// copy in the folder the move is about to take it out of.
//
// Removals are enqueued only for the accounts whose live copies CARRY the token,
// with the casing the server reported — nothing to remove means no intent, so a
// placement-only provider sees no removal STORE at all. A server that stored the
// keyword can be asked to remove it, so removals need no PERMANENTFLAGS gate.
// The `$NotJunk` add is gated on accepts_keywords (the PERMANENTFLAGS `\*`
// probe), for every live mailbox of the account: a STORE of a new keyword into a
// mailbox that refuses one would fail, retry and dead-letter, which would put an
// error in account health for feedback that was only ever a courtesy.
//
// THE KEYWORD CHECK IS LEARNED BY WRITING (#740). Only a read-write SELECT can say
// whether a mailbox stores new keywords, so accepts_keywords is recorded from
// the writeback session's SELECTs (store.UserTx.RecordKeywordProbes) and stays
// at its default, false, for a mailbox Mailroom has never written to. The
// FIRST rescue out of a Junk folder therefore sends no $NotJunk; its own
// keyword removal or move SELECTs Junk read-write and teaches the column, and
// every later rescue sends it. The removal above, which is what keeps a rescue
// rescued, never depended on the probe.
func (tx *UserTx) enqueueJunkVerdictIntents(ctx context.Context, action ulid.ULID, source string, messageID ulid.ULID, up EmailUpdate, moveRole string) error {
	rescue := up.Junk != nil && !*up.Junk
	intoJunk := !rescue && moveRole == "junk"
	if !rescue && !intoJunk {
		return nil
	}
	remove := notJunkVerdictTokens
	if rescue {
		remove = junkVerdictTokens
	}
	carried, err := tx.liveKeywordTokens(ctx, messageID, remove)
	if err != nil {
		return err
	}
	for _, c := range carried {
		if err := tx.enqueueEmailIntent(ctx, action, source, messageID, []ulid.ULID{c.account},
			IntentRemoveKeyword, map[string]string{"keyword": c.token}); err != nil {
			return err
		}
	}
	if !rescue {
		return nil
	}
	accepting, err := tx.keywordAcceptingAccounts(ctx, messageID)
	if err != nil {
		return err
	}
	if len(accepting) == 0 {
		return nil
	}
	return tx.enqueueEmailIntent(ctx, action, source, messageID, accepting,
		IntentAddKeyword, map[string]string{"keyword": KeywordNotJunk})
}

// carriedToken is one keyword, as the server spelled it, on a live copy in one
// account.
type carriedToken struct {
	account ulid.ULID
	token   string
}

// liveKeywordTokens returns, per account, the distinct remote_flags of the
// message's LIVE copies whose lower-case form is one of lowered. Ordered, so the
// intents it produces are enqueued in a stable order.
func (tx *UserTx) liveKeywordTokens(ctx context.Context, messageID ulid.ULID, lowered []string) ([]carriedToken, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT p.account_id, f.flag
		  FROM app.message_provenance p
		 CROSS JOIN LATERAL unnest(p.remote_flags) AS f(flag)
		 WHERE p.user_id = $1 AND p.message_id = $2 AND p.state = 'live'
		   AND lower(f.flag) = ANY($3::text[])
		 ORDER BY p.account_id, f.flag`,
		tx.userID.String(), messageID.String(), lowered)
	if err != nil {
		return nil, fmt.Errorf("store: live keyword tokens: %w", err)
	}
	defer rows.Close()
	var out []carriedToken
	for rows.Next() {
		var acct, flag string
		if err := rows.Scan(&acct, &flag); err != nil {
			return nil, fmt.Errorf("store: scan live keyword token: %w", err)
		}
		u, err := ulid.Parse(acct)
		if err != nil {
			return nil, fmt.Errorf("store: parse account id %q: %w", acct, err)
		}
		out = append(out, carriedToken{account: u, token: flag})
	}
	return out, rows.Err()
}

// keywordAcceptingAccounts returns the accounts holding a live copy of the
// message in which EVERY such copy's mailbox accepts new keywords. One refusing
// mailbox excludes the account: the engine applies an intent to every live copy
// in the account (#296), and a refused copy would fail the whole intent.
func (tx *UserTx) keywordAcceptingAccounts(ctx context.Context, messageID ulid.ULID) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT p.account_id
		  FROM app.message_provenance p
		  JOIN app.mailboxes mb ON mb.user_id = p.user_id AND mb.id = p.mailbox_id
		 WHERE p.user_id = $1 AND p.message_id = $2 AND p.state = 'live'
		 GROUP BY p.account_id
		HAVING bool_and(mb.accepts_keywords)
		 ORDER BY p.account_id`,
		tx.userID.String(), messageID.String())
	if err != nil {
		return nil, fmt.Errorf("store: keyword-accepting accounts: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var acct string
		if err := rows.Scan(&acct); err != nil {
			return nil, fmt.Errorf("store: scan account id: %w", err)
		}
		u, err := ulid.Parse(acct)
		if err != nil {
			return nil, fmt.Errorf("store: parse account id %q: %w", acct, err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ApplyStoredFlagDelta records, on ONE live provenance row, a flag change the
// writeback engine has just made on the server: add/remove are what a UID
// STORE that the server acknowledged put on, or took off, that copy (#576).
//
// remote_flags is "the last flags known to be on the server copy", and until
// now only a sync could refresh it. That was enough while a copy stayed where it
// was — the STORE bumps the mailbox's MODSEQ and the next flag pass reads the
// truth back. It is NOT enough when the same action then MOVES the copy: the
// source UID is gone before any sync looks at it, so the row keeps the flags it
// had before the STORE until the reaper retires it (up to an hour on the default
// cadence), and the flag merge ORs that stale set into the canonical message the
// moment the destination copy is ingested. A "Not junk" rescue on a $Junk
// provider took $Junk off on the server and then had the source row's memory of
// $Junk re-junk the message anyway.
//
// Matching is case-insensitive, as IMAP flags and keywords are in practice; an
// add keeps an existing spelling rather than duplicating it. NOT JMAP-visible —
// remote_flags is sync bookkeeping, like a mailbox's accepts_keywords probe —
// so no change_log row: canonical flags move only through the merge, which
// appends its own. A row that is no longer live (reaped, or retired by a
// UIDVALIDITY roll) is left alone: it no longer describes a server copy. It
// reports whether the row changed.
func (tx *UserTx) ApplyStoredFlagDelta(ctx context.Context, provenanceID ulid.ULID, add, remove []string) (bool, error) {
	var cur []string
	err := tx.tx.QueryRow(ctx, `
		SELECT remote_flags FROM app.message_provenance
		 WHERE user_id = $1 AND id = $2 AND state = 'live'
		 FOR UPDATE`,
		tx.userID.String(), provenanceID.String()).Scan(&cur)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: read remote flags: %w", err)
	}
	next, changed := applyFlagDelta(cur, add, remove)
	if !changed {
		return false, nil
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.message_provenance SET remote_flags = $3
		 WHERE user_id = $1 AND id = $2 AND state = 'live'`,
		tx.userID.String(), provenanceID.String(), next); err != nil {
		return false, fmt.Errorf("store: write remote flags: %w", err)
	}
	return true, nil
}

// applyFlagDelta applies a STORE's add/remove to a flag set, case-insensitively,
// preserving the order and spelling of what was already there. Removal wins over
// an add of the same flag, which is what one STORE of both would leave behind
// on a server that processes +FLAGS then -FLAGS — and the engine never sends
// both for one flag anyway.
func applyFlagDelta(cur, add, remove []string) ([]string, bool) {
	drop := make(map[string]bool, len(remove))
	for _, r := range remove {
		drop[strings.ToLower(r)] = true
	}
	out := make([]string, 0, len(cur)+len(add))
	have := make(map[string]bool, len(cur)+len(add))
	changed := false
	for _, f := range cur {
		k := strings.ToLower(f)
		if drop[k] {
			changed = true
			continue
		}
		if have[k] {
			continue
		}
		have[k] = true
		out = append(out, f)
	}
	for _, a := range add {
		k := strings.ToLower(a)
		if drop[k] || have[k] {
			continue
		}
		have[k] = true
		out = append(out, a)
		changed = true
	}
	return out, changed
}
