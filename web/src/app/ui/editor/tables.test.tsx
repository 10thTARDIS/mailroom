// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// ADR-051 beta 4: simple tables. The toolbar actions and their enabled or disabled
// state, what a table WRITES (mail-client HTML with the fixed border and padding),
// Tab leaving the editor from every cell (the library's own Tab adds a row at the
// last cell, which would be a keyboard trap), and the table at the edge of the
// document. The hostile versions of these are in hostilePaste.test.tsx.

import { afterEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import { button, click, mountEditor, press } from "./editorTestkit";
import type { Mounted } from "./editorTestkit";

// Every test mounts a real editor, some several; the default 5 s is too tight when the
// machine is busy (a loaded CI runner timed out on the first, plainest tests).
vi.setConfig({ testTimeout: 60_000 });
import { CELL_STYLE, HEADER_CELL_STYLE, TABLE_EXTENSIONS, TABLE_STYLE } from "./tables";

let m: Mounted | null = null;
afterEach(async () => {
  await m?.unmount();
  m = null;
});

/** A style string as the DOM re-serialises it (jsdom, like a browser, parses shorthand values). */
function styleText(css: string): string {
  const probe = document.createElement("div");
  probe.setAttribute("style", css);
  return probe.style.cssText;
}

const TABLE_ONLY = ["Add row above", "Add row below", "Add column left", "Add column right", "Delete row", "Delete column", "Header row", "Delete table"];

const disabled = (mm: Mounted, name: string): boolean => button(mm, name).getAttribute("aria-disabled") === "true";

/** Rows of cell texts, read from the editor's own document (not from HTML). */
function grid(mm: Mounted): string[][] {
  const rows: string[][] = [];
  mm.editor.state.doc.descendants((node) => {
    if (node.type.name === "tableRow") {
      const cells: string[] = [];
      node.forEach((c) => cells.push(c.textContent));
      rows.push(cells);
    }
    return true;
  });
  return rows;
}
const kinds = (mm: Mounted): string[] => {
  const out: string[] = [];
  mm.editor.state.doc.descendants((node) => {
    if (node.type.name === "tableRow") out.push(node.firstChild?.type.name ?? "");
    return true;
  });
  return out;
};

/** Where the cursor is: [row, column] of the cell it is in, or null outside a table. */
function cursorCell(mm: Mounted): [number, number] | null {
  const { $from } = mm.editor.state.selection;
  for (let d = $from.depth; d > 0; d--) {
    if ($from.node(d).type.name === "tableCell" || $from.node(d).type.name === "tableHeader") {
      return [$from.index(d - 2), $from.index(d - 1)];
    }
  }
  return null;
}

/** Puts the cursor in cell (r, c) by finding its position in the document. */
async function goTo(mm: Mounted, r: number, c: number): Promise<void> {
  let pos = -1;
  let row = -1;
  mm.editor.state.doc.descendants((node, p) => {
    if (node.type.name === "tableRow") {
      row += 1;
      if (row === r) {
        let off = p + 1;
        node.forEach((cell, _o, i) => {
          if (i === c) pos = off + 2; // into the cell, into its paragraph
          off += cell.nodeSize;
        });
      }
    }
    return true;
  });
  if (pos < 0) throw new Error(`no cell ${r},${c}`);
  await act(async () => {
    mm!.editor.commands.focus();
    mm!.editor.commands.setTextSelection(pos);
  });
}

async function typeInto(mm: Mounted, text: string): Promise<void> {
  await act(async () => {
    mm.editor.commands.insertContent(text);
  });
}

async function withTable(): Promise<Mounted> {
  const mm = await mountEditor("<p>intro</p>");
  await act(async () => {
    mm.editor.commands.focus("end");
  });
  await click(button(mm, "Insert table"));
  return mm;
}

describe("Insert table", () => {
  it("inserts a 3 by 3 table with a header row and puts the cursor in the first cell", async () => {
    m = await withTable();
    expect(grid(m)).toEqual([["", "", ""], ["", "", ""], ["", "", ""]]);
    expect(kinds(m)).toEqual(["tableHeader", "tableCell", "tableCell"]);
    expect(cursorCell(m)).toEqual([0, 0]);
    // The cursor is really there: typing lands in the first header cell.
    await typeInto(m, "Name");
    expect(grid(m)[0]![0]).toBe("Name");
  });

  it("its title and name are the same plain words, and it is a button on the bar", async () => {
    m = await mountEditor("<p>x</p>");
    const b = button(m, "Insert table");
    expect(b.title).toBe("Insert table");
    expect(b.getAttribute("aria-disabled")).toBeNull();
    expect(b.closest('[role="toolbar"]')).not.toBeNull();
  });

  it("is unavailable inside a table (tables stay simple: no table in a cell)", async () => {
    m = await withTable();
    expect(disabled(m, "Insert table")).toBe(true);
    const before = m.html();
    await click(button(m, "Insert table"));
    expect(m.html()).toBe(before);
  });
});

describe("the table controls are on the bar always, and say when they are not available", () => {
  it("outside a table every table-only control is aria-disabled, stays in the roving order, and does nothing", async () => {
    m = await mountEditor("<p>no table here</p>");
    for (const name of TABLE_ONLY) {
      expect(disabled(m, name), name).toBe(true);
      expect(button(m, name).disabled, `${name} must stay focusable`).toBe(false);
      await click(button(m, name));
    }
    expect(m.html()).toBe("<p>no table here</p>");
  });

  it("inside a table they all become available", async () => {
    m = await withTable();
    for (const name of TABLE_ONLY) expect(disabled(m, name), name).toBe(false);
  });

  it("they leave again when the cursor leaves the table", async () => {
    m = await withTable();
    await act(async () => {
      m!.editor.commands.focus("start");
    });
    // (The leading paragraph exists because a table at the edge gets one.)
    expect(cursorCell(m)).toBeNull();
    for (const name of TABLE_ONLY) expect(disabled(m, name), name).toBe(true);
    expect(disabled(m, "Insert table")).toBe(false);
  });

  it("there is no merge, split or resize control", () => {
    return mountEditor("<p>x</p>").then((mm) => {
      m = mm;
      const names = Array.from(mm.host.querySelectorAll("button[aria-label]")).map((b) => b.getAttribute("aria-label")!.toLowerCase());
      for (const n of names) expect(n, n).not.toMatch(/merge|split|resiz|width/);
    });
  });
});

describe("each table action", () => {
  it("Add row below / above", async () => {
    m = await withTable();
    await goTo(m, 1, 1);
    await typeInto(m, "mid");
    await click(button(m, "Add row below"));
    expect(grid(m).map((r) => r[1])).toEqual(["", "mid", "", ""]);
    await goTo(m, 1, 1);
    await click(button(m, "Add row above"));
    expect(grid(m).map((r) => r[1])).toEqual(["", "", "mid", "", ""]);
    expect(kinds(m)[0]).toBe("tableHeader"); // the header row stays on top
  });

  it("Add column right / left", async () => {
    m = await withTable();
    await goTo(m, 1, 1);
    await typeInto(m, "mid");
    await click(button(m, "Add column right"));
    expect(grid(m)[1]).toEqual(["", "mid", "", ""]);
    await goTo(m, 1, 1);
    await click(button(m, "Add column left"));
    expect(grid(m)[1]).toEqual(["", "", "mid", "", ""]);
    expect(grid(m).every((r) => r.length === 5)).toBe(true);
  });

  it("Delete row and Delete column remove only that row or column, and the cursor stays in the table", async () => {
    m = await withTable();
    await goTo(m, 1, 1);
    await typeInto(m, "x");
    await click(button(m, "Delete row"));
    expect(grid(m).length).toBe(2);
    expect(cursorCell(m)).not.toBeNull();
    await goTo(m, 0, 2);
    await click(button(m, "Delete column"));
    expect(grid(m).every((r) => r.length === 2)).toBe(true);
    expect(cursorCell(m)).not.toBeNull();
  });

  it("Header row toggles the top row between header cells and ordinary cells, and says which in aria-pressed", async () => {
    m = await withTable();
    expect(button(m, "Header row").getAttribute("aria-pressed")).toBe("true");
    await click(button(m, "Header row"));
    expect(kinds(m)).toEqual(["tableCell", "tableCell", "tableCell"]);
    expect(button(m, "Header row").getAttribute("aria-pressed")).toBe("false");
    expect(m.html()).not.toContain("<th");
    await click(button(m, "Header row"));
    expect(kinds(m)[0]).toBe("tableHeader");
    expect(m.html()).toContain('<th scope="col"');
  });

  it("Delete table removes the whole table and leaves the text around it", async () => {
    m = await withTable();
    await click(button(m, "Delete table"));
    expect(m.html()).not.toContain("<table");
    expect(m.html()).toContain("intro");
    expect(disabled(m, "Delete table")).toBe(true);
  });

  it("nothing in the commands merges, splits or resizes: the schema has no colwidth", async () => {
    m = await withTable();
    expect(m.html()).not.toMatch(/colwidth|colgroup|min-width|<col/);
    const opts = m.editor.extensionManager.extensions.find((e) => e.name === "table")!.options as { resizable: boolean };
    expect(opts.resizable).toBe(false);
  });
});

describe("what a table writes: mail-client HTML", () => {
  it("fixed inline styles only, a scope on header cells, explicit border and padding on every cell", async () => {
    m = await withTable();
    await typeInto(m, "H");
    const doc = new DOMParser().parseFromString(m.html(), "text/html");
    const table = doc.querySelector("table")!;
    // (Compared through the style object, since jsdom re-serialises shorthand values.)
    const want = styleText;
    expect(table.getAttribute("style")).toBeTruthy();
    expect((table as HTMLElement).style.cssText).toBe(want(TABLE_STYLE));
    expect(table.getAttributeNames()).toEqual(["style"]);
    const ths = Array.from(doc.querySelectorAll<HTMLElement>("th"));
    const tds = Array.from(doc.querySelectorAll<HTMLElement>("td"));
    expect(ths.length).toBe(3);
    expect(tds.length).toBe(6);
    for (const th of ths) {
      expect(th.getAttribute("scope")).toBe("col");
      expect(th.style.cssText).toBe(want(HEADER_CELL_STYLE));
      expect(th.style.getPropertyValue("text-align")).toBe("left");
      expect(th.style.getPropertyValue("font-weight")).toBe("bold");
      expect(th.getAttributeNames().sort()).toEqual(["scope", "style"]);
    }
    for (const td of tds) {
      expect(td.style.cssText).toBe(want(CELL_STYLE));
      expect(td.style.getPropertyValue("border")).toMatch(/^1px solid /);
      expect(td.style.getPropertyValue("padding")).toBe("4px 8px");
      expect(td.getAttributeNames()).toEqual(["style"]);
    }
    expect(doc.querySelectorAll("colgroup, col, thead, [class], [width], [colwidth]").length).toBe(0);
  });

  it("the stored constants are the mail-client forms the ADR names (not widened)", () => {
    expect(TABLE_STYLE).toBe("border-collapse:collapse");
    expect(CELL_STYLE).toBe("border:1px solid #999;padding:4px 8px");
    expect(HEADER_CELL_STYLE).toBe("border:1px solid #999;padding:4px 8px;text-align:left;font-weight:bold");
  });

  it("the live DOM is what is written: no colgroup, no min-width, no resize handle", async () => {
    m = await withTable();
    const live = m.surface;
    expect(live.querySelectorAll("colgroup, col").length).toBe(0);
    expect(live.querySelector("table")!.style.cssText).toBe(styleText(TABLE_STYLE));
    expect(live.querySelectorAll(".column-resize-handle, .tableWrapper").length).toBe(0);
  });

  it("text formatting inside cells survives, and a table inside a quote is allowed by the schema", async () => {
    m = await mountEditor(
      '<blockquote><table><tbody><tr><th>Item</th></tr><tr><td><p><strong>bold</strong> and <em>it</em> <span style="color:#cc0000">red</span></p></td></tr></tbody></table></blockquote>',
    );
    const out = m.html();
    expect(out).toContain("<blockquote><table");
    expect(out).toContain("<strong>bold</strong>");
    expect(out).toContain("<em>it</em>");
    expect(out).toMatch(/<span style="color: ?(#cc0000|rgb\(204, 0, 0\));?">red<\/span>/);
  });

  it("colspan and rowspan on a PASTED table are kept (the schema supports them), clamped; nothing in the UI creates one", async () => {
    m = await mountEditor(
      '<table><tbody><tr><td colspan="2">wide</td></tr><tr><td>a</td><td rowspan="999">b</td></tr></tbody></table>',
    );
    const out = m.html();
    expect(out).toContain('colspan="2"');
    expect(out).toContain('rowspan="20"'); // MAX_SPAN
    expect(out).not.toContain("999");
    await goTo(m, 1, 0);
    await click(button(m, "Add row below"));
    const fresh = new DOMParser().parseFromString(m.html(), "text/html").querySelectorAll("tr")[2]!;
    expect(fresh.querySelector("[colspan], [rowspan]")).toBeNull();
  });
});

describe("a header cell's own bold is not read back as a bold mark", () => {
  it("reopening the editor's own header HTML adds no <strong> (the document does not change on reopen)", async () => {
    m = await withTable();
    await typeInto(m, "Head");
    const saved = m.html();
    await m.unmount();
    m = await mountEditor(saved);
    expect(m.html()).toBe(saved);
    expect(m.html()).not.toContain("<strong>");
  });

  it("a pasted font-weight on a header or cell is dropped (style on a cell is not read)", async () => {
    m = await mountEditor('<table><tr><th style="font-weight:700">h</th><td style="font-weight:bold">c</td></tr></table>');
    expect(m.html()).not.toContain("<strong>");
  });

  it("control: bold from a style on a span or paragraph is still read, inside a cell and outside one", async () => {
    m = await mountEditor('<p><span style="font-weight:bold">out</span></p><table><tr><td><p><span style="font-weight:bold">in</span></p></td></tr></table>');
    expect(m.html()).toContain("<strong>out</strong>");
    expect(m.html()).toContain("<strong>in</strong>");
  });
});

describe("Tab leaves the editor, even from a table", () => {
  it("Tab in the LAST cell is not handled: no row is added, the key is not cancelled, the cursor stays", async () => {
    m = await withTable();
    await goTo(m, 2, 2);
    const rows = grid(m).length;
    const before = m.html();
    const ev = await press(m, { key: "Tab", keyCode: 9 });
    expect(ev.defaultPrevented).toBe(false);
    expect(grid(m).length).toBe(rows);
    expect(m.html()).toBe(before);
    expect(cursorCell(m)).toEqual([2, 2]);
  });

  it("Tab in the first and a middle cell does not move to the next cell either, and Shift+Tab does not move back", async () => {
    m = await withTable();
    for (const [r, c] of [[0, 0], [1, 1]] as const) {
      await goTo(m, r, c);
      const ev = await press(m, { key: "Tab", keyCode: 9 });
      expect(ev.defaultPrevented).toBe(false);
      expect(cursorCell(m)).toEqual([r, c]);
      const back = await press(m, { key: "Tab", keyCode: 9, shiftKey: true });
      expect(back.defaultPrevented).toBe(false);
      expect(cursorCell(m)).toEqual([r, c]);
    }
  });

  it("the table extension on its own binds neither Tab nor Shift-Tab (the override is what stops the row being added)", () => {
    // Without the editor's keydown guard in the way, so only the extension's own
    // keymap is asked. (Probe: restoring the library's Tab makes this go red.)
    const host = document.createElement("div");
    document.body.appendChild(host);
    const ed = new Editor({
      element: host,
      extensions: [StarterKit.configure({ link: false }), ...TABLE_EXTENSIONS],
      content: "<table><tbody><tr><td><p>a</p></td></tr></tbody></table>",
    });
    try {
      ed.commands.setTextSelection(5);
      const ask = (init: KeyboardEventInit): boolean =>
        !!ed.view.someProp("handleKeyDown", (f) => f(ed.view, new KeyboardEvent("keydown", { bubbles: true, cancelable: true, ...init })));
      const rows = (): number => ed.getHTML().split("<tr>").length - 1;
      const n = rows();
      expect(ask({ key: "Tab", code: "Tab", keyCode: 9 })).toBe(false);
      expect(ask({ key: "Tab", code: "Tab", keyCode: 9, shiftKey: true })).toBe(false);
      expect(rows()).toBe(n);
      // Control: the same harness does see a binding that exists (Mod-Backspace
      // is the table's own), so "false" above is not the harness being deaf.
      ed.commands.selectAll();
      expect(ask({ key: "a", code: "KeyA", keyCode: 65, ctrlKey: true })).toBe(true);
    } finally {
      ed.destroy();
      host.remove();
    }
  });

  it("control: the library's own Tab WOULD add a row at the last cell (so the tests above can fail)", () => {
    const host = document.createElement("div");
    document.body.appendChild(host);
    // The library's table node, untouched.
    return import("@tiptap/extension-table").then(({ TableKit }) => {
      const ed = new Editor({
        element: host,
        extensions: [StarterKit.configure({ link: false }), TableKit],
        content: "<table><tbody><tr><td><p>a</p></td></tr></tbody></table>",
      });
      try {
        ed.commands.setTextSelection(5);
        const rows = (): number => ed.getHTML().split("<tr>").length - 1;
        expect(rows()).toBe(1);
        const handled = !!ed.view.someProp("handleKeyDown", (f) => f(ed.view, new KeyboardEvent("keydown", { key: "Tab", code: "Tab", keyCode: 9, bubbles: true, cancelable: true })));
        expect(handled).toBe(true);
        expect(rows()).toBe(2);
      } finally {
        ed.destroy();
        host.remove();
      }
    });
  });
});

describe("arrow keys", () => {
  it("move between cells: the table plugin takes the arrow at a cell's edge, and nothing in the editor swallows it first", async () => {
    // (jsdom has no layout, so which arrow press crosses a cell edge is the real
    // browser lane's to prove in text; here the cells are empty, so every press is
    // at an edge.)
    m = await withTable();
    await goTo(m, 0, 0);
    const steps: Array<[string, [number, number]]> = [
      ["ArrowRight", [0, 1]],
      ["ArrowDown", [1, 1]],
      ["ArrowLeft", [1, 0]],
      ["ArrowUp", [0, 0]],
    ];
    for (const [key, want] of steps) {
      await press(m, { key });
      expect(cursorCell(m), key).toEqual(want);
    }
  });
});

describe("a table at the edge of the document", () => {
  it("gets an empty paragraph beside it, so the cursor can be placed above and below", async () => {
    m = await mountEditor("<p></p>");
    await click(button(m, "Insert table"));
    const doc = m.editor.state.doc;
    expect(doc.firstChild!.type.name).toBe("paragraph");
    expect(doc.lastChild!.type.name).toBe("paragraph");
    expect(doc.childCount).toBe(3);
  });

  it("deleting the table leaves a document of paragraphs", async () => {
    m = await mountEditor("<p></p>");
    await click(button(m, "Insert table"));
    await click(button(m, "Delete table"));
    expect(m.editor.state.doc.childCount).toBe(2);
    expect(m.html()).not.toContain("<table");
  });
});
