// SPDX-License-Identifier: AGPL-3.0-only

// The trap's one decision. No DOM here (this repo's web tests have no renderer —
// ui/attachmentPreviewPolicy.ts's note), which is exactly why the decision was
// pulled out of the three components that used to make it inline.

import { describe, expect, it } from "vitest";

import { closeFocusTarget, tabTarget } from "./focusTrap";

describe("tabTarget — where Tab goes inside a surface that has taken over", () => {
  it("does nothing in the middle, so an ordinary Tab stays the browser's", () => {
    expect(tabTarget(5, 2, false)).toBeNull();
    expect(tabTarget(5, 2, true)).toBeNull();
  });

  it("wraps forward off the last control and backward off the first", () => {
    expect(tabTarget(5, 4, false)).toBe(0);
    expect(tabTarget(5, 0, true)).toBe(4);
    // ...and NOT the other way round on the same edges, which is the swap that
    // would send focus to the wrong end and look like the trap working.
    expect(tabTarget(5, 4, true)).toBeNull();
    expect(tabTarget(5, 0, false)).toBeNull();
  });

  it("holds a single control in place", () => {
    // A surface with one button: both edges are that button, so both directions
    // return to it rather than letting Tab out.
    expect(tabTarget(1, 0, false)).toBe(0);
    expect(tabTarget(1, 0, true)).toBe(0);
  });

  it("never swallows Tab when there is nothing to trap", () => {
    // A surface with no controls must not trap: that is a keyboard with no way out
    // at all, which is worse than a leak.
    expect(tabTarget(0, -1, false)).toBeNull();
    expect(tabTarget(0, -1, true)).toBeNull();
    expect(tabTarget(0, 0, false)).toBeNull();
  });

  it("catches Shift+Tab from the container itself, and leaves plain Tab alone", () => {
    // THE CASE HAND-ROLLED TRAPS MISS. A dialog that focuses its own container
    // (the attachment viewer does, deliberately) has focus on an element that is
    // not in the set: -1. Forward, the browser is already about to enter the
    // dialog and the trap must keep out of it; BACKWARD it is about to walk out of
    // the dialog onto the list behind — which is the whole thing the trap exists
    // to stop.
    expect(tabTarget(3, -1, false)).toBeNull();
    expect(tabTarget(3, -1, true)).toBe(2);
  });
});

describe("closeFocusTarget — where a closing surface hands focus back", () => {
  it("goes to the opener when it is still in the document", () => {
    expect(closeFocusTarget({ isConnected: true })).toBe("opener");
  });

  it("falls back when the opener has since been removed", () => {
    // The row that opened this got archived, refiled, or otherwise unmounted
    // while the surface was up — the exact case a stale ref cannot answer for.
    expect(closeFocusTarget({ isConnected: false })).toBe("fallback");
  });

  it("falls back when there was no opener at all", () => {
    expect(closeFocusTarget(null)).toBe("fallback");
  });
});
