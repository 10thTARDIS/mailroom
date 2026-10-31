// SPDX-License-Identifier: AGPL-3.0-only

// WHAT FEEDS AN AREA, AND WHAT FEEDS NOTHING (#374 increment 3).
//
// An area is fed three ways (core/jmap/types.ts Area): its OWN label, which is
// immutable and is what "placing a message in the area" actually does; any of the
// five CATEGORY TABS; and any number of OTHER labels. The Areas row lets a user
// change the second and third, and every one of those edits goes through
// `feedPatch` below.
//
// ⚠ `tabs` AND `labelIds` ARE REPLACE-ALL, AND THAT IS THE WHOLE REASON THIS
// MODULE IS PURE AND TESTED. internal/api/area.go: "tabs/labelIds are REPLACE-ALL
// when either is present in the patch". So a control that toggles ONE tab must
// send the COMPLETE array of tabs it wants afterwards. Sending only the tab that
// changed does not fail — it succeeds, and silently unsubscribes the area from
// every other tab it had. That is the #159 shape exactly: a write that returns
// success while destroying the thing it was editing, with nothing on screen
// saying so until the next reload. It is worth a unit test on its own.
//
// A FEED IS EXCLUSIVE. The server refuses a tab or label already claimed by
// another area (store.ErrFeedTaken, surfaced verbatim because its message names
// the other area). Nothing here tries to predict that: the client has the areas
// it has, the server has the truth, and a client-side guess would be a second
// opinion that eventually disagrees. The row shows the refusal.
//
// THE OWN LABEL IS NEVER A FEED IN `labelIds`. The server refuses it ("the area's
// own label is already its feed") and the type says `labelIds` excludes it. So
// `feedPatch` strips it defensively on the way out, and `labelsInNoArea` counts it
// as claimed on the way in.
//
// THREE MORE PURE HELPERS RIDE ALONG, and they are here rather than in three files
// because each is one decision the Areas row makes about an area and none of them
// is big enough to be a module: which labels are homeless, what order the arrows
// produce, and which rules place mail in an area.

import type { Area, CategoryTab, Id, Label, Rule } from "../../../core/jmap/types";

/** One edit to an area's feeds. Three verbs rather than "here is the new set",
 * because the control is a toggle and a multi-select: the caller knows what was
 * clicked, and working out the resulting arrays is exactly the part that must not
 * be written out at each call site. */
export type FeedChange =
  | { kind: "toggleTab"; tab: CategoryTab }
  | { kind: "addLabel"; labelId: Id }
  | { kind: "removeLabel"; labelId: Id };

/** The patch an `Area/set update` carries for a feed edit. BOTH arrays, always —
 * see the header. */
export interface FeedPatch {
  tabs: CategoryTab[];
  labelIds: Id[];
}

/**
 * The COMPLETE feed arrays an area should have after one edit.
 *
 * Both arrays are returned whichever one the edit touched, so the patch is
 * self-contained: a caller cannot accidentally send a tab change that omits
 * `labelIds` and wipes the area's label feeds.
 *
 * Order is preserved for the untouched entries and an addition goes on the end,
 * so the arrays do not reshuffle on every edit — the server stores them as a set,
 * but a client that re-sorted would make every write look like a bigger change
 * than it was in a diff.
 */
export function feedPatch(area: Area, change: FeedChange): FeedPatch {
  // Copied, never mutated: `area` is a store object, and the store is shared with
  // every other surface rendering this area.
  const tabs = [...area.tabs];
  // The own label can never be in here. Filtered on the way IN as well as out,
  // because a server that ever sent it would otherwise have this client echo it
  // straight back into a patch the server refuses.
  const labelIds = area.labelIds.filter((id) => id !== area.labelId);

  switch (change.kind) {
    case "toggleTab": {
      const at = tabs.indexOf(change.tab);
      if (at === -1) tabs.push(change.tab);
      else tabs.splice(at, 1);
      break;
    }
    case "addLabel": {
      // The own label is not addable — it is already the feed, and the server
      // refuses the patch outright rather than ignoring it, which would lose the
      // whole edit including the tabs beside it.
      if (change.labelId !== area.labelId && !labelIds.includes(change.labelId)) {
        labelIds.push(change.labelId);
      }
      break;
    }
    case "removeLabel": {
      const at = labelIds.indexOf(change.labelId);
      if (at !== -1) labelIds.splice(at, 1);
      break;
    }
  }

  return { tabs, labelIds };
}

/**
 * The labels no area claims — neither as an area's own label nor as one of its
 * `labelIds`.
 *
 * ⚠ AN AREA'S OWN LABEL IS CLAIMED. It does not appear in `labelIds` (the type
 * says so), so a filter that only looked there would report every area's own
 * label as homeless — which is the most misleading possible answer, since those
 * are the labels most firmly attached to an area. It would also offer "assign
 * Receipts to Receipts", a write the server refuses.
 *
 * Sorted by name so the list does not reorder between renders. Ties break on id,
 * which is total, so the order is total.
 *
 * AN EMPTY RESULT WITH NO AREAS IS NOT WHAT THIS RETURNS, and the caller must not
 * ask: with no areas loaded EVERY label is "in no area", which is arithmetically
 * true and useless to print while the areas are still arriving. The Areas row
 * only renders this section once it has areas (SettingsRoute.tsx draws the same
 * line for the state-line clause).
 */
export function labelsInNoArea(
  labels: readonly Label[],
  areas: readonly Area[],
): Label[] {
  const claimed = new Set<Id>();
  for (const area of areas) {
    claimed.add(area.labelId);
    for (const id of area.labelIds) claimed.add(id);
  }
  return labels
    .filter((label) => !claimed.has(label.id))
    .slice()
    .sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
}

/**
 * The list with the item at `index` moved one place.
 *
 * ⚠ THIS IS WHAT `↑` AND `↓` PRODUCE, AND THE WRITE THAT FOLLOWS RENUMBERS THE
 * WHOLE LIST, because a one-sided position write does not reorder anything:
 * `app.areas.position` has no uniqueness constraint (db/schema.sql) and the store
 * orders by `position, id`, so setting one area's position to its neighbour's
 * leaves them TIED and the tie breaks on creation order. Producing the intended
 * ORDER here and letting core/actions.ts reorderAreas assign `0…n-1` is what makes
 * the arrow mean the same thing every time.
 *
 * An out-of-range move returns the list unchanged rather than clamping: the ends'
 * buttons are disabled, and a clamp would turn a mis-click at the end of the list
 * into a write that renumbers everything for nothing.
 */
export function movedByOne<T>(items: readonly T[], index: number, delta: -1 | 1): T[] {
  const to = index + delta;
  if (index < 0 || index >= items.length || to < 0 || to >= items.length) return [...items];
  const next = [...items];
  const [item] = next.splice(index, 1);
  if (item !== undefined) next.splice(to, 0, item);
  return next;
}

/**
 * The rules that place mail in this area — the ones whose `addLabel` action
 * applies the area's OWN label.
 *
 * ⚠ THE TARGET IS THREE THINGS AT ONCE. `RuleAction.label` is documented as "a
 * label id, a keyword atom, or a label name" (core/jmap/types.ts), so a rule
 * written through the editor names one and a rule written by hand may name
 * another, and matching only the id would report "no rules" for an area three
 * rules feed. All three forms are compared; the keyword and the name come from
 * the Label object, so an area whose label has not loaded matches on id alone
 * rather than guessing.
 *
 * READ-ONLY, and it is a JOIN rather than a stored fact: nothing on `Rule` says
 * "this rule feeds that area". The area's own label IS the placement (types.ts
 * "areas are labels"), which is what makes this derivable at all.
 */
export function rulesForArea(
  rules: readonly Rule[],
  area: Area,
  ownLabel: Label | undefined,
): Rule[] {
  const targets = new Set<string>([area.labelId]);
  if (ownLabel) {
    if (ownLabel.keyword) targets.add(ownLabel.keyword);
    if (ownLabel.name) targets.add(ownLabel.name);
  }
  return rules.filter((rule) =>
    rule.actions.some((a) => a.type === "addLabel" && a.label !== undefined && targets.has(a.label)),
  );
}
