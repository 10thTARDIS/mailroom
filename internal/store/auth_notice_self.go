// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// AuthSelfFacts is what the `authResults` notice needs to tell the user's own
// mail from inbound mail (#766): which mailboxes are Sent or Drafts. Read once
// per request, not once per message.
//
// ⚠ THE FROM ADDRESS IS NOT A SELF SIGNAL. A spoofer writes the user's own
// address in From, and treating that as "own mail" would silence the one notice
// ("unauthenticated") that exists for header-less inbound mail. Self is locally
// authored, or live in a Sent or Drafts mailbox, and nothing the sender wrote.
type AuthSelfFacts struct {
	// OutboundMailboxes are the mailbox ids whose role is sent or drafts.
	OutboundMailboxes map[ulid.ULID]bool
}

// AuthSelfFacts reads the Sent/Drafts mailbox set (one small query).
//
// The role comes from roleForMailbox, the function every other read path uses,
// so "Sent" here means exactly what Mailbox/get reports.
func (tx *UserTx) AuthSelfFacts(ctx context.Context) (AuthSelfFacts, error) {
	out := AuthSelfFacts{OutboundMailboxes: map[ulid.ULID]bool{}}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, imap_name, COALESCE(special_use, '')
		  FROM app.mailboxes
		 WHERE user_id = $1`, tx.userID.String())
	if err != nil {
		return out, fmt.Errorf("store: auth self facts (mailboxes): %w", err)
	}
	for rows.Next() {
		var idStr, name, special string
		if err := rows.Scan(&idStr, &name, &special); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: auth self facts (mailbox row): %w", err)
		}
		if r := roleForMailbox(name, special); r == "sent" || r == "drafts" {
			id, perr := ulid.Parse(idStr)
			if perr != nil {
				rows.Close()
				return out, fmt.Errorf("store: auth self facts: mailbox id %q: %w", idStr, perr)
			}
			out.OutboundMailboxes[id] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: auth self facts (mailboxes): %w", err)
	}

	return out, nil
}

// OwnIdentityAddresses returns the user's fixed identity addresses, normalised.
// REPORT-ONLY (#766): the auth-report counts mail From one of them that is not in
// Sent or Drafts, to show what the self rule costs. The notice never reads it.
func (tx *UserTx) OwnIdentityAddresses(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	arows, err := tx.tx.Query(ctx, `
		SELECT lower(address::text)
		  FROM app.identities
		 WHERE user_id = $1 AND address IS NOT NULL`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: own identity addresses: %w", err)
	}
	defer arows.Close()
	for arows.Next() {
		var a string
		if err := arows.Scan(&a); err != nil {
			return nil, fmt.Errorf("store: own identity addresses row: %w", err)
		}
		out[NormalizeAddress(a)] = true
	}
	return out, arows.Err()
}

// providerDatedExpr is THE predicate for "this message's received_at is a
// provider INTERNALDATE" (#766), as a SQL boolean over the messages table
// aliased `alias`: no local mailbox AND a provenance row of any state. Every
// query that computes a trust time uses it (ProviderDated, MessageAuthRef,
// MessageFromRef, the placement backfill), so they cannot disagree.
//
// WHY BOTH HALVES. Archive rows are created with local_mailbox_id set, and
// their received_at is the archive's guess or the sender's Date header. A row
// with provenance but no local mailbox is IMAP-sourced. An archive row that the
// IMAP sync later also sees gains a provenance row, and keeps local_mailbox_id
// and its archive-era received_at (earliest INTERNALDATE wins), so the
// provenance row alone would wrongly date it. Nothing clears local_mailbox_id
// when provenance attaches.
func providerDatedExpr(alias string) string {
	return "(" + alias + ".local_mailbox_id IS NULL AND EXISTS (SELECT 1 FROM app.message_provenance p " +
		"WHERE p.user_id = " + alias + ".user_id AND p.message_id = " + alias + ".id))"
}

// ProviderDated reports which of the given messages have a received_at the
// provider set (providerDatedExpr). A date-bounded authserv-id entry may only be
// judged against such a time.
//
// There is no column recording where received_at came from, so this is the
// closest marker that needs no migration. It is conservative: an archive row
// with a genuine delivery date gets no bounded trust, and neither does a
// locally authored message (also local_mailbox_id set), which is harmless
// because the notice treats it as self.
func (tx *UserTx) ProviderDated(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID]bool, error) {
	out := map[ulid.ULID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.id = ANY($2) AND `+providerDatedExpr("m"), tx.userID.String(), strs)
	if err != nil {
		return nil, fmt.Errorf("store: provider dated: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: scan provider dated: %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", s, err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// TrustTime is the received time to hand ml.TrustedAt: received when the
// provider dated the message, the zero time (no bounded entry trusted) when not.
func TrustTime(received time.Time, providerDated bool) time.Time {
	if !providerDated {
		return time.Time{}
	}
	return received
}

// IsSelf is THE rule for "this message is the user's own" (#766): live in a
// Sent or Drafts mailbox. (Locally authored is checked by the caller from the
// row.) The API and the admin reports both call it.
func (f AuthSelfFacts) IsSelf(mailboxes []ulid.ULID) bool {
	for _, m := range mailboxes {
		if f.OutboundMailboxes[m] {
			return true
		}
	}
	return false
}

// LiveMailboxIDs reports, per message, the mailboxes it is live in
// (message_provenance, state 'live') for the admin reports; the same read
// Email/get does through attachMailboxIDs.
func (tx *UserTx) LiveMailboxIDs(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID][]ulid.ULID, error) {
	out := map[ulid.ULID][]ulid.ULID{}
	if len(ids) == 0 {
		return out, nil
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, array_agg(DISTINCT mailbox_id)
		  FROM app.message_provenance
		 WHERE user_id = $1 AND message_id = ANY($2) AND state = 'live'
		 GROUP BY message_id`, tx.userID.String(), strs)
	if err != nil {
		return nil, fmt.Errorf("store: live mailbox ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mid string
		var mbs []string
		if err := rows.Scan(&mid, &mbs); err != nil {
			return nil, fmt.Errorf("store: scan live mailbox ids: %w", err)
		}
		m, err := ulid.Parse(mid)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", mid, err)
		}
		for _, b := range mbs {
			bid, err := ulid.Parse(b)
			if err != nil {
				return nil, fmt.Errorf("store: parse mailbox id %q: %w", b, err)
			}
			out[m] = append(out[m], bid)
		}
	}
	return out, rows.Err()
}
