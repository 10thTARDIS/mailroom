// SPDX-License-Identifier: AGPL-3.0-only

// THE OUTBOX (#448, design pass-2 §8): what has not gone yet.
//
// The rail has had an Outbox COUNT since #375 and no screen behind it — the entry
// pointed at Sent, "which is where the mail ends up moments later", with a TODO
// saying there was no submissions view. This is that view's arithmetic.
//
// WHAT IS IN IT, exactly: submissions the server still calls `pending` whose
// `sendAt` is in the FUTURE. Both halves are load-bearing.
//
//   * `undoStatus === "pending"` — `final` has gone (it is in Sent, and listing it
//     here would offer an Undo button that cannot do what it says: the server
//     accepts `undoStatus: "canceled"` only, and only while the job is still
//     pending — internal/api/submission.go cancelSubmission). `canceled` did not
//     go and never will.
//   * `sendAt > now` — a pending submission whose instant has passed is being sent
//     right now; the row would count down through zero and sit at "Sending in 0 s"
//     until the change loop caught up.
//
// ⚠ BOTH OF THOSE WERE TRUE AND TOGETHER THEY HID EVERY FAILED SEND (#589).
// "What has not gone yet" is a category a permanently failed send falls straight
// out of: `undoStatus` is `final` (so the first filter drops it) and `sendAt` is
// necessarily in the past once the job has run (so the second drops it too,
// independently). The row was fetched by the poll, held in the store, and then
// discarded by this function — and the rail badge is computed from these rows,
// so nothing anywhere said a word.
//
// Observed on a live instance: two forwards refused with `550 5.7.1 sender
// address rejected: not allowed`. The user found out from the provider's
// webmail, having watched the undo countdown expire normally — which is the
// interface's most confident moment.
//
// So the category is now "what has not gone yet, AND what tried and did not
// go". Concretely two more states:
//
//   * `sending` — pending with `sendAt` past: in flight, or requeued after a
//     transient failure and awaiting the job's next attempt. The original
//     "Sending in 0 s" objection is answered by a state carrying no number.
//   * `failed` — a permanent refusal. It needs the user, because nothing will
//     retry it: compose/send.go's failSubmission is reached by a permanent SMTP
//     refusal (provider.ErrPermanent), an address the provider cannot accept
//     (provider.ErrEAIUnsupported), an SMTP host the egress guard refuses
//     (provider.ErrHostNotPermitted, #625), a failed SMTP credential lookup,
//     or an account in ingest-only mode (ADR-028) — and failExhausted lands
//     here too when a TRANSIENT failure runs the job's retry budget out (#624),
//     the job dead-lettered behind it. Requeue is the other branch, and only
//     while attempts remain.
//
// ⚠ HOW A FAILED ROW LEAVES, which is the part that decides whether this is an
// alert or a nuisance. Three ways: re-sending — a row is shown only while it is
// the LATEST submission for its message, so the re-send creates a newer one,
// the old failure stops being the latest, and the row and the badge go on their
// own; Dismiss — `dismissed_at` (migration 00059, OutboxRoute.tsx), the act for
// a failure the user does not mean to re-send, without which a provider hiccup
// would leave a banner per message until each was cleared by hand; and deleting
// the draft, which cascades to its submissions (app.submissions.message_id).
//
// The latest-per-message keying self-cleans the common case (the user fixes the
// problem and sends again); dismissal is the exit for a failure that will not
// be re-sent.
//
// It also costs nothing to store: "latest per message" is derived from ULID
// ordering that is already there, and the submissions row keeps its verbatim
// SMTP response for the export that is the only record a send was ever
// attempted (internal/exportplan/plan.go).
//
// TWO STATES, AND THE BOUNDARY IS AN ASSUMPTION THIS FILE STATES RATHER THAN
// HIDES. `Sending in 14 s` is the undo window; `Scheduled · Mon 09:00` is Send
// Later. The server does not tell a client what its undo window is — the only
// number it gives is the submission's own `sendAt` (core/actions.ts SendOutcome
// says the same thing from the other side) — so the design's rule is used:
// anything under five minutes away is treated as the window. A message scheduled
// four minutes out therefore reads as a countdown, which is the harmless
// direction: both rows offer the same action, and the words differ only in
// urgency.
//
// ORDERING IS BY ID, DESCENDING, WHICH IS "NEWEST FIRST". Submission ids are
// ULIDs (ADR-007), so lexicographic order IS creation order. `sendAt` is NOT
// creation order once Send Later exists — a message scheduled for next Monday has
// a later `sendAt` than one going out in ten seconds, and sorting on it would put
// next week at the top of a list about what is happening now.

import type { EmailSubmission, Id, Identity } from "../../core/jmap/types";
import type { Email } from "../../core/jmap/types";
import { addressLabel } from "./addressLabel";

/** How close to `sendAt` a submission has to be for its row to read as the undo
 * window rather than as a schedule (see the header for why this is a client-side
 * rule and not a server fact). */
export const UNDO_WINDOW_MS = 5 * 60 * 1000;

export type OutboxState =
  | { kind: "window"; secondsLeft: number }
  | { kind: "scheduled"; sendAt: string }
  /**
   * `sendAt` has passed and the submission is still `pending`: the send is being
   * attempted right now, or a transient failure put it back in the queue and the
   * job will retry it (compose.SendWorker.requeue returns it to 'scheduled',
   * which is `undoStatus: "pending"` with a `sendAt` in the past).
   *
   * These rows used to be dropped, which is why a send in flight was invisible.
   * The original reason was sound — the countdown would "sit at Sending in 0 s"
   * — so the fix is a state with no number in it rather than a filter.
   */
  | { kind: "sending" }
  /**
   * Still pending, but WAITING on its account rather than being attempted
   * (#52): the user must reconnect it ("reconnect"), or the provider is
   * refusing this server's sign-in and the operator must fix it ("provider").
   * The server says which (EmailSubmission.mailroomHeld). Without this state
   * a held message read "Sending…" for as long as it waited.
   */
  | { kind: "waiting"; why: "reconnect" | "provider" }
  /**
   * The provider refused permanently and nothing will retry it (#589). `reply`
   * is the provider's own words where it gave any.
   */
  | { kind: "failed"; reply: string | null };

export interface OutboxRow {
  id: Id;
  emailId: Id;
  /** The recipients, as the row prints them: display name where the message
   * carried one, else the address. To/Cc/Bcc in that order. */
  recipients: string[];
  /** The subject, or null when it is blank. */
  subject: string | null;
  /** The address this goes out as, or null when the identity is not in the store
   * (a submission through an identity this client has not loaded). */
  alias: string | null;
  /** The mail account the submission goes out on, when its identity is in the
   * store — what a waiting row names and reconnects. */
  accountId: Id | null;
  sendAt: string;
  state: OutboxState;
  /**
   * Whether this row's CONTENT is available — which means its recipients, not
   * merely that some Email object for it exists.
   *
   * FALSE IS NOT AN ERROR AND NOT AN EMPTY MESSAGE. The Outbox batches an
   * `Email/get` for the ids it does not hold, and until that answers the row
   * exists with no recipients and no subject — because the SUBMISSION is the
   * thing that has not gone, and hiding the row until its draft loaded would
   * under-report what is waiting. The row says "not loaded" rather than drawing
   * an empty message.
   *
   * ⚠ IT USED TO MEAN "THE EMAIL IS IN THE STORE", AND THAT IS A DIFFERENT
   * QUESTION (#593). `to`/`cc`/`bcc` are body-only server-side, so they are NOT
   * in EMAIL_LIST_PROPERTIES and no list view ever loads them. A draft opened in
   * Drafts is therefore in the store WITHOUT recipients — `loaded` went true,
   * `outboxMissingEmailIds` skipped it, the targeted fetch that does ask for
   * recipients never ran, and the row printed "no recipients" permanently
   * rather than transiently.
   *
   * Observed live: a re-sent draft showing its subject correctly beside "no
   * recipients", which is the signature — a genuinely unloaded row has neither.
   * A message composed fresh was never in the store, so it took the fetch and
   * looked fine, which is why this survived.
   */
  loaded: boolean;
}

/** The name a recipient is printed under. */
// ⚠ THE SAME `name || email` THIS ROW USED TO HAVE (#600). That expression was
// fixed in the reading pane and left here, so an Outbox row still printed a bare
// display name — on the one screen whose entire job is telling you what happened
// to a message you sent, and where "who is this actually going to" is the
// question. Found in a screenshot of the fixed Outbox, which is to say: the
// defect was visible in the evidence that the OTHER half worked.
//
// Shared with the reading pane rather than restated, so the two cannot drift
// again — the mistake that produced #602 and #603 as well.
const nameOf = addressLabel;

/**
 * The Outbox's rows.
 *
 * `emails` and `identities` are the store's maps; a missing entry in either is a
 * fact about what has loaded, never a reason to drop the row (see `loaded`).
 */
/**
 * Which of the four states a row is in.
 *
 * ⚠ FAILURE IS TESTED FIRST, and on `mailroomState` rather than on `undoStatus`.
 * `undoStatus` is RFC 8621's three-value vocabulary, and it maps both "sent" and
 * "failed" onto `final` (internal/store/submissions.go UndoStatus) — so it
 * cannot tell success from refusal. The Mailroom extension is the only field
 * that can, which is why it exists.
 *
 * ⚠ AN UNPARSEABLE `sendAt` IS NOT A REASON TO HIDE A FAILURE. The original code
 * dropped such rows entirely, reasoning that "every row's whole content is a
 * time, and a row whose time we cannot read has nothing to say". That holds for
 * a countdown and not for a refusal: a failed row's content is the provider's
 * message, and discarding it over an unreadable timestamp would reintroduce the
 * silence this whole change is about. Only the time-bearing states need a time.
 */
function outboxState(s: EmailSubmission, at: number, remaining: number): OutboxState {
  if (s.mailroomState === "failed") {
    return { kind: "failed", reply: smtpReplyOf(s) };
  }
  if (s.undoStatus === "pending" && (s.mailroomHeld === "reconnect" || s.mailroomHeld === "provider")) {
    return { kind: "waiting", why: s.mailroomHeld };
  }
  if (!Number.isFinite(at) || remaining <= 0) {
    // Either a time we cannot read, or one that has passed: the send is being
    // attempted now, or is requeued awaiting the next attempt. "Sending" claims
    // strictly less than a countdown through zero would.
    return { kind: "sending" };
  }
  return remaining < UNDO_WINDOW_MS
    ? { kind: "window", secondsLeft: Math.ceil(remaining / 1000) }
    : { kind: "scheduled", sendAt: s.sendAt };
}

/**
 * Whether this Email has been fetched with the recipient properties, as opposed
 * to merely existing in the store (#593).
 *
 * ⚠ `undefined` AND `null` MEAN DIFFERENT THINGS HERE, and the distinction is
 * the whole mechanism. JMAP omits a property that was not requested and returns
 * `null` for a header the message does not carry, so:
 *
 *   undefined      never asked for — the row cannot name anybody YET
 *   null or []     asked for, and there are none — the row's answer is "none"
 *
 * Testing emptiness instead (`!email.to?.length`) would collapse those two and
 * make a Bcc-only message — which legitimately has no To and no Cc — look
 * permanently unfetched, so every tick would re-request it forever. That is the
 * failure a naive fix introduces while still making the reported symptom go
 * away, which is why #593 asks for the negative control rather than the obvious
 * assertion.
 */
function recipientsKnown(email: Email | undefined): boolean {
  if (email === undefined) return false;
  return email.to !== undefined || email.cc !== undefined || email.bcc !== undefined;
}

/**
 * The provider's own words for why a send failed, or null.
 *
 * Before any DSN arrives the server reports one entry under the `"*"` wildcard;
 * afterwards it reports one per recipient. Either way the first entry that
 * carries a reply is the one to print — a failed submission has a single
 * `smtpReply` under `"*"` in the case this exists for, and preferring an
 * explicit per-recipient verdict when one has arrived is strictly better.
 */
function smtpReplyOf(s: EmailSubmission): string | null {
  const ds = s.deliveryStatus;
  if (!ds) return null;
  const entries = Object.entries(ds);
  // A named recipient's verdict beats the wildcard when both are present.
  const named = entries.find(([k, v]) => k !== "*" && (v?.smtpReply ?? "") !== "");
  const wildcard = entries.find(([k, v]) => k === "*" && (v?.smtpReply ?? "") !== "");
  const chosen = named ?? wildcard;
  const reply = chosen?.[1]?.smtpReply?.trim();
  return reply ? reply : null;
}

/**
 * The submissions the Outbox speaks for, newest first.
 *
 * ⚠ THIS EXISTS BECAUSE THE PREDICATE WAS WRITTEN TWICE AND THE COPIES DRIFTED
 * (#602). The header above says "the rail badge is computed from these rows" —
 * it was not. `Sidebar.tsx` filtered `undoStatus === "pending"` on its own, so a
 * FAILED submission counted zero, the rail entry is gated on that count, and the
 * entry therefore disappeared at exactly the moment a failure appeared: the one
 * event the Outbox exists to report was the one that removed the only way into
 * it. Everything below this line was already correct and had tests; the door was
 * shut.
 *
 * So there is now ONE function that answers "what does the Outbox contain", and
 * both the list and the count call it. A new submission state added later cannot
 * reach one caller and miss the other.
 */
export function outboxSubmissions(
  submissions: Readonly<Record<Id, EmailSubmission>>,
): EmailSubmission[] {
  // Only the LATEST submission per message speaks for that message. An older one
  // is history: a failure the user has already answered by re-sending must stop
  // being shown. Submission ids are ULIDs, so lexicographic max IS the newest.
  const latest = new Map<Id, EmailSubmission>();
  for (const s of Object.values(submissions)) {
    const held = latest.get(s.emailId);
    if (!held || held.id < s.id) latest.set(s.emailId, s);
  }

  return [...latest.values()]
    .filter((s) => {
      // A permanent failure needs the user and nothing will retry it — so it
      // stays until the user ACTS on it (#589): by re-sending (above), or by
      // dismissing it (`dismissedAt`, migration 00059), the act for a failure
      // they do not mean to re-send. Before dismissal existed there was no such
      // act: destroy is refused and discarding the draft only soft-trashes it,
      // so a refused send sat here, and in the rail's count, for good.
      if (s.mailroomState === "failed") return !s.dismissedAt;
      // Everything else this view shows is still on its way out. `final` has
      // gone and `canceled` never will, so neither belongs here.
      return s.undoStatus === "pending";
    })
    .sort((a, b) => (a.id < b.id ? 1 : a.id > b.id ? -1 : 0));
}

/**
 * How many rows the Outbox would show — the rail's badge, and whether the rail
 * entry exists at all.
 *
 * Derived from `outboxSubmissions` rather than counted separately, which also
 * fixes a second, quieter error in the old count: it did not collapse to the
 * latest submission per message, so a message that had been re-sent was counted
 * once per attempt.
 */
export function outboxCount(submissions: Readonly<Record<Id, EmailSubmission>>): number {
  return outboxSubmissions(submissions).length;
}

/**
 * The address this submission was actually sent AS.
 *
 * ⚠ THE IDENTITY IS THE WRONG ANSWER ON A CATCH-ALL (#604). A wildcard
 * identity's own address is the PATTERN `*@domain`, so naming the row by its
 * identity printed `as *@example.net` for a message sent as
 * `parcel@example.net` — and it printed it directly above "Change what the
 * server objected to before sending again", for a refusal whose reason was
 * `550 … sender address rejected`. The line that should answer "which address?"
 * was the line hiding it.
 *
 * The envelope's `mailFrom` is what went to SMTP, so it is right for BOTH kinds
 * of identity — for a concrete one it equals the identity's own address. The
 * identity is kept as a fallback for a submission whose envelope is absent
 * (an older server, or a property set that does not carry it), because a
 * roughly-right "as" line beats no line at all.
 */
function senderAddressOf(
  s: EmailSubmission,
  identities: Readonly<Record<Id, Identity>>,
): string | null {
  const envelope = s.envelope?.mailFrom?.email?.trim();
  if (envelope) return envelope;
  return identities[s.identityId]?.email ?? null;
}

export function outboxRows(
  submissions: Readonly<Record<Id, EmailSubmission>>,
  emails: Readonly<Record<Id, Email>>,
  identities: Readonly<Record<Id, Identity>>,
  now: number,
): OutboxRow[] {
  return outboxSubmissions(submissions)
    .map((s) => {
      const email = emails[s.emailId];
      const at = Date.parse(s.sendAt);
      const remaining = at - now;
      const recipients = email
        ? [...(email.to ?? []), ...(email.cc ?? []), ...(email.bcc ?? [])].map(nameOf)
        : [];
      const subject = (email?.subject ?? "").trim();
      return {
        id: s.id,
        emailId: s.emailId,
        recipients,
        subject: subject === "" ? null : subject,
        alias: senderAddressOf(s, identities),
        accountId: identities[s.identityId]?.accountId || null,
        sendAt: s.sendAt,
        state: outboxState(s, at, remaining),
        loaded: recipientsKnown(email),
      };
    });
}

/** The email ids the Outbox has to fetch before its rows can name anybody — the
 * one batch, not one request per row. */
export function outboxMissingEmailIds(rows: readonly OutboxRow[]): Id[] {
  const out: Id[] = [];
  const seen = new Set<Id>();
  for (const row of rows) {
    if (row.loaded || seen.has(row.emailId)) continue;
    seen.add(row.emailId);
    out.push(row.emailId);
  }
  return out;
}
