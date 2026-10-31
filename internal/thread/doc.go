// SPDX-License-Identifier: AGPL-3.0-only

// Package thread implements Jamie Zawinski's message-threading algorithm
// (https://www.jwz.org/doc/threading.html) — the canonical way to group
// email messages into conversations from Message-ID/References/In-Reply-To
// headers — with a deliberately RESTRICTED subject-fallback step. Issue #12.
//
// ⚠ NOTHING IMPORTS THIS PACKAGE. It is not the threader Mailroom runs.
//
// The running threader is store.FindThreadByHints — one SQL query in
// internal/store/ingest.go that matches Message-ID hints and takes the first
// match. This package has ZERO importers (control: 186 tracked files import
// internal/store), so editing it changes no behaviour whatsoever.
//
// That is not a hypothetical hazard: #210 identified a duplicate-Message-ID
// defect and located it in container.go. The reading was reasonable — the
// logic really is here — and the fix was correct, self-consistent, and
// changed nothing, because this code does not run. A fully-tested package is
// the most convincing thing to mistake for the implementation.
//
// Keeping it unwired is a recorded decision, ADR-030 (#434): threading is
// hint-matching with no cross-thread merge and no subject fallback, a
// deliberate deviation from spec §4. The test corpus could not tell the two
// apart (no messages whose References span more than one thread, over a
// tiny denominator, so that zero sizes the mailbox, not the algorithm). The
// package is kept rather than deleted so that the ADR's revisit, triggered by
// a measured gap on a real mailbox, stays cheap.
//
// This package is store-agnostic: it operates purely on in-memory Message
// values and never touches the database. Persisting the resulting
// (recomputable) thread membership is the ingest layer's job (spec v0.4
// §4/§14, data-model.md §2/§3) — a separate concern from this package.
//
// # Algorithm
//
// The five JWZ steps, faithfully implemented:
//
//  1. Build an id_table of Container objects keyed by Message-ID. Each input
//     Message gets (or fills in) a Container. A Message-ID reused by a
//     second, different Message (spoofed or genuinely duplicated) does not
//     clobber the first container: the second message gets its own,
//     unregistered container, so it can thread *near* the original via its
//     own References but can never merge into or overwrite it.
//  2. Link containers via References (and, only when References is absent,
//     In-Reply-To as the reference of last resort), building the parent
//     chain — including dummy containers for ancestors we never received.
//     Empty/self-referential ids are ignored. An established parent link is
//     never overwritten, and any link that would make a container its own
//     ancestor is rejected, so reference loops (A references B, B
//     references A) terminate safely instead of cycling.
//  3. The root set is every container with no parent.
//  4. Prune empty (message-less) containers: a non-root empty container is
//     always spliced out and its children promoted to its parent; a root
//     empty container is spliced out only if it has exactly one child
//     (promoted to take its place) — with two or more children it is kept
//     as a dummy grouping root, since removing it would otherwise merge
//     unrelated branches into the root set.
//  5. RESTRICTED subject-fallback grouping (see below).
//
// # Restricted subject fallback — the exact rule, and why
//
// JWZ's original step 5 merges any two root-set containers that reduce to
// the same base subject, on the theory that pre-References mail clients
// need some way to thread. That heuristic is dangerous for a modern client:
// two strangers' unrelated conversations that happen to both be titled
// "Status update" would be silently merged into one thread. Over-merging is
// worse than under-merging for a mail client — a missed thread link is a
// minor inconvenience, a false merge leaks context across conversations and
// erodes trust in the UI. This package therefore restricts step 5 to two
// conditions, both of which must hold before any subject-based merge is
// performed, and defaults to NOT merging whenever either is unclear:
//
//  1. Reply-only sources. Only a root container whose own (possibly
//     inherited-from-child) subject normalizes to a REPLY form — i.e. after
//     stripping "Re:"/"Fwd:"/"Fw:" prefixes and bracketed mailing-list tags
//     ("[list-name] ...") case- and space-insensitively, the stripped
//     prefix chain contained a "Re:" — is ever pulled into another
//     container by subject. A bare, non-reply subject is never used to
//     found a merge: someone hitting "reply" is real (if weak) evidence
//     their message continues something; two fresh messages that merely
//     share a title are not evidence of anything. Forward-flagged subjects
//     ("Fwd:"/"Fw:" anywhere in the stripped prefix chain) are excluded
//     entirely — neither as a source nor as a merge target — because
//     forwarding is a deliberate branch into a new context; even if the
//     base subject matches an existing thread, a forward must stay
//     separate from it.
//  2. No merging two established identities. A root container "already has
//     a distinct References-rooted identity" if — *before* any
//     subject-fallback merging in this step — it already has children,
//     meaning real References/In-Reply-To headers already linked at least
//     one other message under it. Two such containers are never merged by
//     subject alone, even if both are reply-form: each is already a real,
//     independently-anchored conversation, and a shared subject line is not
//     strong enough evidence to fold two anchored conversations into one.
//     Merging is only permitted when at least one side is "weak" (a lone
//     container with no children of its own) — the case of a reply whose
//     References header was stripped in transit (mailing-list footer
//     rewriting is the common cause) and needs a subject-based way back
//     into its conversation. This check is evaluated against a snapshot of
//     child-having taken right after step 4, not updated as merges happen
//     within step 5, so the merge decision for any one container never
//     depends on the order in which other containers were processed.
//
// Everything else about JWZ step 5 (grouping by normalized base subject,
// preferring a dummy/empty container over a real one as the canonical
// anchor for a subject, preferring a non-reply subject over a reply subject
// as the anchor) is retained; only the eligibility-to-merge rules above are
// narrowed relative to the original algorithm.
//
// # Non-goals
//
// This package does not decide what counts as "the same account" or "the
// same correspondent set" — the broader cross-account restriction described
// in spec v0.4 §4 ("subject-only merging is allowed only within the same
// account, same correspondent set, and a bounded time window") is the
// caller's responsibility: pass this package only the messages that are
// already known to be in scope for subject-fallback (e.g. one account's
// messages), and it will apply the safety rules documented above on top.
package thread
