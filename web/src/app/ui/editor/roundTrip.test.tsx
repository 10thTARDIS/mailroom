// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE EDITOR HALF OF THE SANITISER ROUND TRIP (ADR-051 §3). Every beta-2 format is
// made through the real toolbar, and the HTML the editor would send is written to
// testdata/sanitizer-roundtrip.json. internal/api/sanitize_editor_roundtrip_test.go
// reads that file and feeds each document through SanitizeOutboundHTML, asserting
// that the tag and the style property and value of every expectation survive.
//
// The two halves are tied together here: this test FAILS when the file differs
// from what the editor produces today (a stale fixture would let the Go test pass
// against HTML the editor no longer writes). To regenerate after an intended
// change:  npx vitest run src/app/ui/editor/roundTrip.test.tsx -u
// (the same twin pattern as the consent-page goldens, with the web side as the
// writer). Each expectation is also checked against the editor's output here, so
// the file cannot claim something its own HTML does not hold.

import { afterEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { button, click, mountEditor, press } from "./editorTestkit";
import type { Mounted } from "./editorTestkit";

interface Expectation {
  tag: string;
  /** Style properties the element must carry, with the value the editor wrote. */
  style?: Record<string, string>;
  /** The tag must be inside this one. */
  within?: string;
}
interface Case {
  name: string;
  html: string;
  expect: Expectation[];
}

let m: Mounted | null = null;
afterEach(async () => {
  await m?.unmount();
  m = null;
});

const cases: Case[] = [];

async function record(name: string, seed: string, steps: (mm: Mounted) => Promise<void>, expectations: Expectation[]): Promise<void> {
  m = await mountEditor(seed);
  await act(async () => {
    m!.editor.commands.selectAll();
  });
  try {
    await steps(m);
  } catch (e) {
    throw new Error(`${name}: ${String(e)}`);
  }
  const html = m.html();
  expect(html, name).not.toBe(seed);
  cases.push({ name, html, expect: expectations });
  await m.unmount();
  m = null;
}

const choose = async (mm: Mounted, label: string, value: string): Promise<void> => {
  const s = mm.host.querySelector<HTMLSelectElement>(`select[aria-label="${label}"]`)!;
  await act(async () => {
    s.value = value;
    s.dispatchEvent(new Event("change", { bubbles: true }));
  });
};
const swatch = async (mm: Mounted, trigger: string, name: string): Promise<void> => {
  await click(button(mm, trigger));
  const sw = mm.host.querySelector<HTMLElement>(`.editor-palette button[aria-label="${name}"]`);
  if (!sw) throw new Error(`no swatch ${name}; palette is ${mm.host.querySelector(".editor-palette")?.getAttribute("aria-label")}`);
  await click(sw);
  // Tiptap returns focus to the editor on the next animation frame; let it land
  // before the next palette opens, or its blur would close that one.
  // Wait for that to happen rather than for a fixed time: a busy machine can take
  // well over a frame, and a fixed sleep made this test fail only under load.
  await vi.waitFor(() => {
    if (!mm.host.querySelector("[contenteditable]")?.contains(document.activeElement)) throw new Error("focus has not returned to the editor");
  }, { timeout: 5000, interval: 10 });
};

describe("editor output for the sanitiser round trip", () => {
  it("builds every format through the toolbar and matches the committed fixture", async () => {
    const P = "<p>sample</p>";
    await record("strike-through", P, (mm) => click(button(mm, "Strikethrough")), [{ tag: "s" }]);
    await record("subscript", P, (mm) => click(button(mm, "Subscript")), [{ tag: "sub" }]);
    await record("superscript", P, (mm) => click(button(mm, "Superscript")), [{ tag: "sup" }]);
    await record("inline code", P, (mm) => click(button(mm, "Inline code")), [{ tag: "code" }]);
    await record("code block", "<p>line one</p>", (mm) => click(button(mm, "Code block")), [{ tag: "code", within: "pre" }]);
    await record("block quote", P, (mm) => click(button(mm, "Block quote")), [{ tag: "blockquote" }]);
    await record("text colour", P, (mm) => swatch(mm, "Text colour", "Red"), [{ tag: "span", style: { color: "rgb(204, 0, 0)" } }]);
    await record("highlight colour", P, (mm) => swatch(mm, "Highlight colour", "Yellow"), [{ tag: "span", style: { "background-color": "rgb(255, 255, 0)" } }]);
    await record("font Sans-serif", P, (mm) => choose(mm, "Font", "Arial, Helvetica, sans-serif"), [{ tag: "span", style: { "font-family": "Arial, Helvetica, sans-serif" } }]);
    await record("font Serif", P, (mm) => choose(mm, "Font", "Georgia, 'Times New Roman', serif"), [{ tag: "span", style: { "font-family": 'Georgia, "Times New Roman", serif' } }]);
    await record("font Monospace", P, (mm) => choose(mm, "Font", "'Courier New', Courier, monospace"), [{ tag: "span", style: { "font-family": '"Courier New", Courier, monospace' } }]);
    await record("font Wide", P, (mm) => choose(mm, "Font", "Verdana, Geneva, sans-serif"), [{ tag: "span", style: { "font-family": "Verdana, Geneva, sans-serif" } }]);
    await record("size Small", P, (mm) => choose(mm, "Font size", "12px"), [{ tag: "span", style: { "font-size": "12px" } }]);
    await record("size Large", P, (mm) => choose(mm, "Font size", "18px"), [{ tag: "span", style: { "font-size": "18px" } }]);
    await record("size Huge", P, (mm) => choose(mm, "Font size", "24px"), [{ tag: "span", style: { "font-size": "24px" } }]);
    await record(
      "paragraph indent, two levels",
      P,
      async (mm) => {
        await click(button(mm, "Increase indent"));
        await click(button(mm, "Increase indent"));
      },
      [{ tag: "p", style: { "margin-left": "4em" } }],
    );
    await record(
      "deepest paragraph indent",
      P,
      async (mm) => {
        for (let i = 0; i < 8; i++) await press(mm, { key: "]", keyCode: 221, ctrlKey: true });
      },
      [{ tag: "p", style: { "margin-left": "12em" } }],
    );
    await record(
      "nested list",
      "<ul><li><p>one</p></li><li><p>two</p></li></ul>",
      async (mm) => {
        await act(async () => {
          mm.editor.commands.focus("end");
        });
        await click(button(mm, "Increase indent"));
      },
      [{ tag: "ul", within: "li" }],
    );
    await record(
      "everything on one span, indent and strike on its paragraph",
      P,
      async (mm) => {
        await swatch(mm, "Text colour", "Dark blue");
        await swatch(mm, "Highlight colour", "Light green");
        await choose(mm, "Font", "Verdana, Geneva, sans-serif");
        await choose(mm, "Font size", "18px");
        await click(button(mm, "Strikethrough"));
        await click(button(mm, "Increase indent"));
      },
      [
        {
          tag: "span",
          style: { color: "rgb(11, 46, 107)", "background-color": "rgb(182, 215, 168)", "font-family": "Verdana, Geneva, sans-serif", "font-size": "18px" },
        },
        { tag: "s" },
        { tag: "p", style: { "margin-left": "2em" } },
      ],
    );
    await record(
      "a quote holding an indented, coloured paragraph",
      "<blockquote><p>quoted</p></blockquote>",
      async (mm) => {
        await swatch(mm, "Text colour", "Purple");
        await click(button(mm, "Increase indent"));
      },
      [{ tag: "blockquote" }, { tag: "p", style: { "margin-left": "2em" }, within: "blockquote" }, { tag: "span", style: { color: "rgb(103, 78, 167)" } }],
    );

    // Every expectation is true of the HTML beside it.
    for (const c of cases) {
      const doc = new DOMParser().parseFromString(c.html, "text/html");
      for (const e of c.expect) {
        const hits = Array.from(doc.querySelectorAll<HTMLElement>(e.within ? `${e.within} ${e.tag}` : e.tag));
        const ok = hits.some((el) => Object.entries(e.style ?? {}).every(([p, v]) => el.style.getPropertyValue(p) === v));
        expect(ok, `${c.name}: ${JSON.stringify(e)} in ${c.html}`).toBe(true);
      }
    }

    await expect(JSON.stringify(cases, null, 2) + "\n").toMatchFileSnapshot("./testdata/sanitizer-roundtrip.json");
  }, 60_000);
});
