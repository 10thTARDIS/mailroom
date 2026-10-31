// SPDX-License-Identifier: AGPL-3.0-only

// ARRANGING THE CATEGORY-TAB STRIP (#228) — the pure half.
//
// The strip's order and its enabled set are per-user server state
// (`app.category_tabs`, ADR-021), edited through `CategoryTab/set`. What lives
// here is only the arithmetic between "the user pressed ↑ on Promotions" and the
// patch that expresses it, kept out of the component so it can be tested.

import type { CategoryTab, CategoryTabState, Id } from "./jmap/types";

/**
 * The strip order: `position`, then the tab's own name.
 *
 * ONE DEFINITION, used by both the strip and the settings sheet. Two sorts would
 * be a list you rearrange in one place and read back differently in the other —
 * and the tie-break is where they would drift, because positions are only
 * distinct until something writes an equal one.
 *
 * It mirrors the server's `ORDER BY position, tab` (store.ListCategoryTabs), so
 * a client that has not re-read yet still agrees with the row order it will get.
 */
export function stripOrder(tabs: Record<string, CategoryTabState>): CategoryTabState[] {
  return Object.values(tabs).sort((a, b) =>
    a.position !== b.position ? a.position - b.position : a.tab.localeCompare(b.tab),
  );
}

/**
 * Primary cannot be switched off.
 *
 * It is the residue AND the destination of the disabled-tab fold, so a disabled
 * Primary would be a tab that is off and still holding mail. `app.category_tabs`
 * makes that unrepresentable with a CHECK and `UpdateCategoryTab` refuses it
 * before the constraint can fire — this function exists so the UI never offers
 * the control at all. **A control that always fails is worse than no control**:
 * it invites a click, spends a round trip, and answers with an error the user
 * could not have avoided.
 */
export function canDisable(tab: CategoryTab): boolean {
  return tab !== "primary";
}

/**
 * The `CategoryTab/set` update patch that moves one tab one slot.
 *
 * `delta` is -1 for earlier in the strip and +1 for later. Returns an EMPTY
 * object when the move is impossible (unknown id, already at the end) — the
 * caller sends nothing rather than a request that changes nothing.
 *
 * ⚠ IT RENUMBERS RATHER THAN SWAPPING, and that is deliberate. A swap of two
 * `position` values is correct only while the positions are already distinct;
 * the moment two rows share one, display order falls to the name tie-break and
 * swapping the numbers moves nothing on screen. Renumbering the whole strip
 * 0…n-1 from the order the user is actually looking at makes ties impossible
 * from the first move onwards, so the arrangement is self-healing rather than
 * dependent on how the rows were seeded.
 *
 * Only the rows whose position actually changed are returned. `UpdateCategoryTab`
 * would drop the others anyway (`IS DISTINCT FROM`), so this is not correctness
 * — it keeps the request honest about what it is changing.
 */
export function moveTab(
  tabs: Record<string, CategoryTabState>,
  id: Id,
  delta: -1 | 1,
): Record<Id, { position: number }> {
  const ordered = stripOrder(tabs);
  const from = ordered.findIndex((t) => t.id === id);
  if (from < 0) return {};
  const to = from + delta;
  // MEASURED, not assumed: only the LOWER half of this guard does work. Removing
  // `to < 0` reddens a test — `splice(-1, 0, …)` inserts before the LAST element,
  // so moving the first tab up would send it to the end. Removing
  // `to >= ordered.length` reddens nothing, because splice clamps past the end
  // and the row lands back where it started. It stays as the statement of intent
  // for the pair, but it is not what makes the end of the strip safe.
  if (to < 0 || to >= ordered.length) return {};

  const moved = [...ordered];
  const [row] = moved.splice(from, 1);
  if (!row) return {};
  moved.splice(to, 0, row);

  const patch: Record<Id, { position: number }> = {};
  moved.forEach((t, i) => {
    if (t.position !== i) patch[t.id] = { position: i };
  });
  return patch;
}

/** How many of a user's labels feed each tab. Derived from the labels the client
 * already holds, so it is a LABEL count and never a message count — saying
 * "3 labels" is a fact about the mapping; saying "3 messages" would be a claim
 * about the mailbox this client has not measured. */
export function labelsPerTab(
  labels: Iterable<{ tab: CategoryTab | null }>,
): Record<string, number> {
  const counts: Record<string, number> = {};
  for (const l of labels) {
    if (l.tab) counts[l.tab] = (counts[l.tab] ?? 0) + 1;
  }
  return counts;
}
