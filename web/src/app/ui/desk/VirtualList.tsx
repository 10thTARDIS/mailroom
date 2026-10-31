// SPDX-License-Identifier: AGPL-3.0-only

// THE VIRTUALISED LIST (#375 increment 3), WHICH IS A GRID (#447).
//
// The desk's list pane is the scroll container — the same element the reading
// pane's grid column already is — and this renders only the items inside it
// that a person could actually see, inside a spacer as tall as the whole list
// so the scrollbar still tells the truth about how much there is.
//
// The arithmetic is NOT here. `virtualWindow` in core/desk.ts owns it, tested
// without a DOM; this component's whole job is the three measurements that
// arithmetic needs (where we are scrolled to, how tall the viewport is, how
// tall a row is) and putting the answer on screen.
//
// ---------------------------------------------------------------------------
// FOCUS LIVES ON THE CONTAINER, NOT ON A ROW (#447)
// ---------------------------------------------------------------------------
//
// `role="grid"` with `tabIndex=0` and `aria-activedescendant` naming the cursor
// row's id — deliberately NOT the other half of that pattern, a roving `tabIndex`
// that moves from row to row.
//
// ⚠ BECAUSE A VIRTUALISED ROW CAN UNMOUNT WHILE IT HOLDS FOCUS. Scroll the
// cursor row out of the window with a roving tabIndex and React removes the
// focused element; focus falls to `<body>`, the whole key map goes quiet, and the
// user's next keystroke does nothing with no indication why. With focus on the
// container that cannot happen: the container is never unmounted by scrolling,
// and the only thing that can go stale is an ATTRIBUTE.
//
// Which leaves the one obligation this pattern does have: the element
// `aria-activedescendant` names MUST be in the document. An id that resolves to
// nothing is worse than no attribute at all — the grid claims a current row, the
// screen reader finds none, and it announces nothing. So the cursor row is
// rendered even when it is outside the window, at its true `top`
// (`pinnedWindow`, in core/desk.ts beside the window arithmetic it extends).
//
// TWO MEASUREMENTS THAT LOOK OPTIONAL AND ARE NOT:
//
//   * THE CHROME OFFSET. The header sentence, the in-tray strip and the tabs
//     are sticky ABOVE the list, inside the same scroll container, and they are
//     not items. So `scroller.scrollTop` is not the list's scroll position —
//     it is the list's position plus however tall that chrome is. Subtracting
//     it is what keeps the window aligned with what the eye sees; without it
//     the first screenful is short by exactly the chrome's height and the rows
//     at the bottom never render until you overscroll.
//
//   * THE ROW-HEIGHT PROBE. Row height is a design token per density
//     (`--desk-row-h`), which the user can change while looking at this list.
//     Reading the number out of `getComputedStyle` once would freeze it at
//     whatever the density was on mount; parsing it on every scroll would be a
//     layout read per frame. Instead an empty, zero-width `<li>` declares the
//     same height and a ResizeObserver watches IT: the browser tells us when
//     the token changes, and the number we use is the one CSS actually applied
//     rather than one we re-derived and hoped matched.

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { ReactNode, RefObject } from "react";
import type { JSX } from "react";

import { pinnedWindow, virtualWindow } from "../../../core/desk";
import { gridKeyIntent } from "./gridKeys";

interface Metrics {
  /** Scroll position WITHIN the list, chrome already subtracted. */
  scrollTop: number;
  viewport: number;
  rowHeight: number;
  /** How tall the sticky chrome above the list is. */
  chrome: number;
}

const ZERO: Metrics = { scrollTop: 0, viewport: 0, rowHeight: 0, chrome: 0 };

/**
 * What the grid needs said about one item, beyond how to draw it.
 *
 * The `li` is the ROW, and the row is what a screen reader announces — but only
 * the surface knows what a row is called (ui/desk/rowName.ts builds the desk's
 * out of the store). So the surface answers these three questions per item and
 * this component owns everything that is the same for every grid.
 */
export interface RowAria {
  /** The row's accessible name, when it has to be BUILT rather than read off the
   * row's own contents. Absent for a heading, the line or a drawer: those say
   * what they are already, and a label would be heard INSTEAD of them. */
  label?: string;
  /** Mirrors selection. Absent — not `false` — for anything that is not a
   * selectable message row: `aria-selected="false"` on an area heading would
   * offer a selection that does not exist. */
  selected?: boolean;
  /**
   * This row is the one the reading pane is showing.
   *
   * On screen that is an accent bar down the row's left edge — colour and shape,
   * and no word at all. `aria-current` is the word: it is what "the one of these
   * you are on" means, and it is a different fact from `aria-selected` (a set you
   * are about to act on) and from the active descendant (where the cursor is).
   */
  current?: boolean;
  /**
   * How the item's content is wrapped:
   *
   *   "cell"      one `gridcell` around it — the default (the line, a drawer, a
   *               "show more", a section error, the empty state).
   *   "rowheader" one `rowheader` — an area heading, which stays an `<h2>` INSIDE
   *               the cell so a reader can still jump between areas by heading.
   *   "own"       the content supplies its own cells (a message row has three).
   */
  cell?: "cell" | "rowheader" | "own";
}

/** What the grid does with the keys it owns. Supplied by the surface, because
 * moving the cursor is the surface's own verb — the desk's also advances an open
 * reading pane, which this component knows nothing about. */
export interface GridKeys {
  /** Move the cursor `rows` message rows (negative walks backwards) — exactly
   * what `j`/`k` do, including whatever they do to an open message. */
  move: (rows: number) => void;
  edge: (to: "first" | "last") => void;
  /** Open the cursor row. */
  open: () => void;
  /** Select the cursor row, extending the range from the anchor when asked. */
  select: (extend: boolean) => void;
}

/** An id for a row that is legal in a DOM id and stable for as long as the item
 * is: `itemKey` already answers "which item is this" (it is React's key), so the
 * only work here is making it safe to put in an attribute. */
function rowDomId(gridId: string, key: string): string {
  return `${gridId}-item-${key.replace(/[^A-Za-z0-9_-]+/g, "-")}`;
}

/**
 * GENERIC OVER THE ITEM, because there are two of these lists now: the desk's
 * (headings, rows, the line, drawers) and the tray page's (wait-group headings
 * and rows). The component only ever needs `kind`, for the per-kind class name
 * — everything about what an item IS belongs to the route that renders it — so
 * constraining it to that is what lets the second surface reuse the measurement
 * and the window rather than growing a second copy of them.
 */
export function VirtualList<T extends { kind: string }>({
  items,
  scrollRef,
  chromeRef,
  cursorIndex,
  renderItem,
  itemKey,
  rowAria,
  keys,
  gridId,
  label,
  labelledBy,
}: {
  items: T[];
  /** The scrolling element: the desk's `.list-pane`. */
  scrollRef: RefObject<HTMLElement | null>;
  /** The sticky chrome above the list. Watched as well as the scroller because
   * the chrome CHANGES HEIGHT — the in-tray strip opens, the selection toolbar
   * appears — while the scroller's own box stays exactly the same size, so
   * nothing else would tell us the offset every item is placed against just
   * moved. */
  chromeRef: RefObject<HTMLElement | null>;
  /** Which ITEM the keyboard cursor is on, or -1 — scrolled into view when the
   * cursor MOVES out of the window (the arrows, `j`/`k`, archive-and-advance, a
   * message opened elsewhere), never when the user scrolls it out, and kept in
   * the DOM even when it is outside it, because it is the grid's active
   * descendant. */
  cursorIndex: number;
  renderItem: (item: T, index: number) => ReactNode;
  itemKey: (item: T, index: number) => string;
  rowAria: (item: T, index: number) => RowAria;
  keys: GridKeys;
  /** The grid's DOM id — what a tab's `aria-controls` points at. */
  gridId: string;
  /** The grid's accessible name, for a surface with no tab to name it. */
  label?: string;
  /** ...or the id of the element that names it: on the desk that is the SELECTED
   * TAB. A `role="grid"` cannot also be a `tabpanel`, and WAI-ARIA's answer for a
   * panel that holds one widget is to let the widget take the panel's place —
   * which means taking its NAME, so the grid is called "Review first 6" rather
   * than something generic that repeats what the tab above it just said. */
  labelledBy?: string;
}): JSX.Element {
  const listRef = useRef<HTMLUListElement | null>(null);
  const probeRef = useRef<HTMLLIElement | null>(null);
  const frame = useRef<number | null>(null);
  const [metrics, setMetrics] = useState<Metrics>(ZERO);
  /** The metrics as last rendered, readable from `measure` without making it
   * depend on them (it is a dependency of the wiring effect). */
  const metricsRef = useRef<Metrics>(ZERO);
  metricsRef.current = metrics;

  /** Where the list's top sits inside the scrolled content — i.e. how tall the
   * sticky chrome above it is. Recomputed rather than cached because the strip
   * opens and closes and the selection toolbar appears and disappears. */
  const chromeOf = (scroller: HTMLElement, list: HTMLElement): number =>
    list.getBoundingClientRect().top - scroller.getBoundingClientRect().top + scroller.scrollTop;

  const measure = useCallback(() => {
    const scroller = scrollRef.current;
    const list = listRef.current;
    const probe = probeRef.current;
    if (!scroller || !list || !probe) return;
    const rowHeight = probe.getBoundingClientRect().height;
    // A HIDDEN PANE MEASURES ZERO, AND ZERO IS NOT A ROW HEIGHT (#673). Below
    // 1100px an open message replaces the list: `.list-pane` is `display: none`,
    // every box in it is 0 tall and its scrollTop reads 0. Taking that as the new
    // metrics dropped the list back to in-flow layout (forty rows, no spacer), so
    // the pane lost its scrollable height and the browser clamped the offset it
    // had kept; closing the message landed far above where it was opened.
    // Once a real height has been seen, a zero is "not on screen", and the last
    // real metrics stand until the pane is back (a ResizeObserver fires then).
    if (rowHeight <= 0 && scroller.clientHeight <= 0 && metricsRef.current.rowHeight > 0) return;
    const chrome = chromeOf(scroller, list);
    const next: Metrics = {
      scrollTop: scroller.scrollTop - chrome,
      viewport: scroller.clientHeight,
      rowHeight,
      chrome,
    };
    setMetrics((prev) =>
      prev.scrollTop === next.scrollTop &&
      prev.viewport === next.viewport &&
      prev.rowHeight === next.rowHeight &&
      prev.chrome === next.chrome
        ? prev
        : next,
    );
  }, [scrollRef]);

  /** One measurement per frame at most. A scroll event fires far more often
   * than the screen repaints, and each measurement is a forced layout read. */
  const schedule = useCallback(() => {
    if (frame.current !== null) return;
    frame.current = requestAnimationFrame(() => {
      frame.current = null;
      measure();
    });
  }, [measure]);

  // ⚠ A PASSIVE EFFECT, NOT A LAYOUT EFFECT, AND THE DIFFERENCE IS THE WHOLE
  // LISTENER (#666). The scroller is the PARENT's element (`.list-pane`), and
  // React attaches a parent's ref AFTER its children's layout effects have run:
  // as a layout effect this read `scrollRef.current`, found null, and returned
  // before `addEventListener` — and with dependencies that never change it was
  // never given a second go. Nothing listened for scroll or resize at all; the
  // window only moved when the item count did, so a long list was its first
  // screenful and then empty spacer. Passive effects run once the whole commit,
  // refs included, is in place.
  useEffect(() => {
    const scroller = scrollRef.current;
    if (!scroller) return;
    measure();
    scroller.addEventListener("scroll", schedule, { passive: true });
    const observer = new ResizeObserver(schedule);
    observer.observe(scroller);
    if (probeRef.current) observer.observe(probeRef.current);
    if (chromeRef.current) observer.observe(chromeRef.current);
    return () => {
      scroller.removeEventListener("scroll", schedule);
      observer.disconnect();
      if (frame.current !== null) cancelAnimationFrame(frame.current);
      frame.current = null;
    };
  }, [scrollRef, chromeRef, measure, schedule]);

  // The list changing length changes the spacer, which changes what is
  // scrollable — and a tab switch changes the chrome above it.
  useLayoutEffect(() => {
    measure();
  }, [items.length, measure]);

  // THE CURSOR IS ALWAYS ON SCREEN. The arrows and `j`/`k` move an index, not the
  // viewport, so without this the cursor walks straight out of the window and the
  // user is pressing keys at a list that appears not to respond. The sticky
  // chrome covers the top of the viewport, so "visible" starts below it.
  //
  // ⚠ KEYED ON THE CURSOR'S IDENTITY, NOT ITS INDEX (#673). Unfolding a section
  // ABOVE the cursor inserts rows, so the cursor's index changes while the cursor
  // stays on the same item; an effect keyed on the index scrolled the pane to a
  // row the user had not moved to. The index is read through a ref so the effect
  // sees the current one without depending on it.
  //
  // ⚠ AND IT SCROLLS ONCE PER CURSOR MOVE, NEVER BECAUSE THE LIST WAS MEASURED.
  // The row height is a dependency only so that a cursor which moved before the
  // first measurement is followed once the height is known. It used to be enough
  // ON ITS OWN: every re-measured row height re-ran this effect, and the effect
  // put the cursor back in view. `measure()` runs on every scroll frame and
  // compares the height with `===`, so a row height that changes without the
  // cursor moving pulls the list back to the cursor: a density or text-size
  // change, a resize across the phone breakpoint, and a height that wobbles in
  // its last bits from one scroll position to the next. With the wobble, the
  // user cannot scroll the cursor row out of view at all (the report:
  // "I can't scroll up or down past the point where that email wouldn't be
  // visible"). `followed` is the cursor this effect last brought into view, so a
  // re-measure finds it already followed and does nothing.
  //
  // A cursor that is momentarily NOT on this list (`cursorKey` null) leaves
  // `followed` alone: its coming back is the same cursor, not a move.
  const cursorIndexRef = useRef(cursorIndex);
  cursorIndexRef.current = cursorIndex;
  const cursorKey =
    cursorIndex >= 0 && items[cursorIndex] !== undefined
      ? itemKey(items[cursorIndex] as T, cursorIndex)
      : null;
  const followed = useRef<string | null>(null);
  useEffect(() => {
    const index = cursorIndexRef.current;
    if (cursorKey === null || index < 0) return;
    if (cursorKey === followed.current) return;
    // Not measured yet: no arithmetic to do. The cursor stays unfollowed, and the
    // measurement re-runs this effect.
    if (metrics.rowHeight <= 0) return;
    const scroller = scrollRef.current;
    const list = listRef.current;
    if (!scroller || !list) return;
    followed.current = cursorKey;
    const chrome = chromeOf(scroller, list);
    const top = chrome + index * metrics.rowHeight;
    const bottom = top + metrics.rowHeight;
    if (top < scroller.scrollTop + chrome) scroller.scrollTop = top - chrome;
    else if (bottom > scroller.scrollTop + scroller.clientHeight) {
      scroller.scrollTop = bottom - scroller.clientHeight;
    }
  }, [cursorKey, metrics.rowHeight, scrollRef]);

  const slice = virtualWindow(metrics.scrollTop, metrics.viewport, metrics.rowHeight, items.length);
  // Before the first measurement there is no row height, so `virtualWindow`
  // answers with an empty window — correct, but it would paint a blank list for
  // one frame. Until then the list lays itself out in ordinary flow (no
  // `is-virtual`, no absolute positioning, no spacer height): a first paint
  // that is right without any measurement at all, replaced by the real window
  // on the effect that runs right after it (the wiring effect above, which
  // cannot measure any sooner than the scroller's ref exists).
  const unmeasured = metrics.rowHeight <= 0;
  const start = unmeasured ? 0 : slice.start;
  const end = unmeasured ? Math.min(items.length, 40) : slice.end;
  const pin = pinnedWindow(slice, cursorIndex, items.length, metrics.rowHeight);

  /**
   * Which indices are in the DOM: the window, plus the cursor row when the list
   * has been scrolled away from it (see the header).
   *
   * ASCENDING, so DOM order and `aria-rowindex` agree — a reader walking the
   * rendered rows must not be told that row 3 comes after row 60.
   */
  const drawn: number[] = [];
  if (pin.pinned !== null && pin.pinned < start) drawn.push(pin.pinned);
  for (let i = start; i < end; i += 1) drawn.push(i);
  if (pin.pinned !== null && pin.pinned >= end) drawn.push(pin.pinned);

  const cursorItem = cursorIndex >= 0 ? items[cursorIndex] : undefined;
  const activeId =
    cursorItem === undefined ? undefined : rowDomId(gridId, itemKey(cursorItem, cursorIndex));

  /** How many rows a PageUp/PageDown crosses: what fits on screen below the
   * sticky chrome, never fewer than one. Counted in ROWS while the arithmetic is
   * in ITEMS, because `move` skips the headings between rows — so a page travels
   * a little further than the eye did on a screenful that holds headings, which
   * is the right way round for a key whose job is to get down a long list. */
  const rowsPerPage = (): number => {
    if (metrics.rowHeight <= 0) return 1;
    return Math.max(1, Math.floor((metrics.viewport - metrics.chrome) / metrics.rowHeight));
  };

  const onKeyDown = (ev: React.KeyboardEvent<HTMLUListElement>): void => {
    const intent = gridKeyIntent(ev);
    if (intent === null) return;
    // A pointer can leave focus on a control INSIDE the grid: a row's buttons are
    // out of the tab order but a click still focuses them, and a heading, a drawer
    // and the line each have real buttons of their own. Movement is safe to take
    // from there — nothing in the list uses the arrows — but Enter and Space
    // belong to whatever has focus, and taking those would break every button in
    // here.
    const onGrid = ev.target === ev.currentTarget;
    if ((intent.kind === "open" || intent.kind === "select") && !onGrid) return;
    ev.preventDefault();
    // Stopped so the shell's one window-level chain does not act on the same
    // keystroke as well (ui/Shell.tsx): React's stopPropagation stops the NATIVE
    // event at its root container, so the window listener never sees it.
    ev.stopPropagation();
    switch (intent.kind) {
      case "move":
        keys.move(intent.delta);
        break;
      case "page":
        keys.move(intent.dir * rowsPerPage());
        break;
      case "edge":
        keys.edge(intent.to);
        break;
      case "open":
        keys.open();
        break;
      case "select":
        keys.select(intent.extend);
        break;
    }
    // Moving from a focused control inside the list brings focus back to the grid,
    // so the active descendant is what the NEXT keystroke is about.
    if (!onGrid) listRef.current?.focus();
  };

  return (
    <ul
      className={unmeasured ? "desk-list" : "desk-list is-virtual"}
      ref={listRef}
      id={gridId}
      role="grid"
      // ONE TAB STOP for the whole list: everything the keyboard does to a row it
      // does to the CURSOR row, which is why the rows' own buttons are out of the
      // tab order (ui/desk/DeskRow.tsx).
      tabIndex={0}
      aria-rowcount={items.length}
      // Several rows can be selected at once (Shift+click, Shift+Space, `x`), and
      // a grid that did not say so would carry `aria-selected` on more rows than
      // it claimed could hold it.
      aria-multiselectable={true}
      {...(labelledBy ? { "aria-labelledby": labelledBy } : {})}
      {...(labelledBy || !label ? {} : { "aria-label": label })}
      {...(activeId ? { "aria-activedescendant": activeId } : {})}
      onKeyDown={onKeyDown}
      style={unmeasured ? undefined : { height: `${slice.totalHeight}px` }}
    >
      {/* The height probe (see the header). Zero-width and aria-hidden: it is
          not a row, it is the browser telling us what a row is. */}
      <li className="desk-probe" aria-hidden="true" ref={probeRef} />
      {drawn.map((index) => {
        const item = items[index];
        if (item === undefined) return null;
        const aria = rowAria(item, index);
        const key = itemKey(item, index);
        return (
          <li
            key={key}
            id={rowDomId(gridId, key)}
            role="row"
            // ONE-BASED, and an index into the WHOLE list rather than into what is
            // rendered — which is the entire point of the attribute on a
            // virtualised grid: "row 412 of 1,208" stays true while 1,180 of those
            // rows are not in the document at all.
            aria-rowindex={index + 1}
            {...(aria.label ? { "aria-label": aria.label } : {})}
            {...(aria.selected === undefined ? {} : { "aria-selected": aria.selected })}
            {...(aria.current ? { "aria-current": true } : {})}
            className={`desk-item desk-item-${item.kind}`}
            style={unmeasured ? undefined : { top: `${index * metrics.rowHeight}px` }}
          >
            {aria.cell === "own" ? (
              renderItem(item, index)
            ) : (
              <div
                role={aria.cell === "rowheader" ? "rowheader" : "gridcell"}
                className="desk-cell-full"
              >
                {renderItem(item, index)}
              </div>
            )}
          </li>
        );
      })}
    </ul>
  );
}
