// SPDX-License-Identifier: AGPL-3.0-only

// THE LINE (#375 increment 3).
//
//   6 to review · stricter · looser · 37 in drawers            0.55
//
// The line is the desk's one setting, and it is drawn where it acts: between
// what is being shown and what is being kept. Moving it moves mail, so the two
// controls do NOT move it — they move a PENDING value and ask the server what
// the desk would look like there:
//
//   at 0.65: 4 to review · 39 in drawers        [Apply] [Cancel]
//
// NOTHING MOVES UNTIL APPLY. A slider that re-split the inbox live would mean
// every exploratory click rearranged a screenful of mail, and "put it back" is
// not a thing a user can do accurately by eye. The preview is a question about
// a desk that does not exist yet (sync.previewLine stores nothing), and Apply
// is the only thing that writes.
//
// THE NUMBER IS ALWAYS PRINTED. The words "stricter" and "looser" say which
// way, not where — and a threshold the user cannot read is one they cannot
// reason about, report, or put back where it was.
//
// AND THE NUMBER IS A SLIDER (#447). It was a `<span>` inside a `<div aria-label>`
// — which is a label ARIA does not read off a generic element, so the whole
// control was two unexplained buttons and a decimal. It is now the widget it
// already looked like: `role="slider"`, focusable, `aria-valuemin/max/now`, and an
// `aria-valuetext` that says the SPLIT rather than the number
// (ui/desk/format.ts lineValueText — "6 to review, 37 in drawers; the line at
// 0.65 would move 2 to the drawers").
//
// THE ARROWS FOLLOW ARIA, NOT THE ROW'S LEFT-TO-RIGHT LAYOUT, and that is a
// deliberate departure from the design's sketch (which had ArrowLeft/Down =
// stricter, matching "to review" being drawn on the left). `aria-valuenow` IS the
// threshold, and a stricter line is a HIGHER threshold; WAI-ARIA's slider keys are
// normative that Up/Right increase the value. Mapping Up to "stricter" therefore
// means the announced number goes up when the user presses up. The other way round
// — press up, hear the number fall — is a slider that lies about its own direction
// to a reader who cannot see which side of the row says "to review".
//
// HOME/END AND THE PAGE KEYS ARE DELIBERATELY ABSENT. Their slider meaning is
// "jump to the minimum/maximum", and the ends of this range are "show everything"
// and "show nothing" — a whole inbox rearranged by one keystroke. Nothing moves
// until Apply, so it would not be destructive, but it would be a preview nobody
// asked for over a line they cannot get back by eye. The two words and the two
// arrows move it a step at a time, which is what this control has always been.

import type { JSX } from "react";

import { t } from "../../i18n";
import { lineValueText, thresholdText } from "./format";
import type { LinePendingMove } from "./format";

/** How far one click moves the line. Small enough that the preview between two
 * clicks is usually a different desk, large enough that crossing a useful
 * distance is not twenty clicks. */
export const LINE_STEP = 0.05;

export function TheLine({
  reviewCount,
  drawerCount,
  threshold,
  pending,
  preview,
  previewFailed,
  onStricter,
  onLooser,
  onApply,
  onCancel,
  busy,
}: {
  reviewCount: number;
  drawerCount: number;
  /** The stored line — what the desk on screen was split at. */
  threshold: number;
  /** The line being considered, or null when nothing is pending. */
  pending: number | null;
  /** What the server said the pending line would show, or null while the
   * preview is still in flight. */
  preview: { review: number; drawers: number } | null;
  /** The preview came back with nothing usable. Said out loud rather than
   * left as a spinner that never resolves. */
  previewFailed: boolean;
  onStricter: () => void;
  onLooser: () => void;
  onApply: () => void;
  onCancel: () => void;
  busy: boolean;
}): JSX.Element {
  const value = pending ?? threshold;

  /**
   * The third clause of the value text, or null.
   *
   * It needs BOTH a pending line and an answer about it: while the preview is in
   * flight (or has failed) there is no number, and a slider that guessed one would
   * be claiming a consequence the server never stated. The direction is read from
   * the pending line against the stored one rather than from which button was
   * pressed — after four clicks the last press is not where the line has got to.
   */
  const move: LinePendingMove | null =
    pending === null || preview === null
      ? null
      : {
          threshold: thresholdText(pending),
          moves: Math.abs(preview.review - reviewCount),
          direction: pending >= threshold ? "stricter" : "looser",
        };

  /**
   * The two arrows are the two buttons, and Enter/Escape are Apply/Cancel — the
   * whole control from the keyboard, without reaching for Tab.
   *
   * ⚠ A KEY THIS SLIDER OWNS IS CONSUMED WHETHER OR NOT IT CAN ACT ON IT. The line
   * sits inside the list's grid, which owns the arrows for moving the cursor and
   * pulls focus back to itself when it does (ui/desk/VirtualList.tsx) — so an
   * ArrowUp that fell through here while a write was in flight would scroll the
   * desk AND take focus off the control the user was holding. Consuming it and
   * doing nothing is the honest answer for a disabled control.
   */
  const onKeyDown = (ev: React.KeyboardEvent<HTMLSpanElement>): void => {
    if (ev.ctrlKey || ev.metaKey || ev.altKey || ev.shiftKey) return;
    const mine =
      ev.key === "ArrowUp" ||
      ev.key === "ArrowRight" ||
      ev.key === "ArrowDown" ||
      ev.key === "ArrowLeft" ||
      // Enter and Escape only mean something here while a line is pending: with
      // nothing to apply or cancel, Escape is still the shell's (close the message)
      // and Enter is still nobody's.
      (pending !== null && (ev.key === "Enter" || ev.key === "Escape"));
    if (!mine) return;
    ev.preventDefault();
    // Stopped so the shell's chain does not ALSO act: Escape would otherwise close
    // the open message as well as cancelling the pending line, and the arrows would
    // reach the grid and move the cursor (ui/Shell.tsx).
    ev.stopPropagation();
    if (busy) return;
    switch (ev.key) {
      case "ArrowUp":
      case "ArrowRight":
        onStricter();
        return;
      case "ArrowDown":
      case "ArrowLeft":
        onLooser();
        return;
      case "Enter":
        // Only what the button would do: Apply waits for a preview, or for the
        // preview to have failed (see the button's own `disabled`).
        if (preview !== null || previewFailed) onApply();
        return;
      case "Escape":
        onCancel();
        return;
      default:
        return;
    }
  };

  return (
    <div className="desk-line">
      <span className="desk-line-side">{t("desk.line.toReview", { n: reviewCount })}</span>
      <button type="button" className="desk-line-move" onClick={onStricter} disabled={busy}>
        {t("desk.line.stricter")}
      </button>
      <button type="button" className="desk-line-move" onClick={onLooser} disabled={busy}>
        {t("desk.line.looser")}
      </button>
      <span className="desk-line-side">{t("desk.line.inDrawers", { n: drawerCount })}</span>
      <span
        className="desk-line-value"
        id="desk-line-slider"
        role="slider"
        tabIndex={0}
        aria-label={t("desk.line.label")}
        aria-valuemin={0}
        aria-valuemax={1}
        aria-valuenow={value}
        aria-valuetext={lineValueText(reviewCount, drawerCount, move)}
        // `aria-disabled` rather than absent from the tab order: the line is still
        // worth READING while a write is in flight or the preferences have not
        // loaded, and a control that vanishes from the keyboard is one a reader
        // assumes they imagined.
        {...(busy ? { "aria-disabled": true } : {})}
        onKeyDown={onKeyDown}
      >
        {thresholdText(value)}
      </span>

      {pending !== null ? (
        <span className="desk-line-preview">
          <span className="desk-line-preview-text">
            {preview
              ? t("desk.line.preview", {
                  threshold: thresholdText(pending),
                  review: preview.review,
                  drawers: preview.drawers,
                })
              : previewFailed
                ? t("desk.line.previewFailed")
                : t("desk.line.previewing")}
          </span>
          <button
            type="button"
            className="button button-primary"
            onClick={onApply}
            // Applying a line nobody has seen the consequences of is the one
            // thing this control exists to prevent, so Apply waits for the
            // preview — or for the preview to have failed, which is a state the
            // user can still legitimately decide from.
            disabled={busy || (preview === null && !previewFailed)}
          >
            {t("desk.line.apply")}
          </button>
          <button type="button" className="button button-quiet" onClick={onCancel} disabled={busy}>
            {t("action.cancel")}
          </button>
        </span>
      ) : null}
    </div>
  );
}
