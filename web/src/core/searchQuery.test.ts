// SPDX-License-Identifier: AGPL-3.0-only

// The query tokenizer the search box shares between suggestions and the options
// form (#724), and the live-search gate.

import { describe, expect, it } from "vitest";

import { liveQueryOf, operatorTerm, quoteValue, segments, tokenAtCaret } from "./searchQuery";

describe("segments", () => {
  it("splits where the server's tokenizer does", () => {
    const got = segments('from:alice "end of year" -spam (a OR b) -(c d) OR x https://example.com/y');
    expect(got.map((s) => [s.kind, s.raw])).toEqual([
      ["word", "from:alice"],
      ["word", '"end of year"'],
      ["word", "-spam"],
      ["group", "(a OR b)"],
      ["group", "-(c d)"],
      ["or", "OR"],
      ["word", "x"],
      ["word", "https://example.com/y"],
    ]);
  });

  it("reads operators case-insensitively and only for real operator names", () => {
    const [a, b, c] = segments('FROM:x nope:y "from":z');
    expect([a?.field, a?.value]).toEqual(["from", "x"]);
    expect(b?.field).toBe("");
    expect(c?.field).toBe("");
  });

  it("unquotes a quoted value and remembers that it was quoted", () => {
    const [s] = segments('subject:"end of year"');
    expect(s).toMatchObject({ field: "subject", value: "end of year", quoted: true });
  });

  it("is total: the segments tile the input, separated only by whitespace", () => {
    for (const q of ['(unclosed "quote', ") stray (", "--x", "-", "a\tb\nc", '""', "((()))", ' x "a b" (c d) ']) {
      let at = 0;
      for (const s of segments(q)) {
        expect(q.slice(at, s.start).trim()).toBe("");
        expect(q.slice(s.start, s.end)).toBe(s.raw);
        at = s.end;
      }
      expect(q.slice(at).trim()).toBe("");
    }
  });
});

describe("quoteValue / operatorTerm", () => {
  it("quotes only what the parser would split", () => {
    expect(quoteValue("alice@example.com")).toBe("alice@example.com");
    expect(quoteValue("Side Projects")).toBe('"Side Projects"');
    expect(quoteValue("Work (old)")).toBe('"Work (old)"');
    expect(operatorTerm("area", 'say "hi"')).toBe('area:"say hi"');
  });
});

describe("tokenAtCaret", () => {
  it("finds the token around the caret and its decoration", () => {
    expect(tokenAtCaret("a -(from:al b", 9)).toEqual({ start: 2, end: 11, lead: "-(", text: "from:al" });
    expect(tokenAtCaret("a b ", 4)).toEqual({ start: 4, end: 4, lead: "", text: "" });
  });
});

describe("liveQueryOf: what a pause in typing runs", () => {
  it("runs a finished or half-typed bare word, as a prefix while it is still being typed", () => {
    expect(liveQueryOf("from:alice invo")).toEqual({ query: "from:alice invo", prefix: true });
    // A trailing space says the word is finished: whole-word matching, typo tolerance on.
    expect(liveQueryOf("from:alice invoice ")).toEqual({ query: "from:alice invoice", prefix: false });
  });

  it("never prefixes an operator value, a phrase or an exclusion", () => {
    expect(liveQueryOf("from:alice")?.prefix).toBe(false);
    expect(liveQueryOf('"end of year"')?.prefix).toBe(false);
    expect(liveQueryOf("invoice -spa")?.prefix).toBe(false);
  });

  it("waits while a term is incomplete", () => {
    for (const raw of ["", "   ", "from:", "invoice from:", 'subject:"end of', "invoice -", "invoice ("]) {
      expect(liveQueryOf(raw)).toBeNull();
    }
  });
});
