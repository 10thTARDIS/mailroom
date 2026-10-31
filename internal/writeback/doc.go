// SPDX-License-Identifier: AGPL-3.0-only

// Package writeback mirrors local intent deltas back to the IMAP server
// (spec §4/§4a/§6). A user action — mark seen, star, label, move, delete,
// append a built draft — is recorded locally as a canonical mutation AND as a
// writeback intent (a DELTA, never a target state). This package drains that
// queue, applies each delta to the origin server, and handles idempotency,
// conflicts, and dead-lettering.
//
// # State machine
//
// Each intent walks a small state machine in app.writeback_intents:
//
//	pending ──claim──▶ inflight ──success──▶ applied
//	                      │
//	                      ├─ transient failure, attempts left ─▶ pending (retry)
//	                      └─ conflict / attempts exhausted ────▶ dead (surfaced)
//
// ClaimPendingIntents moves a batch pending→inflight under FOR UPDATE SKIP
// LOCKED and bumps attempts; the engine applies each via an IMAPWriter, then
// MarkIntentApplied (→applied) or MarkIntentFailed (→pending to retry, or
// →dead with a surfaced dead_reason).
//
// # Idempotency
//
// Re-application is safe for two independent reasons:
//
//   - Enqueue is idempotent on (user_id, idempotency_key): the same logical
//     mutation enqueued twice yields exactly one intent (EnqueueIntent).
//   - The pending→inflight→applied transitions plus SKIP LOCKED mean a given
//     intent is claimed by one worker at a time, and MarkIntentApplied is
//     guarded on state='inflight' so a duplicate completion is a no-op.
//
// The IMAP flag/keyword ops are also intrinsically idempotent: STORE +FLAGS
// \Seen on an already-seen message, or +FLAGS \Deleted on an already-deleted
// one, is a server-side no-op. So even if a worker crashes after issuing the
// STORE but before MarkIntentApplied, re-running the delta converges to the
// same server state.
//
// The crash itself is now recovered rather than merely survivable. An intent
// left inflight by a dead process is reclaimed by ClaimPendingIntents once its
// claimed_at ages past a 15-minute lease (#520, migration 00053), the same
// shape app.jobs got in 00041. Until then such a row was read by NOTHING —
// fail-safe, in that nothing re-fired, but silent: the user's change never
// reached IMAP and only 'dead' was ever surfaced.
//
// ⚠ THIS PARAGRAPH USED TO SAY THE REAPER WAS "A FOLLOW-UP" AND THAT A
// MESSAGE-ID PROBE FOR APPEND WAS "A LATER CHANGE". Both had stopped being
// true, in opposite directions, and the doc is where that was discovered.
//
// APPEND is the op that is not NATURALLY idempotent — a retried APPEND can
// create a duplicate message — but it does not rely on that, because it already
// asks the server whether the Message-ID is present in the target mailbox and
// skips the append if it is. That probe shipped as the self-append dedup for
// providers that file their own Sent copy (spec §4, ADR-001); it is the same
// mechanism this doc once described as pending. Its one gap is a payload with
// an empty message_id_header, where the probe is skipped.
//
// move-via-copy+expunge keeps a narrower version of the caveat: a completed
// move leaves no live provenance at the old coordinate, so re-application is
// refused as a conflict rather than duplicating, but a crash BETWEEN the copy
// and the expunge can still duplicate. That path is only taken when the server
// does not advertise the MOVE extension.
//
// # Conflict handling (provider-wins for destructive divergence)
//
// Before applying a message-targeted op the engine reads the message's live
// provenance in the account (MessageProvenanceTargetsForAccount) and compares
// it to the intent's baseline. A conflict is any of:
//
//   - no live provenance — the message was moved or expunged remotely;
//   - provenance state is stale_uidvalidity — the mailbox was recreated;
//   - the intent's recorded expected uidvalidity no longer matches.
//
// # Every copy, not one (#296)
//
// An intent is enqueued per ACCOUNT, but a message can have several live copies
// INSIDE one account: re-delivered mail records a provenance row per delivery
// (#206), and a copy can be filed in two folders. The engine therefore resolves
// an intent to the full set of live copies and applies the delta to every one of
// them.
//
// Reaching only one used to be invisible for adds and silently wrong for
// removals, because ingest merges the flags of every live copy with OR: clearing
// \Seen on one copy left the others carrying it, and the next poll merged it
// straight back. If any copy fails the whole intent is retried — the ops
// affected are idempotent, so re-applying to copies that already succeeded costs
// a redundant no-op, whereas marking "applied" after reaching some of the copies
// costs the user's mutation.
//
// On a conflict the engine does NOT blindly mutate the server. Per spec §4's
// reconciliation stance it resolves destructive divergence provider-wins: the
// server's current reality is authoritative, the stale local delta is
// dead-lettered with a clear dead_reason, and the account-health surface shows
// the user what did not apply. (A future refinement may auto-reconcile
// non-destructive cases — e.g. re-resolve the new UID and retry a pure flag
// set — rather than dead-letter them; that policy is deliberately conservative
// for now.)
//
// # Bounded retries and dead-lettering
//
// Transient IMAP failures (network, timeout, server busy) send the intent back
// to pending until Engine.maxAttempts is reached, after which it is
// dead-lettered with a dead_reason. Dead intents are SURFACED, never silently
// dropped (CLAUDE.md).
//
// # Seams and the internal/sync follow-up
//
// The engine depends only on two interfaces, so it unit-tests with fakes — no
// database, no live IMAP:
//
//   - IntentStore — the store surface (claim, provenance, mark applied/failed).
//     StoreIntents adapts *store.Store to it, opening a short UserTx per call
//     so no DB transaction is ever held open across a network IMAP round-trip.
//   - IMAPWriter — the write operations this engine needs: StoreFlags,
//     Copy, Move, Expunge, Append, SearchMessageID, UIDPresent (plus
//     SupportsMove). It is expressed in
//     mailroom's own sync.UID/sync.Flag anti-corruption types, deliberately so
//     that internal/sync's SyncSession can satisfy it directly. SyncSession
//     (internal/sync/session.go) DOES expose STORE/COPY/MOVE/EXPUNGE/APPEND —
//     sync.Writer (internal/sync/writer.go) narrows its batch (UIDSet) methods
//     down to the single-UID shapes IMAPWriter expects — and production wires
//     a live *session through imapsync.NewWriter as the engine's IMAPWriter
//     (internal/writeback/runner.go). Tests still exercise the engine
//     end-to-end against a fake writer as well.
//
// # Gmail mode (#52, ADR-047 §3)
//
// A Gmail OAuth account's intents are applied with Gmail's own primitives —
// labels via STORE ±X-GM-LABELS, MOVE only into and out of Trash and Spam,
// EXPUNGE only there — never the generic folder ops above. The runner turns
// the mode on from the account row (WithGmail); gmail.go has the translation
// table and the rules that keep it safe. While store.GmailWritebackReady is
// false such an account is ingest-only and the runner never dials it.
//
// A third, optional seam — BlobReader — lets the engine resolve an APPEND
// intent's bytes by content hash instead of requiring them inline in the
// payload (mailroom#89): the store no longer writes a message's raw bytes into
// app.writeback_intents (a 50 MiB attachment would otherwise write ~67 MiB of
// base64 JSONB per autosave), so the engine reads them back from the blob
// store the compose/send path already wrote them to. A legacy intent enqueued
// before this change may still carry its body inline; the engine honors that
// shape too, so nothing already queued is stranded by the upgrade.
package writeback
