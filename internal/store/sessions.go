// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Server-side browser sessions (#937, ADR-048, migration 00082).
//
// A row is one signed-in browser. The cookie's secret is kept only as its
// SHA-256 (token_hash), and the provider refresh token and latest ID token only
// as opaque sealed ciphertext: SEALING HAPPENS IN THE CALLER (cmd/mailroom's
// session adapter), not here, so this package takes and returns []byte and never
// sees a token.
//
// Like push_subscriptions and api_keys, sessions are OPERATIONAL data, not
// JMAP-visible objects: every write here appends NO change_log row (ADR-048 §2).
//
// The one cross-user read is LookupSession, which runs before anyone is known: it
// goes through app.session_for_token (SECURITY DEFINER, exact hash match, refuses
// a revoked row or a disabled or deleted user). Every other method runs inside a
// UserTx, so RLS scopes it to the caller's own sessions: user B can neither read,
// touch nor revoke user A's.

// Session revocation reasons (app.sessions.revoked_reason's closed set).
const (
	SessionRevokedSignedOut          = "signed_out"
	SessionRevokedSignedOutElsewhere = "signed_out_elsewhere"
	SessionRevokedReplaced           = "replaced"
	SessionRevokedRefused            = "refused"
	SessionRevokedExpired            = "expired"
)

// SessionRef is what the per-request lookup yields.
type SessionRef struct {
	ID            ulid.ULID
	UserID        ulid.ULID
	LastCheckedAt time.Time
}

// NewSession is the input to CreateSession. The caller picks the id because the
// sealed tokens bind it as associated data, so it must exist before sealing.
type NewSession struct {
	ID        ulid.ULID
	TokenHash [32]byte
	// RefreshCT and IDTokenCT are sealed by the caller; either may be empty.
	RefreshCT []byte
	IDTokenCT []byte
	UserAgent string
	Addr      string
}

// maxSessionUserAgent mirrors the table's CHECK (octet_length <= 512).
const maxSessionUserAgent = 512

// LookupSession resolves a cookie-secret hash to its live session. ok is false
// when there is none (unknown, revoked, or the user disabled or deleted); an
// error is a database failure, which callers must NOT treat as "unauthenticated".
func (s *Store) LookupSession(ctx context.Context, hash [32]byte) (SessionRef, bool, error) {
	var ref SessionRef
	var ok bool
	err := s.SystemTx(ctx, func(tx *SystemTx) error {
		var sid, uid string
		qerr := tx.tx.QueryRow(ctx,
			`SELECT session_id, user_id, last_checked_at FROM app.session_for_token($1)`, hash[:],
		).Scan(&sid, &uid, &ref.LastCheckedAt)
		if qerr != nil {
			if isNoRows(qerr) {
				return nil
			}
			return fmt.Errorf("store: look up session: %w", qerr)
		}
		var perr error
		if ref.ID, perr = ulid.Parse(sid); perr != nil {
			return fmt.Errorf("store: parse session id: %w", perr)
		}
		if ref.UserID, perr = ulid.Parse(uid); perr != nil {
			return fmt.Errorf("store: parse session user id: %w", perr)
		}
		ok = true
		return nil
	})
	if err != nil {
		return SessionRef{}, false, err
	}
	return ref, ok, nil
}

// CreateSession inserts a new session for the transaction's user.
func (tx *UserTx) CreateSession(ctx context.Context, in NewSession) error {
	if in.ID == (ulid.ULID{}) {
		return errors.New("store: create session: id is required")
	}
	ua := in.UserAgent
	if len(ua) > maxSessionUserAgent {
		ua = ua[:maxSessionUserAgent]
	}
	// An empty ciphertext is stored as NULL ("none issued"), never as empty bytes.
	var refresh, idt []byte
	if len(in.RefreshCT) > 0 {
		refresh = in.RefreshCT
	}
	if len(in.IDTokenCT) > 0 {
		idt = in.IDTokenCT
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.sessions (id, user_id, token_hash, refresh_ct, id_token_ct, user_agent, last_addr)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		in.ID.String(), tx.userID.String(), in.TokenHash[:], refresh, idt, ua, in.Addr); err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// TouchSession records that the session was used. It writes at most once a
// minute per session, so a busy tab does not turn every request into a write.
func (tx *UserTx) TouchSession(ctx context.Context, id ulid.ULID, addr string, at time.Time) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.sessions
		   SET last_seen_at = $3, last_addr = $2
		 WHERE id = $1 AND revoked_at IS NULL
		   AND last_seen_at < $3::timestamptz - interval '1 minute'`,
		id.String(), addr, at.UTC()); err != nil {
		return fmt.Errorf("store: touch session: %w", err)
	}
	return nil
}

// RevokeSession ends a session, for EVERY reason (sign-out, refused, expired,
// replaced), and in the same transaction deletes the push subscriptions that were
// registered from it: a session that is over must not leave a subscription behind
// that keeps receiving pushes, whose text is derived from mail (ADR-048 section 5).
// It reports whether a live session was revoked: false when it was already
// revoked, or is not the caller's.
//
// ⚠ REASON "replaced" TAKES THE SUBSCRIPTION TOO. A second sign-in revokes the
// first session as replaced, so the browser's subscription is deleted with it; the
// web client's reconcile-on-connect (increment 4) re-registers it under the new
// session. Until that client ships, a replaced sign-in loses push on that browser.
//
// The delete runs whether or not the update revoked anything: an already-revoked
// session has none left, and a session that is not the caller's matches nothing
// (user_id is in the predicate as well as RLS).
func (tx *UserTx) RevokeSession(ctx context.Context, id ulid.ULID, reason string) (bool, error) {
	return tx.revokeSessionRow(ctx, id, reason)
}

// revokeSessionRow is THE single-session revocation: every code path that sets
// app.sessions.revoked_at for one session goes through it (RevokeSession, and
// RenewSessionLocked's refuse branch), so none can end a session and forget its
// push subscriptions. The multi-session path (revokeSessionsWhere) has its own
// UPDATE ... RETURNING and pairs it with the same deleteSessionPush. A new place
// that sets revoked_at must use one of the two; TestEveryRevokedAtWriterDeletesPush
// reads the source to hold that.
func (tx *UserTx) revokeSessionRow(ctx context.Context, id ulid.ULID, reason string) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.sessions SET revoked_at = now(), revoked_reason = $2
		 WHERE id = $1 AND revoked_at IS NULL`, id.String(), reason)
	if err != nil {
		return false, fmt.Errorf("store: revoke session: %w", err)
	}
	if _, err := tx.deleteSessionPush(ctx, []ulid.ULID{id}); err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// deleteSessionPush deletes this user's push subscriptions registered from the
// given sessions, in the caller's transaction, and reports how many.
func (tx *UserTx) deleteSessionPush(ctx context.Context, ids []ulid.ULID) (int64, error) {
	tag, err := tx.tx.Exec(ctx,
		`DELETE FROM app.push_subscriptions WHERE user_id = $1 AND session_id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return 0, fmt.Errorf("store: delete push subscriptions of revoked sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SessionIDToken returns the sealed ID token of one of the caller's sessions
// (the logout hint), or nil when there is none or the session is not theirs.
func (tx *UserTx) SessionIDToken(ctx context.Context, id ulid.ULID) ([]byte, error) {
	var ct []byte
	err := tx.tx.QueryRow(ctx, `SELECT id_token_ct FROM app.sessions WHERE id = $1`, id.String()).Scan(&ct)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read session id token: %w", err)
	}
	return ct, nil
}

// SessionRefreshToken returns the sealed refresh token of one of the caller's
// sessions (logout revokes it at the provider), or nil when there is none or the
// session is not theirs.
func (tx *UserTx) SessionRefreshToken(ctx context.Context, id ulid.ULID) ([]byte, error) {
	var ct []byte
	err := tx.tx.QueryRow(ctx, `SELECT refresh_ct FROM app.sessions WHERE id = $1`, id.String()).Scan(&ct)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read session refresh token: %w", err)
	}
	return ct, nil
}

// ---- Signed-in devices, the stream re-check and the sweep (#937 increment 3) ----

// SessionRow is one live session as the signed-in devices list shows it. The
// raw user agent stays here: the API layer reduces it to a description and
// never returns the string.
type SessionRow struct {
	ID            ulid.ULID
	CreatedAt     time.Time
	LastSeenAt    time.Time
	LastCheckedAt time.Time
	UserAgent     string
	LastAddr      string
}

// ListLiveSessions returns the caller's sessions that are not revoked, newest
// use first. A revoked row is never listed (ADR-048 section 7).
func (tx *UserTx) ListLiveSessions(ctx context.Context) ([]SessionRow, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, created_at, last_seen_at, last_checked_at, user_agent, last_addr
		  FROM app.sessions
		 WHERE user_id = $1 AND revoked_at IS NULL
		 ORDER BY last_seen_at DESC, id DESC`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var (
			r  SessionRow
			id string
		)
		if err := rows.Scan(&id, &r.CreatedAt, &r.LastSeenAt, &r.LastCheckedAt, &r.UserAgent, &r.LastAddr); err != nil {
			return nil, fmt.Errorf("store: scan session: %w", err)
		}
		if r.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse session id: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	return out, nil
}

// RevokedSessions is what a device sign-out did.
type RevokedSessions struct {
	// IDs are the sessions that were live and are now revoked.
	IDs []ulid.ULID
	// RefreshCT is each revoked session's SEALED refresh token (absent when it
	// had none), for revocation at the provider. The caller opens it.
	RefreshCT map[ulid.ULID][]byte
	// PushDeleted is how many push subscriptions were deleted with them.
	PushDeleted int64
}

// RevokeSessions revokes the given sessions of THIS user (RLS: another user's id
// matches nothing and is simply absent from the result) and, in the same
// transaction, deletes the push subscriptions registered from them. It appends
// no change_log row: sessions and push subscriptions are operational data.
func (tx *UserTx) RevokeSessions(ctx context.Context, ids []ulid.ULID, reason string) (RevokedSessions, error) {
	if len(ids) == 0 {
		return RevokedSessions{RefreshCT: map[ulid.ULID][]byte{}}, nil
	}
	return tx.revokeSessionsWhere(ctx, reason, `id = ANY($3)`, ulidStrings(ids))
}

// RevokeSessionsExcept revokes every live session of this user but keep.
func (tx *UserTx) RevokeSessionsExcept(ctx context.Context, keep ulid.ULID, reason string) (RevokedSessions, error) {
	return tx.revokeSessionsWhere(ctx, reason, `id <> $3`, keep.String())
}

func (tx *UserTx) revokeSessionsWhere(ctx context.Context, reason, cond string, arg any) (RevokedSessions, error) {
	out := RevokedSessions{RefreshCT: map[ulid.ULID][]byte{}}
	// cond is one of two constants above, never caller input.
	rows, err := tx.tx.Query(ctx, `
		UPDATE app.sessions SET revoked_at = now(), revoked_reason = $2
		 WHERE user_id = $1 AND revoked_at IS NULL AND `+cond+`
		RETURNING id, refresh_ct`, tx.userID.String(), reason, arg)
	if err != nil {
		return RevokedSessions{}, fmt.Errorf("store: revoke sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id string
			ct []byte
		)
		if err := rows.Scan(&id, &ct); err != nil {
			return RevokedSessions{}, fmt.Errorf("store: scan revoked session: %w", err)
		}
		sid, perr := ulid.Parse(id)
		if perr != nil {
			return RevokedSessions{}, fmt.Errorf("store: parse session id: %w", perr)
		}
		out.IDs = append(out.IDs, sid)
		if len(ct) > 0 {
			out.RefreshCT[sid] = ct
		}
	}
	if err := rows.Err(); err != nil {
		return RevokedSessions{}, fmt.Errorf("store: revoke sessions: %w", err)
	}
	rows.Close()
	if len(out.IDs) == 0 {
		return out, nil
	}
	if out.PushDeleted, err = tx.deleteSessionPush(ctx, out.IDs); err != nil {
		return RevokedSessions{}, err
	}
	return out, nil
}

// SessionLive is the stream re-check (ADR-048 section 6): is this session still
// usable? It is false when the row is gone or revoked, the user is disabled or
// deleted (users_self shows the caller their own row), or the last provider
// confirmation is older than allowance as of now. It STAMPS NOTHING and never
// renews: a stream check is not use.
func (tx *UserTx) SessionLive(ctx context.Context, id ulid.ULID, allowance time.Duration, now time.Time) (bool, error) {
	var checked time.Time
	err := tx.tx.QueryRow(ctx, `
		SELECT s.last_checked_at
		  FROM app.sessions s
		  JOIN app.users u ON u.id = s.user_id
		 WHERE s.id = $1 AND s.user_id = $2 AND s.revoked_at IS NULL
		   AND u.disabled_at IS NULL AND u.deleted_at IS NULL`,
		id.String(), tx.userID.String()).Scan(&checked)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: check session: %w", err)
	}
	return now.Sub(checked) <= allowance, nil
}

// SweepReasons is every key SweepSessions reports, in order: the closed set of
// revoked_reason plus "unseen" (a live session nobody used for 45 days), and the
// push subscriptions deleted with them, counted apart: "push_unseen" (of an unseen
// session) and "push_revoked" (a leftover of a session revoked before sign-out
// deleted its subscriptions).
var SweepReasons = []string{"expired", "push_revoked", "push_unseen", "refused", "replaced", "signed_out", "signed_out_elsewhere", "unseen"}

// SweepSessions deletes revoked sessions older than 7 days and live ones unseen
// for 45 (ADR-048 section 7), and the push subscriptions of those sessions,
// through app.sweep_sessions, and reports the count per reason, EVERY reason
// present even at zero. It runs without a user
// identity; the function is the only cross-user path.
func (s *Store) SweepSessions(ctx context.Context, now time.Time) (map[string]int64, error) {
	out := make(map[string]int64, len(SweepReasons))
	err := s.SystemTx(ctx, func(tx *SystemTx) error {
		rows, err := tx.tx.Query(ctx, `SELECT reason, deleted FROM app.sweep_sessions($1)`, now.UTC())
		if err != nil {
			return fmt.Errorf("store: sweep sessions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				reason string
				n      int64
			)
			if err := rows.Scan(&reason, &n); err != nil {
				return fmt.Errorf("store: scan session sweep: %w", err)
			}
			out[reason] = n
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	// A reason the function did not name still appears, as zero: a missing key
	// must never read as "nothing to do".
	for _, r := range SweepReasons {
		if _, ok := out[r]; !ok {
			out[r] = 0
		}
	}
	return out, nil
}
