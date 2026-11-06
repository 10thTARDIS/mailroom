// SPDX-License-Identifier: AGPL-3.0-only

// Simple tables (ADR-051 beta 4): rows and columns, a header row, borders. No
// merging and no column resizing in the UI. Tiptap's TableKit nodes, extended
// here so that they follow the same fixed-list rule as the style marks:
//
//   * what a table WRITES is mail-client HTML, fixed in this file: inline styles
//     only, an explicit border and padding on every cell, `scope="col"` on a
//     header cell. No `<colgroup>`, no `min-width`, no `colwidth`, no class.
//   * what a table READS is structure and text only. A pasted or seeded `style`,
//     `background`, `bgcolor`, `width`, `class`, `align` or event handler on a
//     table, row or cell is not parsed, so it cannot be kept; our own styles are
//     applied on output.
//   * `colspan` and `rowspan` ARE read (a pasted table may have them, and the
//     schema supports them) and written back, clamped to 1..MAX_SPAN. Nothing in
//     the UI creates one: there is no merge or split control.
//
// Tab and Shift-Tab are NOT bound (the library binds Tab to "next cell, and at
// the last cell add a row", which is a keyboard trap). They leave the editor, as
// everywhere else in the body (see buildEditorProps in ./extensions.ts); the
// arrow keys move between cells natively.

import { Extension } from "@tiptap/core";
import { Table, TableCell, TableHeader, TableRow } from "@tiptap/extension-table";
import { Plugin } from "@tiptap/pm/state";

/** The border and padding every cell carries when it is sent. A constant, never
 * read from markup. */
export const CELL_STYLE = "border:1px solid #999;padding:4px 8px";
/** A header cell: the same, left-aligned and bold. */
export const HEADER_CELL_STYLE = `${CELL_STYLE};text-align:left;font-weight:bold`;
export const TABLE_STYLE = "border-collapse:collapse";

/** The largest colspan/rowspan kept from pasted markup. */
export const MAX_SPAN = 20;

function span(raw: unknown): number {
  const n = typeof raw === "number" ? raw : Number.parseInt(String(raw ?? ""), 10);
  return Number.isInteger(n) && n >= 1 ? Math.min(n, MAX_SPAN) : 1;
}

function spanAttribute(name: "colspan" | "rowspan") {
  return {
    default: 1,
    parseHTML: (el: HTMLElement) => span(el.getAttribute(name)),
    renderHTML: (attrs: Record<string, unknown>) => {
      const n = span(attrs[name]);
      return n > 1 ? { [name]: String(n) } : {};
    },
  };
}

/** The attributes of a cell. `colwidth` stays defined (the table commands read it)
 * but is always null and never written; `align` (text-align) is not kept. */
function cellAttributes() {
  return {
    colspan: spanAttribute("colspan"),
    rowspan: spanAttribute("rowspan"),
    colwidth: { default: null, parseHTML: () => null, renderHTML: () => ({}) },
  };
}

const TableNode = Table.configure({
  resizable: false,
  // No node view: the live DOM is exactly what renderHTML writes (the library's
  // view adds a <colgroup> and a min-width), so what is on screen is what is sent.
  View: null,
  allowTableNodeSelection: false,
  HTMLAttributes: {},
}).extend({
  renderHTML() {
    return ["table", { style: TABLE_STYLE }, ["tbody", 0]];
  },
  addKeyboardShortcuts() {
    // The library's own shortcuts minus Tab and Shift-Tab (the keep-it-short way
    // to say "no Tab trap"): Backspace and Delete over a whole-table selection
    // still delete the table.
    const { Tab: _tab, "Shift-Tab": _shiftTab, ...rest } = this.parent?.() ?? {};
    void _tab;
    void _shiftTab;
    return rest;
  },
});

const CellNode = TableCell.configure({ HTMLAttributes: {} }).extend({
  addAttributes: cellAttributes,
  renderHTML({ HTMLAttributes }) {
    return ["td", { ...HTMLAttributes, style: CELL_STYLE }, 0];
  },
});

const HeaderNode = TableHeader.configure({ HTMLAttributes: {} }).extend({
  addAttributes: cellAttributes,
  renderHTML({ HTMLAttributes }) {
    return ["th", { scope: "col", ...HTMLAttributes, style: HEADER_CELL_STYLE }, 0];
  },
});

const RowNode = TableRow.configure({ HTMLAttributes: {} });

/** With no gap cursor, the cursor cannot be placed above a table that starts the
 * document or below one that ends it. So a table at either edge gets an empty
 * paragraph beside it (on any edit, which covers insert, paste and delete). */
const TableEdges = Extension.create({
  name: "tableEdges",
  addProseMirrorPlugins() {
    return [
      new Plugin({
        appendTransaction: (trs, _old, state) => {
          if (!trs.some((t) => t.docChanged)) return null;
          const { doc } = state;
          const first = doc.firstChild;
          const last = doc.lastChild;
          const para = state.schema.nodes["paragraph"];
          if (!para || !first || !last) return null;
          const tr = state.tr;
          if (last.type.name === "table") tr.insert(doc.content.size, para.create());
          if (first.type.name === "table") tr.insert(0, para.create());
          return tr.docChanged ? tr : null;
        },
      }),
    ];
  },
});

export const TABLE_EXTENSIONS = [TableNode, RowNode, HeaderNode, CellNode, TableEdges];
