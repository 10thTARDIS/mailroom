// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE EDITOR HALF OF THE ROUND TRIP (ADR-051 §3 and §6). Every format of betas
// 1 to 4 (the basic marks and lists, links, inline pictures, the beta-2 styles,
// and tables) is made through the real toolbar, and the HTML the editor would send
// is written to testdata/sanitizer-roundtrip.json, with the plain text the same
// document gives. internal/api/sanitize_editor_roundtrip_test.go reads that file and
// takes each document through the whole path, asserting at EVERY stage that the
// tag, the attributes and the style properties and values of every expectation
// survive: SanitizeOutboundHTML, the MIME builder, parsing that MIME back as a
// received message, and the reader's sanitiser. (internal/compose reads the same
// file for the plain-text half: an aligned table must survive format=flowed.)
//
// Drafts reopen: each case's HTML is also loaded back into the editor, and what
// the editor writes must be the same document.
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
import { button, click, inCell, mountEditor, paste, press } from "./editorTestkit";
import { htmlToText } from "./plainText";
import type { Mounted } from "./editorTestkit";

interface Expectation {
  tag: string;
  /** Style properties the element must carry, with the value the editor wrote. */
  style?: Record<string, string>;
  /** Attributes the element must carry, with the value the editor wrote. */
  attrs?: Record<string, string>;
  /** The tag must be inside this one. */
  within?: string;
}
interface Case {
  name: string;
  html: string;
  /** The plain-text rendering of the same document (rich to plain). */
  text: string;
  expect: Expectation[];
}

let m: Mounted | null = null;
afterEach(async () => {
  await m?.unmount();
  m = null;
});

const cases: Case[] = [];

async function record(
  name: string,
  seed: string,
  steps: (mm: Mounted) => Promise<void>,
  expectations: Expectation[],
  opts: { selectAll?: boolean } = {},
): Promise<void> {
  m = await mountEditor(seed);
  await act(async () => {
    if (opts.selectAll === false) m!.editor.commands.focus("end");
    else m!.editor.commands.selectAll();
  });
  try {
    await steps(m);
  } catch (e) {
    throw new Error(`${name}: ${String(e)}`);
  }
  const html = m.html();
  expect(html, name).not.toBe(seed);
  cases.push({ name, html, text: htmlToText(html), expect: expectations });
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
    // ---- beta 1: the basic marks, lists, links and pictures ----
    await record("bold", P, (mm) => click(button(mm, "Bold")), [{ tag: "strong" }]);
    await record("italic", P, (mm) => click(button(mm, "Italic")), [{ tag: "em" }]);
    await record("underline", P, (mm) => click(button(mm, "Underline")), [{ tag: "u" }]);
    await record("bulleted list", P, (mm) => click(button(mm, "Bulleted list")), [{ tag: "ul" }, { tag: "li", within: "ul" }]);
    await record("numbered list", P, (mm) => click(button(mm, "Numbered list")), [{ tag: "ol" }, { tag: "li", within: "ol" }]);
    await record(
      "link",
      P,
      async (mm) => {
        const prompt = vi.spyOn(window, "prompt").mockReturnValue("https://example.com/page");
        try {
          await click(button(mm, "Insert link"));
        } finally {
          prompt.mockRestore();
        }
      },
      [{ tag: "a", attrs: { href: "https://example.com/page" } }],
    );
    await record(
      "inline picture",
      "<p>see:</p>",
      async (mm) => {
        await act(async () => {
          mm.editor.chain().focus("end").insertContent({ type: "image", attrs: { src: "cid:pic1@inline.mailroom", alt: "a picture" } }).run();
        });
      },
      [{ tag: "img", attrs: { src: "cid:pic1@inline.mailroom", alt: "a picture" }, style: { "max-width": "100%" } }],
      { selectAll: false },
    );
    // ---- beta 2: styles ----
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

    // ---- beta 4: tables ----
    const CELL = { border: "1px solid rgb(153, 153, 153)", padding: "4px 8px" };
    const HEAD = { ...CELL, "text-align": "left", "font-weight": "bold" };
    const fill = async (mm: Mounted, rows: string[][]): Promise<void> => {
      for (let r = 0; r < rows.length; r++) for (let c = 0; c < rows[r]!.length; c++) await inCell(mm, r, c, rows[r]![c]);
    };
    await record(
      "table, no header row",
      "<p>before</p>",
      async (mm) => {
        await click(button(mm, "Insert table"));
        await click(button(mm, "Header row"));
        await fill(mm, [["a1", "b1", "c1"], ["a2", "b2", "c2"], ["a3", "b3", "c3"]]);
      },
      [
        { tag: "table", style: { "border-collapse": "collapse" } },
        { tag: "td", style: CELL, within: "table" },
      ],
      { selectAll: false },
    );
    await record(
      "table, header row",
      "<p>before</p>",
      async (mm) => {
        await click(button(mm, "Insert table"));
        await fill(mm, [["Item", "Qty", "Price"], ["Apple", "3", "1.50"], ["Watermelon", "12", "0.20"]]);
      },
      [
        { tag: "table", style: { "border-collapse": "collapse" } },
        { tag: "th", style: HEAD, attrs: { scope: "col" }, within: "table" },
        { tag: "td", style: CELL, within: "table" },
      ],
      { selectAll: false },
    );
    await record(
      "table, formatting inside cells",
      "<p>before</p>",
      async (mm) => {
        await click(button(mm, "Insert table"));
        await fill(mm, [["Name", "Note"], ["word", "other"]]);
        await inCell(mm, 1, 0);
        await act(async () => {
          const to = mm.editor.state.selection.from;
          mm.editor.commands.setTextSelection({ from: to - 4, to });
        });
        await click(button(mm, "Bold"));
        await swatch(mm, "Text colour", "Red");
        await inCell(mm, 1, 1);
        await act(async () => {
          const to = mm.editor.state.selection.from;
          mm.editor.commands.setTextSelection({ from: to - 5, to });
        });
        await click(button(mm, "Italic"));
      },
      [
        { tag: "th", style: HEAD, attrs: { scope: "col" }, within: "table" },
        { tag: "strong", within: "td" },
        { tag: "span", style: { color: "rgb(204, 0, 0)" }, within: "td" },
        { tag: "em", within: "td" },
      ],
      { selectAll: false },
    );
    await record(
      "table inside a quote",
      "<blockquote><p>quoted</p></blockquote>",
      async (mm) => {
        await click(button(mm, "Insert table"));
        await fill(mm, [["h1", "h2"], ["x", "y"]]);
      },
      [
        { tag: "blockquote" },
        { tag: "table", style: { "border-collapse": "collapse" }, within: "blockquote" },
        { tag: "th", style: HEAD, attrs: { scope: "col" }, within: "blockquote" },
        { tag: "td", style: CELL, within: "blockquote" },
      ],
      { selectAll: false },
    );
    await record(
      "pasted table with colspan and rowspan",
      "<p>before</p>",
      async (mm) => {
        await paste(mm, {
          html: '<table><tr><th colspan="2">Heading</th></tr><tr><td rowspan="2">left</td><td>right</td></tr><tr><td>below</td></tr></table>',
        });
      },
      [
        { tag: "th", style: HEAD, attrs: { scope: "col", colspan: "2" }, within: "table" },
        { tag: "td", style: CELL, attrs: { rowspan: "2" }, within: "table" },
      ],
      { selectAll: false },
    );

    // Every expectation is true of the HTML beside it.
    for (const c of cases) {
      const doc = new DOMParser().parseFromString(c.html, "text/html");
      for (const e of c.expect) {
        const hits = Array.from(doc.querySelectorAll<HTMLElement>(e.within ? `${e.within} ${e.tag}` : e.tag));
        const ok = hits.some(
          (el) =>
            Object.entries(e.style ?? {}).every(([p, v]) => el.style.getPropertyValue(p) === v) &&
            Object.entries(e.attrs ?? {}).every(([a, v]) => el.getAttribute(a) === v),
        );
        expect(ok, `${c.name}: ${JSON.stringify(e)} in ${c.html}`).toBe(true);
      }
    }

    await expect(JSON.stringify(cases, null, 2) + "\n").toMatchFileSnapshot("./testdata/sanitizer-roundtrip.json");
  }, 120_000);

  // DRAFTS REOPEN. A saved draft is the editor's own HTML put back into the
  // editor; what it writes again must be the same document, or opening a draft
  // would change it. Compared after normalising (parsed and re-serialised by the
  // DOM, so attribute quoting and entity forms cannot make a difference).
  it("every case's HTML loads back into the editor and is written out as the same document", async () => {
    expect(cases.length).toBeGreaterThan(30);
    const norm = (h: string): string => {
      const ser = new XMLSerializer();
      return Array.from(new DOMParser().parseFromString(h, "text/html").body.childNodes).map((n) => ser.serializeToString(n)).join("");
    };
    for (const c of cases) {
      const re = await mountEditor(c.html);
      try {
        expect(norm(re.html()), `reopening "${c.name}"`).toBe(norm(c.html));
      } finally {
        await re.unmount();
      }
    }
  }, 120_000);

  it("control: the reopen comparison can fail (a document that is changed on the way in is reported)", async () => {
    const norm = (h: string): string => {
      const ser = new XMLSerializer();
      return Array.from(new DOMParser().parseFromString(h, "text/html").body.childNodes).map((n) => ser.serializeToString(n)).join("");
    };
    // A cell with a style the editor does not keep: loading it changes the document.
    const drifting = '<table style="border-collapse: collapse;"><tbody><tr><td style="color: red; border: 1px solid rgb(153, 153, 153); padding: 4px 8px"><p>x</p></td></tr></tbody></table>';
    const re = await mountEditor(drifting);
    try {
      expect(norm(re.html())).not.toBe(norm(drifting));
    } finally {
      await re.unmount();
    }
  });
});
