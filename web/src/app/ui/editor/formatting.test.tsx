// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// ADR-051 beta 2: the new formatting. As in RichTextEditor.test.tsx, each toolbar
// action is tested by its button AND by its shortcut (a separate code path), and
// the style marks are tested on every route a value can arrive by: the toolbar,
// the commands, a seed and a paste. The hostile versions of those are in
// hostilePaste.test.tsx.

import { afterEach, describe, expect, it } from "vitest";
import { act } from "react";
import { button, click, mountEditor, press } from "./editorTestkit";
import type { KeyInit, Mounted } from "./editorTestkit";
import { FONT_FAMILIES, FONT_SIZES } from "./styleValues";
import { HIGHLIGHT_COLOURS, TEXT_COLOURS } from "./RichTextEditor";

let m: Mounted | null = null;
afterEach(async () => {
  await m?.unmount();
  m = null;
});

async function selectAll(mm: Mounted): Promise<void> {
  await act(async () => {
    mm.editor.commands.selectAll();
  });
}

async function cursorAtEnd(mm: Mounted): Promise<void> {
  await act(async () => {
    mm.editor.commands.focus("end");
  });
}

function select(mm: Mounted, name: string): HTMLSelectElement {
  const s = mm.host.querySelector<HTMLSelectElement>(`select[aria-label="${name}"]`);
  if (!s) throw new Error(`no select ${name}`);
  return s;
}

async function choose(mm: Mounted, name: string, value: string): Promise<void> {
  const s = select(mm, name);
  await act(async () => {
    s.value = value;
    s.dispatchEvent(new Event("change", { bubbles: true }));
  });
}

/** A style value as the serializer writes it: jsdom (like a browser) re-serializes
 * `#cc0000` as `rgb(204, 0, 0)` and single quotes as double. Compared by parsing. */
function styleOf(html: string, selector: string): CSSStyleDeclaration {
  const doc = new DOMParser().parseFromString(html, "text/html");
  const el = doc.querySelector(selector) as HTMLElement | null;
  if (!el) throw new Error(`no ${selector} in ${html}`);
  return el.style;
}

describe("simple marks and blocks: button and shortcut", () => {
  const cases: Array<[string, string, KeyInit, string]> = [
    ["Strikethrough", "Strikethrough (Ctrl+Shift+S)", { key: "S", keyCode: 83, ctrlKey: true, shiftKey: true }, "<p><s>hello</s></p>"],
    ["Subscript", "Subscript (Ctrl+,)", { key: ",", keyCode: 188, ctrlKey: true }, "<p><sub>hello</sub></p>"],
    ["Superscript", "Superscript (Ctrl+.)", { key: ".", keyCode: 190, ctrlKey: true }, "<p><sup>hello</sup></p>"],
    ["Inline code", "Inline code (Ctrl+E)", { key: "e", keyCode: 69, ctrlKey: true }, "<p><code>hello</code></p>"],
    ["Block quote", "Block quote (Ctrl+Shift+B)", { key: "B", keyCode: 66, ctrlKey: true, shiftKey: true }, "<blockquote><p>hello</p></blockquote>"],
    ["Code block", "Code block (Ctrl+Alt+C)", { key: "c", keyCode: 67, ctrlKey: true, altKey: true }, "<pre><code>hello</code></pre>"],
  ];
  for (const [name, title, key, want] of cases) {
    it(`${name}: the button formats the selection, toggles off, and its title lists ${title.match(/\((.*)\)/)![1]}`, async () => {
      m = await mountEditor("<p>hello</p>");
      await selectAll(m);
      expect(button(m, name).title).toBe(title);
      expect(button(m, name).getAttribute("aria-pressed")).toBe("false");
      await click(button(m, name));
      expect(m.html()).toBe(want);
      expect(m.changes.at(-1)).toBe(want);
      expect(button(m, name).getAttribute("aria-pressed")).toBe("true");
      if (name === "Block quote") return; // (toggling off a quote is its own test: it needs a cursor, not select-all)
      await click(button(m, name));
      expect(m.html()).toBe("<p>hello</p>");
    });
    it(`${name}: the shortcut formats the selection`, async () => {
      m = await mountEditor("<p>hello</p>");
      await selectAll(m);
      await press(m, key);
      expect(m.html()).toBe(want);
    });
  }

  it("Block quote toggles off again with the cursor in it", async () => {
    m = await mountEditor("<blockquote><p>hello</p></blockquote>");
    await cursorAtEnd(m);
    expect(button(m, "Block quote").getAttribute("aria-pressed")).toBe("true");
    await click(button(m, "Block quote"));
    expect(m.html()).toBe("<p>hello</p>");
  });

  it("a code block keeps its text verbatim and writes no class (a pasted language- class is not kept)", async () => {
    m = await mountEditor('<pre><code class="language-js evil">a\n  b</code></pre>');
    expect(m.html()).toBe("<pre><code>a\n  b</code></pre>");
  });

  it("control: the shortcut test can fail (an unbound chord changes nothing)", async () => {
    m = await mountEditor("<p>hello</p>");
    await selectAll(m);
    await press(m, { key: "q", keyCode: 81, ctrlKey: true, shiftKey: true });
    expect(m.html()).toBe("<p>hello</p>");
  });
});

describe("undo and redo", () => {
  const aria = (mm: Mounted, name: string) => button(mm, name).getAttribute("aria-disabled");

  it("both are disabled when there is nothing to undo or redo, and stay in the tab order", async () => {
    m = await mountEditor("<p>hello</p>");
    expect(aria(m, "Undo")).toBe("true");
    expect(aria(m, "Redo")).toBe("true");
    expect(button(m, "Undo").disabled).toBe(false); // aria-disabled keeps it focusable for the roving group
    expect(button(m, "Undo").title).toBe("Undo (Ctrl+Z)");
    expect(button(m, "Redo").title).toBe("Redo (Ctrl+Shift+Z)");
    await click(button(m, "Undo")); // a click on a disabled one does nothing
    expect(m.html()).toBe("<p>hello</p>");
  });

  it("an edit enables Undo; Undo enables Redo; Redo puts the edit back", async () => {
    m = await mountEditor("<p>hello</p>");
    await selectAll(m);
    await click(button(m, "Bold"));
    expect(aria(m, "Undo")).toBeNull();
    expect(aria(m, "Redo")).toBe("true");
    await click(button(m, "Undo"));
    expect(m.html()).toBe("<p>hello</p>");
    expect(aria(m, "Undo")).toBe("true");
    expect(aria(m, "Redo")).toBeNull();
    await click(button(m, "Redo"));
    expect(m.html()).toBe("<p><strong>hello</strong></p>");
    expect(aria(m, "Redo")).toBe("true");
  });

  const keys: Array<[string, KeyInit]> = [
    ["Ctrl+Z", { key: "z", keyCode: 90, ctrlKey: true }],
  ];
  for (const [name, key] of keys) {
    it(`${name} undoes`, async () => {
      m = await mountEditor("<p>hello</p>");
      await selectAll(m);
      await click(button(m, "Bold"));
      await press(m, key);
      expect(m.html()).toBe("<p>hello</p>");
    });
  }
  for (const [name, key] of [
    ["Ctrl+Shift+Z", { key: "Z", keyCode: 90, ctrlKey: true, shiftKey: true }],
    ["Ctrl+Y", { key: "y", keyCode: 89, ctrlKey: true }],
  ] as Array<[string, KeyInit]>) {
    it(`${name} redoes`, async () => {
      m = await mountEditor("<p>hello</p>");
      await selectAll(m);
      await click(button(m, "Bold"));
      await click(button(m, "Undo"));
      expect(m.html()).toBe("<p>hello</p>");
      await press(m, key);
      expect(m.html()).toBe("<p><strong>hello</strong></p>");
    });
  }
});

describe("indent and outdent", () => {
  const list = "<ul><li><p>one</p></li><li><p>two</p></li></ul>";
  const nested = "<ul><li><p>one</p><ul><li><p>two</p></li></ul></li></ul>";

  it("titles list Ctrl+] and Ctrl+[", async () => {
    m = await mountEditor("");
    expect(button(m, "Increase indent").title).toBe("Increase indent (Ctrl+])");
    expect(button(m, "Decrease indent").title).toBe("Decrease indent (Ctrl+[)");
  });

  it("in a list, the buttons sink and lift the item", async () => {
    m = await mountEditor(list);
    await cursorAtEnd(m);
    await click(button(m, "Increase indent"));
    expect(m.html()).toBe(nested);
    await click(button(m, "Decrease indent"));
    expect(m.html()).toBe(list);
  });

  it("in a list, Ctrl+] and Ctrl+[ sink and lift the item", async () => {
    m = await mountEditor(list);
    await cursorAtEnd(m);
    const ev = await press(m, { key: "]", keyCode: 221, ctrlKey: true });
    expect(ev.defaultPrevented).toBe(true);
    expect(m.html()).toBe(nested);
    await press(m, { key: "[", keyCode: 219, ctrlKey: true });
    expect(m.html()).toBe(list);
  });

  it("the first item has nothing to nest under: nothing changes, and no error", async () => {
    m = await mountEditor(list);
    await act(async () => {
      m!.editor.commands.focus("start");
    });
    await click(button(m, "Increase indent"));
    expect(m.html()).toBe(list);
  });

  it("a paragraph indents two em per level, shows no style at level 0, and round-trips through getHTML and the seed", async () => {
    m = await mountEditor("<p>hello</p>");
    await selectAll(m);
    await click(button(m, "Increase indent"));
    expect(m.html()).toBe('<p style="margin-left: 2em;">hello</p>');
    await click(button(m, "Increase indent"));
    const two = m.html();
    expect(two).toBe('<p style="margin-left: 4em;">hello</p>');
    await m.unmount();
    m = await mountEditor(two); // the draft reopens
    expect(m.html()).toBe(two);
    await selectAll(m);
    await click(button(m, "Decrease indent"));
    await click(button(m, "Decrease indent"));
    expect(m.html()).toBe("<p>hello</p>");
    await click(button(m, "Decrease indent")); // below zero is clamped
    expect(m.html()).toBe("<p>hello</p>");
  });

  it("the indent stops at level 6", async () => {
    m = await mountEditor("<p>hello</p>");
    await selectAll(m);
    for (let i = 0; i < 9; i++) await press(m, { key: "]", keyCode: 221, ctrlKey: true });
    expect(m.html()).toBe('<p style="margin-left: 12em;">hello</p>');
  });

  it("every selected paragraph is indented, a paragraph in a block quote too", async () => {
    m = await mountEditor("<p>a</p><blockquote><p>b</p></blockquote>");
    await selectAll(m);
    await click(button(m, "Increase indent"));
    expect(m.html()).toBe('<p style="margin-left: 2em;">a</p><blockquote><p style="margin-left: 2em;">b</p></blockquote>');
  });

  it("an off-range margin-left is clamped or dropped on the way in", async () => {
    m = await mountEditor(
      '<p style="margin-left: 999em">a</p><p style="margin-left: -9999px">b</p><p style="margin-left: 3pt">c</p><p style="margin-left: 64px">d</p><p style="margin-left: 2.4em">e</p>',
    );
    expect(m.html()).toBe(
      '<p style="margin-left: 12em;">a</p><p>b</p><p>c</p><p style="margin-left: 4em;">d</p><p style="margin-left: 2em;">e</p>',
    );
  });

  it("the attribute is an integer in range even when set directly", async () => {
    m = await mountEditor('<p>a</p>');
    await act(async () => {
      m!.editor.commands.setContent({
        type: "doc",
        content: [
          { type: "paragraph", attrs: { indent: 99 }, content: [{ type: "text", text: "a" }] },
          { type: "paragraph", attrs: { indent: "1; background:url(https://evil.example/x)" }, content: [{ type: "text", text: "b" }] },
          { type: "paragraph", attrs: { indent: -3 }, content: [{ type: "text", text: "c" }] },
        ],
      });
    });
    expect(m.html()).toBe('<p style="margin-left: 12em;">a</p><p>b</p><p>c</p>');
  });

  it("Tab still leaves the editor in a list and a paragraph (it does not indent)", async () => {
    m = await mountEditor(list);
    await cursorAtEnd(m);
    const ev = await press(m, { key: "Tab", keyCode: 9 });
    expect(ev.defaultPrevented).toBe(false);
    expect(m.html()).toBe(list);
    await m.unmount();
    m = await mountEditor("<p>x</p>");
    await cursorAtEnd(m);
    await press(m, { key: "Tab", keyCode: 9 });
    expect(m.html()).toBe("<p>x</p>");
  });
});

describe("font family and size", () => {
  it("the selects are named, and list Default plus each family / Small, Normal, Large, Huge", async () => {
    m = await mountEditor("");
    expect(Array.from(select(m, "Font").options).map((o) => o.textContent)).toEqual(["Default font", "Sans-serif", "Serif", "Monospace", "Wide"]);
    expect(Array.from(select(m, "Font").options).map((o) => o.value)).toEqual(["", ...FONT_FAMILIES.map((f) => f.css)]);
    expect(Array.from(select(m, "Font size").options).map((o) => o.textContent)).toEqual(["Small", "Normal", "Large", "Huge"]);
    expect(Array.from(select(m, "Font size").options).map((o) => o.value)).toEqual(["12px", "", "18px", "24px"]);
  });

  for (const f of FONT_FAMILIES) {
    it(`family ${f.css}: set from the select, read back as the select's value, removed by Default`, async () => {
      m = await mountEditor("<p>hello</p>");
      await selectAll(m);
      await choose(m, "Font", f.css);
      expect(m.editor.getAttributes("textStyle")["fontFamily"]).toBe(f.css);
      const style = styleOf(m.html(), "span");
      expect(style.fontFamily.replace(/"/g, "'")).toBe(f.css);
      expect(select(m, "Font").value).toBe(f.css);
      await choose(m, "Font", "");
      expect(m.html()).toBe("<p>hello</p>");
    });
  }

  for (const s of FONT_SIZES) {
    it(`size ${s.css}: set from the select and removed by Normal`, async () => {
      m = await mountEditor("<p>hello</p>");
      await selectAll(m);
      await choose(m, "Font size", s.css);
      expect(styleOf(m.html(), "span").fontSize).toBe(s.css);
      expect(select(m, "Font size").value).toBe(s.css);
      await choose(m, "Font size", "");
      expect(m.html()).toBe("<p>hello</p>");
    });
  }

  it("a select leaves focus where it is, so the arrow keys keep stepping through it", async () => {
    m = await mountEditor("<p>hello</p>");
    await selectAll(m);
    const s = select(m, "Font size");
    s.focus();
    await choose(m, "Font size", "18px");
    expect(document.activeElement).toBe(s);
  });

  it("the selects are in the toolbar's roving tab order", async () => {
    m = await mountEditor("");
    const s = select(m, "Font");
    expect(s.className).toContain("editor-tool");
    expect(s.tabIndex).toBe(-1);
    await act(async () => {
      s.focus();
    });
    expect(s.tabIndex).toBe(0);
  });

  it("seeded and reopened drafts keep their families and sizes", async () => {
    const seed = '<p><span style="font-family: Verdana, Geneva, sans-serif; font-size: 24px">big wide</span></p>';
    m = await mountEditor(seed);
    const style = styleOf(m.html(), "span");
    expect(style.fontFamily).toBe("Verdana, Geneva, sans-serif");
    expect(style.fontSize).toBe("24px");
  });
});

// (Assembled: the token check scans .tsx for var(--name) uses.)
const VAR_X = ["var", "(--x)"].join("");

describe("the commands refuse a value that is not on the lists", () => {
  it("setColor, setBackgroundColor, setFontFamily and setFontSize return false and change nothing", async () => {
    m = await mountEditor("<p>hello</p>");
    await selectAll(m);
    const results: boolean[] = [];
    await act(async () => {
      const c = m!.editor.commands;
      results.push(c.setColor("url(https://evil.example/x)"));
      results.push(c.setColor("red"));
      results.push(c.setColor(VAR_X));
      results.push(c.setBackgroundColor("expression(alert(1))"));
      results.push(c.setFontFamily("'x'; src: url(https://evil.example/f)"));
      results.push(c.setFontFamily("Comic Sans MS"));
      results.push(c.setFontSize("999px"));
      results.push(c.setFontSize("12px !important"));
    });
    expect(results).toEqual(Array(8).fill(false));
    expect(m.html()).toBe("<p>hello</p>");
  });

  it("control: a listed value through the same command works (the refusals can be told from a no-op)", async () => {
    m = await mountEditor("<p>hello</p>");
    await selectAll(m);
    let ok = false;
    await act(async () => {
      ok = m!.editor.commands.setColor("#336699");
    });
    expect(ok).toBe(true);
    expect(styleOf(m.html(), "span").color).toBe("rgb(51, 102, 153)");
  });

  it("a mark built with raw attributes still writes only gated values (the render gate)", async () => {
    m = await mountEditor("<p>hello</p>");
    await selectAll(m);
    await act(async () => {
      m!.editor.commands.setMark("textStyle", {
        color: "url(https://evil.example/x)",
        backgroundColor: "red",
        fontFamily: "x, y",
        fontSize: "999px",
      });
    });
    expect(m.html()).not.toMatch(/evil|url\(|999|red|style/);
    await act(async () => {
      m!.editor.commands.setMark("textStyle", { color: "rgb(10,20,30)", fontSize: "18px" });
    });
    const s = styleOf(m.html(), "span");
    expect(s.color).toBe("rgb(10, 20, 30)");
    expect(s.fontSize).toBe("18px");
  });
});

describe("colour palettes", () => {
  const trigger = (mm: Mounted, name: string) => button(mm, name);
  const panel = (mm: Mounted) => mm.host.querySelector<HTMLElement>(".editor-palette");
  const swatchNames = (mm: Mounted) => Array.from(panel(mm)!.querySelectorAll("button")).map((b) => b.getAttribute("aria-label") ?? b.textContent);

  it("the text colour palette opens as a named group of named swatches plus Remove colour, and focus moves in", async () => {
    m = await mountEditor("<p>hello</p>");
    expect(panel(m)).toBeNull();
    expect(trigger(m, "Text colour").getAttribute("aria-expanded")).toBe("false");
    await click(trigger(m, "Text colour"));
    expect(trigger(m, "Text colour").getAttribute("aria-expanded")).toBe("true");
    const p = panel(m)!;
    expect(p.getAttribute("role")).toBe("group");
    expect(p.getAttribute("aria-label")).toBe("Text colour");
    expect(trigger(m, "Text colour").getAttribute("aria-controls")).toBe(p.id);
    expect(swatchNames(m)).toEqual(["Black", "Dark grey", "Grey", "Red", "Orange", "Green", "Blue", "Dark blue", "Purple", "Brown", "Remove colour"]);
    expect(document.activeElement).toBe(p.querySelector("button"));
  });

  it("the highlight palette is its own, named, list", async () => {
    m = await mountEditor("<p>hello</p>");
    await click(trigger(m, "Highlight colour"));
    expect(panel(m)!.getAttribute("aria-label")).toBe("Highlight colour");
    expect(swatchNames(m)).toEqual(["Yellow", "Light green", "Light blue", "Light pink", "Light orange", "Light grey", "Light red", "Light purple", "Remove colour"]);
  });

  for (const c of TEXT_COLOURS) {
    it(`text colour ${c.id} (${c.hex}) is applied and closes the palette`, async () => {
      m = await mountEditor("<p>hello</p>");
      await selectAll(m);
      await click(trigger(m, "Text colour"));
      await click(panel(m)!.querySelector<HTMLElement>(`[data-colour="${c.hex}"]`)!);
      expect(m.editor.getAttributes("textStyle")["color"]).toBe(c.hex);
      expect(styleOf(m.html(), "span").color).not.toBe("");
      expect(panel(m)).toBeNull();
    });
  }
  for (const c of HIGHLIGHT_COLOURS) {
    it(`highlight ${c.id} (${c.hex}) is applied as a background colour`, async () => {
      m = await mountEditor("<p>hello</p>");
      await selectAll(m);
      await click(trigger(m, "Highlight colour"));
      await click(panel(m)!.querySelector<HTMLElement>(`[data-colour="${c.hex}"]`)!);
      expect(m.editor.getAttributes("textStyle")["backgroundColor"]).toBe(c.hex);
      expect(styleOf(m.html(), "span").backgroundColor).not.toBe("");
    });
  }

  it("Remove colour clears it and leaves no empty span", async () => {
    m = await mountEditor('<p><span style="color: #cc0000">hello</span></p>');
    await selectAll(m);
    await click(trigger(m, "Text colour"));
    await click(panel(m)!.querySelector<HTMLElement>(".editor-swatch-remove")!);
    expect(m.html()).toBe("<p>hello</p>");
  });

  it("the arrow keys step through the swatches (one tab stop), wrapping; Home and End jump", async () => {
    m = await mountEditor("<p>hello</p>");
    await click(trigger(m, "Text colour"));
    const buttons = Array.from(panel(m)!.querySelectorAll<HTMLButtonElement>("button"));
    expect(buttons.map((b) => b.tabIndex)).toEqual([0, ...Array(buttons.length - 1).fill(-1)]);
    const key = async (k: string) =>
      act(async () => {
        document.activeElement!.dispatchEvent(new KeyboardEvent("keydown", { key: k, bubbles: true, cancelable: true }));
      });
    await key("ArrowRight");
    expect(document.activeElement).toBe(buttons[1]);
    await key("ArrowLeft");
    await key("ArrowLeft");
    expect(document.activeElement).toBe(buttons[buttons.length - 1]); // wraps to Remove colour
    await key("Home");
    expect(document.activeElement).toBe(buttons[0]);
    await key("End");
    expect(document.activeElement).toBe(buttons[buttons.length - 1]);
  });

  it("Escape closes the palette and returns focus to its button", async () => {
    m = await mountEditor("<p>hello</p>");
    await click(trigger(m, "Highlight colour"));
    await act(async () => {
      document.activeElement!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
    });
    expect(panel(m)).toBeNull();
    expect(document.activeElement).toBe(trigger(m, "Highlight colour"));
  });

  it("clicking the button again closes it; opening the other one switches", async () => {
    m = await mountEditor("<p>hello</p>");
    await click(trigger(m, "Text colour"));
    await click(trigger(m, "Highlight colour"));
    expect(panel(m)!.getAttribute("aria-label")).toBe("Highlight colour");
    await click(trigger(m, "Highlight colour"));
    expect(panel(m)).toBeNull();
  });

  it("every swatch value is a canonical colour the gate accepts as it stands", () => {
    for (const c of [...TEXT_COLOURS, ...HIGHLIGHT_COLOURS]) expect(c.hex).toMatch(/^#[0-9a-f]{6}$/);
  });
});

describe("pasted and seeded styles are kept when they are on the lists", () => {
  it("colour, highlight, family and size survive a paste, with colours normalised", async () => {
    m = await mountEditor(
      '<p><span style="color: #369; background-color: rgb(10,20,30); font-family: \'Courier New\', Courier, monospace; font-size: 12px">x</span></p>',
    );
    await selectAll(m);
    const json = JSON.stringify(m.editor.getJSON());
    expect(json).toContain('"color":"#336699"');
    expect(json).toContain('"backgroundColor":"#0a141e"');
    expect(json).toContain("'Courier New', Courier, monospace");
    expect(json).toContain('"fontSize":"12px"');
  });

  it("nested spans merge: an inner size keeps the outer colour", async () => {
    m = await mountEditor('<p><span style="color: #cc0000"><span style="font-size: 18px">x</span></span></p>');
    const s = styleOf(m.html(), "span");
    expect(s.color).toBe("rgb(204, 0, 0)");
    expect(s.fontSize).toBe("18px");
  });
});
