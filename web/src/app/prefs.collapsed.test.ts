// SPDX-License-Identifier: AGPL-3.0-only

// The folded-sections memory (#375 increment 7).
//
// The round trip is the whole of it, and it is worth a test for one reason: the
// failure is not an error. A decode that dropped a member simply unfolds a
// section the user folded, on every reload, forever — and the user reads that
// as "this app does not remember anything" rather than as a bug worth
// reporting. So both directions are pinned, including the malformed inputs a
// hand-edited or half-written localStorage value can produce.
//
// The pure pair is tested rather than load/save: these run with no DOM and no
// localStorage (vitest's default node environment), and the wrappers add only
// the try/catch that turns a storage-disabled browser into an empty set.

import { describe, expect, it } from "vitest";

import { decodeCollapsed, encodeCollapsed } from "./prefs";
import { UNSORTED } from "./ui/desk/flatten";

const A = "01J8Z5R2K7QW3M4N5P6Q7R8S9T";
const B = "01J8Z5R2K7QW3M4N5P6Q7R8SAA";

describe("encodeCollapsed / decodeCollapsed", () => {
  it("round-trips a set of area ids", () => {
    const set = new Set([A, B]);
    expect(decodeCollapsed(encodeCollapsed(set))).toEqual(set);
  });

  it("round-trips Needs sorting, which is a sentinel rather than an id", () => {
    // The one section with no area id. If the sentinel did not survive, the
    // section that most needs folding on a phone would be the one that never
    // stays folded.
    const set = new Set([A, UNSORTED]);
    expect(decodeCollapsed(encodeCollapsed(set))).toEqual(set);
  });

  it("is stable: the same members always produce the same string", () => {
    expect(encodeCollapsed(new Set([A, B]))).toBe(encodeCollapsed(new Set([B, A])));
  });

  it("round-trips the empty set as nothing folded", () => {
    expect(encodeCollapsed(new Set())).toBe("");
    expect(decodeCollapsed("")).toEqual(new Set());
  });

  it("reads an absent value as nothing folded", () => {
    // The first run on a device, and the storage-disabled browser: neither is
    // an error and neither may hide a section.
    expect(decodeCollapsed(null)).toEqual(new Set());
  });

  it("survives a mangled value instead of throwing inside a render", () => {
    expect(decodeCollapsed(",,")).toEqual(new Set());
    expect(decodeCollapsed(`,${A},,${B},`)).toEqual(new Set([A, B]));
  });
});
