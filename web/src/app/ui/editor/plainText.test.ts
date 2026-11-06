// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// Rich to plain (ADR-051 §3): each format degrades to something sensible.

import { describe, expect, it } from "vitest";
import { htmlToText } from "./plainText";

describe("htmlToText", () => {
  it("keeps paragraphs on separate lines (unchanged behaviour)", () => {
    expect(htmlToText("<p>one</p><p>two</p>")).toBe("one\ntwo");
  });

  it("a block quote's lines start with > ", () => {
    expect(htmlToText("<p>before</p><blockquote><p>quoted</p><p>more</p></blockquote>")).toBe("before\n> quoted\n> more");
  });

  it("a quote inside a quote gets two", () => {
    expect(htmlToText("<blockquote><p>a</p><blockquote><p>b</p></blockquote></blockquote>")).toBe("> a\n> > b");
  });

  it("a code block is kept verbatim: line breaks and indentation survive", () => {
    expect(htmlToText("<p>run:</p><pre><code>if (x) {\n    y();   // two  spaces\n}</code></pre><p>done</p>")).toBe(
      "run:\nif (x) {\n    y();   // two  spaces\n}\ndone",
    );
  });

  it("inline code is its text", () => {
    expect(htmlToText("<p>use <code>a  b</code> here</p>")).toBe("use a b here");
  });

  it("strike-through, colour, highlight, font and size are dropped, the text kept", () => {
    expect(
      htmlToText(
        '<p><s>old</s> <span style="color: #cc0000; background-color: #ffff00; font-family: Georgia, serif; font-size: 18px">new</span></p>',
      ),
    ).toBe("old new");
  });

  it("subscript and superscript are kept as their text", () => {
    expect(htmlToText("<p>H<sub>2</sub>O and x<sup>2</sup></p>")).toBe("H2O and x2");
  });

  it("paragraph indentation becomes leading spaces, two per level", () => {
    expect(htmlToText('<p style="margin-left: 2em">one</p><p style="margin-left: 6em">three</p><p>none</p>')).toBe(
      "  one\n      three\nnone",
    );
  });

  it("an indent on the FIRST line is not trimmed away", () => {
    expect(htmlToText('<p style="margin-left: 4em">x</p>')).toBe("    x");
  });

  it("an indent inside a quote follows the quote's marker", () => {
    expect(htmlToText('<blockquote><p style="margin-left: 2em">x</p></blockquote>')).toBe(">   x");
  });

  it("an off-range or hostile margin is no indent", () => {
    expect(htmlToText('<p style="margin-left: -9999px">a</p><p style="margin-left: 2em !important">b</p>')).toBe("a\nb");
  });

  it("lists keep their markers, and a nested list is indented under its item", () => {
    expect(htmlToText("<ul><li><p>a</p><ul><li><p>b</p></li></ul></li><li><p>c</p></li></ul>")).toBe("• a\n  • b\n• c");
  });

  it("a stray private-use marker in the input cannot make a space", () => {
    expect(htmlToText("<p>x</p>")).toBe("x");
  });
});
