// SPDX-License-Identifier: AGPL-3.0-only

// Toasts: how a rejected mutation gets back to the user, where Undo Send lives,
// and — since #375 increment 6 — where every desk action's UNDO lives.
//
// An aria-live region, so a screen reader hears the same thing the eye sees
// (spec §12: accessibility).
//
// THE UNDO TOAST IS A DEADLINE ON SCREEN, and that is a hostile shape for
// anyone who reads slowly, uses a screen reader, or was interrupted. So it
// PAUSES: while the toast has focus or the pointer is over it, each tick pushes
// the offer's deadline out by however much was left when the pause began
// (core/undo.ts `extend`), and the number holds at what it was showing rather
// than resetting to the full window. Move away and it carries on from there.
//
// The countdown is printed because the alternative is worse: an Undo button that
// silently stops working is a button people learn not to trust.
//
// AND THE KEYBOARD IS THE REAL PATH TO IT. Tabbing to a toast pinned at the
// bottom of the document is a hunt, and eight seconds is not a hunt's worth of
// time — so Ctrl/Cmd+Z runs the latest offer from wherever focus already is
// (ui/Shell.tsx), and the sheet lists it. The pointer/focus pause below is for
// the person who DID reach the toast and is still reading it.
//
// ...WHICH LEFT ONE GAP, AND `g u` CLOSES IT (#447). Ctrl/⌘+Z is the fast path for
// someone who has already decided; a reader who has not — who wants to hear what
// happened before undoing it — needs FOCUS on the button, because that is what
// makes the toast readable and, not incidentally, what pauses the countdown while
// they read. `focusUndoToast` below is that key's whole implementation, and it
// lives here rather than in the shell so the selector and the markup it depends on
// are the same file: a query written elsewhere is a query nobody updates when a
// class name changes.

import { useCallback, useEffect, useRef, useState } from "react";
import type { JSX } from "react";

import { undoSecondsLeft } from "../../core/undo";
import { closeFocusTarget, focusMessageListSoon } from "./focusTrap";
import type { Toast } from "../state";
import { useApp } from "../state";
import { t } from "../i18n";

/** How often the countdown re-reads the clock. One second, because that is the
 * granularity it prints; anything finer would re-render for a number that
 * cannot have changed. */
const TICK_MS = 1_000;

/** The class the `g u` key looks for. A constant so the query and the button
 * cannot be renamed apart. */
const UNDO_ACTION_CLASS = "toast-undo-action";

/**
 * Put focus on the undo offer that is on screen, and say whether there was one.
 *
 * A DOM query rather than a ref handed up to the shell, deliberately: there is at
 * most ONE undo toast at a time (app/state.tsx offerUndo replaces the visible one
 * outright), so there is nothing to disambiguate — and a ref would have to be
 * published from a component that only exists while an offer does, which is a
 * subscription to work around a single-element lookup. `querySelectorAll` and the
 * LAST match anyway, so if that invariant ever changes this focuses the newest
 * rather than the oldest.
 *
 * The return value is the honest half: `false` means there was no offer to reach,
 * and the caller must not swallow the keystroke as though something happened.
 */
export function focusUndoToast(): boolean {
  const buttons = document.querySelectorAll<HTMLButtonElement>(`.toast-undo .${UNDO_ACTION_CLASS}`);
  const button = buttons[buttons.length - 1];
  if (!button) return false;
  const active = document.activeElement;
  undoFocusOrigin = active instanceof HTMLElement && active !== document.body ? active : null;
  button.focus();
  return true;
}

/**
 * WHERE FOCUS WAS BEFORE `g u` TOOK IT, so that dismissing the toast can put it
 * back (#447).
 *
 * ⚠ THIS EXISTS BECAUSE THE LANE CAUGHT IT. `g u` moved focus onto the Undo
 * button, and Escape then dismissed the toast — which REMOVES the focused element,
 * so focus fell to `<body>`, every key went quiet and nothing on screen said why.
 * A reader who used the one key increment A added for them lost their place
 * entirely. web/ACCESSIBILITY.md already had the rule written down twice, for
 * settings and for focused reading ("an element that becomes display: none while
 * focused hands focus to `<body>`"); this was the third place and nobody had
 * looked. scripts/a11y-e2e.sh's archive step is what looked.
 *
 * MODULE-SCOPED, not component state, because the component that TAKES focus and
 * the component that loses it are two renders of something that only exists while
 * an offer does — and because at most one undo toast is ever on screen
 * (app/state.tsx offerUndo replaces the visible one outright), which is the same
 * invariant the selector above rests on.
 */
let undoFocusOrigin: HTMLElement | null = null;

/**
 * Hand focus back, when this toast is going away WHILE IT STILL HOLDS IT.
 *
 * The decision is `closeFocusTarget`'s (ui/focusTrap.ts), the same one settings
 * uses on close: the opener if it is still answerable, else a named fallback —
 * never `<body>`, which is where a screen reader loses its place entirely. Doing
 * nothing when focus is elsewhere is deliberate: a toast timing out in the corner
 * while the user is typing must not yank the caret out of the composer.
 */
function releaseUndoFocus(root: HTMLElement | null): void {
  if (!root || !root.contains(document.activeElement)) return;
  const origin = undoFocusOrigin;
  undoFocusOrigin = null;
  if (closeFocusTarget(origin) === "opener") {
    origin?.focus();
    return;
  }
  focusMessageListSoon();
}

export function Toasts(): JSX.Element {
  const { toasts, dismissToast, runUndo, extendUndo } = useApp();
  return (
    <div className="toasts" role="status" aria-live="polite">
      {toasts.map((toast) =>
        toast.undo ? (
          <UndoToast
            key={toast.id}
            toast={toast}
            undo={toast.undo}
            onUndo={runUndo}
            onExtend={extendUndo}
            onDismiss={dismissToast}
          />
        ) : (
          <div key={toast.id} className={`toast toast-${toast.kind}`}>
            <span className="toast-message">{toast.message}</span>
            {toast.action ? (
              <button
                type="button"
                className="link-button"
                onClick={() => {
                  toast.action?.run();
                  dismissToast(toast.id);
                }}
              >
                {toast.action.label}
              </button>
            ) : null}
            {toast.secondary ? (
              <button
                type="button"
                className="link-button"
                onClick={() => {
                  toast.secondary?.run();
                  dismissToast(toast.id);
                }}
              >
                {toast.secondary.label}
              </button>
            ) : null}
            <button
              type="button"
              className="icon-button"
              aria-label={t("action.close")}
              onClick={() => dismissToast(toast.id)}
            >
              ✕
            </button>
          </div>
        ),
      )}
    </div>
  );
}

/**
 * One undo offer: what happened, the button, and the seconds left.
 *
 * Its own component because it owns a TIMER and a pause, and putting either in
 * the list above would give every toast a ticking clock — including the error
 * toasts that sit there until dismissed.
 */
function UndoToast({
  toast,
  undo,
  onUndo,
  onExtend,
  onDismiss,
}: {
  toast: Toast;
  /** `toast.undo`, narrowed by the caller — a component that took the whole
   * toast and asserted the field would be asserting something the type system
   * had already been told. */
  undo: { entryId: number; expiresAt: number };
  onUndo: (entryId: number) => void;
  onExtend: (entryId: number, ttlMs: number) => void;
  onDismiss: (id: number) => void;
}): JSX.Element {
  const [now, setNow] = useState(() => Date.now());
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const paused = hovered || focused;
  /** What was left on the clock when the pause began — the amount every tick
   * re-grants, which is what makes this a pause rather than a reset. */
  const held = useRef(0);

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), TICK_MS);
    return () => clearInterval(timer);
  }, []);

  // Captured on the TRANSITION into a pause only. Deliberately not keyed on
  // `undo.expiresAt`: that value moves on every paused tick (because this
  // component is what moves it), so depending on it would re-capture the
  // deadline it had just pushed out and the countdown would creep upward.
  useEffect(() => {
    if (paused) held.current = Math.max(0, undo.expiresAt - Date.now());
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [paused]);

  // Re-granted on entry to the pause and on every tick within it, so the offer
  // never lapses while it is being read.
  useEffect(() => {
    if (!paused) return;
    onExtend(undo.entryId, held.current);
  }, [paused, now, onExtend, undo.entryId]);

  const left = undoSecondsLeft(undo.expiresAt, now);

  // The offer ran out. The toast goes with it rather than sitting there with a
  // button that would find nothing in the log — "the control that does nothing"
  // this codebase keeps a rule about.
  useEffect(() => {
    if (left === 0) onDismiss(toast.id);
  }, [left, onDismiss, toast.id]);
  // (The expiry path needs no focus handling of its own: the countdown is PAUSED
  // while the toast has focus, so an offer cannot lapse under a reader — and if
  // that pause ever broke, the unmount catch-all below still puts focus back.)

  const stopHold = useCallback(() => {
    setHovered(false);
    setFocused(false);
  }, []);

  const rootRef = useRef<HTMLDivElement | null>(null);
  /** Stop the pause AND put focus back, in that order, on every path a reader
   * can take out of this toast: Escape, the Undo button, and the ✕. */
  const leave = useCallback(() => {
    stopHold();
    releaseUndoFocus(rootRef.current);
  }, [stopHold]);

  // THE CATCH-ALL, and it is safe precisely because it refuses to steal. A toast
  // can also vanish with nobody dismissing it — a NEW offer replaces the visible
  // one outright (app/state.tsx offerUndo), and the old one unmounts holding
  // focus. `focusMessageListSoon` acts on the next frame and ONLY when focus was
  // genuinely dropped (`<body>`, nothing, or a detached node), so it cannot fight
  // `leave()` above or anything that took focus deliberately in between.
  useEffect(() => () => focusMessageListSoon(), []);

  return (
    <div
      className="toast toast-undo"
      ref={rootRef}
      onPointerEnter={() => setHovered(true)}
      onPointerLeave={() => setHovered(false)}
      onFocus={() => setFocused(true)}
      onBlur={() => setFocused(false)}
      onKeyDown={(ev) => {
        if (ev.key !== "Escape") return;
        // Stopped here so the shell's one Escape chain does not ALSO peel a
        // route layer for the same press (ui/Shell.tsx): dismissing the toast
        // is the whole of what this keystroke meant.
        ev.stopPropagation();
        leave();
        onDismiss(toast.id);
      }}
    >
      <span className="toast-message">{toast.message}</span>
      <button
        type="button"
        className={`link-button ${UNDO_ACTION_CLASS}`}
        onClick={() => {
          leave();
          onUndo(undo.entryId);
        }}
      >
        {t("action.undo")}
      </button>
      {/* `role="timer"` is the whole accessibility answer here, and it is not
          decoration: a timer's implicit `aria-live` is OFF, so this number
          changing once a second does NOT re-announce the toast it sits inside
          (the toasts container is a polite live region — without this, "Archived
          3 messages, Undo, 7 s… 6 s…" would be read out for the life of every
          offer). The `aria-label` keeps the value reachable on demand, spelled
          out, for a reader who navigates to it — "8 s" alone says nothing about
          what is running out. */}
      <span
        className="toast-countdown"
        role="timer"
        aria-label={t("undo.timeLeft", { n: left })}
      >
        {t("undo.secondsLeft", { n: left })}
      </span>
      <button
        type="button"
        className="icon-button"
        aria-label={t("undo.dismiss")}
        onClick={() => {
          leave();
          onDismiss(toast.id);
        }}
      >
        ✕
      </button>
    </div>
  );
}
