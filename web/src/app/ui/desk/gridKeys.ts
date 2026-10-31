// SPDX-License-Identifier: AGPL-3.0-only

// WHAT A KEYSTROKE MEANS TO THE DESK'S GRID (#447).
//
// The desk's list is a `role="grid"` with one tab stop and its current row named
// by `aria-activedescendant` (ui/desk/VirtualList.tsx). A grid that says that
// owes the keyboard the movement a grid is expected to have — arrows between
// rows, Home/End to the ends, PageUp/PageDown by a screenful — and until #447
// this list deliberately had NONE of it: DeskRoute's comment said unmodified
// arrows were "the browser's own scrolling, which is the only way to move a long
// list without a pointer". That was true of a `<ul>` of links. It is not true of
// a grid, which owns its own arrows and must, because the thing being moved is
// the active descendant and the browser cannot know about it.
//
// THE MAPPING IS HERE, NOT IN THE COMPONENT, for the reason every other decision
// in this directory is pure: a `switch` inside a keydown closure cannot be
// asserted without a DOM, and this repo's web tests have no renderer at all (see
// ui/attachmentPreviewPolicy.ts's note). What the grid does with the answer is
// the component's business; WHICH answer a key deserves is testable, and worth
// testing, because the interesting cases are the keys this must NOT claim.
//
// WHAT IT DELIBERATELY RETURNS NULL FOR:
//
//   * `j`/`k` — they work from anywhere on the desk, not only while the grid has
//     focus, so they belong to the route's own verb chain (ui/Shell.tsx hands it
//     every key the grid does not consume). Claiming them here would give the
//     same movement two implementations, and the route's is the one that also
//     advances an open reading pane.
//   * Shift+Arrow — that is "this belongs on the other side of the line" (#464),
//     a verb over the selection, and it must reach the route.
//   * anything with Ctrl/⌘/Alt — Ctrl+Home is "top of the document", ⌘+↓ is
//     "end of the list" on a Mac at the OS level, and Ctrl/⌘+Z is undo. A grid
//     that swallowed modified keys would be a grid you cannot get out of.

/** The shape this needs of a keyboard event, so a test can hand it a literal. */
export interface GridKeyLike {
  key: string;
  shiftKey?: boolean;
  ctrlKey?: boolean;
  metaKey?: boolean;
  altKey?: boolean;
}

/**
 * What the grid should do about a key.
 *
 * `page` is a DIRECTION, not a distance: how many rows fit on screen is a
 * measurement, and this module knows no pixels (the same split core/desk.ts's
 * `virtualWindow` makes with the component that measures for it).
 */
export type GridIntent =
  | { kind: "move"; delta: number }
  | { kind: "page"; dir: 1 | -1 }
  | { kind: "edge"; to: "first" | "last" }
  | { kind: "open" }
  | { kind: "select"; extend: boolean };

export function gridKeyIntent(ev: GridKeyLike): GridIntent | null {
  // One test for all three: a modified key is somebody else's.
  if (ev.ctrlKey || ev.metaKey || ev.altKey) return null;
  switch (ev.key) {
    case "ArrowDown":
      return ev.shiftKey ? null : { kind: "move", delta: 1 };
    case "ArrowUp":
      return ev.shiftKey ? null : { kind: "move", delta: -1 };
    case "PageDown":
      return ev.shiftKey ? null : { kind: "page", dir: 1 };
    case "PageUp":
      return ev.shiftKey ? null : { kind: "page", dir: -1 };
    case "Home":
      return ev.shiftKey ? null : { kind: "edge", to: "first" };
    case "End":
      return ev.shiftKey ? null : { kind: "edge", to: "last" };
    case "Enter":
    case "o":
      return ev.shiftKey ? null : { kind: "open" };
    // SPACE SELECTS, and Shift+Space extends the selection to here — the same
    // pair `x` and Shift+x already are (DeskRoute's verb chain), because the
    // range is the thing a keyboard user cannot build any other way.
    case " ":
      return { kind: "select", extend: ev.shiftKey === true };
    default:
      return null;
  }
}
