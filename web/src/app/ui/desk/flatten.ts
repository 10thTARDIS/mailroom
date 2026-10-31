// SPDX-License-Identifier: AGPL-3.0-only

// THE DESK AS ONE FLAT LIST (#375 increment 3).
//
// The desk draws several lists at once — an area per section, Needs sorting,
// the line, a drawer per area — and it has to virtualise them, because a drawer
// legitimately holds months of mail (the sweep is what keeps it from growing
// without bound, and the sweep defaults to off). Virtualising N nested lists is
// N scroll containers, N sets of arithmetic and N places for the keyboard
// cursor to fall between; virtualising ONE list of equal-height items is the
// arithmetic already written and tested in core/desk.ts (virtualWindow).
//
// So the whole tab becomes a single `DeskItem[]` in display order, every item
// one row tall (`--desk-row-h`), and everything downstream — the window, the
// cursor, `j`/`k` walking ACROSS section boundaries and into an open drawer —
// is an index into that one array. This module is that translation, and it is
// pure: it takes what the store already decided (which ids are visible, what
// the server counted) and returns the order they are drawn in. No React, no
// measurement, nothing that knows a pixel.

import type { Id } from "../../../core/jmap/types";
import type { DeskTab } from "../../../core/desk";

/**
 * One drawn row of the desk, whatever kind of thing it is.
 *
 * EVERY KIND IS ONE ROW TALL. That is not a rendering coincidence, it is the
 * contract that makes `virtualWindow`'s arithmetic correct: the window is
 * computed from `index * rowHeight`, so an item that rendered taller would put
 * every item after it at the wrong offset — with no error, just rows drawn on
 * top of each other. A kind that genuinely needs more room has to become two
 * items, not a taller one.
 */
export type DeskItem =
  /** An area's heading. `count` is what the SUMMARY says the area holds (the
   * number the heading prints); `total` is what the SECTION's own query said,
   * which is what "Show 50 more of 218" is a bound on. They are two different
   * measurements and are deliberately not collapsed. */
  | {
      kind: "heading";
      section: string;
      areaId: Id | null;
      count: number;
      total: number;
      /** Folded to its heading (#375 increment 7). Carried on the ITEM rather
       * than looked up again by the renderer, so what `aria-expanded` says and
       * what the list actually emitted are the same decision. */
      collapsed: boolean;
    }
  | { kind: "row"; id: Id; section: string }
  | { kind: "more"; section: string; remaining: number }
  /** A section whose query failed, IN PLACE OF ITS ROWS. An errored section
   * must never render as an empty one: "this area has no mail" and "we could
   * not ask" are different facts, and only one of them is good news. */
  | { kind: "error"; section: string; message: string }
  | { kind: "line" }
  | { kind: "drawer"; areaId: Id; open: boolean }
  | { kind: "empty" };

/** One surface section — an area's, or Needs sorting's (`areaId: null`) — as
 * the store currently has it. */
export interface SectionView {
  /** desk.sectionKey(), so an item can name the section it belongs to without
   * carrying the section itself. */
  key: string;
  areaId: Id | null;
  /** The rows to draw: the server's ids after the overlay (store.sectionIds). */
  ids: Id[];
  /** What the server's own `calculateTotal` said this section holds. */
  total: number;
  /** What the heading prints (Area/getSummary through desk.areaCounts). */
  count: number;
  error: string | null;
}

/** One drawer on Everything else. Closed drawers carry no ids — nothing has
 * been fetched for them, and their count comes from the summary. */
export interface DrawerView {
  areaId: Id;
  key: string;
  open: boolean;
  ids: Id[];
  total: number;
  error: string | null;
}

/**
 * What Needs sorting is called in the collapsed set.
 *
 * Its `areaId` is null — that is what "no area claims this mail" means — and
 * null is not a key. A literal string is, it survives the JSON-free
 * localStorage round trip in app/prefs.ts unchanged, and it cannot collide with
 * a ULID (ADR-007: Crockford base32, uppercase, 26 characters).
 */
export const UNSORTED = "unsorted";

/** The collapsed-set key for a section, which is its area or the sentinel. */
export function collapseKey(areaId: Id | null): Id | typeof UNSORTED {
  return areaId ?? UNSORTED;
}

export interface DeskModel {
  tab: DeskTab;
  /** Review first: every area in draw order, Needs sorting last
   * (desk.deskSections). */
  sections: SectionView[];
  /** Everything else: one per area, position order. */
  drawers: DrawerView[];
  /** How many rows the in-tray strip currently has. The strip is not part of
   * the virtualised list — it is chrome above it — but "is this desk empty"
   * is a question about the tray as well as the sections. */
  trayCount: number;
  /**
   * The sections folded to their headings (#375 increment 7), by area id —
   * with `"unsorted"` standing for Needs sorting, which has no area to be
   * named by.
   *
   * A SET RATHER THAN A FLAG ON EACH SECTION, because this is a per-DEVICE
   * memory (app/prefs.ts) about a list the server knows nothing about: the
   * sections are rebuilt from the store on every render, and a flag inside
   * them would have to be re-applied at every rebuild. Optional so every
   * existing caller — and every existing test — means "nothing is folded".
   */
  collapsed?: ReadonlySet<Id | typeof UNSORTED>;
}

/**
 * Whether Review first has genuinely nothing to show.
 *
 * A section with an ERROR is not empty, it is UNKNOWN. Counting it as empty
 * would put "Nothing to review" over a query that failed — the same wrong
 * answer as rendering an errored section as an empty list, which is the one
 * thing a mail client must never do (core/sync.ts says so where the desk's
 * per-section errors are set).
 */
function reviewIsEmpty(model: DeskModel): boolean {
  if (model.trayCount > 0) return false;
  return model.sections.every((s) => s.ids.length === 0 && s.error === null);
}

/**
 * The tab, flattened, in display order.
 *
 * Review first:  section heading, its rows, its "show more" — for every area,
 *                including the ones holding NOTHING (an empty area still shows
 *                its heading with 0, because the user has to be able to see
 *                that the partition is whole) — then THE LINE, last, because on
 *                this tab the line is the floor: everything above it is what
 *                you are being shown, and the drawers are on the other side.
 *
 * Everything else: THE LINE first — here it is the ceiling, with the drawers
 *                hanging below it — then one closed row per drawer, with its
 *                rows underneath only when it is open. A closed drawer
 *                contributes exactly one item, which is what lets the first
 *                paint of this tab happen without waiting for any drawer's
 *                contents.
 *
 * THE EMPTY STATE REPLACES THE SECTIONS, it does not sit above them. With
 * nothing in the tray and nothing in any area, a column of a dozen headings
 * each saying `0` states the same fact twelve times; the one sentence
 * ("Nothing in the tray. Nothing to review. N in drawers.") says it once and
 * says the third part, which the headings cannot. The tabs and the line remain
 * either way — they are how you get to the mail that IS there.
 */
export function flatten(model: DeskModel): DeskItem[] {
  const items: DeskItem[] = [];

  if (model.tab === "review") {
    if (reviewIsEmpty(model)) {
      items.push({ kind: "empty" });
      items.push({ kind: "line" });
      return items;
    }
    for (const section of model.sections) {
      const collapsed = model.collapsed?.has(collapseKey(section.areaId)) ?? false;
      items.push({
        kind: "heading",
        section: section.key,
        areaId: section.areaId,
        count: section.count,
        total: section.total,
        collapsed,
      });
      // FOLDED: THE HEADING AND NOTHING ELSE (#375 increment 7). Not the rows,
      // not the "show more", and not the section's error either — which is the
      // one exception worth stating out loud, because "an errored section must
      // never render as an empty one" is the rule two comments up. It does not
      // render as an empty one: it renders as a FOLDED one, with the summary's
      // count still printed beside its name and the fold one tap from being
      // undone, and the connection state is on the header sentence above
      // regardless. What the user hid stays hidden until they ask for it back.
      if (collapsed) continue;
      if (section.error !== null) {
        items.push({ kind: "error", section: section.key, message: section.error });
        continue;
      }
      for (const id of section.ids) items.push({ kind: "row", id, section: section.key });
      // A BOUND, REPORTED. `ids.length < total` means the window is a slice of
      // the section and not the section, so the button says how much is
      // missing rather than the list quietly ending (CLAUDE.md: a bounded
      // result must report its bound).
      if (section.ids.length < section.total) {
        items.push({
          kind: "more",
          section: section.key,
          remaining: section.total - section.ids.length,
        });
      }
    }
    items.push({ kind: "line" });
    return items;
  }

  items.push({ kind: "line" });
  if (model.drawers.length === 0) {
    // No areas at all: there are no drawers to draw and no sections either, so
    // the tab would otherwise be a line over nothing.
    items.push({ kind: "empty" });
    return items;
  }
  for (const drawer of model.drawers) {
    items.push({ kind: "drawer", areaId: drawer.areaId, open: drawer.open });
    if (!drawer.open) continue;
    if (drawer.error !== null) {
      items.push({ kind: "error", section: drawer.key, message: drawer.error });
      continue;
    }
    for (const id of drawer.ids) items.push({ kind: "row", id, section: drawer.key });
    if (drawer.ids.length < drawer.total) {
      items.push({
        kind: "more",
        section: drawer.key,
        remaining: drawer.total - drawer.ids.length,
      });
    }
  }
  return items;
}

/**
 * What the row-walking helpers below need of an item list.
 *
 * DELIBERATELY WIDER THAN `DeskItem`. The TRAY page (#375 increment 4) is a
 * second virtualised list — group headings by how long each thing has waited,
 * rows underneath — and `j`/`k` must skip its headings exactly as they skip the
 * desk's. Two copies of "walk forwards to the next item whose kind is row"
 * would be two chances to get the end-of-list clamp subtly different, which the
 * user would feel as the cursor wrapping on one screen and not the other. So
 * the helpers take the shape they actually depend on: a discriminated list in
 * which the rows, and only the rows, carry an `id`.
 */
export interface RowWalkable {
  kind: string;
  id?: Id;
}

/** Every row id in the flattened order — what a select-all on this tab means. */
export function rowIds(items: readonly RowWalkable[]): Id[] {
  const out: Id[] = [];
  for (const item of items) if (item.kind === "row" && item.id) out.push(item.id);
  return out;
}

/** Where a row sits in the ITEM list (not the row list) — the index the
 * virtual window is expressed in, so scrolling the cursor into view is one
 * multiplication. -1 when the cursor is not on this tab. */
export function rowIndexOf(items: readonly RowWalkable[], id: Id | null): number {
  if (id === null) return -1;
  return items.findIndex((item) => item.kind === "row" && item.id === id);
}

/**
 * WHERE THE READER IS IN THE LIST — the "3 of 6" on the phone's reading pane
 * (#375 increment 7).
 *
 * ONE-BASED, because it is a sentence a person reads and people count from one.
 * That is not a style choice: "0 of 6" is either an off-by-one bug or a claim
 * that the message is not in the list, and both readings are wrong when the
 * reader is looking at the first message.
 *
 * COUNTED OVER ROWS, NOT ITEMS. The denominator has to be the number of
 * MESSAGES the list is showing, so it agrees with what a thumb can reach by
 * swiping — counting the headings, the line and the drawer rows in would make
 * "3 of 14" out of six messages.
 *
 * Null for a message the list does not contain (opened from a link, or already
 * moved out of the view by the action that opened the next one), so the caller
 * drops the clause rather than printing a position nobody can navigate to.
 */
export function positionOf(
  items: readonly RowWalkable[],
  id: Id | null,
): { index: number; count: number } | null {
  if (id === null) return null;
  let count = 0;
  let index = 0;
  for (const item of items) {
    if (item.kind !== "row" || !item.id) continue;
    count += 1;
    if (item.id === id) index = count;
  }
  return index === 0 ? null : { index, count };
}

/** The first row of the list, walking from whichever end `dir` starts at. */
function edgeRowId(items: readonly RowWalkable[], dir: number): Id | null {
  const from = dir > 0 ? 0 : items.length - 1;
  for (let i = from; i >= 0 && i < items.length; i += dir) {
    const item = items[i];
    if (item && item.kind === "row" && item.id) return item.id;
  }
  return null;
}

/**
 * The row `step` places from the cursor — `j` and `k`.
 *
 * IT SKIPS EVERYTHING THAT IS NOT A ROW, and that is the whole function.
 * Headings, the line, drawer rows and "show more" buttons are all items in the
 * same array, so walking the array itself would land the cursor on a heading —
 * where `e` would archive nothing, `Enter` would open nothing, and the user
 * would simply have to press `j` again for reasons the screen does not
 * explain. Crossing a heading is how the cursor moves from one area to the
 * next, and into an open drawer.
 *
 * At either end it STAYS PUT rather than wrapping (the same clamp ListRoute's
 * `j`/`k` make): a list that jumps from the bottom back to the top loses the
 * user's place in exactly the moment they were reading carefully. A cursor
 * that is no longer on this tab starts again from the near end.
 */
export function nextRowId(
  items: readonly RowWalkable[],
  cursor: Id | null,
  step: number,
): Id | null {
  const dir = step >= 0 ? 1 : -1;
  const from = rowIndexOf(items, cursor);
  if (from === -1) return edgeRowId(items, dir);
  for (let i = from + dir; i >= 0 && i < items.length; i += dir) {
    const item = items[i];
    if (item && item.kind === "row" && item.id) return item.id;
  }
  return cursor;
}

/**
 * Where the cursor goes when its row LEAVES THE LIST (#685) — the row that took
 * its place: the first row after it that is still here, else the nearest one
 * before it, else nothing.
 *
 * ⚠ NOT THE FIRST ROW OF THE LIST. That was the rule, and it is what made
 * archive-and-advance scroll the desk: the optimistic overlay drops the archived
 * row in one render, the reading pane's advance arrives only after the archive
 * has awaited its queue, and in between the cursor was put on the top row — the
 * pane scrolled up to it — and then on the row after the archived one, now off
 * the bottom of the screen: scrolled again, to the bottom edge. Picking the
 * neighbour here is the same choice the advance makes (useListVerbs
 * advancePastOpen), so the two agree and nothing has to scroll.
 *
 * `before` is the list as it was when the cursor was last on it; `after` is the
 * list now. A cursor `before` never held (another tab, a first load) has no
 * neighbour and starts at the top, as it always did.
 */
export function cursorAfterRemoval(
  before: readonly Id[],
  after: readonly Id[],
  cursor: Id | null,
): Id | null {
  if (cursor !== null && after.includes(cursor)) return cursor;
  const at = cursor === null ? -1 : before.indexOf(cursor);
  if (at === -1) return after[0] ?? null;
  const still = new Set(after);
  for (let i = at + 1; i < before.length; i += 1) {
    const id = before[i] as Id;
    if (still.has(id)) return id;
  }
  for (let i = at - 1; i >= 0; i -= 1) {
    const id = before[i] as Id;
    if (still.has(id)) return id;
  }
  return after[0] ?? null;
}
