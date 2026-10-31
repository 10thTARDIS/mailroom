// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"math"
	"net/http"
	"strconv"

	"mailroom/internal/store"
)

// Per-user quota decisions (spec §10, matrix §14).
//
// The quota LIMITS are the app.users columns (quota_storage_bytes,
// quota_sync_connections, quota_llm_daily_tokens); this file is the pure logic
// that turns a limit + current usage into an allow/deny and the RIGHT rejection
// shape. It holds no database code — reading a user's limit and usage is a store
// concern (CLAUDE.md: only internal/store touches the database), fed in through a
// QuotaSource; see the wiring note at the bottom of this file.
//
// THE INVARIANT, restated because it is the whole point (spec §10): a quota that
// is hit produces an explicit, clean rejection and NEVER a silently dropped
// mutation or silently dropped mail.
//   - An over-quota /jmap request-time action (an upload, an Email/set create)
//     gets a JMAP overQuota error (RFC 8620 §5.3 has the type) — the client learns
//     nothing was stored.
//   - Quota exhaustion MID-SYNC (the provider still has mail we have no room for)
//     must PAUSE sync and raise account health, not drop the mail. That decision
//     is the same Quota.Admits below, made in the ingest path; QuotaExhausted is
//     the sentinel the sync loop pauses on. (The pause lives in the sync runner,
//     which owns ingest — see the report; this file gives it the decision.)

// QuotaKind names the enforced quotas — one per app.users quota column.
type QuotaKind string

const (
	QuotaStorageBytes    QuotaKind = "storageBytes"
	QuotaSyncConnections QuotaKind = "syncConnections"
	QuotaLLMDailyTokens  QuotaKind = "llmDailyTokens"
)

// Quota is one user's limit for a resource paired with their current usage of it.
// A non-positive Limit means the quota is not enforced: the schema uses 0 for
// "LLM disabled", and a 0 storage limit is read here as "no storage cap" — an
// operator who wants an unlimited resource sets its column to 0.
type Quota struct {
	Kind  QuotaKind
	Limit int64
	Used  int64
}

// Unlimited reports whether this quota is not enforced.
func (q Quota) Unlimited() bool { return q.Limit <= 0 }

// Remaining is the headroom left, clamped at 0 (never negative). MaxInt64 for an
// unlimited quota.
func (q Quota) Remaining() int64 {
	if q.Unlimited() {
		return math.MaxInt64
	}
	if r := q.Limit - q.Used; r > 0 {
		return r
	}
	return 0
}

// Admits reports whether adding delta more of the resource stays within the
// quota. delta <= 0 (a delete, a no-op) always admits. This is the one decision
// every enforcement point makes: the upload endpoint (delta = blob size), the
// ingest path (delta = message size), the LLM budget (delta = tokens).
//
// The rule itself lives ONCE, in store.QuotaAdmits (a leaf both this package and
// internal/ingest import), so the mid-sync ingest pause and this request-time
// check can never drift apart. Quota.Unlimited() is q.Limit <= 0, exactly the
// unlimited branch store.QuotaAdmits encodes.
func (q Quota) Admits(delta int64) bool {
	return store.QuotaAdmits(q.Limit, q.Used, delta)
}

// overQuotaSetError is the RFC 8620 §5.3 per-object error for an over-quota
// create/update inside a /set (e.g. Email/set create when storage is full). The
// existing setOverQuota constant is exactly this type.
func overQuotaSetError(q Quota) SetError {
	return SetError{
		Type: setOverQuota,
		Description: "over the " + string(q.Kind) + " quota (" +
			strconv.FormatInt(q.Used, 10) + " of " + strconv.FormatInt(q.Limit, 10) + " used)",
	}
}

// writeOverQuota answers an over-quota HTTP request (the blob upload endpoint,
// RFC 8620 §6.1) with a clean 413 and a JMAP-shaped error, so the client learns
// the upload was NOT stored rather than believing it was.
//
// 413 (not 429): 429 says "slow down and retry" — which is wrong, retrying will
// not free space. overQuota is a durable refusal until the user frees room or the
// operator raises the limit.
func writeOverQuota(w http.ResponseWriter, q Quota) {
	hardenBlobResponse(w)
	writeJSON(w, http.StatusRequestEntityTooLarge,
		newRequestError("overQuota", http.StatusRequestEntityTooLarge,
			"over the "+string(q.Kind)+" quota; nothing was stored"))
}

// QuotaSource reads a user's quota limit and current usage. It is the seam
// between this decision logic and the database, satisfied by internal/store: the
// API layer must never run SQL itself.
//
// It is intentionally not yet consumed by a handler in this milestone — the
// store methods it needs (a users-quota read and a per-user storage-usage sum)
// are proposed in the #38 report for the store owner to add, since this agent
// does not edit internal/store. Once they exist, the upload endpoint and
// Email/set create call StorageQuota + Quota.Admits before writing, and the
// ingest path calls the same before an APPEND — all failing with the shapes
// above. Declaring the seam here keeps the contract in the layer that owns the
// decision.
//
//	type QuotaSource interface {
//	    // StorageQuota returns the user's storage limit and current bytes used,
//	    // read in one RLS-scoped transaction.
//	    StorageQuota(ctx context.Context, userID ulid.ULID) (Quota, error)
//	}
