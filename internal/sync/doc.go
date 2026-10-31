// SPDX-License-Identifier: AGPL-3.0-only

// Package sync is the anti-corruption layer over go-imap v2 (spec §3/§4):
// the SyncSession interface plus mailroom's own Mailbox/MailboxStatus/
// Message/Notification/Error types. No go-imap type appears in any exported
// signature of this package — the single-account sync engine (#10) and
// everything above it only ever sees our types, so go-imap's pre-stable API
// churn stays contained here (session.go, mapping.go and folders.go are the only files
// that import go-imap).
//
// Dial connects and authenticates (LOGIN over IMAPS/STARTTLS/plaintext, see
// Config); ListMailboxes/Select/FetchByUID/Supports/Idle/Logout/Close round
// out what incremental sync needs. The intent-delta writeback queue and
// bulk archive import described in spec §4/§4a are not implemented here —
// they build on top of this seam in a later milestone.
//
// CONDSTORE/QRESYNC (RFC 7162, spec §4's "cheap incremental sync"): see
// NOTES.md for the full go-imap v2 beta.8 spike findings. Summary:
// CONDSTORE is fully exposed by the library (SelectOptions.CondStore,
// FetchOptions.ModSeq/ChangedSince, SelectData.HighestModSeq) and is what
// SyncSession uses today. QRESYNC is NOT usable — the library only exposes
// the QRESYNC capability *name* for detection (imap.CapQResync) but has no
// SELECT QRESYNC parameters, no VANISHED response handling, and no
// UID FETCH ... (VANISHED) support anywhere in the tree. The sync engine
// must fall back to CONDSTORE-only reconciliation (diff UIDs via an
// open-ended UIDSetRange against the current UIDNEXT, plus MODSEQ-filtered
// flag fetches) until QRESYNC is contributed upstream — see NOTES.md for
// the path-forward discussion.
//
// # UID SEARCH is a rationed command (#638)
//
// The QRESYNC gap above has a price: with no VANISHED list, the only way to
// learn what left a mailbox is LiveUIDs — EXAMINE plus UID SEARCH ALL — once
// per mailbox. An account with hundreds of folders therefore issues hundreds of SEARCHes per
// full reconciliation, and providers ration that: Migadu answers 60 per rolling
// minute PER ACCOUNT and refuses the rest with a plain `NO search rate limit
// exceeded: ... please wait 42s before trying again` — no response code.
//
// Two mechanisms live here as a result, and both are opt-in so that nothing
// changes for a server without such a limit:
//
//   - Config.SearchLimiter paces every UID SEARCH this package issues: the
//     observation (LiveUIDs) and writeback's two lookups (SearchMessageID,
//     UIDPresent), which spend the same per-account budget. Nil means no pacing.
//     NewSearchLimiter is the token bucket; syncrunner owns one per account.
//   - classify recognises the refusal as ErrorKindRateLimited, by RFC 5530's
//     [LIMIT] response code first and by the refusal text second (scoped to the
//     SEARCH ops, so an unrelated NO can never be misread). Callers ask through
//     IsRateLimited / RetryAfterOf and never match text themselves — the text
//     stays behind this seam like every other go-imap detail.
//
// Build note: this package imports github.com/emersion/go-imap/v2/imapclient,
// which requires a go.sum entry for github.com/emersion/go-sasl that is not
// yet present in this repo's go.sum (only the /go.mod hash is there, not
// the module content hash). See NOTES.md "Known gap" for the exact error
// and the one-line fix, which this change deliberately does not apply
// (go.mod/go.sum are out of scope for this change per the task that
// produced it).
package sync
