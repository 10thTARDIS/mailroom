// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The value gate for inline styles (ADR-051 beta 2): what a colour, highlight,
// font family or font size may be, and what a paragraph indent may be. Pure
// functions, so the cases are exhaustive here; the editor tests then show the
// gate is wired into every path (paste, seed, commands, render).

import { describe, expect, it } from "vitest";
import { FONT_FAMILIES, FONT_SIZES, declaredValue, normaliseColour, normaliseFontFamily, normaliseFontSize, spanStyle } from "./styleValues";
import { MAX_INDENT, clampIndent, indentStyle, parseIndent } from "./indentValue";

describe("colours", () => {
  const good: Array<[string, string]> = [
    ["#336699", "#336699"],
    ["#369", "#336699"],
    ["#ABCDEF", "#abcdef"],
    ["rgb(10,20,30)", "#0a141e"],
    ["rgb(10, 20, 30)", "#0a141e"],
    ["  RGB( 255 , 0 , 128 )  ", "#ff0080"],
    ["rgb(0,0,0)", "#000000"],
  ];
  for (const [input, want] of good) {
    it(`accepts ${JSON.stringify(input)} as ${want}`, () => expect(normaliseColour(input)).toBe(want));
  }

  const bad = [
    "red",
    "transparent",
    "currentcolor",
    "inherit",
    "#12",
    "#12345",
    "#1234567",
    "#gggggg",
    "rgb(256,0,0)",
    "rgb(1.5,0,0)",
    "rgb(-1,0,0)",
    "rgb(10%,0,0)",
    "rgb(1 2 3)",
    "rgba(1,2,3,0.5)",
    "hsl(0,0%,0%)",
    "var(--x)",
    "url(https://evil.example/x)",
    "expression(alert(1))",
    "#336699 !important",
    "#336699;background:url(x)",
    "#336699/*c*/",
    "\\75 rl(https://evil.example/x)",
    "#33669\\39",
    "rgb(1,2,3) url(x)",
    "",
    "   ",
    null,
    undefined,
    42,
  ];
  for (const input of bad) {
    it(`refuses ${JSON.stringify(input)}`, () => expect(normaliseColour(input)).toBeNull());
  }
});

describe("font families", () => {
  for (const f of FONT_FAMILIES) {
    it(`accepts the listed family ${f.css}`, () => expect(normaliseFontFamily(f.css)).toBe(f.css));
  }
  it("accepts the same list written with double quotes, other spacing or case, and returns OUR string", () => {
    expect(normaliseFontFamily(`Georgia, "Times New Roman", serif`)).toBe("Georgia, 'Times New Roman', serif");
    expect(normaliseFontFamily("georgia,'times new roman',SERIF")).toBe("Georgia, 'Times New Roman', serif");
    expect(normaliseFontFamily(`  "Courier New" ,Courier,  monospace `)).toBe("'Courier New', Courier, monospace");
  });
  const bad = [
    "'x'",
    "Comic Sans MS, cursive",
    "Arial",
    "Arial, Helvetica",
    "Arial, Helvetica, sans-serif, evil",
    "Arial, Helvetica, sans-serif; src: url(https://evil.example/f)",
    "Arial, Helvetica, sans-serif !important",
    "Ar\\69 al, Helvetica, sans-serif",
    "Arial, Helvetica, sans-serif/*x*/",
    "var(--font)",
    "url(https://evil.example/f)",
    "inherit",
    "",
    null,
  ];
  for (const input of bad) {
    it(`refuses ${JSON.stringify(input)}`, () => expect(normaliseFontFamily(input)).toBeNull());
  }
});

describe("font sizes", () => {
  for (const s of FONT_SIZES) {
    it(`accepts the listed size ${s.css}`, () => expect(normaliseFontSize(s.css)).toBe(s.css));
  }
  it("accepts case and padding", () => expect(normaliseFontSize(" 18PX ")).toBe("18px"));
  for (const input of ["999px", "14px", "12", "12em", "1.5rem", "large", "12px !important", "12px;x:y", "calc(12px)", "var(--s)", "\\31 2px", "-12px", "", null]) {
    it(`refuses ${JSON.stringify(input)}`, () => expect(normaliseFontSize(input)).toBeNull());
  }
});

describe("reading a declaration", () => {
  it("takes the last declaration, as CSS does", () => {
    expect(declaredValue("color: red; color: #336699", "color")).toBe("#336699");
  });
  it("does not fall back to an earlier one when the last is unusable", () => {
    expect(declaredValue("color: #336699; color: #222 !important", "color")).toBeNull();
  });
  it("matches the property exactly and ignores an escaped name", () => {
    expect(declaredValue("background-color: #336699", "color")).toBeNull();
    expect(declaredValue("c\\olor: #336699", "color")).toBeNull();
  });
  it("is null for nothing and for oversized input", () => {
    expect(declaredValue(null, "color")).toBeNull();
    expect(declaredValue("color:" + "x".repeat(5000), "color")).toBeNull();
  });
  it("an inner span falls back to the enclosing span for what it does not set", () => {
    const doc = new DOMParser().parseFromString('<span style="color:#111111"><span style="font-size:18px">x</span></span>', "text/html");
    const inner = doc.querySelector("span span") as Element;
    expect(spanStyle(inner, "font-size")).toBe("18px");
    expect(spanStyle(inner, "color")).toBe("#111111");
    expect(spanStyle(inner, "font-family")).toBeNull();
  });
});

describe("paragraph indent", () => {
  it("MAX_INDENT is six", () => expect(MAX_INDENT).toBe(6));
  const cases: Array<[string, number]> = [
    ["margin-left: 2em", 1],
    ["margin-left:4em", 2],
    ["margin-left: 12em", 6],
    ["margin-left: 999em", 6],
    ["margin-left: 32px", 1],
    ["margin-left: 64px", 2],
    ["margin-left: 2.4em", 1],
    ["margin-left: 0", 0],
    ["margin-left: 0em", 0],
    ["margin-left: -9999px", 0],
    ["margin-left: -2em", 0],
    ["margin-left: 3pt", 0],
    ["margin-left: 2rem", 0],
    ["margin-left: calc(2em)", 0],
    ["margin-left: var(--x)", 0],
    ["margin-left: 2em !important", 0],
    ["margin-left: 2\\65 m", 0],
    ["margin: 0 0 0 2em", 0],
    ["color: red", 0],
  ];
  for (const [style, want] of cases) {
    it(`${style} is level ${want}`, () => expect(parseIndent(style)).toBe(want));
  }
  it("clampIndent is an integer 0 to 6 whatever it is given", () => {
    expect([clampIndent(0), clampIndent(3), clampIndent(6), clampIndent(7), clampIndent(-1), clampIndent(2.9)]).toEqual([0, 3, 6, 6, 0, 2]);
    expect([clampIndent("4"), clampIndent("x"), clampIndent(NaN), clampIndent(Infinity), clampIndent(null), clampIndent({})]).toEqual([4, 0, 0, 0, 0, 0]);
  });
  it("indentStyle is margin-left in em, two per level, and nothing for level zero", () => {
    expect([indentStyle(0), indentStyle(1), indentStyle(6), indentStyle(99)]).toEqual([null, "margin-left: 2em", "margin-left: 12em", "margin-left: 12em"]);
  });
});
