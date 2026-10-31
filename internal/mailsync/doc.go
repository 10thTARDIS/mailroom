// SPDX-License-Identifier: AGPL-3.0-only

// Package mailsync drives incremental, single-account IMAP synchronization
// (spec §4 sync model, §16 onboarding). It selects each mailbox on an account,
// reconciles new messages and CONDSTORE flag changes against the store's
// per-mailbox cursors, and feeds every fetched message into canonical ingest.
//
// # Reconciliation model
//
// The engine reconciles CONDSTORE-only: QRESYNC is not usable through the
// go-imap client this project wraps (see internal/sync/NOTES.md), so there is
// no server-side VANISHED list. New messages are found by fetching the
// open-ended UID range above the last-seen UID; flag changes are found with a
// MODSEQ-filtered (CHANGEDSINCE) fetch over the already-known UID range and
// re-ingested — ingest deduplicates by content hash and merges the flags. A
// UIDVALIDITY change retires the mailbox's provenance generation and triggers
// a full re-fetch under the new validity.
//
// # The expunge reaper (#86)
//
// Everything above can only ever ADD. Nothing in a CONDSTORE poll can see a
// message LEAVE — so, before the reaper, a message deleted or moved away in any
// other IMAP client (phone, webmail, Thunderbird) never left its Mailroom folder,
// permanently. The reaper is the missing half: on a slow, separately-configured
// cadence (WithReapInterval, default hourly, persisted per mailbox in
// app.mailboxes.last_reaped_at), it asks the server for a mailbox's whole live
// UID set (sync.SyncSession.LiveUIDs — UID SEARCH ALL) and diffs it against the
// provenance rows we hold for that (mailbox, uidvalidity). A UID we hold and the
// server does not is a message that was expunged or moved away THERE; its
// provenance row is dropped, an Email change_log row records the membership
// change, and a message left in no location at all is soft-deleted (ADR-009 —
// never hard-deleted).
//
// The reaper is the only part of this package that can DESTROY information, so
// every one of its gates fails closed. A mailbox whose poll failed is not a reap
// candidate; a failed LiveUIDs reaps nothing; an untrustworthy answer (wrong
// UIDVALIDITY generation, or an empty UID set from a mailbox the server just said
// holds messages) is refused rather than acted on; the reap is bounded by the
// UIDNEXT the server reported, so a message ingested concurrently cannot be
// caught in it; and the cadence cursor advances only inside the transaction that
// actually reaped. "We failed to ask" is never "the server said it's gone".
//
// # The gap backfill (#93)
//
// The reaper asks "what did we ingest that the server no longer has?". Nothing
// asked the mirror question, and it turns out to be the one that loses mail.
//
// The poll fetches `lastSeen+1:*` and then advances lastSeen to the highest UID it
// INGESTED. A FETCH that silently drops a UID in the middle of that range returns
// NO ERROR — a truncated response looks exactly like this — so the cursor advances
// past the dropped message anyway. The next poll asks for `cursor+1:*`. The message
// is now below the cursor: the poll will never request it again, and the reaper
// cannot see it either, because the reaper only drops provenance rows and this
// message has none. It stays on the server, unknown to Mailroom, forever.
//
// It is undetectable from the FETCH itself — a hole in a FETCH response is exactly
// what an expunged UID looks like — so the fix is the same INDEPENDENT proof #88
// demanded of the post-roll re-fetch, applied to the ordinary poll: every UID the
// server reports below our own cursor must have a live provenance row. The reaper
// is already reading the whole live UID set every pass, so this costs one extra
// query and no extra round-trip. The UIDs that fail the proof are re-fetched and
// ingested (idempotently — ingest dedups on content hash), never merely logged.
//
// The two conclusions drawn from that one UID set are rigorously separate and must
// stay so: the reaper REMOVES provenance for UIDs the server does not have; the
// backfill ADDS provenance for UIDs the server does have and we never ingested. A
// gap is evidence that OUR FETCH was incomplete, never that anything was deleted.
// Accordingly the backfill is purely additive, it runs even when the reaper is
// disabled (WithReapInterval(0) switches off deletion, not completeness), it runs
// BEFORE any destructive phase, and a mailbox with an open gap is not reaped at all
// — an incomplete view of a folder is not a licence to delete rows from it.
//
// # The observation is rationed by the server (#638)
//
// Both of the above rest on ONE question per mailbox — LiveUIDs, EXAMINE plus
// UID SEARCH ALL — and that question is rationed. Migadu answers at most 60
// SEARCHes per rolling minute, charged PER ACCOUNT, and refuses the rest. An
// account with hundreds of mailboxes had most of them refused on its first sync and
// kept losing many per pass hours later, because a flat cadence
// brings every mailbox observed in one pass due again in one pass.
//
// Three things follow, none of which changes what the engine CONCLUDES:
//
//   - The SEARCHes are paced. sync.Config.SearchLimiter (one token bucket per
//     account, built by syncrunner) spaces them under the provider's ceiling,
//     leaving headroom for writeback — which issues UID SEARCHes of its own
//     against the same budget — and for the user's other clients.
//   - A refusal ENDS the pass's observation loop (sync.IsRateLimited). Every
//     SEARCH after it would be refused too, so continuing only produces noise.
//     The mailboxes left unobserved are counted in Summary.MailboxesDeferred
//     and are NOT recorded as errors: nothing about them was concluded.
//   - The cadence is jittered per mailbox (reapJitter), deterministically from
//     the mailbox id, by up to 20% of the interval — so a cohort observed
//     together does not come due together forever after.
//
// The fail-closed guarantees are untouched by all of it. A deferred mailbox is
// in exactly the state a mailbox whose observation failed has always been in:
// not verified, not reaped, cursor not stamped, due again next pass.
//
// # Skipping what did not change (#620)
//
// That account's passes took 174–324 s, most of it SELECTing and SEARCHing
// folders in which nothing had happened. When the server supports LIST-STATUS
// (RFC 5819, or IMAP4rev2), the one LIST a pass already sends also returns every
// folder's UIDVALIDITY, UIDNEXT, MESSAGES and HIGHESTMODSEQ
// (sync.Mailbox.Status), and two pieces of work are skipped on the strength of
// it — against two DIFFERENT baselines:
//
//   - The POLL (phase 1) is skipped when the four numbers equal what the last
//     poll's own SELECT reported (app.mailboxes.poll_uidnext/poll_messages
//     beside the existing cursors) AND the poll owes nothing below UIDNEXT —
//     i.e. exactly when the SELECT would lead to no FETCH at all. The folder is
//     still upserted and is still a phase-2 target. Summary.MailboxesUnchanged.
//   - A due OBSERVATION (phase 2) is skipped when the four numbers equal what
//     the last REAL observation saw (app.mailboxes.observed_*), and that
//     observation is under 24 hours old. Only the cadence cursor is re-stamped;
//     nothing is verified or reaped. Summary.ObservationsSkipped.
//
// ⚠ The observation's baseline is the last OBSERVATION, never the last poll. A
// poll whose FETCH silently lost a message (#93) still advances the poll
// cursors, and the observation is the only thing that finds the loss — so
// "unchanged since the last poll" is exactly the wrong reason to skip it. For
// the same reason an observation whose proof did not reach the top of the UID
// space it saw records no baseline at all.
//
// Every "unknown" means "do the work": no CONDSTORE, no status from the server,
// a zero HIGHESTMODSEQ, or a baseline never recorded (every folder on the first
// pass after migration 00058) all behave exactly as before #620.
//
// # Bounded fetches (#656)
//
// No FETCH that downloads bodies names more than DefaultFetchChunk UIDs. The
// new-message pass walks [lastSeen+1, UIDNEXT) in closed chunks and leaves only
// the last one open-ended (`start:*`), so arrivals during the pass are still
// caught; without a UIDNEXT it is the single open-ended command it always was.
// The flag pass's body fallback and the gap backfill split their UID lists the
// same way. A backlog of thousands of messages moved into a folder on the
// provider used to be ONE multi-gigabyte command, which the provider eventually
// refused outright on every pass.
//
// A pass that fails part-way keeps what it did: messages are ingested as they
// stream, and the last-seen cursor is advanced to the end of the last COMPLETED
// chunk so the next pass resumes there. That write carries no poll-time
// baseline (poll_uidnext stays NULL), so the #620 skip can never treat a
// half-fetched folder as unchanged; the folder is also not a phase-2 target on
// that pass, exactly like any other failed poll. A UIDVALIDITY roll records
// nothing part-way — its cursors belong to the reconcile (#88) — and simply
// retries its re-fetch.
//
// # Boundaries and testability
//
// Credential decryption and mail-DEK resolution are the CALLER's job: SyncAccount
// takes an already-built sync.Config plus an unwrapped DEK and its id
// (mirroring ingest.Ingestor.Ingest, where key management also lives outside).
//
// The engine depends only on narrow interfaces — a SessionFactory (default:
// sync.Dial), an Ingester (satisfied by *ingest.Ingestor), and a MailboxStore
// (satisfied by StoreAdapter over *store.Store) — so the whole sync loop is
// unit-testable with fakes, without a live IMAP server or a database.
//
// This package touches the database only through the store layer (CLAUDE.md):
// StoreAdapter runs each store call inside a Store.UserTx.
package mailsync
