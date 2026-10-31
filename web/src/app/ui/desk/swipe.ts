// SPDX-License-Identifier: AGPL-3.0-only

// WHAT A SWIPE ON A ROW MEANT (#375 increment 7).
//
// Pure arithmetic over one gesture, in its own module and under test, for the
// same reason ui/pane/popover.ts is: the failure this guards against does not
// throw. A threshold that is slightly too generous ARCHIVES MAIL the user was
// only scrolling past — and it does it silently, on the surface where the
// pointer is least precise and the undo toast is smallest. So the decision is a
// function of three numbers and nothing else: no DOM, no pointer events, no
// React.
//
// THE RULE, and why each half of it exists:
//
//   * VERTICAL DRIFT VETOES EVERYTHING. A phone list is scrolled by dragging
//     it, and a scroll that wanders 30px sideways is still a scroll. This is
//     checked FIRST and it is absolute: under 24px of drift or the gesture was
//     not a swipe, whatever the horizontal travel was. Without it, every
//     enthusiastic flick down the list is a coin toss between scrolling and
//     archiving, which is the one failure mode that costs mail.
//   * THE TRAVEL IS EITHER PROPORTIONAL OR ABSOLUTE. 35% of the row is the
//     "clearly deliberate on this screen" measure; 96px is the floor a wide row
//     would otherwise raise out of thumb reach. Either one commits the gesture,
//     which is `min` of the two — spelled out below rather than written as a
//     min, because "or" is what the design says and the two are only the same
//     number by arithmetic.
//
// DIRECTION IS THE SIGN, and the two directions are deliberately asymmetric in
// consequence: RIGHT (with the reading direction, the longer, more committed
// motion) archives; LEFT flags — in the tray, takes the message out. The
// caller decides what "flag" means on its own surface; this only names which
// of the two happened.

/** What a completed gesture asked for, in the vocabulary the caller acts on. */
export type SwipeAction = "archive" | "flag";

/** The share of the ROW's width that commits a swipe on a narrow screen. */
export const SWIPE_FRACTION = 0.35;

/** ...and the absolute travel that commits one whatever the row's width. */
export const SWIPE_MIN_PX = 96;

/** How far the finger may wander off the horizontal before the gesture is read
 * as a scroll instead. Strictly less than this: 24px of drift is a scroll. */
export const SWIPE_MAX_DRIFT_PX = 24;

/**
 * Which action a finished drag asked for, or null for "none of them".
 *
 * NULL IS THE DEFAULT ANSWER and every guard falls into it: a drag that was
 * mostly vertical, one that did not travel far enough, a tap (dx = 0), and a
 * row whose width could not be measured. That last one matters more than it
 * looks — a `rowWidth` of 0 would make the proportional threshold 0, and every
 * tap would archive a message. A row we cannot measure gets the absolute
 * threshold alone rather than a threshold of nothing.
 */
export function swipeDecision(dx: number, dy: number, rowWidth: number): SwipeAction | null {
  if (!Number.isFinite(dx) || !Number.isFinite(dy)) return null;
  // The veto, first and unconditionally: a gesture that wandered this far off
  // the horizontal was a scroll, however far sideways it also went.
  if (Math.abs(dy) >= SWIPE_MAX_DRIFT_PX) return null;

  const travel = Math.abs(dx);
  const proportional =
    Number.isFinite(rowWidth) && rowWidth > 0 ? rowWidth * SWIPE_FRACTION : Number.POSITIVE_INFINITY;
  if (travel < proportional && travel < SWIPE_MIN_PX) return null;

  if (dx > 0) return "archive";
  if (dx < 0) return "flag";
  // dx === 0 cannot reach here (travel would be below both thresholds), but a
  // direction is not derivable from it and guessing one is how a zero-length
  // gesture becomes an archive.
  return null;
}
