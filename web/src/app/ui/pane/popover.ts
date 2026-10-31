// SPDX-License-Identifier: AGPL-3.0-only

// WHERE THE SENDER POPOVER GOES (#375 increment 5).
//
// Pure arithmetic, in its own module and under test, because the failure it
// guards against is not a crash: a popover placed off the bottom of a short
// window, or off the right edge beside a sender in a narrow pane, renders
// perfectly and is simply unreadable — and it only happens at sizes nobody
// tests by hand. So the decision is a function of four rectangles and nothing
// else: no DOM, no React, no measurement of its own.
//
// The rule, in order:
//   1. BELOW the anchor, left edges aligned. That is where a popover opened
//      from a name belongs — it does not cover the name it is about.
//   2. If it does not fit below but DOES fit above, flip above. Flipping is
//      preferred to squashing: the content is a list of short lines and
//      cropping the bottom one loses a whole fact.
//   3. Otherwise clamp into the viewport, margin included. A popover taller
//      than the window is pinned to the top and keeps its own scroll — better
//      the first lines than the middle ones.
// Horizontally it is always clamped, never flipped: a popover whose left edge
// jumps to the other side of the name reads as a different popover.

export interface AnchorRect {
  top: number;
  left: number;
  width: number;
  height: number;
}

export interface PopoverSize {
  width: number;
  height: number;
}

export interface Viewport {
  width: number;
  height: number;
}

export interface PopoverPlacement {
  top: number;
  left: number;
  /** Which side of the anchor it ended up on — the caller uses it for the
   * pointer/shadow direction, and a test can assert the flip happened rather
   * than inferring it from a coordinate. */
  side: "below" | "above";
}

/** Breathing room between the popover and both the anchor and the window edge.
 * One number for both: they are the same kind of gap. */
export const POPOVER_GAP = 6;

function clamp(v: number, min: number, max: number): number {
  // min wins when the popover is wider/taller than the space: pinning to the
  // near edge shows the beginning of the content rather than its end.
  return Math.max(min, Math.min(max, v));
}

export function placePopover(
  anchor: AnchorRect,
  popover: PopoverSize,
  viewport: Viewport,
  gap: number = POPOVER_GAP,
): PopoverPlacement {
  const below = anchor.top + anchor.height + gap;
  const above = anchor.top - gap - popover.height;

  const fitsBelow = below + popover.height <= viewport.height - gap;
  const fitsAbove = above >= gap;
  const side: PopoverPlacement["side"] = fitsBelow || !fitsAbove ? "below" : "above";

  const rawTop = side === "below" ? below : above;
  const top = clamp(rawTop, gap, Math.max(gap, viewport.height - gap - popover.height));
  const left = clamp(anchor.left, gap, Math.max(gap, viewport.width - gap - popover.width));

  return { top, left, side };
}
