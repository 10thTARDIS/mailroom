// SPDX-License-Identifier: AGPL-3.0-only

// Paragraph indentation (ADR-051 beta 2). Tiptap has no extension for it, so this
// is ours: an `indent` attribute, 0 to 6, on paragraphs (and headings, should the
// schema ever have them), written as `margin-left: Nem` (N times 2) and read back
// from `margin-left` in em or px. The attribute is always an integer in range: it
// is parsed defensively (./indentValue.ts) and clamped again on render, so neither
// pasted markup nor a hand-built document can write another margin.
//
// `indent` and `outdent` do the right thing for where the cursor is: in a list
// item they nest and un-nest the item (sink and lift); elsewhere they change the
// indent of every paragraph the selection touches. Mod-] and Mod-[ are the
// shortcuts. Tab is NOT bound: it leaves the editor, so the body never traps the
// keyboard (see buildEditorProps in ./extensions.ts).

import { Extension } from "@tiptap/core";
import type { CommandProps } from "@tiptap/core";
import type { EditorState } from "@tiptap/pm/state";
import { clampIndent, indentStyle, parseIndent } from "./indentValue";

const TYPES = ["paragraph", "heading"];

declare module "@tiptap/core" {
  interface Commands<ReturnType> {
    indent: {
      /** Nests the list item, or indents the selected paragraphs by one level. */
      indent: () => ReturnType;
      /** The reverse. */
      outdent: () => ReturnType;
    };
  }
}

function inListItem(state: EditorState): boolean {
  const { $from } = state.selection;
  for (let d = $from.depth; d > 0; d--) if ($from.node(d).type.name === "listItem") return true;
  return false;
}

export const Indent = Extension.create({
  name: "indent",

  addGlobalAttributes() {
    return [
      {
        types: TYPES,
        attributes: {
          indent: {
            default: 0,
            parseHTML: (el: HTMLElement) => parseIndent(el.getAttribute("style")),
            renderHTML: (attrs: Record<string, unknown>) => {
              const style = indentStyle(clampIndent(attrs["indent"]));
              return style ? { style } : {};
            },
          },
        },
      },
    ];
  },

  addCommands() {
    const shift =
      (delta: 1 | -1) =>
      () =>
      ({ state, tr, dispatch, commands }: CommandProps) => {
        if (inListItem(state)) {
          // Nothing to nest under (the first item) or nothing to lift out of is a
          // no-op, but the key is still ours: the chord must not fall through.
          if (delta > 0) commands.sinkListItem("listItem");
          else commands.liftListItem("listItem");
          return true;
        }
        const { from, to } = state.selection;
        state.doc.nodesBetween(from, to, (node, pos) => {
          if (!TYPES.includes(node.type.name)) return true;
          const next = clampIndent(clampIndent(node.attrs["indent"]) + delta);
          if (dispatch && next !== node.attrs["indent"]) tr.setNodeMarkup(pos, undefined, { ...node.attrs, indent: next });
          return false;
        });
        return true;
      };
    return { indent: shift(1), outdent: shift(-1) };
  },

  addKeyboardShortcuts() {
    return {
      "Mod-]": () => this.editor.commands.indent(),
      "Mod-[": () => this.editor.commands.outdent(),
    };
  },
});
