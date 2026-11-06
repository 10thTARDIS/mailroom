// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// Rich to plain (ADR-051 §3): each format degrades to something sensible.

import { describe, expect, it } from "vitest";
import { htmlToText, TABLE_COLUMN_MAX } from "./plainText";

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

// Tables become aligned text (ADR-051 §3): columns padded to the widest cell in
// code points, a dash rule under a header row, cells' line breaks as spaces, long
// cells cut at TABLE_COLUMN_MAX with an ellipsis.
describe("htmlToText: tables", () => {
  const td = (s: string) => `<td><p>${s}</p></td>`;
  const th = (s: string) => `<th scope="col"><p>${s}</p></th>`;
  const table = (...rows: string[]) => `<table><tbody>${rows.map((r) => `<tr>${r}</tr>`).join("")}</tbody></table>`;

  it("aligns columns to the widest cell, with two spaces between, and a rule under the header row", () => {
    const html = table(th("Item") + th("Qty") + th("Price"), td("Apple") + td("3") + td("1.50"), td("Watermelon") + td("12") + td("0.2"));
    expect(htmlToText(html)).toBe(
      ["Item        Qty  Price", "----------  ---  -----", "Apple       3    1.50", "Watermelon  12   0.2"].join("\n"),
    );
  });

  it("no header rule when the first row is not all header cells", () => {
    expect(htmlToText(table(td("a") + td("bb"), td("ccc") + td("d")))).toBe("a    bb\nccc  d");
  });

  it("empty cells keep their column; a row's trailing padding is trimmed", () => {
    expect(htmlToText(table(th("A") + th("B") + th("C"), td("1") + td("") + td("3"), td("") + td("") + td("")))).toBe(
      ["A  B  C", "-  -  -", "1     3", ""].join("\n").trimEnd(),
    );
  });

  it("a table that starts with an empty cell keeps its alignment (the padding survives the trim)", () => {
    expect(htmlToText(table(td("") + td("x"), td("long") + td("y")))).toBe("      x\nlong  y");
  });

  it("a cell's inner line breaks become spaces", () => {
    expect(htmlToText(table(td("one<br>two") + td("x")))).toBe("one two  x");
    expect(htmlToText(table("<td><p>first</p><p>second</p></td><td>x</td>"))).toBe("first second  x");
  });

  it("counts code points: an accented letter and an emoji are one column each", () => {
    const out = htmlToText(table(th("Nom") + th("n"), td("Éloïse") + td("1"), td("😀😀😀😀😀😀") + td("2")));
    const lines = out.split("\n");
    // Both data rows put the second column at the same code point offset.
    const col2 = lines.slice(2).map((l) => Array.from(l).length - 1);
    expect(col2[0]).toBe(col2[1]);
    expect(Array.from(lines[0]!).indexOf("n")).toBe(Array.from("😀😀😀😀😀😀").length + 2);
  });

  it("a column is capped at TABLE_COLUMN_MAX and a longer cell ends in an ellipsis", () => {
    expect(TABLE_COLUMN_MAX).toBe(40);
    const long = "x".repeat(100);
    const out = htmlToText(table(th("A") + th("B"), td(long) + td("y")));
    const row = out.split("\n")[2]!;
    expect(row).toBe(`${"x".repeat(39)}…  y`);
    expect(Array.from(out.split("\n")[1]!.split("  ")[0]!).length).toBe(40);
  });

  it("a cell of exactly the cap is not cut", () => {
    const exact = "y".repeat(TABLE_COLUMN_MAX);
    expect(htmlToText(table(td(exact) + td("z")))).toBe(`${exact}  z`);
  });

  it("the cut never splits a code point", () => {
    const out = htmlToText(table(td("😀".repeat(60)) + td("z")));
    const first = out.split("  ")[0]!;
    expect(Array.from(first).length).toBe(40);
    expect(first.endsWith("…")).toBe(true);
    expect(first).not.toMatch(/[\ud800-\udbff]$/);
  });

  it("text before and after a table stays on its own lines; a table inside a quote is quoted", () => {
    expect(htmlToText(`<p>before</p>${table(td("a") + td("b"))}<p>after</p>`)).toBe("before\na  b\nafter");
    expect(htmlToText(`<blockquote>${table(th("h") + th("i"), td("a") + td("b"))}</blockquote>`)).toBe("> h  i\n> -  -\n> a  b");
  });

  it("a table inside a table is flattened into its cell's text, not drawn twice", () => {
    expect(htmlToText(table(td("outer" + table(td("inner"))) + td("x")))).toBe("outer inner  x");
  });

  it("a table with no rows gives no text and the surrounding text is untouched", () => {
    expect(htmlToText("<p>a</p><table></table><p>b</p>")).toBe("a\nb");
  });

  it("control: the check can fail (the same table with the columns NOT aligned differs)", () => {
    const aligned = htmlToText(table(td("a") + td("b"), td("ccc") + td("d")));
    expect(aligned).not.toBe("a b\nccc d");
  });
});
