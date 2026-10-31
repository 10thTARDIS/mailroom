// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// LocalDKIM is Mailroom's own DKIM verdict on one message (app.message_local_auth,
// migration 00066, ADR-036): what the check at ingest found on a message that
// arrived with no Authentication-Results from a trusted authserv-id.
type LocalDKIM struct {
	// Result is internal/dkimverify's Status as text. Never "temperror": a
	// transient failure writes no row at all.
	Result string
	// Domain is d= of the signature that decided Result ("" when none read).
	Domain string
	// Aligned: for a pass, d= is the From domain or its parent and is not a
	// public suffix. Only an aligned pass is ever displayed.
	Aligned bool
	// DisplayBlocked is why a PASS is not displayed although it verified
	// (e.g. "duplicate_header:from": a header the signature covers appears
	// twice, RFC 6376 §8.15), "" otherwise. Aligned is false whenever it is set.
	DisplayBlocked string
	// CheckedAt is when the check ran.
	CheckedAt time.Time
}

// LocalDKIMMethod is the only method the table holds today.
const LocalDKIMMethod = "dkim"

// SetLocalDKIM records the local DKIM verdict of a message created in THIS
// transaction.
//
// ⚠ IT APPENDS NO change_log ROW, ON PURPOSE. The verdict is written in the
// same transaction as the message (InsertMessage appended its Email 'created'
// row), so it is part of the object that row announces, and it is never
// updated afterwards, so there is no later mutation to announce. The
// load-bearing invariant is "every mutation of a JMAP-visible object appends a
// row"; this is not a mutation of an existing object. Do NOT call it for a
// message that already existed: a verdict added later would change what
// Email/get returns with no row for a client to learn it from.
//
// The EXISTS reads app.messages under RLS, so a message that is not the
// caller's writes nothing (a foreign-key check alone runs outside RLS and would
// let one user plant a row against another's message id).
func (tx *UserTx) SetLocalDKIM(ctx context.Context, messageID ulid.ULID, v LocalDKIM) error {
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.message_local_auth (message_id, user_id, method, result, domain, aligned, display_blocked)
		SELECT $1::app.ulid, $2::app.ulid, $3::text, $4::text, $5::text, $6::boolean, $7::text
		 WHERE EXISTS (SELECT 1 FROM app.messages WHERE id = $1::app.ulid)
		ON CONFLICT (message_id, method) DO NOTHING`,
		messageID.String(), tx.userID.String(), LocalDKIMMethod, v.Result, v.Domain, v.Aligned, v.DisplayBlocked); err != nil {
		return fmt.Errorf("store: set local dkim: %w", err)
	}
	return nil
}

// LocalDKIMFor returns the local DKIM verdicts of the given messages in ONE
// query (a message with none is absent from the map). Batched per request like
// ProviderDated: Email/get must not query per message.
func (tx *UserTx) LocalDKIMFor(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID]LocalDKIM, error) {
	out := map[ulid.ULID]LocalDKIM{}
	if len(ids) == 0 {
		return out, nil
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT message_id, result, domain, aligned, display_blocked, checked_at
		  FROM app.message_local_auth
		 WHERE user_id = $1 AND method = $2 AND message_id = ANY($3)`,
		tx.userID.String(), LocalDKIMMethod, strs)
	if err != nil {
		return nil, fmt.Errorf("store: local dkim: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			s string
			v LocalDKIM
		)
		if err := rows.Scan(&s, &v.Result, &v.Domain, &v.Aligned, &v.DisplayBlocked, &v.CheckedAt); err != nil {
			return nil, fmt.Errorf("store: scan local dkim: %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", s, err)
		}
		out[id] = v
	}
	return out, rows.Err()
}

// LocalDKIMPass is the ONE definition of a local verdict that may be displayed
// and counted as an affirmative pass: result pass AND aligned. A local fail, a
// key problem, or an unaligned pass is stored and never used (ADR-036).
func (v *LocalDKIM) LocalDKIMPass() bool { return v != nil && v.Result == "pass" && v.Aligned }
