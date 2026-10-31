// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/oklog/ulid/v2"
)

// API keys for agents and third-party clients (#49, spec §13a, ADR-042).
//
// THE STORE HOLDS ONLY A HASH. The token is generated, shown once and hashed in
// internal/auth; nothing here ever sees it, so nothing here can log it. A key's
// rows are app.api_keys (the key), app.api_key_events (what it did) and
// app.api_key_pending (what it asked a human to approve), all user_id-led and
// FORCE RLS like every user table.
//
// None of the three is a JMAP-synced object (no /changes), exactly like
// app.push_subscriptions: a write here appends NO change_log row, and that is
// the change_log invariant's own scope rather than an exception to it.

// Errors the API key surface distinguishes.
var (
	// ErrAPIKeyNameTaken is a create or rename onto a name another of the
	// user's keys (revoked ones included — they keep their audit) already has.
	ErrAPIKeyNameTaken = errors.New("store: an API key with this name already exists")
	// ErrAPIKeyNotFound is an id that is not one of the caller's keys.
	ErrAPIKeyNotFound = errors.New("store: API key not found")
)

// APIKey is one key as the store holds it. AccountIDs nil means every account
// of the user; a non-nil slice is the restriction. The token is not here.
type APIKey struct {
	ID               ulid.ULID
	Name             string
	TokenHint        string
	Scopes           []string
	AccountIDs       []ulid.ULID
	LabelIDs         []ulid.ULID
	ConfirmScopes    []string
	RatePerMin       int
	DailyBudget      int
	SuspendOnAnomaly bool
	CreatedAt        time.Time
	LastUsedAt       *time.Time
	RevokedAt        *time.Time
	SuspendedAt      *time.Time
	SuspendedReason  string
}

// Active reports whether the key may be used right now.
func (k APIKey) Active() bool { return k.RevokedAt == nil && k.SuspendedAt == nil }

// NewAPIKey is a key to create. TokenHash is SHA-256 of the token.
type NewAPIKey struct {
	Name             string
	TokenHash        [32]byte
	TokenHint        string
	Scopes           []string
	AccountIDs       []ulid.ULID
	ConfirmScopes    []string
	RatePerMin       int
	DailyBudget      int
	SuspendOnAnomaly bool
}

const apiKeyColumns = `id, name, token_hint, scopes, account_ids, label_ids, confirm_scopes,
	rate_per_min, daily_budget, suspend_on_anomaly, created_at, last_used_at, revoked_at,
	suspended_at, COALESCE(suspended_reason, '')`

func scanAPIKey(row interface{ Scan(...any) error }) (APIKey, error) {
	var (
		k                  APIKey
		id                 string
		accounts, labels   []string
		scopes, confirming []string
	)
	if err := row.Scan(&id, &k.Name, &k.TokenHint, &scopes, &accounts, &labels, &confirming,
		&k.RatePerMin, &k.DailyBudget, &k.SuspendOnAnomaly, &k.CreatedAt, &k.LastUsedAt,
		&k.RevokedAt, &k.SuspendedAt, &k.SuspendedReason); err != nil {
		return APIKey{}, err
	}
	var err error
	if k.ID, err = ulid.Parse(id); err != nil {
		return APIKey{}, fmt.Errorf("store: parse API key id %q: %w", id, err)
	}
	k.Scopes = nonNilStrings(scopes)
	k.ConfirmScopes = nonNilStrings(confirming)
	if accounts != nil {
		if k.AccountIDs, err = parseULIDs(accounts); err != nil {
			return APIKey{}, err
		}
	}
	if labels != nil {
		if k.LabelIDs, err = parseULIDs(labels); err != nil {
			return APIKey{}, err
		}
	}
	return k, nil
}

// nullableULIDs binds nil as SQL NULL (the "every account" restriction) and any
// other slice, empty included, as an array.
func nullableULIDs(ids []ulid.ULID) any {
	if ids == nil {
		return nil
	}
	return ulidStrings(ids)
}

// CreateAPIKey inserts a key. The scope set, the confirm set and the limits are
// also bounded by CHECK constraints (migration 00074), so a caller that skipped
// the API's validation is refused by the database rather than trusted.
func (tx *UserTx) CreateAPIKey(ctx context.Context, n NewAPIKey) (APIKey, error) {
	id := NewULID()
	row := tx.tx.QueryRow(ctx, `
		INSERT INTO app.api_keys (id, user_id, name, token_hash, token_hint, scopes, account_ids,
		                          confirm_scopes, rate_per_min, daily_budget, suspend_on_anomaly)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+apiKeyColumns,
		id.String(), tx.userID.String(), n.Name, n.TokenHash[:], n.TokenHint, n.Scopes,
		nullableULIDs(n.AccountIDs), nonNilStrings(n.ConfirmScopes), n.RatePerMin, n.DailyBudget,
		n.SuspendOnAnomaly)
	k, err := scanAPIKey(row)
	if err != nil {
		if isUniqueViolation(err) {
			return APIKey{}, ErrAPIKeyNameTaken
		}
		return APIKey{}, fmt.Errorf("store: create API key: %w", err)
	}
	return k, nil
}

// ListAPIKeys returns every key of the caller, revoked ones included.
func (tx *UserTx) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := tx.tx.Query(ctx, `SELECT `+apiKeyColumns+`
		  FROM app.api_keys WHERE user_id = $1 ORDER BY created_at, id`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list API keys: %w", err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan API key: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list API keys: %w", err)
	}
	return out, nil
}

// GetAPIKey reads one of the caller's keys.
func (tx *UserTx) GetAPIKey(ctx context.Context, id ulid.ULID) (APIKey, bool, error) {
	k, err := scanAPIKey(tx.tx.QueryRow(ctx, `SELECT `+apiKeyColumns+`
		  FROM app.api_keys WHERE user_id = $1 AND id = $2`, tx.userID.String(), id.String()))
	if err != nil {
		if isNoRows(err) {
			return APIKey{}, false, nil
		}
		return APIKey{}, false, fmt.Errorf("store: get API key: %w", err)
	}
	return k, true, nil
}

// LockAPIKey reads one of the caller's keys FOR UPDATE. Every admission of a
// call by a key holds this lock across "how much has it spent" and "record
// this call", so two concurrent calls cannot both take the last unit of a
// limit (ADR-042 §5).
func (tx *UserTx) LockAPIKey(ctx context.Context, id ulid.ULID) (APIKey, bool, error) {
	k, err := scanAPIKey(tx.tx.QueryRow(ctx, `SELECT `+apiKeyColumns+`
		  FROM app.api_keys WHERE user_id = $1 AND id = $2 FOR UPDATE`, tx.userID.String(), id.String()))
	if err != nil {
		if isNoRows(err) {
			return APIKey{}, false, nil
		}
		return APIKey{}, false, fmt.Errorf("store: lock API key: %w", err)
	}
	return k, true, nil
}

// UpdateAPIKey writes a key's mutable settings: name, scopes, account
// restriction, confirm scopes, limits, the anomaly switch and the suspension.
// The hash, the hint, the creation time and the revocation are not reachable
// from here — a revocation goes through RevokeAPIKey and is permanent.
func (tx *UserTx) UpdateAPIKey(ctx context.Context, k APIKey) error {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.api_keys
		   SET name = $3, scopes = $4, account_ids = $5, confirm_scopes = $6,
		       rate_per_min = $7, daily_budget = $8, suspend_on_anomaly = $9,
		       suspended_at = $10, suspended_reason = NULLIF($11, '')
		 WHERE user_id = $1 AND id = $2 AND revoked_at IS NULL`,
		tx.userID.String(), k.ID.String(), k.Name, k.Scopes, nullableULIDs(k.AccountIDs),
		nonNilStrings(k.ConfirmScopes), k.RatePerMin, k.DailyBudget, k.SuspendOnAnomaly,
		k.SuspendedAt, k.SuspendedReason)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrAPIKeyNameTaken
		}
		return fmt.Errorf("store: update API key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAPIKeyNotFound
	}
	return nil
}

// RevokeAPIKey revokes a key for good. A key already revoked reports found
// without changing its revocation time.
func (tx *UserTx) RevokeAPIKey(ctx context.Context, id ulid.ULID) (found bool, err error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.api_keys SET revoked_at = COALESCE(revoked_at, now())
		 WHERE user_id = $1 AND id = $2`, tx.userID.String(), id.String())
	if err != nil {
		return false, fmt.Errorf("store: revoke API key: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SuspendAPIKey suspends an active key for an anomaly rule (ADR-042 §6; every
// rule but off hours since #49) and records it in app.audit_log
// (api_key.suspend, actor 'system', with the rule and the reason), in the
// caller's transaction. It leaves an already suspended or revoked key as it is
// and reports whether THIS call suspended it, so a caller holding a stale copy
// of the key cannot suspend, audit or alert twice.
func (tx *UserTx) SuspendAPIKey(ctx context.Context, id ulid.ULID, rule, reason string) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.api_keys SET suspended_at = now(), suspended_reason = $3
		 WHERE user_id = $1 AND id = $2 AND suspended_at IS NULL AND revoked_at IS NULL`,
		tx.userID.String(), id.String(), reason)
	if err != nil {
		return false, fmt.Errorf("store: suspend API key: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	b, err := json.Marshal(map[string]any{"rule": rule, "reason": reason})
	if err != nil {
		return false, fmt.Errorf("store: audit API key suspension: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
		VALUES ($1, 'system', 'api_key.suspend', 'ApiKey', $2, $3)`,
		tx.userID.String(), id.String(), b); err != nil {
		return false, fmt.Errorf("store: audit API key suspension: %w", err)
	}
	return true, nil
}

// TouchAPIKeyUsed stamps last_used_at, at most once a minute so a busy key does
// not rewrite its row on every call.
func (tx *UserTx) TouchAPIKeyUsed(ctx context.Context, id ulid.ULID) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.api_keys SET last_used_at = now()
		 WHERE user_id = $1 AND id = $2
		   AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`,
		tx.userID.String(), id.String()); err != nil {
		return fmt.Errorf("store: touch API key: %w", err)
	}
	return nil
}

// AuditAPIKeyLifecycle records a session's create, update or revoke of a key
// in app.audit_log (the append-only log every other lifecycle event uses).
// detail carries settings, never the token.
func (tx *UserTx) AuditAPIKeyLifecycle(ctx context.Context, action string, keyID ulid.ULID, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("store: audit API key: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
		VALUES ($1, 'user', $2, 'ApiKey', $3, $4)`,
		tx.userID.String(), action, keyID.String(), b); err != nil {
		return fmt.Errorf("store: audit API key: %w", err)
	}
	return nil
}

// ResolveAPIKeyToken maps the SHA-256 of a presented bearer token to the user
// and key it belongs to, through app.api_key_for_token — the one narrow
// cross-user read (ADR-042 §2), run in a SystemTx because no identity exists
// yet. ok is false for an unknown, revoked or suspended key and for a user who
// can no longer sign in; the caller must not distinguish them.
func (s *Store) ResolveAPIKeyToken(ctx context.Context, hash [32]byte) (userID, keyID ulid.ULID, ok bool, err error) {
	err = s.SystemTx(ctx, func(tx *SystemTx) error {
		var u, k string
		qerr := tx.tx.QueryRow(ctx, `SELECT user_id, key_id FROM app.api_key_for_token($1)`, hash[:]).Scan(&u, &k)
		if qerr != nil {
			if isNoRows(qerr) {
				return nil
			}
			return fmt.Errorf("store: resolve API key: %w", qerr)
		}
		var perr error
		if userID, perr = ulid.Parse(u); perr != nil {
			return fmt.Errorf("store: parse user id: %w", perr)
		}
		if keyID, perr = ulid.Parse(k); perr != nil {
			return fmt.Errorf("store: parse key id: %w", perr)
		}
		ok = true
		return nil
	})
	if err != nil {
		return ulid.ULID{}, ulid.ULID{}, false, err
	}
	return userID, keyID, ok, nil
}

// ---- events ----------------------------------------------------------------

// Outcomes of app.api_key_events (the CHECK constraint's closed set).
const (
	KeyOutcomeOK           = "ok"
	KeyOutcomeDenied       = "denied"
	KeyOutcomeRateLimited  = "rate_limited"
	KeyOutcomeOverBudget   = "over_budget"
	KeyOutcomePending      = "pending"
	KeyOutcomeApproved     = "approved"
	KeyOutcomeDeniedByUser = "denied_by_user"
	KeyOutcomeExpired      = "expired"
	KeyOutcomeAnomaly      = "anomaly"
)

// Anomaly rules (app.api_key_events.anomaly).
const (
	AnomalyMassDelete = "mass_delete"
	AnomalyMassSend   = "mass_send"
	AnomalyOffHours   = "off_hours"
	// AnomalyCriticalRefusals: a key went past its per-minute cap of
	// security-meaningful refusals (#49 review).
	AnomalyCriticalRefusals = "critical_refusals"
)

// APIKeyEvent is one audit row. Summary holds ids and counts, never content.
type APIKeyEvent struct {
	ID        ulid.ULID
	KeyID     ulid.ULID
	At        time.Time
	Method    string
	Outcome   string
	Cost      int
	Deletes   int
	Sends     int
	Summary   map[string]any
	ClientIP  string
	PendingID *ulid.ULID
	ErrorType string
	Anomaly   string
	// Reason is why a refused call was refused (RefusalReason*), "" otherwise.
	Reason string
}

// Refusal reasons: the server's fixed list (the reason column's CHECK).
const (
	RefusalMethod      = "method"       // a method not opened to keys
	RefusalSessionOnly = "session_only" // a method that is a person's alone
	RefusalRestriction = "restriction"  // an account-restricted key, outside its accounts
	RefusalArguments   = "arguments"
	RefusalScope       = "scope"
	RefusalRate        = "rate"
	RefusalBudget      = "budget"
	RefusalApproval    = "approval" // an approval already run or no longer approved
	RefusalMassDelete  = "mass_delete"
	RefusalMassSend    = "mass_send"
	// RefusalPendingCap: the key already has its maximum of requests waiting
	// for a person (#852). Routine, not critical: it is a full queue, not an
	// attempt at something the key may not do.
	RefusalPendingCap = "pending_cap"
)

// CriticalRefusalReasons is THE list of security-meaningful refusals (#49
// review). They are written in full under a cap of their own, larger than the
// routine one, and crossing it is an anomaly. The usage query counts by it.
var CriticalRefusalReasons = []string{RefusalSessionOnly, RefusalRestriction, RefusalApproval,
	RefusalMassDelete, RefusalMassSend}

// IsCriticalRefusal reports whether reason is in CriticalRefusalReasons.
func IsCriticalRefusal(reason string) bool { return slices.Contains(CriticalRefusalReasons, reason) }

// InsertAPIKeyEvent appends an audit row and returns its id.
func (tx *UserTx) InsertAPIKeyEvent(ctx context.Context, e APIKeyEvent) (ulid.ULID, error) {
	id := NewULID()
	summary := e.Summary
	if summary == nil {
		summary = map[string]any{}
	}
	b, err := json.Marshal(summary)
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("store: API key event summary: %w", err)
	}
	var ip any
	if addr, perr := netip.ParseAddr(e.ClientIP); perr == nil {
		ip = addr.String()
	}
	var pending any
	if e.PendingID != nil {
		pending = e.PendingID.String()
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.api_key_events (id, user_id, key_id, method, outcome, cost, deletes, sends,
		                                summary, client_ip, pending_id, error_type, anomaly, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::inet, $11, NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, ''))`,
		id.String(), tx.userID.String(), e.KeyID.String(), e.Method, e.Outcome, e.Cost, e.Deletes,
		e.Sends, b, ip, pending, e.ErrorType, e.Anomaly, e.Reason); err != nil {
		return ulid.ULID{}, fmt.Errorf("store: insert API key event: %w", err)
	}
	return id, nil
}

// SetAPIKeyEventError records the error a call that was admitted before it ran
// ended with. It is the only change ever made to an event row (a column-level
// grant, migration 00074).
func (tx *UserTx) SetAPIKeyEventError(ctx context.Context, id ulid.ULID, errType string) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.api_key_events SET error_type = $3 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String(), errType); err != nil {
		return fmt.Errorf("store: API key event error: %w", err)
	}
	return nil
}

// APIKeyUsage is what a key has spent, by the windows the limits and the
// anomaly rules read. Only ADMITTED calls count (ok, pending, approved): a
// refused call cost nothing, so it must not keep an agent locked out.
type APIKeyUsage struct {
	CostLastMinute  int
	CostLast24h     int
	DeletesInWindow int
	SendsInWindow   int
	// RoutineRefusalsLastMinute and CriticalRefusalsLastMinute count the
	// refused calls WRITTEN in full in the last minute, split by
	// CriticalRefusalReasons, for their two caps.
	RoutineRefusalsLastMinute  int
	CriticalRefusalsLastMinute int
}

// APIKeyUsageAt sums a key's admitted calls. deleteWindow and sendWindow are
// the anomaly windows (ADR-042 §6).
//
// The per-minute sum also counts REFUSED calls (denied, rate_limited,
// over_budget), each written at cost 1 (#49 review): a refusal is not free, so
// a key looping on refused calls spends its own rate. The 24-hour budget and the
// delete and send windows count admitted calls only.
func (tx *UserTx) APIKeyUsageAt(ctx context.Context, keyID ulid.ULID, now time.Time, deleteWindow, sendWindow time.Duration) (APIKeyUsage, error) {
	var u APIKeyUsage
	var suppressed int
	err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(sum(cost) FILTER (WHERE at > $3::timestamptz - interval '1 minute'), 0),
		       COALESCE(sum(cost) FILTER (WHERE outcome IN ('ok', 'pending', 'approved')), 0),
		       COALESCE(sum(deletes) FILTER (WHERE outcome IN ('ok', 'pending', 'approved')
		                                       AND at > $3::timestamptz - $4::float8 * interval '1 second'), 0),
		       COALESCE(sum(sends) FILTER (WHERE outcome IN ('ok', 'pending', 'approved')
		                                     AND at > $3::timestamptz - $5::float8 * interval '1 second'), 0),
		       count(*) FILTER (WHERE outcome IN ('denied', 'rate_limited', 'over_budget')
		                          AND at > $3::timestamptz - interval '1 minute'
		                          AND NOT (COALESCE(reason, '') = ANY($6::text[]))),
		       count(*) FILTER (WHERE outcome IN ('denied', 'rate_limited', 'over_budget')
		                          AND at > $3::timestamptz - interval '1 minute'
		                          AND reason = ANY($6::text[])),
		       (SELECT COALESCE(sum(count), 0) FROM app.api_key_suppressed_refusals
		         WHERE user_id = $1 AND key_id = $2
		           AND minute >= date_trunc('minute', $3::timestamptz - interval '1 minute'))
		  FROM app.api_key_events
		 WHERE user_id = $1 AND key_id = $2
		   AND at > $3::timestamptz - interval '24 hours'
		   AND outcome IN ('ok', 'pending', 'approved', 'denied', 'rate_limited', 'over_budget')`,
		tx.userID.String(), keyID.String(), now, deleteWindow.Seconds(), sendWindow.Seconds(), CriticalRefusalReasons).
		Scan(&u.CostLastMinute, &u.CostLast24h, &u.DeletesInWindow, &u.SendsInWindow,
			&u.RoutineRefusalsLastMinute, &u.CriticalRefusalsLastMinute, &suppressed)
	if err != nil {
		return APIKeyUsage{}, fmt.Errorf("store: API key usage: %w", err)
	}
	// Suppressed refusals cost 1 each too. They are bucketed by minute, so the
	// window is the current and the previous minute: a slight over-count, in
	// the direction of throttling.
	u.CostLastMinute += suppressed
	return u, nil
}

// SuppressedRefusal is a count of routine refusals by one key in one minute
// that were not written one by one (app.api_key_suppressed_refusals).
type SuppressedRefusal struct {
	KeyID   ulid.ULID
	Minute  time.Time
	Outcome string
	Method  string
	Reason  string
	Count   int
}

// RecordSuppressedRefusal counts one routine refusal past the key's
// per-minute cap in this minute's row for (outcome, method, reason).
func (tx *UserTx) RecordSuppressedRefusal(ctx context.Context, keyID ulid.ULID, outcome, method, reason string) error {
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.api_key_suppressed_refusals (user_id, key_id, minute, outcome, method, reason, count)
		VALUES ($1, $2, date_trunc('minute', now()), $3, $4, $5, 1)
		ON CONFLICT (user_id, key_id, minute, outcome, method, reason)
		DO UPDATE SET count = app.api_key_suppressed_refusals.count + 1`,
		tx.userID.String(), keyID.String(), outcome, method, reason); err != nil {
		return fmt.Errorf("store: count suppressed refusal: %w", err)
	}
	return nil
}

// ListSuppressedRefusals returns a key's (or every key's, keyID nil)
// suppressed-refusal counts, newest minute first, at most limit.
func (tx *UserTx) ListSuppressedRefusals(ctx context.Context, keyID *ulid.ULID, limit int) ([]SuppressedRefusal, error) {
	var key any
	if keyID != nil {
		key = keyID.String()
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT key_id, minute, outcome, method, reason, count
		  FROM app.api_key_suppressed_refusals
		 WHERE user_id = $1 AND ($2::text IS NULL OR key_id = $2)
		 ORDER BY minute DESC, key_id, outcome, method, reason
		 LIMIT $3`, tx.userID.String(), key, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list suppressed refusals: %w", err)
	}
	defer rows.Close()
	var out []SuppressedRefusal
	for rows.Next() {
		var r SuppressedRefusal
		var kid string
		if err := rows.Scan(&kid, &r.Minute, &r.Outcome, &r.Method, &r.Reason, &r.Count); err != nil {
			return nil, fmt.Errorf("store: scan suppressed refusal: %w", err)
		}
		if r.KeyID, err = ulid.Parse(kid); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// APIKeyAnomalySince reports whether an anomaly of this rule was already
// recorded for the key since the given time (so one episode alerts once).
func (tx *UserTx) APIKeyAnomalySince(ctx context.Context, keyID ulid.ULID, rule string, since time.Time) (bool, error) {
	var exists bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.api_key_events
		                WHERE user_id = $1 AND key_id = $2 AND outcome = 'anomaly'
		                  AND anomaly = $3 AND at >= $4)`,
		tx.userID.String(), keyID.String(), rule, since).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: API key anomaly lookup: %w", err)
	}
	return exists, nil
}

// ListAPIKeyEvents returns a key's (or every key's, keyID nil) audit rows,
// newest first, strictly older than before when it is set, at most limit.
func (tx *UserTx) ListAPIKeyEvents(ctx context.Context, keyID *ulid.ULID, before *ulid.ULID, limit int) ([]APIKeyEvent, error) {
	var key, cursor any
	if keyID != nil {
		key = keyID.String()
	}
	if before != nil {
		cursor = before.String()
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, key_id, at, method, outcome, cost, deletes, sends, summary,
		       COALESCE(host(client_ip), ''), pending_id, COALESCE(error_type, ''), COALESCE(anomaly, '')
		  FROM app.api_key_events
		 WHERE user_id = $1 AND ($2::text IS NULL OR key_id = $2)
		   AND ($3::text IS NULL OR id < $3)
		 ORDER BY id DESC
		 LIMIT $4`,
		tx.userID.String(), key, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list API key events: %w", err)
	}
	defer rows.Close()
	var out []APIKeyEvent
	for rows.Next() {
		var (
			e          APIKeyEvent
			id, kid    string
			pending    *string
			summaryRaw []byte
		)
		if err := rows.Scan(&id, &kid, &e.At, &e.Method, &e.Outcome, &e.Cost, &e.Deletes, &e.Sends,
			&summaryRaw, &e.ClientIP, &pending, &e.ErrorType, &e.Anomaly); err != nil {
			return nil, fmt.Errorf("store: scan API key event: %w", err)
		}
		if e.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse event id: %w", err)
		}
		if e.KeyID, err = ulid.Parse(kid); err != nil {
			return nil, fmt.Errorf("store: parse event key id: %w", err)
		}
		if pending != nil {
			p, perr := ulid.Parse(*pending)
			if perr != nil {
				return nil, fmt.Errorf("store: parse event pending id: %w", perr)
			}
			e.PendingID = &p
		}
		if err := json.Unmarshal(summaryRaw, &e.Summary); err != nil {
			return nil, fmt.Errorf("store: decode event summary: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list API key events: %w", err)
	}
	return out, nil
}

// ---- pending approvals -------------------------------------------------------

// Pending-approval states (the CHECK constraint's closed set).
const (
	PendingStatePending  = "pending"
	PendingStateApproved = "approved"
	PendingStateDenied   = "denied"
	PendingStateExpired  = "expired"
	// PendingStateFailed is an approval whose re-validation or execution
	// refused it (the key was revoked, the draft changed, a limit was spent).
	PendingStateFailed = "failed"
)

// APIKeyPending is a destructive call held for a human (ADR-042 §7).
type APIKeyPending struct {
	ID        ulid.ULID
	KeyID     ulid.ULID
	Kind      string // "send" | "delete"
	Method    string
	Payload   json.RawMessage
	Summary   string
	CreatedAt time.Time
	ExpiresAt time.Time
	State     string
	DecidedAt *time.Time
	Detail    string
}

const pendingColumns = `id, key_id, kind, method, payload, summary, created_at, expires_at, state,
	decided_at, COALESCE(detail, '')`

func scanPending(row interface{ Scan(...any) error }) (APIKeyPending, error) {
	var (
		p       APIKeyPending
		id, kid string
		payload []byte
	)
	if err := row.Scan(&id, &kid, &p.Kind, &p.Method, &payload, &p.Summary, &p.CreatedAt,
		&p.ExpiresAt, &p.State, &p.DecidedAt, &p.Detail); err != nil {
		return APIKeyPending{}, err
	}
	var err error
	if p.ID, err = ulid.Parse(id); err != nil {
		return APIKeyPending{}, fmt.Errorf("store: parse pending id: %w", err)
	}
	if p.KeyID, err = ulid.Parse(kid); err != nil {
		return APIKeyPending{}, fmt.Errorf("store: parse pending key id: %w", err)
	}
	p.Payload = payload
	return p, nil
}

// CreateAPIKeyPending stores a held call and returns its id.
func (tx *UserTx) CreateAPIKeyPending(ctx context.Context, p APIKeyPending) (ulid.ULID, error) {
	id := NewULID()
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.api_key_pending (id, user_id, key_id, kind, method, payload, summary, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id.String(), tx.userID.String(), p.KeyID.String(), p.Kind, p.Method, []byte(p.Payload),
		p.Summary, p.ExpiresAt); err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create pending approval: %w", err)
	}
	return id, nil
}

// PendingFilter narrows ListAPIKeyPending. Zero values mean "any".
type PendingFilter struct {
	KeyID  *ulid.ULID
	IDs    []ulid.ULID
	States []string
}

// ListAPIKeyPending returns the caller's held calls, newest first, at most limit.
func (tx *UserTx) ListAPIKeyPending(ctx context.Context, f PendingFilter, limit int) ([]APIKeyPending, error) {
	var key, ids, states any
	if f.KeyID != nil {
		key = f.KeyID.String()
	}
	if f.IDs != nil {
		ids = ulidStrings(f.IDs)
	}
	if f.States != nil {
		states = f.States
	}
	rows, err := tx.tx.Query(ctx, `SELECT `+pendingColumns+`
		  FROM app.api_key_pending
		 WHERE user_id = $1
		   AND ($2::text IS NULL OR key_id = $2)
		   AND ($3::text[] IS NULL OR id = ANY($3))
		   AND ($4::text[] IS NULL OR state = ANY($4))
		 ORDER BY created_at DESC, id DESC
		 LIMIT $5`,
		tx.userID.String(), key, ids, states, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list pending approvals: %w", err)
	}
	defer rows.Close()
	var out []APIKeyPending
	for rows.Next() {
		p, err := scanPending(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan pending approval: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list pending approvals: %w", err)
	}
	return out, nil
}

// LockAPIKeyPending reads one held call FOR UPDATE, so two approvals of the
// same request cannot both execute it.
func (tx *UserTx) LockAPIKeyPending(ctx context.Context, id ulid.ULID) (APIKeyPending, bool, error) {
	p, err := scanPending(tx.tx.QueryRow(ctx, `SELECT `+pendingColumns+`
		  FROM app.api_key_pending WHERE user_id = $1 AND id = $2 FOR UPDATE`,
		tx.userID.String(), id.String()))
	if err != nil {
		if isNoRows(err) {
			return APIKeyPending{}, false, nil
		}
		return APIKeyPending{}, false, fmt.Errorf("store: lock pending approval: %w", err)
	}
	return p, true, nil
}

// DecideAPIKeyPending moves a held call out of 'pending'. It reports false when
// the request was no longer pending (decided or expired by someone else).
func (tx *UserTx) DecideAPIKeyPending(ctx context.Context, id ulid.ULID, state, detail string) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.api_key_pending SET state = $3, decided_at = now(), detail = NULLIF($4, '')
		 WHERE user_id = $1 AND id = $2 AND state = 'pending'`,
		tx.userID.String(), id.String(), state, detail)
	if err != nil {
		return false, fmt.Errorf("store: decide pending approval: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FailApprovedAPIKeyPending records that an approved request was refused when
// it ran (a limit spent, a scope narrowed in between). Only an 'approved' row
// moves; the decision time stays the approval's.
func (tx *UserTx) FailApprovedAPIKeyPending(ctx context.Context, id ulid.ULID, detail string) error {
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.api_key_pending SET state = 'failed', detail = $3
		 WHERE user_id = $1 AND id = $2 AND state = 'approved'`,
		tx.userID.String(), id.String(), detail); err != nil {
		return fmt.Errorf("store: fail approved request: %w", err)
	}
	return nil
}

// ExpireAPIKeyPending moves every pending request of the caller whose deadline
// has passed to 'expired' and returns them, so the caller can audit each one.
func (tx *UserTx) ExpireAPIKeyPending(ctx context.Context, now time.Time) ([]APIKeyPending, error) {
	rows, err := tx.tx.Query(ctx, `
		UPDATE app.api_key_pending SET state = 'expired', decided_at = now(),
		       detail = 'not answered within the approval window'
		 WHERE user_id = $1 AND state = 'pending' AND expires_at <= $2
		 RETURNING `+pendingColumns,
		tx.userID.String(), now)
	if err != nil {
		return nil, fmt.Errorf("store: expire pending approvals: %w", err)
	}
	defer rows.Close()
	var out []APIKeyPending
	for rows.Next() {
		p, err := scanPending(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan expired approval: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: expire pending approvals: %w", err)
	}
	return out, nil
}

// UsersWithDuePending lists the users with a pending request past its
// deadline, through app.api_key_pending_due_users (ids only). The sweep then
// expires each user's requests in that user's own UserTx.
func (s *Store) UsersWithDuePending(ctx context.Context) ([]ulid.ULID, error) {
	var out []ulid.ULID
	err := s.SystemTx(ctx, func(tx *SystemTx) error {
		rows, err := tx.tx.Query(ctx, `SELECT app.api_key_pending_due_users()`)
		if err != nil {
			return fmt.Errorf("store: due pending approvals: %w", err)
		}
		defer rows.Close()
		var raw []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("store: scan due user: %w", err)
			}
			raw = append(raw, id)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: due pending approvals: %w", err)
		}
		out, err = parseULIDs(raw)
		return err
	})
	return out, err
}

// ---- the account restriction -------------------------------------------------

// keyAccountsSQL is THE definition of "this message is in one of these
// accounts" for an API key's account restriction (ADR-042 §5): a LIVE
// provenance row in one of them — EmailQuery.InAccount's own definition — or,
// for a message that exists only here so far (a draft not yet appended), the
// local mailbox it displays in belonging to one of them. alias is the
// app.messages alias; arr is the placeholder of a text[] of account ids.
//
// Email/query (EmailQuery.RestrictAccounts) and Email/get / Email/set
// (MessagesInAccounts) both use it, so what a key can find and what it can
// fetch or change cannot drift apart.
//
// WHERE THE MESSAGE IS NOW DECIDES (#49 review). A message the user moved
// carries moved_mailbox_id, the canonical placement that overrides provenance
// (mailboxMembershipSQL's first arm); until writeback catches up its live
// provenance still names the OLD account. Reading provenance first let a key
// restricted to the old account keep seeing, and acting on, mail moved out of
// it, and hid the mail from a key restricted to the new one.
func keyAccountsSQL(alias, arr string) string {
	return `(CASE WHEN ` + alias + `.moved_mailbox_id IS NOT NULL
	   THEN EXISTS (SELECT 1 FROM app.mailboxes kmv
	          WHERE kmv.user_id = ` + alias + `.user_id AND kmv.id = ` + alias + `.moved_mailbox_id
	            AND kmv.account_id = ANY(` + arr + `))
	   ELSE (EXISTS (SELECT 1 FROM app.message_provenance kp
	          WHERE kp.user_id = ` + alias + `.user_id AND kp.message_id = ` + alias + `.id
	            AND kp.state = 'live' AND kp.account_id = ANY(` + arr + `))
	     OR EXISTS (SELECT 1 FROM app.mailboxes kmb
	          WHERE kmb.user_id = ` + alias + `.user_id AND kmb.id = ` + alias + `.local_mailbox_id
	            AND kmb.account_id = ANY(` + arr + `))) END)`
}

// MessagesInAccounts reports which of ids are in one of accounts (keyAccountsSQL).
// An id that is absent, another user's, or in another account is simply not in
// the result.
func (tx *UserTx) MessagesInAccounts(ctx context.Context, ids, accounts []ulid.ULID) (map[ulid.ULID]bool, error) {
	out := map[ulid.ULID]bool{}
	if len(ids) == 0 || len(accounts) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id FROM app.messages m
		 WHERE m.user_id = $1 AND m.id = ANY($2) AND `+keyAccountsSQL("m", "$3"),
		tx.userID.String(), ulidStrings(ids), ulidStrings(accounts))
	if err != nil {
		return nil, fmt.Errorf("store: messages in accounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan message in accounts: %w", err)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id: %w", err)
		}
		out[u] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: messages in accounts: %w", err)
	}
	return out, nil
}

// RecordAPIKeyDraft remembers that a key wrote this draft (app.api_key_drafts),
// with the keyed fingerprint of what it asked for (nil records none; such a
// draft is never collapsed into, see RecentAPIKeyDraft).
func (tx *UserTx) RecordAPIKeyDraft(ctx context.Context, keyID, messageID ulid.ULID, fingerprint []byte) error {
	var fp any
	if len(fingerprint) > 0 {
		fp = fingerprint
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.api_key_drafts (user_id, message_id, key_id, content_fingerprint) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, message_id) DO NOTHING`,
		tx.userID.String(), messageID.String(), keyID.String(), fp); err != nil {
		return fmt.Errorf("store: record key draft: %w", err)
	}
	return nil
}

// IsAPIKeyDraft reports whether this key wrote this message.
func (tx *UserTx) IsAPIKeyDraft(ctx context.Context, keyID, messageID ulid.ULID) (bool, error) {
	var ok bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.api_key_drafts
		                WHERE user_id = $1 AND message_id = $2 AND key_id = $3)`,
		tx.userID.String(), messageID.String(), keyID.String()).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: key draft lookup: %w", err)
	}
	return ok, nil
}

// LockAndCountUnrevokedAPIKeys counts the user's keys that are not revoked
// (suspended ones included), holding a per-user transaction lock so two
// concurrent creates cannot both pass a cap checked against the count.
func (tx *UserTx) LockAndCountUnrevokedAPIKeys(ctx context.Context) (int, error) {
	if _, err := tx.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"api_key_create:"+tx.userID.String()); err != nil {
		return 0, fmt.Errorf("store: lock key creation: %w", err)
	}
	var n int
	if err := tx.tx.QueryRow(ctx, `SELECT count(*) FROM app.api_keys WHERE user_id = $1 AND revoked_at IS NULL`,
		tx.userID.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count keys: %w", err)
	}
	return n, nil
}

// MarkAPIKeyPendingRan records that an approved request was admitted to run.
// Called in the admission transaction that writes its 'approved' audit row.
// ran is false when no row changed — the request is not approved, or it has
// already run (or the sweep failed it) — and the caller must then not run it:
// one approval is one run (#49 review).
func (tx *UserTx) MarkAPIKeyPendingRan(ctx context.Context, id ulid.ULID) (ran bool, err error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.api_key_pending SET ran_at = now()
		 WHERE user_id = $1 AND id = $2 AND state = 'approved' AND ran_at IS NULL`,
		tx.userID.String(), id.String())
	if err != nil {
		return false, fmt.Errorf("store: mark approved request ran: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// StuckApprovalAge is how long an approved request may go without being
// admitted to run before the sweep calls it failed (ADR-042 §7).
const StuckApprovalAge = 10 * time.Minute

// FailStuckAPIKeyPending marks approved requests that never ran as failed and
// returns them: the process stopped between the decision and the run. Aged by
// the DATABASE clock, like the decision time it compares and like the sweep's
// definer that finds these users.
func (tx *UserTx) FailStuckAPIKeyPending(ctx context.Context) ([]APIKeyPending, error) {
	rows, err := tx.tx.Query(ctx, `
		UPDATE app.api_key_pending SET state = 'failed',
		       detail = 'approved, but the server stopped before it ran; nothing was done'
		 WHERE user_id = $1 AND state = 'approved' AND ran_at IS NULL
		   AND decided_at <= now() - $2::float8 * interval '1 second'
		 RETURNING `+pendingColumns,
		tx.userID.String(), StuckApprovalAge.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: fail stuck approvals: %w", err)
	}
	defer rows.Close()
	var out []APIKeyPending
	for rows.Next() {
		p, err := scanPending(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan stuck approval: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
