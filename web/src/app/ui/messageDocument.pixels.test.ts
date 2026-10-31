// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { blockedPixelCount, restoreRemoteContent } from "./messageDocument";

// Tracking-pixel suppression, client half (#55 increment 2).
//
// The server parks a beacon's URL under a name this module does not restore, so
// the suppression is structural rather than a decision made here. These tests
// exist because "structural" is a claim, and an untested claim about a security
// boundary is the thing this project keeps paying for.

// What internal/api/sanitize.go actually emits for each case. Written as literal
// server output rather than built by a helper: if the server's attribute names
// change, these strings stop matching reality and the Go-side
// TestRemoteContentContract fails in the same commit.
const REAL_IMAGE = '<img data-mailroom-remote-src="https://cdn.example/hero.png" data-mailroom-blocked="">';
// ⚠ NOTE THE LEADING SPACE in the `src=` assertions below. The sanitizer writes
// ` src="` before an attribute, and `data-mailroom-pixel-src="` also ENDS in
// `src="` — so an assertion without the boundary matches the PARKED attribute
// itself. Both directions were wrong here at first: the negative one passed
// trivially, and the positive one would have passed whether or not the image was
// ever restored. internal/api/sanitize_test.go documents the same trap.
const BEACON = '<img data-mailroom-pixel-src="https://tracker.example/o.gif" width="1" height="1" data-mailroom-blocked="">';
const BEACON_SRCSET = '<img data-mailroom-pixel-srcset="https://tracker.example/o.gif 1x" width="1" height="1" data-mailroom-blocked="">';

describe("restoreRemoteContent", () => {
  it("restores a real remote image", () => {
    const out = restoreRemoteContent(REAL_IMAGE);
    expect(out).toContain(' src="https://cdn.example/hero.png"');
    expect(out).not.toContain("data-mailroom-remote-src");
  });

  // ⚠ THE ASSERTION THE WHOLE INCREMENT RESTS ON. If restoring ever turned a
  // parked beacon back into a live src, the feature would be decorative while
  // still appearing to work: the banner would say pixels were blocked, and they
  // would load anyway.
  it("does NOT restore a tracking pixel, even for an allowed sender", () => {
    const out = restoreRemoteContent(BEACON);
    expect(out).toContain('data-mailroom-pixel-src="https://tracker.example/o.gif"');
    expect(out).not.toContain(' src="https://tracker.example/o.gif"');
  });

  it("does NOT restore a tracking pixel's srcset either", () => {
    // The hole: handling only `src` would leave a beacon's srcset restorable.
    const out = restoreRemoteContent(BEACON_SRCSET);
    expect(out).not.toContain(' srcset="https://tracker.example/o.gif 1x"');
    expect(out).toContain("data-mailroom-pixel-srcset=");
  });

  it("restores the real images in a message that also carries beacons", () => {
    // The common case on a real mailbox: every allowed message has both.
    const out = restoreRemoteContent(REAL_IMAGE + BEACON);
    expect(out).toContain(' src="https://cdn.example/hero.png"');
    expect(out).not.toContain(' src="https://tracker.example/o.gif"');
  });
});

describe("blockedPixelCount", () => {
  it("is zero when there are none", () => {
    expect(blockedPixelCount(REAL_IMAGE)).toBe(0);
    expect(blockedPixelCount("<p>plain text</p>")).toBe(0);
  });

  it("counts every beacon, not every message", () => {
    expect(blockedPixelCount(BEACON)).toBe(1);
    expect(blockedPixelCount(BEACON + REAL_IMAGE + BEACON)).toBe(2);
  });

  it("counts src and srcset beacons together", () => {
    expect(blockedPixelCount(BEACON + BEACON_SRCSET)).toBe(2);
  });

  // ⚠ A message that merely MENTIONS the marker in its text must not inflate the
  // count. It cannot produce a live one — the server escapes every `"` in text to
  // `&#34;`, which is why the restore is a sound string replacement at all — so
  // the sequence with its quote can only appear where the server wrote it.
  it("is not fooled by a message that talks about the marker", () => {
    const hostile = "<p>data-mailroom-pixel-src=&#34;https://evil.example/x.gif&#34;</p>";
    expect(blockedPixelCount(hostile)).toBe(0);
  });
});
