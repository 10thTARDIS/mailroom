// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE ROUND TRIP OF A QUOTE, per supported format (ADR-051 §7, with §3 and §6).
//
// For every document the server's reader sanitiser produced from a received
// message (quote-received.json: every format of betas 1 to 4, the hostile
// payloads, and the layout fixtures):
//
//   1. the quote is built from it (the real quote module) and seeded into the
//      real editor, as the composer does;
//   2. what the editor writes is the draft that would be SAVED, and every format
//      the original had is still in it (the same expectations the beta-4 harness
//      uses, now required inside the quote);
//   3. that draft REOPENS as the same document, after normalising;
//   4. the saved HTML is written to testdata/quote-roundtrip.json, which
//      internal/api/sanitize_quote_roundtrip_test.go takes through the outbound
//      sanitiser, the MIME builder, the received-message parser and the reader's
//      sanitiser, asserting the same formats at every stage.
//
// Twin pattern of ../editor/roundTrip.test.tsx: this test fails when the file
// differs from what the editor produces today. To regenerate after an intended
// change:  npx vitest run src/app/ui/compose/quote.roundtrip.test.tsx -u

import { afterEach, describe, expect, it, vi } from "vitest";
import { mountEditor } from "../editor/editorTestkit";
import type { Mounted } from "../editor/editorTestkit";
import { htmlToText } from "../editor/plainText";
import editorCases from "../editor/testdata/sanitizer-roundtrip.json";
import { RECEIVED, norm, partsFor, quoteOf, seedOf } from "./quoteTestkit";

vi.setConfig({ testTimeout: 120_000 });

interface Expectation {
  tag: string;
  style?: Record<string, string>;
  attrs?: Record<string, string>;
  within?: string;
}
interface SavedCase {
  name: string;
  group: string;
  /** The HTML the saved draft carries. */
  html: string;
  text: string;
  expect: Expectation[];
}

const css = (s: string): string => s.toLowerCase().replace(/"/g, "'").replace(/\s+/g, "");

/** Whether the document holds an element satisfying the expectation. */
function holds(doc: Document, e: Expectation): boolean {
  return Array.from(doc.querySelectorAll(e.tag)).some((el) => {
    if (e.within && !el.parentElement?.closest(e.within)) return false;
    const decls = new Map<string, string>();
    for (const d of (el.getAttribute("style") ?? "").split(";")) {
      const i = d.indexOf(":");
      if (i > 0) decls.set(d.slice(0, i).trim().toLowerCase(), d.slice(i + 1).trim());
    }
    for (const [p, v] of Object.entries(e.style ?? {})) {
      const got = decls.get(p);
      if (got === undefined || css(got) !== css(v)) return false;
    }
    for (const [k, v] of Object.entries(e.attrs ?? {})) if (el.getAttribute(k) !== v) return false;
    return true;
  });
}

const FORMAT_EXPECT = new Map<string, Expectation[]>(
  (editorCases as Array<{ name: string; expect: Expectation[] }>).map((c) => [c.name, c.expect]),
);

/** The formats of an original, required INSIDE the quote: the expectations of the
 * editor round trip, each also within a blockquote. */
function expectationsFor(c: { name: string; group: string }): Expectation[] {
  if (c.group !== "format") return [];
  return (FORMAT_EXPECT.get(c.name) ?? []).map((e) => ({ ...e, within: e.within && e.within !== "blockquote" ? e.within : "blockquote" }));
}

let m: Mounted | null = null;
afterEach(async () => {
  await m?.unmount();
  m = null;
});

const saved: SavedCase[] = [];
const emptied: string[] = [];

describe("a quote makes the round trip, per format", () => {
  it("every case: quoted, saved with its formats, reopened as the same document", async () => {
    expect(RECEIVED.filter((c) => c.group === "format").length).toBe(FORMAT_EXPECT.size);
    for (const c of RECEIVED) {
      const q = quoteOf(c.html, partsFor(c.html));
      if (q.html === "") {
        // Only a hostile original can have nothing left to quote (the composer then
        // falls back to its plain text); a format or a layout never does.
        expect(c.group, `${c.name}: something is quoted`).toBe("hostile");
        emptied.push(c.name);
        continue;
      }
      const e = await mountEditor(seedOf(q));
      let html: string;
      try {
        html = e.html();
      } finally {
        await e.unmount();
      }
      const doc = new DOMParser().parseFromString(html, "text/html");
      const exp = expectationsFor(c);
      for (const x of exp) expect(holds(doc, x), `${c.name}: the saved draft keeps ${JSON.stringify(x)}\n${html}`).toBe(true);

      // The draft reopens as the same document.
      const again = await mountEditor(html);
      try {
        expect(norm(again.html()), `${c.name}: reopening changes the draft`).toBe(norm(html));
      } finally {
        await again.unmount();
      }
      saved.push({ name: c.name, group: c.group, html, text: htmlToText(html), expect: exp });
    }
    // Control: the cases that emptied are the ones made only of active content.
    expect(emptied.sort()).toEqual(["embed", "form with input", "iframe", "img onerror", "object", "svg onload"]);
    expect(saved.filter((c) => c.group === "hostile").length).toBeGreaterThan(8);
    await expect(JSON.stringify(saved, null, 2) + "\n").toMatchFileSnapshot("./testdata/quote-roundtrip.json");
  });

  it("the expectation matcher can fail: a missing tag, a wrong value and a missing ancestor are reported", () => {
    const doc = new DOMParser().parseFromString('<blockquote><p><span style="color: rgb(1, 2, 3)">x</span></p></blockquote>', "text/html");
    expect(holds(doc, { tag: "span", style: { color: "rgb(1, 2, 3)" }, within: "blockquote" })).toBe(true);
    expect(holds(doc, { tag: "em" })).toBe(false);
    expect(holds(doc, { tag: "span", style: { color: "rgb(1, 2, 4)" } })).toBe(false);
    expect(holds(doc, { tag: "span", within: "ul" })).toBe(false);
  });

  it("control: a format lost on the way in IS reported (a quote with the style stripped fails the check)", async () => {
    const original = RECEIVED.find((c) => c.name === "text colour")!;
    const exp = expectationsFor(original);
    expect(exp.length).toBeGreaterThan(0);
    const stripped = original.html.replace(/ style="[^"]*"/g, "");
    const e = await mountEditor(seedOf(quoteOf(stripped)));
    try {
      const doc = new DOMParser().parseFromString(e.html(), "text/html");
      expect(exp.every((x) => holds(doc, x))).toBe(false);
    } finally {
      await e.unmount();
    }
  });
});
