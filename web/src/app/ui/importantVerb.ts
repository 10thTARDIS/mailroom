// SPDX-License-Identifier: AGPL-3.0-only

// THE DIRECTION A BULK IMPORTANCE VERB TAKES (#921).
//
// The desk's and the tray's selection bars gained "Mark as important" for a
// whole selection, and the verb that already corrected ONE row had to answer
// the question a bar asks for MANY: which way does the gesture go?
//
// The old answer — "the first row decides, the rest follow its opposite" —
// was right for one row and wrong for a selection: a mixed selection whose
// FIRST row was already important got marked NOT important, silently un-marking
// the rest of it. One gesture, one direction, and the direction must be one
// the user can read off the bar before they press it.

/** The direction a bulk importance verb takes (#921): mark important unless
 * EVERY selected message already is, so a mixed selection is never silently
 * un-marked. An empty list answers "mark important": the bars only exist with
 * a selection, and a value computed on an empty one should point forward, not
 * backward. */
export function importantTarget(flags: readonly boolean[]): boolean {
  if (flags.length === 0) return true;
  return !flags.every((flag) => flag);
}

/** What a bulk importance gesture does (#921): the direction, by
 * `importantTarget`, and the rows that actually CHANGE — the only ones sent,
 * and the only ones its Undo sends back the other way. Rows already where the
 * gesture points are left out, so Undo can never flip a row the user did not
 * change. `flags[i]` is whether `ids[i]` is important now. */
export function importantChange<T>(
  ids: readonly T[],
  flags: readonly boolean[],
): { important: boolean; changed: T[] } {
  const important = importantTarget(flags);
  return { important, changed: ids.filter((_, i) => (flags[i] ?? false) !== important) };
}
