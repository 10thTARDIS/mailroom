// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { unflow } from "./flowed";

// The fixtures below are written with VISIBLE trailing spaces on the soft
// breaks; that is the whole point of the feature, and a formatter that strips
// them would silently unmake these tests.

describe("unflow (RFC 3676, #890)", () => {
  it("joins a two-line soft-wrapped paragraph", () => {
    expect(unflow("the quick brown fox \njumps over the lazy dog", false)).toBe(
      "the quick brown fox jumps over the lazy dog",
    );
  });

  it("joins a whole chain of soft wraps into one line", () => {
    expect(unflow("a \nb \nc", false)).toBe("a b c");
  });

  it("keeps the soft-break space with DelSp=no and drops it with DelSp=yes", () => {
    // The space after "two" is the space the sender wrapped on: DelSp=no says
    // keep it, DelSp=yes says the sender folded a real space away.
    expect(unflow("two \nlines", false)).toBe("two lines");
    expect(unflow("two \nlines", true)).toBe("twolines");
  });

  it("keeps a hard break: a line without a trailing space ends the paragraph", () => {
    expect(unflow("first line\nsecond line", false)).toBe("first line\nsecond line");
    // ...and a soft break followed by a hard break stops joining there.
    expect(unflow("soft \nhard\nnext", false)).toBe("soft hard\nnext");
  });

  it("never joins the signature delimiter in either direction", () => {
    // RFC 3676 §4.2: the delimiter "shall NOT be folded or unfolded" — the
    // soft break before it does not swallow it, and it does not swallow the
    // signature after it.
    expect(unflow("body line \n-- \nsignature", false)).toBe("body line \n-- \nsignature");
    expect(unflow("-- \nsignature", true)).toBe("-- \nsignature");
  });

  it("joins quoted lines at the same depth, and not across depths", () => {
    expect(unflow("> a soft \n> wrapped quote", false)).toBe("> a soft wrapped quote");
    expect(unflow(">> deep \n>> still deep", false)).toBe(">> deep still deep");
    // A reply inside a quote is a different depth: the break is real.
    expect(unflow("> outer \n>> inner", false)).toBe("> outer \n>> inner");
  });

  it("un-stuffs a leading single space (space-stuffing, §4.1.2)", () => {
    // Two leading spaces on the wire are one real leading space in the text.
    expect(unflow("  real leading space", false)).toBe(" real leading space");
    // ...and the same after a quote mark, where one of the two spaces is the
    // quote's own.
    expect(unflow(">  a", false)).toBe(">  a");
    // A single leading space after a quote is the quote's space, not content.
    expect(unflow("> plain", false)).toBe("> plain");
  });

  it("passes through text without soft breaks, normalizing CRLF to LF", () => {
    expect(unflow("first\r\nsecond\r\nthird", false)).toBe("first\nsecond\nthird");
    expect(unflow("no wrapping at all here", false)).toBe("no wrapping at all here");
    expect(unflow("", false)).toBe("");
  });

  it("unflows a very long soft-wrapped paragraph in linear time (hostile input)", () => {
    // 200,000 soft lines with DelSp=yes: the old append-and-slice loop was
    // quadratic here and took tens of seconds; joining pieces once is instant.
    const n = 200_000;
    const text = "ab \n".repeat(n) + "end";
    const started = performance.now();
    const out = unflow(text, true);
    const elapsed = performance.now() - started;
    expect(out.length).toBe(2 * n + 3);
    expect(out.startsWith("abab")).toBe(true);
    expect(elapsed).toBeLessThan(2000);
  });
});
