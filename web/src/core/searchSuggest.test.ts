// SPDX-License-Identifier: AGPL-3.0-only

// Tests for search suggestions (#57).
//
// The load-bearing property is the FIRST test: `apply` must always be the input with only
// its last token replaced. A completion that quietly rewrites the rest of the query, or
// that produces a query meaning something other than the row the user clicked, is worse
// than no completion at all — the user would have to re-read their own search to check it.

import { describe, expect, it } from "vitest";

import { MAX_SUGGESTIONS, lastToken, sendersOf, suggestSearch } from "./searchSuggest";
import type { SuggestSources } from "./searchSuggest";

const SRC: SuggestSources = {
  labels: [{ name: "Promotions" }, { name: "Shopping" }, { name: "Big Project" }],
  contacts: [
    { email: "alice@example.com", name: "Alice Adams" },
    { email: "bob@example.net", name: "Natalie Bright" },
    { email: "carol@example.org", name: null },
  ],
  accounts: [{ name: "primary" }, { name: "work" }],
  history: ["from:alice invoice", "is:unread", "label:Shopping"],
};

describe("suggestSearch: it only ever edits the last token", () => {
  it("keeps everything before the token exactly as typed", () => {
    for (const input of ["is:unread lab", "from:bob subject:tax lab", "  spaced   lab"]) {
      for (const s of suggestSearch(input, SRC)) {
        const { start } = lastToken(input);
        expect(s.apply.slice(0, start)).toBe(input.slice(0, start));
      }
    }
  });

  it("does not touch an earlier token that looks like the one being edited", () => {
    const got = suggestSearch("label:Shopping label:Sho", SRC);
    expect(got.length).toBeGreaterThan(0);
    for (const s of got) {
      expect(s.apply.startsWith("label:Shopping ")).toBe(true);
    }
  });
});

describe("suggestSearch: operators", () => {
  it("offers the operators a bare word could start, with what each means", () => {
    const got = suggestSearch("su", SRC);
    expect(got.map((s) => s.label)).toEqual(["subject:"]);
    expect(got[0]?.detail).toBeTruthy();
    expect(got[0]?.kind).toBe("operator");
  });

  it("offers several when the prefix is ambiguous", () => {
    expect(suggestSearch("b", SRC).map((s) => s.label)).toEqual(["body:", "before:"]);
  });

  // The word itself is already a valid full-text search, so echoing it back would waste
  // the top row on something Enter already does.
  it("does not offer the typed word back as a suggestion", () => {
    for (const s of suggestSearch("invoice", SRC)) {
      expect(s.label).not.toBe("invoice");
    }
  });

  it("offers nothing for a word that starts no operator", () => {
    expect(suggestSearch("zzz", SRC)).toEqual([]);
  });
});

describe("suggestSearch: values", () => {
  it("lists every label for a bare label:", () => {
    const got = suggestSearch("label:", SRC);
    expect(got.map((s) => s.label)).toEqual(["Promotions", "Shopping", "Big Project"]);
    expect(got[0]?.kind).toBe("value");
  });

  it("filters labels by what has been typed", () => {
    expect(suggestSearch("label:sho", SRC).map((s) => s.label)).toEqual(["Shopping"]);
  });

  // THE ONE THAT WOULD SILENTLY CORRUPT A QUERY: a value with a space must come back
  // quoted, or `label:Big Project` becomes a label filter plus a stray full-text term.
  it("quotes a value containing a space", () => {
    const got = suggestSearch("label:Big", SRC);
    expect(got[0]?.apply).toBe('label:"Big Project"');
  });

  it("offers the fixed is: and has: values", () => {
    // #623: IS_VALUES used to also carry `unflagged`, which internal/search
    // /parse.go's flagAtom does not accept — a suggestion for a query that
    // always matched nothing. `unread` is now the only `is:un` completion.
    expect(suggestSearch("is:un", SRC).map((s) => s.label)).toEqual(["unread"]);
    expect(suggestSearch("is:tr", SRC).map((s) => s.label)).toEqual(["trashed"]);
    expect(suggestSearch("has:", SRC).map((s) => s.label)).toEqual([
      "attachment",
      "pdf",
      "image",
      "document",
      "spreadsheet",
      "presentation",
      "unsubscribe",
    ]);
    expect(suggestSearch("is:sn", SRC).map((s) => s.label)).toEqual(["snoozed"]);
    expect(suggestSearch("is:imp", SRC).map((s) => s.label)).toEqual(["important"]);
  });

  // #723: in: is new, and its three values are the whole vocabulary.
  it("offers the in: values", () => {
    expect(suggestSearch("in:", SRC).map((s) => s.label)).toEqual(["anywhere", "trash", "junk"]);
    expect(suggestSearch("in:j", SRC).map((s) => s.label)).toEqual(["junk"]);
  });

  // #722/#723: every new operator is discoverable from its first letters.
  it("offers the new operators", () => {
    for (const op of [
      "alias:",
      "area:",
      "list:",
      "filename:",
      "larger:",
      "smaller:",
      "newer_than:",
      "older_than:",
      "in:",
    ]) {
      const got = suggestSearch(op.slice(0, -1), SRC).map((s) => s.label);
      expect(got).toContain(op);
    }
  });

  it("completes an address on either the address or the display name", () => {
    expect(suggestSearch("from:ali", SRC).map((s) => s.label)).toContain("alice@example.com");
    expect(suggestSearch("from:natal", SRC).map((s) => s.label)).toEqual(["bob@example.net"]);
  });

  // "ali" is a prefix of alice@ and also appears inside "Natalie". The more specific
  // intent must win, or the top row is the wrong person.
  it("ranks an address-prefix match above a name-substring match", () => {
    const got = suggestSearch("from:al", {
      ...SRC,
      contacts: [
        { email: "bob@example.net", name: "Natalie Bright" },
        { email: "alice@example.com", name: "Alice Adams" },
      ],
    });
    expect(got[0]?.label).toBe("alice@example.com");
  });

  it("survives a contact with no display name", () => {
    expect(suggestSearch("from:carol", SRC).map((s) => s.label)).toEqual(["carol@example.org"]);
  });

  // An open set (dates, IMAP keywords) gets no value suggestions rather than wrong ones.
  it("offers no values for operators whose values it cannot know", () => {
    expect(suggestSearch("before:2", SRC)).toEqual([]);
    expect(suggestSearch("keyword:x", SRC)).toEqual([]);
  });

  it("offers nothing rather than everything for an unknown operator", () => {
    expect(suggestSearch("nonsense:x", SRC)).toEqual([]);
  });
});

describe("suggestSearch: history", () => {
  it("offers recent searches for an empty box, most recent first", () => {
    const got = suggestSearch("", SRC);
    expect(got.map((s) => s.label)).toEqual(["from:alice invoice", "is:unread", "label:Shopping"]);
    expect(got[0]?.kind).toBe("history");
    // A history row applies the WHOLE query, since that is what was remembered.
    expect(got[0]?.apply).toBe("from:alice invoice");
  });

  it("offers only whitespace-trimmed emptiness, not a half-typed query", () => {
    expect(suggestSearch("   ", SRC).map((s) => s.kind)).toEqual(["history", "history", "history"]);
    // Once something is typed, history is not mixed in — a half-typed query and a
    // remembered one answer different questions.
    for (const s of suggestSearch("is:", SRC)) {
      expect(s.kind).not.toBe("history");
    }
  });

  it("is empty when there is no history and nothing typed", () => {
    expect(suggestSearch("", { ...SRC, history: [] })).toEqual([]);
  });
});

describe("sendersOf", () => {
  it("de-duplicates by address, case-insensitively, keeping the first name seen", () => {
    const got = sendersOf([
      { from: [{ email: "Alice@Example.com", name: "Alice Adams" }] },
      { from: [{ email: "alice@example.com", name: "A. Adams" }] },
      { from: [{ email: "bob@example.net" }] },
    ]);
    expect(got).toEqual([
      { email: "Alice@Example.com", name: "Alice Adams" },
      { email: "bob@example.net" },
    ]);
  });

  it("keeps most-recent-first order, which is the order it was given", () => {
    const got = sendersOf([
      { from: [{ email: "new@example.com" }] },
      { from: [{ email: "old@example.com" }] },
    ]);
    expect(got.map((s) => s.email)).toEqual(["new@example.com", "old@example.com"]);
  });

  it("skips messages with no from, a null from, or a blank address", () => {
    const got = sendersOf([{}, { from: null }, { from: [] }, { from: [{ email: "  " }] }]);
    expect(got).toEqual([]);
  });
});

describe("suggestSearch: bounds", () => {
  it("never returns more than the cap", () => {
    const many = Array.from({ length: 200 }, (_, i) => ({ name: `Label ${i}` }));
    expect(suggestSearch("label:", { ...SRC, labels: many }).length).toBe(MAX_SUGGESTIONS);
    const manyContacts = Array.from({ length: 200 }, (_, i) => ({ email: `a${i}@example.com` }));
    expect(suggestSearch("from:a", { ...SRC, contacts: manyContacts }).length).toBe(
      MAX_SUGGESTIONS,
    );
    const manyQueries = Array.from({ length: 200 }, (_, i) => `q${i}`);
    expect(suggestSearch("", { ...SRC, history: manyQueries }).length).toBe(MAX_SUGGESTIONS);
  });

  it("handles an input that is only separators", () => {
    expect(() => suggestSearch(":", SRC)).not.toThrow();
    expect(() => suggestSearch("::::", SRC)).not.toThrow();
    expect(() => suggestSearch('label:"', SRC)).not.toThrow();
  });
});
