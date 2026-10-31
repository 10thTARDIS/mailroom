// SPDX-License-Identifier: AGPL-3.0-only

// #724: suggestions for the token the CARET is in, contacts from the directory
// for every address operator, and area names.

import { describe, expect, it } from "vitest";

import { suggestSearch } from "./searchSuggest";
import type { SuggestSources } from "./searchSuggest";

const SRC: SuggestSources = {
  labels: [{ name: "Promotions" }],
  contacts: [
    { email: "alice@example.com", name: "Alice Adams" },
    { email: "bob@example.net", name: "Natalie Bright" },
    { email: "carol@example.org", name: null },
  ],
  accounts: [],
  history: [],
};

describe("suggestSearch: the caret's token", () => {
  it("completes a token in the middle and keeps what follows it exactly", () => {
    const input = "from:ali invoice is:unread";
    const got = suggestSearch(input, SRC, "from:ali".length);
    const s = got.find((g) => g.label === "alice@example.com");
    expect(s?.apply).toBe("from:alice@example.com invoice is:unread");
    // The caret lands just after the inserted value, not at the end of the query.
    expect(s?.caret).toBe("from:alice@example.com".length);
  });

  it("looks at the caret's token, not the last one", () => {
    // The last token is a bare word that starts no operator; the caret is in `from:`.
    // Anywhere inside the token counts as being in it.
    expect(suggestSearch("from:car zzz", SRC, 3).map((s) => s.label)).toEqual(["carol@example.org"]);
    expect(suggestSearch("from:car zzz", SRC, 8).map((s) => s.label)).toEqual(["carol@example.org"]);
    expect(suggestSearch("from:car zzz", SRC).map((s) => s.label)).toEqual([]);
  });

  it("keeps a negation or an opening bracket in front of the completed term", () => {
    expect(suggestSearch("-from:ali", SRC)[0]?.apply).toBe("-from:alice@example.com");
    expect(suggestSearch("(from:ali", SRC)[0]?.apply).toBe("(from:alice@example.com");
  });

  it("completes addresses for to:, cc:, alias: and deliveredto: too, inserting the exact address", () => {
    for (const op of ["to:", "cc:", "alias:", "deliveredto:"]) {
      const got = suggestSearch(`${op}nat`, SRC);
      expect(got.map((s) => s.apply)).toEqual([`${op}bob@example.net`]);
      expect(got[0]?.detail).toBe("Natalie Bright");
    }
  });

  it("merges fetched contacts after loaded senders, one row per address", () => {
    const got = suggestSearch("from:", {
      ...SRC,
      directory: [
        { email: "ALICE@example.com", name: "Alice (directory)" },
        { email: "dora@example.com", name: "Dora Explorer" },
      ],
    });
    expect(got.map((s) => s.label)).toEqual([
      "alice@example.com",
      "bob@example.net",
      "carol@example.org",
      "dora@example.com",
    ]);
  });

  it("finds a contact only the directory knows", () => {
    const got = suggestSearch("to:dor", { ...SRC, directory: [{ email: "dora@example.com", name: "Dora" }] });
    expect(got.map((s) => s.apply)).toEqual(["to:dora@example.com"]);
  });

  it("suggests area names after area:, quoted when they need it", () => {
    const src = { ...SRC, areas: [{ name: "Work" }, { name: "Side Projects" }] };
    expect(suggestSearch("area:", src).map((s) => s.label)).toEqual(["Work", "Side Projects"]);
    expect(suggestSearch("area:side", src)[0]?.apply).toBe('area:"Side Projects"');
    // A value the user already opened a quote on is matched without the quote.
    expect(suggestSearch('area:"Side', src)[0]?.apply).toBe('area:"Side Projects"');
  });
});
