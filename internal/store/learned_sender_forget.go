// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mailroom/internal/placement"
)

// FORGET A SENDER (#167, ADR-032 amendment, migration 00102).
//
// Learned sorting has no stored model: its evidence is read fresh at each
// decision from the user's filings and the corrections of its own placements.
// Forgetting is therefore a MARK, not a deletion: app.learned_sender_forgotten
// records when the user asked, and the evidence reads below ignore any filing
// or correction OLDER than the mark. A filing made after it counts again.
//
// WHICH MARK HIDES WHICH EVIDENCE.
//   - An ADDRESS mark hides that address's older filings from the address key
//     AND from its domain's evidence. Without the second half the domain
//     fallback would go on acting on the very filings the user told learned
//     sorting to forget.
//   - A DOMAIN mark hides older filings from the DOMAIN key only. The user
//     asked about the domain's learning, not about each address's own.
//   - A correction is hidden by the mark of the key it was recorded on.
//
// The correction gate is NOT touched: it is the record of what learned sorting
// DID and how that went, not of what it knows.
//
// CHANGE_LOG: none, like the trust table (learned_sender_trust.go).

// forgetMarks reads every forget mark of the user.
func (tx *UserTx) forgetMarks(ctx context.Context) (map[placement.Key]time.Time, error) {
	rows, err := tx.tx.Query(ctx,
		`SELECT key_kind, key_value, forgotten_at FROM app.learned_sender_forgotten WHERE user_id = $1`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: forget marks: %w", err)
	}
	defer rows.Close()
	out := map[placement.Key]time.Time{}
	for rows.Next() {
		var kind, value string
		var at time.Time
		if err := rows.Scan(&kind, &value, &at); err != nil {
			return nil, fmt.Errorf("store: scan forget mark: %w", err)
		}
		out[placement.Key{Kind: placement.KeyKind(kind), Value: value}] = at
	}
	return out, rows.Err()
}

// ForgetSender records that learned sorting forgets everything it knew about
// key (an address or a domain) as of now, and returns the mark's time. The
// value is lowercased and trimmed; an address must be a usable From address and
// a domain must have no '@'.
func (tx *UserTx) ForgetSender(ctx context.Context, kind placement.KeyKind, value string) (time.Time, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	switch kind {
	case placement.KeyAddress:
		a, ok := normaliseForgetAddress(v)
		if !ok {
			return time.Time{}, ErrForgetInvalid
		}
		v = a
	case placement.KeyDomain:
		if v == "" || strings.ContainsAny(v, "@ \t\r\n<>,;") || len(v) > 320 {
			return time.Time{}, ErrForgetInvalid
		}
	default:
		return time.Time{}, ErrForgetInvalid
	}
	var at time.Time
	if err := tx.tx.QueryRow(ctx, `
		INSERT INTO app.learned_sender_forgotten (user_id, key_kind, key_value, forgotten_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id, key_kind, key_value) DO UPDATE SET forgotten_at = excluded.forgotten_at
		RETURNING forgotten_at`, tx.userID.String(), string(kind), v).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("store: forget sender: %w", err)
	}
	return at, nil
}

// ErrForgetInvalid is returned for an address or domain Forget cannot key on.
var ErrForgetInvalid = fmt.Errorf("store: not a usable address or domain to forget")

// normaliseForgetAddress is the key a Forget mark is stored under: lowercased
// and trimmed by KeysFor (the rule the evidence is keyed with), with no
// whitespace or brackets. Unlike trust it is NOT restricted to ASCII: the ASCII
// rule is a security measure for what learned sorting BELIEVES
// (normaliseTrustAddress); forgetting a sender whose address has an accent is
// harmless, and refusing it would leave the user unable to forget them.
func normaliseForgetAddress(addr string) (string, bool) {
	a, _, ok := placement.KeysFor(addr)
	if !ok || strings.ContainsAny(a.Value, " \t\r\n<>,;") {
		return "", false
	}
	return a.Value, true
}
