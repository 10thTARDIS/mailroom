// SPDX-License-Identifier: AGPL-3.0-only

// THE TWO HALVES OF TAKING OVER THE MAIN AREA (#447): keeping Tab inside a
// surface while it is up, and handing focus back when it goes.
//
// There were three copies of the first half — the attachment viewer, the desk's
// confirm dialog and the composer each had the same fifteen lines of
// querySelectorAll / first / last / wrap, with three subtly different selectors —
// and none of the second half: closing any of them left focus wherever the DOM
// happened to drop it, which is `<body>`. From `<body>` the shell's key chain
// still works (it listens on `window`), so nothing looks broken; what is gone is
// the reader's PLACE in the list, and the grid's `aria-activedescendant` is
// announcing a row that nothing is focused on.
//
// WHY THE WRAP DECISION IS A PURE FUNCTION. `tabTarget` is the whole of the trap's
// logic and it is the part that is wrong in most hand-rolled traps: the case
// nobody writes is "focus is not on any of the controls we found", which is what a
// dialog that focuses its own container looks like on the FIRST Tab. Forward from
// there, the browser is already right and the trap must keep its hands off;
// BACKWARD from there, the browser is about to walk out of the dialog. Getting
// that one case wrong leaks focus onto a live message list behind a modal, which
// is the failure the traps exist to prevent — so it is tested rather than reasoned
// about in a comment.

/**
 * Everything a surface of this shape can contain that Tab reaches.
 *
 * `[contenteditable]` is in the list because the composer's rich-text body is one
 * and the composer is the surface most likely to be trapped; `[tabindex='-1']` is
 * excluded because that is precisely how this app marks "focusable, but not by
 * Tab" (a dialog container, a desk row's buttons).
 */
export const FOCUSABLE =
  "button:not([disabled]), [href], input:not([disabled]), select, textarea, [contenteditable], [tabindex]:not([tabindex='-1'])";

/**
 * Which of `count` controls Tab should move to, or null to let the browser do
 * what it was going to.
 *
 * `active` is the index of the currently focused control, or -1 when focus is on
 * something that is not one of them (the dialog's own container, an iframe).
 */
export function tabTarget(count: number, active: number, shift: boolean): number | null {
  // Nothing to trap: a surface with no controls at all must not swallow Tab, or
  // the keyboard would have no way out of it whatsoever.
  if (count <= 0) return null;
  if (active < 0) {
    // Forward from outside the set — the container, which precedes its own
    // children — is already going into the surface. Backward is about to leave it.
    return shift ? count - 1 : null;
  }
  if (shift) return active === 0 ? count - 1 : null;
  return active === count - 1 ? 0 : null;
}

/** The controls inside `root` that Tab can reach, in document order. */
export function focusablesIn(root: HTMLElement): HTMLElement[] {
  const all = [...root.querySelectorAll<HTMLElement>(FOCUSABLE)];
  // A control that is not rendered is not a tab stop. `offsetParent` is the cheap
  // test for that, and `isContentEditable` is its exception (an editable surface
  // can legitimately report no offsetParent).
  const visible = all.filter((el) => el.offsetParent !== null || el.isContentEditable);
  // ⚠ FALL BACK TO THE UNFILTERED LIST rather than to nothing. If the visibility
  // test is ever wrong about this surface, an empty list means the trap stops
  // trapping — a silent leak onto whatever is behind it — whereas a stale list
  // still keeps Tab inside. The failure modes are not symmetrical, so the
  // fallback is the safe one.
  return visible.length > 0 ? visible : all;
}

/**
 * The trap. Call it from a keydown handler on (or above) `root`; it acts only on
 * Tab and only at the two edges, so every other Tab inside the surface is the
 * browser's own ordinary move.
 */
export function trapTab(
  root: HTMLElement | null | undefined,
  ev: { key: string; shiftKey: boolean; preventDefault: () => void },
): void {
  if (ev.key !== "Tab" || !root) return;
  const nodes = focusablesIn(root);
  const active = document.activeElement as HTMLElement | null;
  const to = tabTarget(nodes.length, active ? nodes.indexOf(active) : -1, ev.shiftKey);
  if (to === null) return;
  ev.preventDefault();
  nodes[to]?.focus();
}

/**
 * ON CLOSE: the opener, if it is still answerable, or a named fallback.
 *
 * Pulled out for the same reason `tabTarget` was — the wrong answer this could
 * give (send focus to a node that has since left the document, which silently
 * focuses nothing and leaves it wherever the browser drops it, `<body>`) is a
 * pure decision with no DOM in it, so it is tested the same way rather than
 * eyeballed inside a component. The caller does the actual `.focus()` call,
 * because *which* fallback element to use is the caller's own surface, not this
 * module's.
 */
export function closeFocusTarget(opener: { isConnected: boolean } | null): "opener" | "fallback" {
  return opener !== null && opener.isConnected ? "opener" : "fallback";
}

/**
 * HAND FOCUS BACK TO THE MESSAGE LIST — the other half.
 *
 * The desk's and the tray page's lists are both `.desk-list`, a `role="grid"` with
 * one tab stop and its own `aria-activedescendant`, so putting focus there puts
 * the reader back on the row they came from with no further bookkeeping.
 *
 * DEFERRED BY A FRAME, because the caller is a component that is going away: the
 * element it is handing focus to may still be `display: none` at the moment the
 * cleanup runs (the surface being removed and the list being shown are the same
 * commit), and focusing a `display: none` element does nothing at all — silently.
 *
 * AND IT REFUSES TO STEAL. If, by the time the frame arrives, focus is on
 * something real, that something took it deliberately (a composer opening over the
 * message that just closed) and this must not fight it. Only `<body>`, nothing at
 * all, or a detached node mean "focus was dropped".
 */
export function focusMessageListSoon(): void {
  requestAnimationFrame(() => {
    const active = document.activeElement as HTMLElement | null;
    if (active && active !== document.body && active.isConnected) return;
    document.querySelector<HTMLElement>(".desk-list")?.focus();
  });
}
