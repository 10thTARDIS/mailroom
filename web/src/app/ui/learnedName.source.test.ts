// SPDX-License-Identifier: AGPL-3.0-only
import { describe, expect, it } from "vitest";

// The feature's name is "Learned sorting" (#168). The old phrase for the area
// tag must not creep back into any shipped web source.
const sources = import.meta.glob(["../**/*.ts", "../**/*.tsx", "!../**/*.test.ts", "!../**/*.test.tsx"], {
  query: "?raw",
  import: "default",
  eager: true,
});

describe("the old name is gone", () => {
  it("scans real files", () => {
    expect(Object.keys(sources).length).toBeGreaterThan(50);
  });
  it('no source says "learned from your moves"', () => {
    const hits = Object.entries(sources).filter(([, src]) => /learned from your moves/i.test(src as string));
    expect(hits.map(([f]) => f)).toEqual([]);
  });
});
