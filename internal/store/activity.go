// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The Activity view (ADR-053, #45): one merged, newest-first list of what
// happened to a user's mail and access, READ from records that already exist.
// It writes nothing, so it appends no change_log row and is not a synced
// object.
//
// ONE STATEMENT. Each branch reads one source for the caller, bounded by
// LIMIT n+1 before the UNION ALL, so the statement reads at most
// branches x (n+1) rows however large a source is. The outer query orders the
// union by (at, kind, id) descending, the same tuple the cursor carries, so
// entries that share a timestamp are neither skipped nor repeated.
//
// EVERY BRANCH CARRIES `user_id = $1` ITSELF. RLS would filter a branch that
// forgot, but the isolation suite proves each branch alone (ADR-053 probes),
// so a policy change cannot turn one omission into a leak.

// Activity sources (the filter groups) and kinds (the branches).
const (
	ActivitySourceAITool     = "ai_tool"
	ActivitySourceDecryption = "decryption"
	ActivitySourcePurge      = "purge"
	ActivitySourceSession    = "session"
	ActivitySourceBulk       = "bulk"

	ActivityKindAITool     = "ai_tool"
	ActivityKindAIKey      = "ai_key"
	ActivityKindDecryption = "decryption"
	ActivityKindPurge      = "purge"
	ActivityKindSignIn     = "sign_in"
	ActivityKindSignOut    = "sign_out"
	ActivityKindPlacement  = "placement"
	ActivityKindRuleApply  = "rule_apply"

	// The audit_log vocabulary of a rule bulk apply (ADR-053, gap 1).
	RuleApplyObjectType = "Rule"
	RuleApplyAction     = "rule.apply"
	RuleApplyUndoAction = "rule.apply_undo"
)

// ActivitySources is the closed list a filter may name, in display order.
var ActivitySources = []string{
	ActivitySourceAITool, ActivitySourceDecryption, ActivitySourcePurge,
	ActivitySourceSession, ActivitySourceBulk,
}

// Activity page bounds: Limit is 1..ActivityMaxLimit.
const (
	ActivityDefaultLimit = 50
	ActivityMaxLimit     = 100
)

// ErrActivityArgs is returned for a filter or cursor the view will not run.
var ErrActivityArgs = errors.New("store: invalid activity arguments")

// activityAIEventPredicate is the rows of api_key_events the view shows: a
// refusal or anything not plainly ok, a write, a delete or a send. A routine
// read stays on the per-key page. It MUST stay textually identical to the
// partial index in migration 00094 or the planner will not use the index.
const activityAIEventPredicate = `(e.outcome <> 'ok' OR e.method LIKE '%/set' OR e.deletes > 0 OR e.sends > 0)`

// ActivityCursor is the keyset position: the last entry of the previous page.
type ActivityCursor struct {
	At   time.Time
	Kind string
	ID   string
}

// Encode is the opaque form a client hands back.
func (c ActivityCursor) Encode() string {
	raw := strconv.FormatInt(c.At.UTC().UnixMicro(), 10) + "|" + c.Kind + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// kindSource maps a branch to its filter group.
var kindSource = map[string]string{
	ActivityKindAITool: ActivitySourceAITool, ActivityKindAIKey: ActivitySourceAITool,
	ActivityKindDecryption: ActivitySourceDecryption,
	ActivityKindPurge:      ActivitySourcePurge,
	ActivityKindSignIn:     ActivitySourceSession, ActivityKindSignOut: ActivitySourceSession,
	ActivityKindPlacement: ActivitySourceBulk, ActivityKindRuleApply: ActivitySourceBulk,
}

// ActivitySourceOf is the filter group of a kind ("" for an unknown kind).
func ActivitySourceOf(kind string) string { return kindSource[kind] }

// ParseActivityCursor decodes a cursor and refuses anything that is not one.
func ParseActivityCursor(s string) (ActivityCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ActivityCursor{}, fmt.Errorf("%w: not a cursor", ErrActivityArgs)
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 {
		return ActivityCursor{}, fmt.Errorf("%w: not a cursor", ErrActivityArgs)
	}
	us, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return ActivityCursor{}, fmt.Errorf("%w: not a cursor", ErrActivityArgs)
	}
	if _, ok := kindSource[parts[1]]; !ok || parts[2] == "" {
		return ActivityCursor{}, fmt.Errorf("%w: not a cursor", ErrActivityArgs)
	}
	return ActivityCursor{At: time.UnixMicro(us).UTC(), Kind: parts[1], ID: parts[2]}, nil
}

// ActivityFilter selects a page.
type ActivityFilter struct {
	// Sources limits the list to these filter groups; empty means all.
	Sources []string
	// Since is inclusive and Until exclusive, on the entry time; zero means open.
	Since, Until time.Time
	// After is the cursor of the last entry already seen.
	After *ActivityCursor
	// Limit is 1..ActivityMaxLimit; 0 takes ActivityDefaultLimit.
	Limit int
}

// ActivityEntry is one row of the merged list. Fields a kind does not have are
// empty. Nothing here is message content.
type ActivityEntry struct {
	// ID is unique within Kind (a ULID, or an audit row's number).
	ID   string
	Kind string
	At   time.Time
	// Outcome: an AI call's outcome, "decrypted"/"failed", or a session's end
	// reason.
	Outcome string
	// KeyName is the AI key's name (empty when the key no longer exists).
	KeyName string
	// Method is an AI call's JMAP method, or an AI-key audit action.
	Method string
	// Reason is why an AI call was refused, from the server's fixed list.
	Reason string
	// Purpose is "body" or "attachment" for a decryption.
	Purpose string
	// Fingerprint is the decrypting key's primary fingerprint.
	Fingerprint string
	// UserAgent is the session's raw user agent (the API layer describes it
	// and never returns it).
	UserAgent string
	// Count is how many things a bulk action touched (0 when not recorded).
	Count int
	// ObjectType and ObjectID name what the entry concerns ("" when nothing).
	ObjectType string
	ObjectID   string
	// ObjectExists is true only when that object is still there to link to.
	ObjectExists bool
}

// ActivityPage is one page and where it stops.
type ActivityPage struct {
	Entries []ActivityEntry
	// Next is the cursor of the following page; "" at the end.
	Next  string
	Limit int
	// Covers is, per selected source, the time of the oldest record that
	// source holds for this user (nil when it holds none): how far back the
	// list can go. A short-retention source (sessions) is the one that matters.
	Covers map[string]*time.Time
}

type actBuilder struct {
	args []any
}

func (b *actBuilder) param(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// timeCond is the date-range and cursor predicate for one branch. col is the
// time column, idExpr the branch's id expression as compared in its own order
// (a typed value for audit rows, text otherwise).
func (b *actBuilder) timeCond(f ActivityFilter, kind, col, idExpr string, audit bool) string {
	var c []string
	if !f.Since.IsZero() {
		c = append(c, col+" >= "+b.param(f.Since.UTC()))
	}
	if !f.Until.IsZero() {
		c = append(c, col+" < "+b.param(f.Until.UTC()))
	}
	if f.After != nil {
		at := b.param(f.After.At.UTC())
		switch {
		case kind < f.After.Kind: // this branch sorts below the cursor's kind: ties still follow it
			c = append(c, col+" <= "+at)
		case kind > f.After.Kind:
			c = append(c, col+" < "+at)
		default:
			var id string
			if audit {
				n, err := strconv.ParseInt(f.After.ID, 10, 64)
				if err != nil {
					n = -1
				}
				id = b.param(n)
			} else {
				id = b.param(f.After.ID)
			}
			c = append(c, "("+col+", "+idExpr+") < ("+at+", "+id+")")
		}
	}
	if len(c) == 0 {
		return ""
	}
	return " AND " + strings.Join(c, " AND ")
}

// activityBranch builds the SQL of one kind. Every branch returns the same
// columns: at, kind, id, outcome, key_name, method, reason, purpose,
// fingerprint, user_agent, cnt, object_type, object_id, object_exists.
func (b *actBuilder) activityBranch(kind string, f ActivityFilter, n string) string {
	switch kind {
	case ActivityKindAITool:
		return `(SELECT e.at AS at, 'ai_tool' AS kind, e.id::text AS id, e.outcome::text AS outcome,
			k.name::text AS key_name, e.method::text AS method, e.reason::text AS reason,
			NULL::text AS purpose, NULL::text AS fingerprint, NULL::text AS user_agent, NULL::int AS cnt,
			'ApiKey'::text AS object_type, e.key_id::text AS object_id, (k.id IS NOT NULL) AS object_exists
		   FROM app.api_key_events e
		   LEFT JOIN app.api_keys k ON k.id = e.key_id AND k.user_id = $1
		  WHERE e.user_id = $1 AND ` + activityAIEventPredicate +
			b.timeCond(f, kind, "e.at", "e.id", false) + `
		  ORDER BY e.at DESC, e.id DESC LIMIT ` + n + `)`
	case ActivityKindAIKey:
		return `(SELECT a.created_at AS at, 'ai_key' AS kind, lpad(a.id::text, 20, '0') AS id, NULL::text AS outcome,
			k.name::text AS key_name, a.action::text AS method, NULL::text AS reason,
			NULL::text AS purpose, NULL::text AS fingerprint, NULL::text AS user_agent, NULL::int AS cnt,
			'ApiKey'::text AS object_type, a.object_id::text AS object_id, (k.id IS NOT NULL) AS object_exists
		   FROM app.audit_log a
		   LEFT JOIN app.api_keys k ON k.user_id = $1 AND k.id::text = a.object_id
		  WHERE a.user_id = $1 AND a.object_type = 'ApiKey' AND a.action LIKE 'api\_key.%'` +
			b.timeCond(f, kind, "a.created_at", "a.id", true) + `
		  ORDER BY a.created_at DESC, a.id DESC LIMIT ` + n + `)`
	case ActivityKindDecryption:
		return `(SELECT d.decrypted_at AS at, 'decryption' AS kind, d.id::text AS id, d.outcome::text AS outcome,
			NULL::text AS key_name, NULL::text AS method, NULL::text AS reason,
			d.purpose::text AS purpose, d.key_fingerprint::text AS fingerprint, NULL::text AS user_agent, NULL::int AS cnt,
			'Email'::text AS object_type, d.message_id::text AS object_id,
			EXISTS (SELECT 1 FROM app.messages m WHERE m.user_id = $1 AND m.id = d.message_id) AS object_exists
		   FROM app.pgp_decrypt_audit d
		  WHERE d.user_id = $1` +
			b.timeCond(f, kind, "d.decrypted_at", "d.id", false) + `
		  ORDER BY d.decrypted_at DESC, d.id DESC LIMIT ` + n + `)`
	case ActivityKindPurge:
		return `(SELECT a.created_at AS at, 'purge' AS kind, lpad(a.id::text, 20, '0') AS id, NULL::text AS outcome,
			NULL::text AS key_name, NULL::text AS method, NULL::text AS reason,
			NULL::text AS purpose, NULL::text AS fingerprint, NULL::text AS user_agent, NULL::int AS cnt,
			'Email'::text AS object_type, a.object_id::text AS object_id, false AS object_exists
		   FROM app.audit_log a
		  WHERE a.user_id = $1 AND a.object_type = 'Email' AND a.action = '` + PurgeAction + `'` +
			b.timeCond(f, kind, "a.created_at", "a.id", true) + `
		  ORDER BY a.created_at DESC, a.id DESC LIMIT ` + n + `)`
	case ActivityKindSignIn:
		return `(SELECT s.created_at AS at, 'sign_in' AS kind, s.id::text AS id, NULL::text AS outcome,
			NULL::text AS key_name, NULL::text AS method, NULL::text AS reason,
			NULL::text AS purpose, NULL::text AS fingerprint, s.user_agent::text AS user_agent, NULL::int AS cnt,
			NULL::text AS object_type, NULL::text AS object_id, false AS object_exists
		   FROM app.sessions s
		  WHERE s.user_id = $1` +
			b.timeCond(f, kind, "s.created_at", "s.id", false) + `
		  ORDER BY s.created_at DESC, s.id DESC LIMIT ` + n + `)`
	case ActivityKindSignOut:
		// 'replaced' is a new sign-in taking an old session's place: noise.
		return `(SELECT s.revoked_at AS at, 'sign_out' AS kind, s.id::text AS id, s.revoked_reason::text AS outcome,
			NULL::text AS key_name, NULL::text AS method, NULL::text AS reason,
			NULL::text AS purpose, NULL::text AS fingerprint, s.user_agent::text AS user_agent, NULL::int AS cnt,
			NULL::text AS object_type, NULL::text AS object_id, false AS object_exists
		   FROM app.sessions s
		  WHERE s.user_id = $1 AND s.revoked_at IS NOT NULL AND s.revoked_reason <> 'replaced'` +
			b.timeCond(f, kind, "s.revoked_at", "s.id", false) + `
		  ORDER BY s.revoked_at DESC, s.id DESC LIMIT ` + n + `)`
	case ActivityKindPlacement:
		// One entry per batch and direction (a backfill writes one audit row per
		// message moved). Reads that user's backfill rows, which are operator-run
		// and few; the group is bounded by LIMIT like every other branch.
		cond := b.timeCond(f, kind, "g.at", "g.bid", false)
		if cond != "" {
			cond = " WHERE true" + cond
		}
		return `(SELECT g.at AS at, 'placement' AS kind, g.bid AS id, g.action AS outcome,
			NULL::text AS key_name, NULL::text AS method, NULL::text AS reason,
			NULL::text AS purpose, NULL::text AS fingerprint, NULL::text AS user_agent, g.cnt AS cnt,
			NULL::text AS object_type, NULL::text AS object_id, false AS object_exists
		   FROM (SELECT max(a.created_at) AS at, a.object_id::text AS bid, a.action::text AS action, count(*)::int AS cnt
		           FROM app.audit_log a
		          WHERE a.user_id = $1 AND a.object_type = '` + backfillObjectType + `'
		            AND a.action IN ('` + backfillActionMove + `', '` + backfillActionUndo + `')
		          GROUP BY a.object_id, a.action) g` + cond + `
		  ORDER BY g.at DESC, g.bid DESC LIMIT ` + n + `)`
	case ActivityKindRuleApply:
		return `(SELECT a.created_at AS at, 'rule_apply' AS kind, lpad(a.id::text, 20, '0') AS id, a.action::text AS outcome,
			r.name::text AS key_name, NULL::text AS method, NULL::text AS reason,
			NULL::text AS purpose, NULL::text AS fingerprint, NULL::text AS user_agent,
			COALESCE((a.detail->>'changed')::int, 0) AS cnt,
			'Rule'::text AS object_type, a.object_id::text AS object_id, (r.id IS NOT NULL) AS object_exists
		   FROM app.audit_log a
		   LEFT JOIN app.rules r ON r.user_id = $1 AND r.id::text = a.object_id
		  WHERE a.user_id = $1 AND a.object_type = '` + RuleApplyObjectType + `'
		    AND a.action IN ('` + RuleApplyAction + `', '` + RuleApplyUndoAction + `')` +
			b.timeCond(f, kind, "a.created_at", "a.id", true) + `
		  ORDER BY a.created_at DESC, a.id DESC LIMIT ` + n + `)`
	}
	return ""
}

// kindsFor lists the branches the filter selects, in a fixed order.
func kindsFor(sources []string) ([]string, error) {
	want := map[string]bool{}
	for _, s := range sources {
		ok := false
		for _, k := range ActivitySources {
			if s == k {
				ok = true
			}
		}
		if !ok {
			return nil, fmt.Errorf("%w: unknown source %q", ErrActivityArgs, s)
		}
		want[s] = true
	}
	var kinds []string
	for k, src := range kindSource {
		if len(want) == 0 || want[src] {
			kinds = append(kinds, k)
		}
	}
	sort.Strings(kinds)
	return kinds, nil
}

// activityQuery is the whole statement for a page: the selected branches, each
// bounded and carrying its own user predicate, unioned and ordered by the
// cursor's tuple. Split from ListActivity so a test can run it on a connection
// that RLS does not filter and prove each branch scopes by user on its own.
func activityQuery(userID string, kinds []string, f ActivityFilter, limit int) (string, []any) {
	b := &actBuilder{args: []any{userID}}
	n := strconv.Itoa(limit + 1)
	branches := make([]string, 0, len(kinds))
	for _, k := range kinds {
		branches = append(branches, b.activityBranch(k, f, n))
	}
	q := `SELECT * FROM (` + strings.Join(branches, "\nUNION ALL\n") + `) u
		ORDER BY at DESC, kind COLLATE "C" DESC, id COLLATE "C" DESC
		LIMIT ` + n
	return q, b.args
}

// ListActivity returns one page of the caller's merged activity, newest first.
func (tx *UserTx) ListActivity(ctx context.Context, f ActivityFilter) (ActivityPage, error) {
	limit := f.Limit
	if limit == 0 {
		limit = ActivityDefaultLimit
	}
	if limit < 1 || limit > ActivityMaxLimit {
		return ActivityPage{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrActivityArgs, ActivityMaxLimit)
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Since.Before(f.Until) {
		return ActivityPage{}, fmt.Errorf("%w: since must be before until", ErrActivityArgs)
	}
	kinds, err := kindsFor(f.Sources)
	if err != nil {
		return ActivityPage{}, err
	}
	if f.After != nil {
		if _, ok := kindSource[f.After.Kind]; !ok {
			return ActivityPage{}, fmt.Errorf("%w: not a cursor", ErrActivityArgs)
		}
	}

	q, qargs := activityQuery(tx.userID.String(), kinds, f, limit)
	rows, err := tx.tx.Query(ctx, q, qargs...)
	if err != nil {
		return ActivityPage{}, fmt.Errorf("store: list activity: %w", err)
	}
	defer rows.Close()
	page := ActivityPage{Limit: limit, Entries: []ActivityEntry{}}
	for rows.Next() {
		var (
			e                                                  ActivityEntry
			outcome, keyName, method, reason, purpose, fpr, ua *string
			cnt                                                *int
			objType, objID                                     *string
		)
		if err := rows.Scan(&e.At, &e.Kind, &e.ID, &outcome, &keyName, &method, &reason, &purpose, &fpr, &ua,
			&cnt, &objType, &objID, &e.ObjectExists); err != nil {
			return ActivityPage{}, fmt.Errorf("store: scan activity: %w", err)
		}
		deref := func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		}
		e.Outcome, e.KeyName, e.Method, e.Reason = deref(outcome), deref(keyName), deref(method), deref(reason)
		e.Purpose, e.Fingerprint, e.UserAgent = deref(purpose), deref(fpr), deref(ua)
		e.ObjectType, e.ObjectID = deref(objType), deref(objID)
		if cnt != nil {
			e.Count = *cnt
		}
		if e.Kind == ActivityKindPurge || e.Kind == ActivityKindAIKey || e.Kind == ActivityKindRuleApply {
			e.ID = strings.TrimLeft(e.ID, "0")
			if e.ID == "" {
				e.ID = "0"
			}
		}
		page.Entries = append(page.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return ActivityPage{}, fmt.Errorf("store: list activity: %w", err)
	}
	rows.Close()
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		last := page.Entries[limit-1]
		page.Next = ActivityCursor{At: last.At, Kind: last.Kind, ID: last.ID}.Encode()
	}
	if page.Covers, err = tx.activityCovers(ctx, kinds); err != nil {
		return ActivityPage{}, err
	}
	return page, nil
}

// activityCovers is the oldest record per selected source: each is one probe of
// an index that leads with user_id.
func (tx *UserTx) activityCovers(ctx context.Context, kinds []string) (map[string]*time.Time, error) {
	probe := map[string]string{
		ActivityKindAITool: `SELECT min(e.at) FROM app.api_key_events e WHERE e.user_id = $1 AND ` + activityAIEventPredicate,
		ActivityKindAIKey: `SELECT min(a.created_at) FROM app.audit_log a
		   WHERE a.user_id = $1 AND a.object_type = 'ApiKey' AND a.action LIKE 'api\_key.%'`,
		ActivityKindDecryption: `SELECT min(decrypted_at) FROM app.pgp_decrypt_audit WHERE user_id = $1`,
		ActivityKindPurge: `SELECT min(a.created_at) FROM app.audit_log a
		   WHERE a.user_id = $1 AND a.object_type = 'Email' AND a.action = '` + PurgeAction + `'`,
		ActivityKindSignIn:  `SELECT min(created_at) FROM app.sessions WHERE user_id = $1`,
		ActivityKindSignOut: `SELECT min(revoked_at) FROM app.sessions WHERE user_id = $1 AND revoked_at IS NOT NULL AND revoked_reason <> 'replaced'`,
		ActivityKindPlacement: `SELECT min(a.created_at) FROM app.audit_log a
		   WHERE a.user_id = $1 AND a.object_type = '` + backfillObjectType + `'`,
		ActivityKindRuleApply: `SELECT min(a.created_at) FROM app.audit_log a
		   WHERE a.user_id = $1 AND a.object_type = '` + RuleApplyObjectType + `'`,
	}
	out := map[string]*time.Time{}
	for _, k := range kinds {
		var t *time.Time
		if err := tx.tx.QueryRow(ctx, probe[k], tx.userID.String()).Scan(&t); err != nil {
			return nil, fmt.Errorf("store: activity coverage (%s): %w", k, err)
		}
		src := kindSource[k]
		// A source with two kinds reports the older of their oldest records.
		if prev, seen := out[src]; seen && prev != nil && (t == nil || !t.Before(*prev)) {
			continue
		}
		out[src] = t
	}
	return out, nil
}

// AuditRuleApply records a rule bulk apply (or its undo) in app.audit_log in
// the caller's transaction (ADR-053, gap 1). Counts only, never a subject or
// sender.
func (tx *UserTx) AuditRuleApply(ctx context.Context, ruleID string, undo bool, changed int) error {
	action := RuleApplyAction
	if undo {
		action = RuleApplyUndoAction
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail)
		VALUES ($1, 'user', $2, $3, $4, jsonb_build_object('changed', $5::int))`,
		tx.userID.String(), action, RuleApplyObjectType, ruleID, changed); err != nil {
		return fmt.Errorf("store: audit rule apply: %w", err)
	}
	return nil
}
