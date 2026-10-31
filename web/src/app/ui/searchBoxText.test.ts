// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { searchBoxTextFor } from "./searchBoxText";

describe("searchBoxTextFor (#724)", () => {
  it("leaves the box alone when the view is the live search the box itself asked for", () => {
    // The user typed "invoice" after the pause that sent "invo": the arriving view
    // must not put "invo" back.
    expect(searchBoxTextFor({ kind: "search", query: "invo", prefix: true }, "invo")).toBeNull();
  });

  it("mirrors any other search, and empties for a non-search view", () => {
    expect(searchBoxTextFor({ kind: "search", query: "from:a" }, null)).toBe("from:a");
    expect(searchBoxTextFor({ kind: "search", query: "from:a" }, "something else")).toBe("from:a");
    expect(searchBoxTextFor({ kind: "role", role: "inbox" }, "invo")).toBe("");
  });
});
