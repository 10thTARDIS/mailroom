// SPDX-License-Identifier: AGPL-3.0-only

// AUTOSAVE (#448, design pass-2 §7): every 15 s while dirty, and on blur of the
// body.
//
// THE SERVER FACT THAT SHAPES THIS. A JMAP Email is immutable apart from its
// keywords and mailboxIds (RFC 8621 §4.6), so a draft is EDITED by creating the
// new version and DESTROYING the old one in the same `Email/set` call —
// internal/api/draft.go says so at the top, and `store.RetireDraft` is what makes
// the draft-conflict rule fall out of it. So every autosave mints a NEW draft id,
// and the previous one must travel in the same call or the Drafts folder fills up
// with one row per fifteen seconds of typing. `MailActions.saveDraft` is the call;
// this module is only the DECISION of whether to make it.
//
// AND THE OTHER SERVER FACT, which is why `draftEmpty` is not merely a nicety:
// an autosave of an untouched composer would fail or litter every fifteen
// seconds and put `Not saved · …` under a composer the user has not started
// writing — a red state produced entirely by us.
//
// ⚠ THAT RULE ONCE INCLUDED "no recipients", and it cost people their writing
// (#601). `createDraft` used to refuse a draft with no recipients at all, so
// `draftEmpty` reported one as empty and nothing — not the interval, not the
// blur, not the flush on the way out — saved a word of it. A FORWARD opens with
// no recipients, which is exactly when someone writes the covering note first.
// The server now accepts such a draft and the rule lives where it is true:
// submission.
//
// SEPARATED FROM THE COMPONENT for the usual reason (no jsdom in this repo's web
// tests) and for a better one: "did anything change?" and "should we save now?"
// are exactly the two questions that, answered wrongly, produce the two failures
// this feature can have — a draft saved on every keystroke, or a draft never
// saved at all. Neither is visible in a screenshot.

/** The composer's state, as far as saving is concerned. Flat and comparable:
 * everything a draft create carries, and nothing that changes for other reasons
 * (focus, the picker being open, an in-flight upload). */
export interface DraftSnapshot {
  from: string;
  to: readonly string[];
  cc: readonly string[];
  bcc: readonly string[];
  subject: string;
  /** The plain-text body (the text/plain part of the draft). */
  text: string;
  /** The HTML body, empty in plain-text mode. */
  html: string;
  /** The blob ids of the attachments, in order. Ids rather than objects: the
   * name and size cannot change without the id changing. */
  attachmentIds: readonly string[];
}

function sameList(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

/**
 * Whether anything worth saving changed.
 *
 * ⚠ FIELD BY FIELD, NOT A JSON COMPARE. `JSON.stringify` would make key ORDER
 * part of the answer, so a snapshot rebuilt in a different order would read as
 * dirty forever — an autosave every fifteen seconds for a composer nobody is
 * touching. It would also silently start comparing any field added to the
 * interface later, including ones that are not part of the draft.
 */
export function draftDirty(prev: DraftSnapshot | null, next: DraftSnapshot): boolean {
  if (prev === null) return true;
  return !(
    prev.from === next.from &&
    prev.subject === next.subject &&
    prev.text === next.text &&
    prev.html === next.html &&
    sameList(prev.to, next.to) &&
    sameList(prev.cc, next.cc) &&
    sameList(prev.bcc, next.bcc) &&
    sameList(prev.attachmentIds, next.attachmentIds)
  );
}

/**
 * Whether this composer has nothing worth saving.
 *
 * "Empty" is the ordinary meaning: no subject, no body, no attachments.
 *
 * ⚠ NO RECIPIENTS USED TO MEAN EMPTY, whatever had been typed (#601), because
 * the server refused such a draft outright. The reasoning was sound and the
 * consequence was not: a FORWARD opens with no recipients — a reply seeds them
 * from the parent, a forward cannot — so writing a note at the top of one and
 * closing the composer discarded it. Every path consults this function,
 * including the flush on the way out that exists precisely so "up to fifteen
 * seconds of typing must not go with it", so nothing saved it. Silently losing
 * what someone wrote is the one thing a composer must never do.
 *
 * The server now accepts a recipient-less draft (internal/compose/build.go), and
 * the "nobody to send it to" rule lives where it is true: submission.
 *
 * ⚠ THE EMPTY CASE IS STILL LOAD-BEARING and is the assertion to protect. An
 * untouched composer must still save nothing, or opening and closing a compose
 * window litters Drafts with blank messages — and a draft costs a round trip, a
 * server-side version and an IMAP APPEND, so "harmless" it is not.
 */
export function draftEmpty(snap: DraftSnapshot): boolean {
  return (
    snap.subject.trim() === "" &&
    snap.text.trim() === "" &&
    snap.html.trim() === "" &&
    snap.attachmentIds.length === 0
  );
}

/** Why the scheduler did or did not save, so the decision can be asserted rather
 * than inferred from a boolean. */
export type AutosaveReason =
  | "empty"
  | "untouched"
  | "clean"
  | "in-flight"
  | "too-soon"
  | "blur"
  | "interval";

export interface AutosaveState {
  /** Whether the snapshot differs from the last one SAVED (draftDirty above). */
  dirty: boolean;
  /** Whether the composer holds nothing saveable (draftEmpty above). */
  empty: boolean;
  /**
   * Whether the USER has changed anything in this composer (#614).
   *
   * ⚠ NOT DERIVABLE FROM THE SNAPSHOT, and that is why it is its own input. A
   * reply or forward opens NON-empty — `Re:`/`Fwd:`, the quote, a forward's
   * attachments — so `empty` never stops it, and with nothing saved yet `dirty`
   * is true from the first render: an untouched reply was saved within one tick.
   * Comparing against the opening snapshot does not rescue it either, because the
   * composer keeps changing itself after mount: the signature is re-seeded when
   * the From identity resolves, and the alias hint can change From. Only the
   * user's own actions may set this; Compose.tsx says which ones do.
   */
  touched: boolean;
  /** A save already in flight. */
  saving: boolean;
  /** When the last save was ATTEMPTED (not when it succeeded), or null if none
   * has been. Attempted, so a failing server is retried on the interval rather
   * than hammered on every tick. */
  lastAttemptAt: number | null;
  now: number;
  /** The interval, 15_000 in the composer. A parameter so the test does not have
   * to wait fifteen seconds to state the rule. */
  intervalMs: number;
  /** True when this decision was provoked by the body losing focus, which is the
   * one trigger that does not wait for the interval: the user has stopped typing
   * and may be about to navigate away. */
  onBlur: boolean;
}

/**
 * Whether to save right now.
 *
 * THE ORDER OF THE GUARDS IS THE BEHAVIOUR:
 *
 *   empty     — never save one, blur or not. THE SERVER WOULD ACCEPT IT:
 *               compose.Build only requires a From address (internal/compose
 *               /build.go), nothing about subject/body/attachments. This is
 *               the client's own rule, the one thing standing between an idle
 *               compose window and a blank row in Drafts every fifteen
 *               seconds.
 *   untouched — the user has not edited anything (#614). Opening a reply or a
 *               forward and closing it again must not leave a draft behind.
 *   clean     — nothing changed since the last save; a save would mint a new
 *               draft id for identical bytes.
 *   in-flight — one at a time. Two overlapping saves each destroy the id they
 *               were told about, and the second's destroy targets a draft the
 *               first already retired.
 *   blur      — save now.
 *   too-soon  — otherwise wait out the interval.
 */
export function autosaveDecision(s: AutosaveState): { save: boolean; reason: AutosaveReason } {
  if (s.empty) return { save: false, reason: "empty" };
  if (!s.touched) return { save: false, reason: "untouched" };
  if (!s.dirty) return { save: false, reason: "clean" };
  if (s.saving) return { save: false, reason: "in-flight" };
  if (s.onBlur) return { save: true, reason: "blur" };
  if (s.lastAttemptAt !== null && s.now - s.lastAttemptAt < s.intervalMs) {
    return { save: false, reason: "too-soon" };
  }
  return { save: true, reason: "interval" };
}
