// SPDX-License-Identifier: AGPL-3.0-only

// THE UNDO LOG (#375 increment 6). DOM-free and framework-agnostic (spec §12):
// no React, no timers of its own, no `Date.now()` — every method that needs the
// time is handed it, which is the only way the expiry rules below are testable
// at all.
//
// WHAT IT IS FOR. Every destructive gesture on the desk is optimistic: the row
// leaves the moment you press the key, and the round trip only decides whether
// the intent survives (core/actions.ts). That is what makes the desk fast, and
// it is also what makes a mis-press expensive — the message is gone from the
// screen before you have finished regretting it. So each verb registers its
// INVERSE here as it acts, and the toast that says what happened carries the
// button that runs it.
//
// AN INVERSE IS NOT A SNAPSHOT. Nothing in this file remembers what a message
// looked like before; it holds a CLOSURE that performs the opposite intent
// (archive ⇄ restore-to-inbox, flag ⇄ unflag, label-added ⇄ label-removed).
// That matters because the state a snapshot would restore is the SERVER's, and
// re-asserting a whole Email object over whatever else has happened since is
// how an undo silently reverts somebody else's change. An intent delta cannot
// do that — it is the same shape as the action it undoes, and it goes down the
// same queue.
//
// THREE ENTRIES, AND THEY EXPIRE. Both bounds are deliberate:
//
//   * the CAP exists because a log that grows is a promise that Ctrl+Z walks
//     backwards through your whole session, and none of these inverses is
//     correct once the mail has moved on — un-archiving something you archived
//     forty actions ago restores it to an inbox you have since triaged.
//   * the TTL exists for the same reason and one more: the toast is the only
//     place the offer is visible, so an entry that outlived every toast would
//     be a keystroke with an invisible, unpredictable effect. The countdown on
//     the toast is this deadline, printed.
//
// The pause (`extend`) is the accessibility half of that: reading a toast takes
// longer than eight seconds for some people, and a deadline that runs out while
// you are still deciding is a deadline that only fast readers get. While the
// toast has focus or the pointer is over it, the surface pushes the deadline
// out — so the countdown stops rather than the offer expiring under the cursor.

/** One reversible thing the user did. */
export interface UndoEntry {
  /** Monotonic within one log — what a toast carries so it can name its own
   * entry after the log has moved on. */
  id: number;
  /** What was done, in the words the toast prints ("Archived 3 messages"). */
  label: string;
  /**
   * The opposite intent. Resolves TRUE when it was applied (which for a queued
   * mutation means "accepted locally and queued", the same bar the original
   * action cleared) and FALSE when it could not be — the caller must not then
   * claim anything was undone.
   */
  inverse: () => Promise<boolean>;
  /** Wall-clock ms after which this entry is no longer offered. */
  expiresAt: number;
}

/** How long an undo stays on offer. Long enough to read a toast and reach for
 * the button; short enough that the inverse is still about the mail as it is
 * now. The pause (`extend`) is what makes this a floor rather than a ceiling. */
export const UNDO_TTL_MS = 8_000;

/** How many entries the log keeps. See the header: past a handful, an inverse
 * is a statement about a screen that has since changed. */
export const UNDO_KEEP = 3;

export class UndoLog {
  /** Newest first. An array rather than a map because "the latest one" is the
   * only query that is not by id, and it is the one the keyboard makes. */
  private entries: UndoEntry[] = [];
  private nextId = 1;

  /**
   * Records an inverse and returns the entry, which is what the toast is built
   * from.
   *
   * NEWEST WINS AND THE OLDEST FALLS OFF: pushing prunes what has expired
   * first, so a burst of actions after a long pause does not spend two of the
   * three slots on offers nobody can still take.
   */
  push(
    label: string,
    inverse: () => Promise<boolean>,
    now: number,
    ttlMs: number = UNDO_TTL_MS,
  ): UndoEntry {
    this.prune(now);
    const entry: UndoEntry = { id: this.nextId, label, inverse, expiresAt: now + ttlMs };
    this.nextId += 1;
    this.entries = [entry, ...this.entries].slice(0, UNDO_KEEP);
    return entry;
  }

  /**
   * The most recent entry still on offer — what Ctrl/Cmd+Z reaches.
   *
   * A READ, not a sweep: it does not drop the expired entries it steps over.
   * Pruning is what `prune` and `push` are for, and a getter that mutated would
   * make "what does Ctrl+Z do" depend on whether anything had asked recently.
   */
  latest(now: number): UndoEntry | null {
    return this.entries.find((e) => e.expiresAt > now) ?? null;
  }

  /**
   * Removes an entry and returns it. THIS IS WHAT MAKES AN UNDO ONE-SHOT: the
   * button and the shortcut both come through here, so a double-press, or a
   * press on a toast that is still on screen after the shortcut already ran,
   * finds nothing and does nothing rather than applying the inverse twice.
   * (Archive ⇄ restore is idempotent, but "restore, then archive again because
   * the next entry surfaced" is not, and neither is a re-file.)
   *
   * Expiry is deliberately NOT checked here. Taking is an explicit act on a
   * known id — the caller found it through `latest(now)`, or it is holding a
   * toast that is only on screen while the entry is live — and refusing a click
   * that landed a millisecond after the deadline would be a button that
   * sometimes silently does nothing. `prune` is what enforces the deadline.
   */
  take(id: number): UndoEntry | null {
    const found = this.entries.find((e) => e.id === id) ?? null;
    if (found) this.entries = this.entries.filter((e) => e.id !== id);
    return found;
  }

  /**
   * Pushes an entry's deadline out — the PAUSE. Called on every tick while the
   * toast has focus or the pointer, so the countdown holds at whatever it was
   * showing instead of expiring under the reader.
   *
   * Only ever touches an entry that is still IN the log: once pruned, an offer
   * is gone, and a hover that resurrected it would make the deadline advisory.
   */
  extend(id: number, now: number, ttlMs: number = UNDO_TTL_MS): void {
    this.entries = this.entries.map((e) =>
      e.id === id ? { ...e, expiresAt: now + ttlMs } : e,
    );
  }

  /** Drops everything past its deadline. */
  prune(now: number): void {
    this.entries = this.entries.filter((e) => e.expiresAt > now);
  }

  /** How many offers are on the books, expired or not — for tests and for the
   * cap's own assertion. */
  get size(): number {
    return this.entries.length;
  }
}

/**
 * The number the toast prints: whole seconds left, never negative.
 *
 * ROUNDED UP, so a fresh eight-second offer reads `8 s` on its first frame
 * rather than `7 s` (a countdown that starts one below the number it was given
 * looks like it lost a second before you saw it), and so it only reaches `0` at
 * the instant the offer actually ends.
 */
export function undoSecondsLeft(expiresAt: number, now: number): number {
  return Math.max(0, Math.ceil((expiresAt - now) / 1000));
}
