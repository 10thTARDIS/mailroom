// SPDX-License-Identifier: AGPL-3.0-only
import { describe, expect, it } from "vitest";

// Every modal stops its own Escape. The shell owns one window-level Escape
// chain (ui/Shell.tsx) that peels a route: it closes the open message or the
// composer. A modal whose Escape handler lets the keystroke bubble closes
// itself AND the layer behind it. The risky-download question shipped with
// exactly that bug, and four more modals had it. This scan keeps it fixed.
const sources = import.meta.glob(["./**/*.tsx", "!./**/*.test.tsx"], {
  query: "?raw",
  import: "default",
  eager: true,
});

const modals = Object.entries(sources).filter(([, src]) => /className="modal-backdrop/.test(src));

describe("modals stop their own Escape", () => {
  it("finds the modals it claims to check", () => {
    // A count floor, so a moved directory or renamed class cannot make the
    // check below pass over nothing.
    expect(modals.length).toBeGreaterThanOrEqual(9);
  });

  it.each(modals)("%s stops Escape for each backdrop", (_name, src) => {
    const backdrops = (src.match(/className="modal-backdrop/g) ?? []).length;
    const stops = (src.match(/\.stopPropagation\(\)/g) ?? []).length;
    expect(stops).toBeGreaterThanOrEqual(backdrops);
  });
});
