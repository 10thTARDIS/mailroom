// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"

	"mailroom/internal/placement"
)

// TRUSTED SENDERS FOR LEARNED SORTING (#169, ADR-032 amendment, migration 00101).
//
// The user marks ONE EXACT From address as trusted. For that address, and only
// that address, learned sorting skips the requirement of a trusted DMARC pass
// (senderChecks). It skips nothing else, and it changes learning only: the
// warnings shown to the user are untouched.
//
// CHANGE_LOG: none, and deliberately. app.learned_sender_trust is a private
// per-user setting, not a JMAP-visible object (no /get, no /changes, no state
// string of its own), exactly like the rule-suggestion dismissals (migration
// 00085): the load-bearing invariant covers mutations of JMAP-visible objects.
// The LearnedSender view the API returns is computed, never synced.

// normaliseTrustAddress lowercases and trims addr and reports whether it is an
// address learned sorting can key on (placement.KeysFor's rule), so a value the
// ingest path would never build a key from is never stored.
func normaliseTrustAddress(addr string) (string, bool) {
	a, _, ok := placement.KeysFor(addr)
	if !ok || strings.ContainsAny(a.Value, " \t\r\n<>,;") {
		return "", false
	}
	return a.Value, true
}

// ErrSenderInvalid is returned for a sender that is not one usable From address.
var ErrSenderInvalid = fmt.Errorf("store: not a usable sender address")

// SetSenderTrusted trusts or untrusts one exact address and reports whether the
// stored state changed (false: it already was in that state).
func (tx *UserTx) SetSenderTrusted(ctx context.Context, addr string, trusted bool) (changed bool, err error) {
	a, ok := normaliseTrustAddress(addr)
	if !ok {
		return false, ErrSenderInvalid
	}
	if trusted {
		tag, err := tx.tx.Exec(ctx, `
			INSERT INTO app.learned_sender_trust (user_id, address) VALUES ($1, $2)
			ON CONFLICT (user_id, address) DO NOTHING`, tx.userID.String(), a)
		if err != nil {
			return false, fmt.Errorf("store: trust sender: %w", err)
		}
		return tag.RowsAffected() > 0, nil
	}
	tag, err := tx.tx.Exec(ctx,
		`DELETE FROM app.learned_sender_trust WHERE user_id = $1 AND address = $2`, tx.userID.String(), a)
	if err != nil {
		return false, fmt.Errorf("store: untrust sender: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SenderTrusted reports whether addr (already lowercased by the caller's key) is
// trusted. THE ONE READ the ingest path, the explain path, the backfill and
// LearnedSender/get share.
func (tx *UserTx) SenderTrusted(ctx context.Context, addr string) (bool, error) {
	var ok bool
	if err := tx.tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM app.learned_sender_trust WHERE user_id = $1 AND address = $2)`,
		tx.userID.String(), strings.ToLower(strings.TrimSpace(addr))).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: sender trusted: %w", err)
	}
	return ok, nil
}

// TrustedSenders lists every trusted address, in order, for the user export.
func (tx *UserTx) TrustedSenders(ctx context.Context) ([]string, error) {
	rows, err := tx.tx.Query(ctx,
		`SELECT address FROM app.learned_sender_trust WHERE user_id = $1 ORDER BY address`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: trusted senders: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("store: scan trusted sender: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// trustedSenderSet is every trusted address as a set: what senderChecks reads.
// A user's trusted list is a handful of rows, read once per decision.
func (tx *UserTx) trustedSenderSet(ctx context.Context) (map[string]bool, error) {
	list, err := tx.TrustedSenders(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list))
	for _, a := range list {
		out[a] = true
	}
	return out, nil
}
