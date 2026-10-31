// SPDX-License-Identifier: AGPL-3.0-only

// The draggable divider between the message list and the reading pane (#61).
//
// It is a WAI-ARIA window splitter: role="separator" with aria-valuenow/min/max
// and a tabindex, so it is operable by keyboard and reports its position to
// assistive tech. A splitter that only responds to a mouse is a splitter half
// the users cannot move.
//
// Two details are load-bearing:
//
//   * POINTER CAPTURE. The reading pane contains a cross-origin sandboxed
//     iframe (the message body). Without setPointerCapture, the moment the
//     cursor crosses into that frame during a drag the parent stops receiving
//     pointermove and the drag dies mid-gesture — the classic iframe-swallows-
//     the-drag bug. Capture routes every event to this element until release,
//     so the frame never sees them.
//
//   * The size is committed to prefs on every move, but prefs writes are cheap
//     (one localStorage set) and the alternative — committing only on release —
//     loses the position if the pointer is released outside the window.

import { useCallback, useRef } from "react";
import type { JSX, KeyboardEvent as ReactKeyboardEvent, PointerEvent as ReactPointerEvent } from "react";

import { t } from "../i18n";
import { splitBounds } from "../prefs";
import type { PaneLayout } from "../prefs";

/** Keyboard step, in px. Shift multiplies it — a coarse and a fine gear. */
const STEP = 16;
const COARSE = 64;

interface Props {
  layout: PaneLayout;
  size: number;
  /** Commits a new size; returns the value actually applied after clamping. */
  onResize: (px: number) => number;
  /** The gesture's boundaries (#375 increment 5). The split is SYNCED now, and
   * a value arriving from another device mid-drag must not yank the divider out
   * from under the pointer — the shell holds it until `onDragEnd`. Optional so
   * a caller that does not care is not forced to pass two no-ops. */
  onDragStart?: () => void;
  onDragEnd?: () => void;
  /**
   * The DOCKED splitter (#617): the one between the list and a composer or
   * attachment viewer that has taken the reading pane's column. It is a child of
   * the WORKSPACE grid, not of `.content`, so its parent's left edge is the rail's
   * and a drag measured from there would be wrong by the rail's width. It measures
   * from `#messages` — the list — instead, which is also the element the synced
   * percent is measured from (prefs.contentSizePx), so the two cannot disagree.
   */
  docked?: boolean;
}

export function PaneSplitter({
  layout,
  size,
  onResize,
  onDragStart,
  onDragEnd,
  docked = false,
}: Props): JSX.Element {
  const ref = useRef<HTMLDivElement | null>(null);
  const horizontal = layout === "horizontal";
  const { min, max, def } = splitBounds(layout);

  const onPointerDown = useCallback(
    (ev: ReactPointerEvent<HTMLDivElement>) => {
      // Only the primary button drags; let anything else (context menu) through.
      if (ev.button !== 0) return;
      const el = ref.current;
      if (!el) return;
      const parent = docked ? document.getElementById("messages") : el.parentElement;
      if (!parent) return;

      ev.preventDefault();
      onDragStart?.();
      el.setPointerCapture(ev.pointerId);
      // Suppresses text selection across the whole shell for the gesture's
      // duration; without it a drag paints a selection over the message list.
      document.body.classList.add("is-splitting");

      const rect = parent.getBoundingClientRect();
      const origin = horizontal ? rect.top : rect.left;

      const move = (e: PointerEvent): void => {
        onResize((horizontal ? e.clientY : e.clientX) - origin);
      };
      const end = (): void => {
        el.releasePointerCapture(ev.pointerId);
        document.body.classList.remove("is-splitting");
        el.removeEventListener("pointermove", move);
        el.removeEventListener("pointerup", end);
        el.removeEventListener("pointercancel", end);
        // On pointercancel too: a cancelled gesture is still a gesture that
        // ended, and leaving the shell believing a drag is in flight would
        // silently swallow every later change from another device.
        onDragEnd?.();
      };

      el.addEventListener("pointermove", move);
      el.addEventListener("pointerup", end);
      el.addEventListener("pointercancel", end);
    },
    [horizontal, onResize, onDragStart, onDragEnd, docked],
  );

  const onKeyDown = useCallback(
    (ev: ReactKeyboardEvent<HTMLDivElement>) => {
      const step = ev.shiftKey ? COARSE : STEP;
      const grow = horizontal ? "ArrowDown" : "ArrowRight";
      const shrink = horizontal ? "ArrowUp" : "ArrowLeft";
      switch (ev.key) {
        case grow:
          ev.preventDefault();
          onResize(size + step);
          return;
        case shrink:
          ev.preventDefault();
          onResize(size - step);
          return;
        case "Home":
          ev.preventDefault();
          onResize(min);
          return;
        case "End":
          ev.preventDefault();
          onResize(max);
          return;
        case "Enter":
          // Reset to the default, matching the double-click affordance.
          ev.preventDefault();
          onResize(def);
          return;
        default:
      }
    },
    [horizontal, size, min, max, def, onResize],
  );

  return (
    <div
      ref={ref}
      className={docked ? "pane-splitter pane-splitter-docked" : "pane-splitter"}
      role="separator"
      tabIndex={0}
      aria-orientation={horizontal ? "horizontal" : "vertical"}
      aria-label={t("settings.paneResize")}
      aria-valuenow={size}
      aria-valuemin={min}
      aria-valuemax={max}
      onPointerDown={onPointerDown}
      onKeyDown={onKeyDown}
      onDoubleClick={() => onResize(def)}
    />
  );
}
