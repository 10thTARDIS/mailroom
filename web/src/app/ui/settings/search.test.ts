// SPDX-License-Identifier: AGPL-3.0-only

// Each test is named after the way the search can fail the user, not after the
// branch it covers. The load-bearing one is "an alias finds the row that
// contains it": without VALUES matching, this whole field is a table of contents
// over twelve headings the user already has on screen.

import { describe, expect, it } from "vitest";

import { filterSettings } from "./search";
import type { SearchableRow } from "./search";

/** Three rows in the shape SettingsRoute builds them: a name, the state line as
 * worded, and the values the row names. */
const ROWS: SearchableRow[] = [
  {
    id: "appearance",
    name: "Appearance",
    stateText: "theme dark · density normal · message opens beside the list",
    values: [],
  },
  {
    id: "identities",
    name: "Identities and signatures",
    stateText: "3 identities across 2 accounts · 2 signatures · 1 template",
    // The alias itself is a VALUE: nothing in the state line above says it.
    values: ["you@example.com", "shop@example.com", "*@example.org", "Sign-off"],
  },
  {
    id: "reading",
    name: "Reading and safety",
    stateText: "remote images: ask each time · 2 senders always allowed",
    values: ["news@example.net", "anyone at example.org"],
  },
];

describe("filterSettings", () => {
  it("matches a row by its name", () => {
    expect(filterSettings(ROWS, "appear").map((r) => r.id)).toEqual(["appearance"]);
  });

  it("matches a row by a word of its state line", () => {
    expect(filterSettings(ROWS, "dark").map((r) => r.id)).toEqual(["appearance"]);
    expect(filterSettings(ROWS, "signatures").map((r) => r.id)).toEqual(["identities"]);
  });

  it("finds the row that CONTAINS an alias, by the alias", () => {
    // THE POINT OF THE VALUES CHANNEL. "shop@example.com" appears nowhere in the
    // Identities row's name or state line — a search over those two would
    // return nothing for the most specific thing a user could type.
    expect(filterSettings(ROWS, "shop@example.com").map((r) => r.id)).toEqual(["identities"]);
    // And by a fragment of one, because nobody types a whole address into a
    // filter field.
    expect(filterSettings(ROWS, "shop@").map((r) => r.id)).toEqual(["identities"]);
  });

  it("finds a remembered sender in the row that holds it", () => {
    expect(filterSettings(ROWS, "example.net").map((r) => r.id)).toEqual(["reading"]);
  });

  it("returns nothing when nothing matches", () => {
    expect(filterSettings(ROWS, "quiet hours")).toEqual([]);
  });

  it("returns every row for an empty or whitespace query", () => {
    expect(filterSettings(ROWS, "").map((r) => r.id)).toEqual([
      "appearance",
      "identities",
      "reading",
    ]);
    expect(filterSettings(ROWS, "   ").map((r) => r.id)).toEqual([
      "appearance",
      "identities",
      "reading",
    ]);
  });

  it("is case-insensitive in both directions", () => {
    expect(filterSettings(ROWS, "APPEARANCE").map((r) => r.id)).toEqual(["appearance"]);
    expect(filterSettings(ROWS, "sign-off").map((r) => r.id)).toEqual(["identities"]);
  });

  it("narrows with a second term rather than widening", () => {
    // Both terms are present on the Appearance row but in the other order, and
    // "density" is on no other row — so two words must still find it, and must
    // not drag in a row that matches only one of them.
    expect(filterSettings(ROWS, "density theme").map((r) => r.id)).toEqual(["appearance"]);
    expect(filterSettings(ROWS, "density signatures")).toEqual([]);
  });

  it("keeps the caller's row order and does not mutate the input", () => {
    const before = ROWS.map((r) => r.id);
    expect(filterSettings(ROWS, "e").map((r) => r.id)).toEqual(before);
    expect(ROWS.map((r) => r.id)).toEqual(before);
  });
});
