// SPDX-License-Identifier: AGPL-3.0-only

// THE COMPOSE EDITOR (ADR-051): Tiptap 3 on ProseMirror, replacing the
// execCommand editor. It is loaded as its own chunk (./LazyRichEditor.tsx) and
// only when the composer opens.
//
// What did not change: the component is UNCONTROLLED. `initialHTML` seeds the
// document once, every edit goes out through `onChange` as HTML, and the composer
// re-KEYS this component when the seed legitimately changes (a From change while
// the body is untouched, a template insert), which is a remount rather than a
// re-seed. The HTML is still sanitized by the server before it is sent.
//
// What did: nothing here writes HTML into the page. The seed goes through
// `setContent`-style schema parsing, the read-back is the editor's serializer,
// and a paste is parsed into the schema in an inert document (ADR-015 amended by
// ADR-051 §4).

import { useEffect, useRef, useState } from "react";
import type { CSSProperties, FocusEvent, JSX, KeyboardEvent } from "react";
import { EditorContent, useEditor, useEditorState } from "@tiptap/react";
import type { Editor } from "@tiptap/core";
import { t, type MessageKey } from "../../i18n";
import { buildEditorProps, buildExtensions } from "./extensions";
import { FONT_FAMILIES, FONT_SIZES } from "./styleValues";
import type { EditorHooks } from "./extensions";
import { isAllowedLinkUri, linkFromPrompt } from "./uri";

export interface RichTextEditorProps {
  initialHTML: string;
  onChange: (html: string) => void;
  /** Uploads an image and resolves to the Content-ID to reference it by, or null
   * when the upload failed (the editor then inserts nothing). */
  onImageFile: (file: File) => Promise<string | null>;
  onBlur?: () => void;
  /** Says something to the user once (a toast): remote pictures left out, a link
   * refused. */
  onNotice: (message: string) => void;
  /** Hands the live editor to the caller once it exists (null when it goes). For
   * tests, which read the document without an edit having happened. */
  onReady?: (editor: Editor | null) => void;
  label: string;
}

const isMac = (): boolean =>
  typeof navigator !== "undefined" && /mac|iphone|ipad/i.test(navigator.platform || "");

/** "Ctrl+B" or, on a Mac, "⌘B". */
function chord(key: string, shift = false, alt = false): string {
  return isMac()
    ? `${alt ? "⌥" : ""}${shift ? "⇧" : ""}⌘${key.toUpperCase()}`
    : `Ctrl+${alt ? "Alt+" : ""}${shift ? "Shift+" : ""}${key.toUpperCase()}`;
}

/** The text and highlight palettes: a small fixed set, each with a name (the colour
 * is never the only way to tell them apart). Values are canonical `#rrggbb`, which
 * the style gate (./styleValues.ts) accepts as they are. */
export const TEXT_COLOURS: ReadonlyArray<{ id: string; hex: string }> = [
  { id: "black", hex: "#000000" },
  { id: "darkGrey", hex: "#434343" },
  { id: "grey", hex: "#999999" },
  { id: "red", hex: "#cc0000" },
  { id: "orange", hex: "#e69138" },
  { id: "green", hex: "#38761d" },
  { id: "blue", hex: "#1155cc" },
  { id: "darkBlue", hex: "#0b2e6b" },
  { id: "purple", hex: "#674ea7" },
  { id: "brown", hex: "#7f6000" },
];
export const HIGHLIGHT_COLOURS: ReadonlyArray<{ id: string; hex: string }> = [
  { id: "yellow", hex: "#ffff00" },
  { id: "lightGreen", hex: "#b6d7a8" },
  { id: "lightBlue", hex: "#cfe2f3" },
  { id: "lightPink", hex: "#ead1dc" },
  { id: "lightOrange", hex: "#fce5cd" },
  { id: "lightGrey", hex: "#d9d9d9" },
  { id: "lightRed", hex: "#f4cccc" },
  { id: "lightPurple", hex: "#d9d2e9" },
];

const FAMILY_LABEL: Record<string, MessageKey> = {
  sans: "compose.fontSans",
  serif: "compose.fontSerif",
  mono: "compose.fontMono",
  wide: "compose.fontWide",
};
const SIZE_LABEL: Record<string, MessageKey> = {
  small: "compose.sizeSmall",
  large: "compose.sizeLarge",
  huge: "compose.sizeHuge",
};

type PaletteKind = "text" | "highlight";

/** The editor's HTML for the draft. An empty paragraph is written `<p><br></p>`:
 * mail clients draw a bare `<p></p>` with no height, which would lose the blank
 * lines the writer typed. (The serializer's output escapes all text, so a literal
 * empty-paragraph tag can only be an empty paragraph.) */
export function editorHtml(editor: Editor): string {
  return editor.getHTML().replace(/<p(\s[^>]*)?><\/p>/g, "<p$1><br></p>");
}

/** Whether the table the cursor is in has a header row (its first row is all header cells). */
function firstRowIsHeader(ed: Editor): boolean {
  const { $from } = ed.state.selection;
  for (let d = $from.depth; d > 0; d--) {
    const node = $from.node(d);
    if (node.type.name === "table") {
      const row = node.firstChild;
      if (!row || row.childCount === 0) return false;
      let all = true;
      row.forEach((cell) => {
        if (cell.type.name !== "tableHeader") all = false;
      });
      return all;
    }
  }
  return false;
}

interface Tool {
  id: string;
  labelKey: MessageKey;
  title: string;
  glyph: string;
  cssGlyph?: boolean;
  active?: boolean;
  /** Not available right now (undo with nothing to undo). Marked with
   * aria-disabled and not `disabled`, so the button stays in the roving tab order
   * and the arrow keys do not skip it. */
  disabled?: boolean;
  /** The colour a palette button currently applies, as a validated #rrggbb. */
  swatch?: string | null;
  /** Opens a palette instead of acting. */
  palette?: PaletteKind;
  run: () => void;
}

/** One stop on the toolbar: a button, or a select (font, size). All are `.editor-tool`
 * elements in one roving tab order. */
type Item =
  | { kind: "tool"; tool: Tool }
  | { kind: "select"; id: string; labelKey: MessageKey; value: string; options: Array<{ value: string; label: string }>; onChange: (v: string) => void };

export default function RichTextEditor({
  initialHTML,
  onChange,
  onImageFile,
  onBlur,
  onNotice,
  onReady,
  label,
}: RichTextEditorProps): JSX.Element {
  const imgRef = useRef<HTMLInputElement>(null);
  const toolbarRef = useRef<HTMLDivElement>(null);
  const paletteRef = useRef<HTMLDivElement>(null);
  const [rove, setRove] = useState(0);
  // Which colour palette is open (a row under the toolbar, not a floating popup:
  // the toolbar scrolls sideways on a narrow screen and would clip a popup), and
  // which of its swatches holds the roving tab stop.
  const [palette, setPalette] = useState<PaletteKind | null>(null);
  const [paletteRove, setPaletteRove] = useState(0);

  // The callbacks the editor was built with must see the newest props without
  // rebuilding the editor (which would lose the document).
  const latest = useRef({ onChange, onImageFile, onBlur, onNotice });
  latest.current = { onChange, onImageFile, onBlur, onNotice };
  const editorRef = useRef<Editor | null>(null);

  // One hooks object for the editor's whole life: it reads everything through
  // refs, so a re-render never needs to rebuild the editor.
  const hooks = useRef<EditorHooks>({
    insertImageFile: (file, pos) => {
      void latest.current.onImageFile(file).then((cid) => {
        const ed = editorRef.current;
        if (!cid || !ed || ed.isDestroyed) return;
        const image = { type: "image", attrs: { src: `cid:${cid}`, alt: file.name } };
        if (pos === null) ed.chain().focus().insertContent(image).run();
        else ed.chain().focus().insertContentAt(pos, image).run();
      });
    },
    remoteImagesDropped: () => latest.current.onNotice(t("compose.remoteImagesLeftOut")),
  });

  const editor = useEditor({
    // Render on mount rather than after it: nothing here is server-rendered, and
    // the composer's own tests and focus handling expect the surface to exist.
    immediatelyRender: true,
    // ProseMirror's base rules ship in our own stylesheet (styles.css), not
    // injected at run time (ADR-051 §2).
    injectCSS: false,
    extensions: buildExtensions(hooks.current),
    content: initialHTML,
    editorProps: buildEditorProps(hooks.current, {
      class: "editor-surface",
      role: "textbox",
      "aria-multiline": "true",
      "aria-label": label,
      // A table wider than the body scrolls sideways inside the surface (beta 4);
      // the surface is the focusable, named scroller, and the page never scrolls
      // sideways. Declared so the phone-width check treats it as deliberate.
      "data-scroll-x": "intentional",
    }),
    onUpdate: ({ editor: ed }) => latest.current.onChange(editorHtml(ed)),
    onBlur: () => latest.current.onBlur?.(),
  });
  editorRef.current = editor;
  useEffect(() => {
    onReady?.(editor);
    return () => onReady?.(null);
    // Only when the editor instance changes; onReady is a test hook.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editor]);

  // Which buttons are pressed; re-evaluated on every transaction.
  const state = useEditorState({
    editor,
    selector: ({ editor: ed }) => {
      const style = ed?.getAttributes("textStyle") ?? {};
      return {
        bold: ed?.isActive("bold") ?? false,
        italic: ed?.isActive("italic") ?? false,
        underline: ed?.isActive("underline") ?? false,
        strike: ed?.isActive("strike") ?? false,
        subscript: ed?.isActive("subscript") ?? false,
        superscript: ed?.isActive("superscript") ?? false,
        code: ed?.isActive("code") ?? false,
        codeBlock: ed?.isActive("codeBlock") ?? false,
        blockquote: ed?.isActive("blockquote") ?? false,
        bulletList: ed?.isActive("bulletList") ?? false,
        orderedList: ed?.isActive("orderedList") ?? false,
        inTable: ed?.isActive("table") ?? false,
        headerRow: ed ? firstRowIsHeader(ed) : false,
        canUndo: ed?.can().undo() ?? false,
        canRedo: ed?.can().redo() ?? false,
        fontFamily: typeof style["fontFamily"] === "string" ? (style["fontFamily"] as string) : "",
        fontSize: typeof style["fontSize"] === "string" ? (style["fontSize"] as string) : "",
        // (Already gated on the way in; only the apply buttons' indicator.)
        colour: typeof style["color"] === "string" ? (style["color"] as string) : null,
        highlight: typeof style["backgroundColor"] === "string" ? (style["backgroundColor"] as string) : null,
      };
    },
  });

  const addLink = (): void => {
    if (!editor) return;
    const url = linkFromPrompt(window.prompt(t("compose.linkPrompt")));
    if (url === "") return;
    if (!isAllowedLinkUri(url)) {
      latest.current.onNotice(t("compose.linkRefused"));
      editor.chain().focus().run();
      return;
    }
    editor.chain().focus().setLink({ href: url }).run();
  };

  const chain = () => editor?.chain().focus();
  const togglePalette = (kind: PaletteKind): void => {
    setPaletteRove(0);
    setPalette((open) => (open === kind ? null : kind));
  };
  const applyColour = (kind: PaletteKind, hex: string | null): void => {
    const c = chain();
    if (kind === "text") void (hex === null ? c?.unsetColor() : c?.setColor(hex))?.run();
    else void (hex === null ? c?.unsetBackgroundColor() : c?.setBackgroundColor(hex))?.run();
    setPalette(null);
  };
  // A select changes the selection's style but leaves focus on the select, so the
  // arrow keys can keep stepping through the options (some browsers change the
  // value on every arrow press). The selection survives the focus change.
  const applyFont = (kind: "family" | "size", value: string): void => {
    const c = editor?.chain();
    if (!c) return;
    if (kind === "family") void (value === "" ? c.unsetFontFamily() : c.setFontFamily(value)).run();
    else void (value === "" ? c.unsetFontSize() : c.setFontSize(value)).run();
  };

  const items: Item[] = [
    {
      kind: "tool",
      tool: {
        id: "undo",
        labelKey: "compose.undo",
        glyph: "↶",
        cssGlyph: true,
        title: `${t("compose.undo")} (${chord("Z")})`,
        disabled: !state?.canUndo,
        run: () => void chain()?.undo().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "redo",
        labelKey: "compose.redo",
        glyph: "↷",
        cssGlyph: true,
        title: `${t("compose.redo")} (${chord("Z", true)})`,
        disabled: !state?.canRedo,
        run: () => void chain()?.redo().run(),
      },
    },
    {
      kind: "select",
      id: "fontFamily",
      labelKey: "compose.fontFamily",
      value: FONT_FAMILIES.some((f) => f.css === state?.fontFamily) ? (state?.fontFamily ?? "") : "",
      options: [
        { value: "", label: t("compose.fontDefault") },
        ...FONT_FAMILIES.map((f) => ({ value: f.css, label: t(FAMILY_LABEL[f.id] as MessageKey) })),
      ],
      onChange: (v) => applyFont("family", v),
    },
    {
      kind: "select",
      id: "fontSize",
      labelKey: "compose.fontSize",
      value: FONT_SIZES.some((f) => f.css === state?.fontSize) ? (state?.fontSize ?? "") : "",
      options: [
        { value: FONT_SIZES[0]!.css, label: t("compose.sizeSmall") },
        { value: "", label: t("compose.sizeNormal") },
        ...FONT_SIZES.slice(1).map((f) => ({ value: f.css, label: t(SIZE_LABEL[f.id] as MessageKey) })),
      ],
      onChange: (v) => applyFont("size", v),
    },
    {
      kind: "tool",
      tool: {
        id: "bold",
        labelKey: "compose.bold",
        glyph: "B",
        title: `${t("compose.bold")} (${chord("B")})`,
        active: state?.bold,
        run: () => void chain()?.toggleBold().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "italic",
        labelKey: "compose.italic",
        glyph: "I",
        title: `${t("compose.italic")} (${chord("I")})`,
        active: state?.italic,
        run: () => void chain()?.toggleItalic().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "underline",
        labelKey: "compose.underline",
        glyph: "U",
        title: `${t("compose.underline")} (${chord("U")})`,
        active: state?.underline,
        run: () => void chain()?.toggleUnderline().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "strike",
        labelKey: "compose.strike",
        glyph: "S",
        cssGlyph: true,
        title: `${t("compose.strike")} (${chord("S", true)})`,
        active: state?.strike,
        run: () => void chain()?.toggleStrike().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "subscript",
        labelKey: "compose.subscript",
        glyph: "x₂",
        cssGlyph: true,
        title: `${t("compose.subscript")} (${chord(",")})`,
        active: state?.subscript,
        run: () => void chain()?.toggleSubscript().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "superscript",
        labelKey: "compose.superscript",
        glyph: "x²",
        cssGlyph: true,
        title: `${t("compose.superscript")} (${chord(".")})`,
        active: state?.superscript,
        run: () => void chain()?.toggleSuperscript().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "textColour",
        labelKey: "compose.textColour",
        glyph: "A",
        cssGlyph: true,
        title: t("compose.textColour"),
        palette: "text",
        swatch: state?.colour,
        run: () => togglePalette("text"),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "highlightColour",
        labelKey: "compose.highlightColour",
        glyph: "A",
        cssGlyph: true,
        title: t("compose.highlightColour"),
        palette: "highlight",
        swatch: state?.highlight,
        run: () => togglePalette("highlight"),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "bulletList",
        labelKey: "compose.bulletList",
        glyph: "•",
        title: `${t("compose.bulletList")} (${chord("8", true)})`,
        active: state?.bulletList,
        run: () => void chain()?.toggleBulletList().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "orderedList",
        labelKey: "compose.numberList",
        glyph: "1.",
        cssGlyph: true,
        title: `${t("compose.numberList")} (${chord("7", true)})`,
        active: state?.orderedList,
        run: () => void chain()?.toggleOrderedList().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "outdent",
        labelKey: "compose.outdent",
        glyph: "⇤",
        cssGlyph: true,
        title: `${t("compose.outdent")} (${chord("[")})`,
        run: () => void chain()?.outdent().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "indent",
        labelKey: "compose.indent",
        glyph: "⇥",
        cssGlyph: true,
        title: `${t("compose.indent")} (${chord("]")})`,
        run: () => void chain()?.indent().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "blockquote",
        labelKey: "compose.blockquote",
        glyph: "”",
        cssGlyph: true,
        title: `${t("compose.blockquote")} (${chord("B", true)})`,
        active: state?.blockquote,
        run: () => void chain()?.toggleBlockquote().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "code",
        labelKey: "compose.code",
        glyph: "</>",
        cssGlyph: true,
        title: `${t("compose.code")} (${chord("E")})`,
        active: state?.code,
        run: () => void chain()?.toggleCode().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "codeBlock",
        labelKey: "compose.codeBlock",
        glyph: "{ }",
        cssGlyph: true,
        title: `${t("compose.codeBlock")} (${chord("C", false, true)})`,
        active: state?.codeBlock,
        run: () => void chain()?.toggleCodeBlock().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "link",
        labelKey: "compose.link",
        glyph: "🔗",
        title: t("compose.link"),
        run: addLink,
      },
    },
    {
      kind: "tool",
      tool: {
        id: "image",
        labelKey: "compose.insertImage",
        glyph: "🖼",
        title: t("compose.insertImage"),
        run: () => imgRef.current?.click(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "insertTable",
        labelKey: "compose.tableInsert",
        glyph: "▦",
        cssGlyph: true,
        title: t("compose.tableInsert"),
        // One table at a time in a cell is plenty: tables are simple.
        disabled: state?.inTable,
        run: () => void chain()?.insertTable({ rows: 3, cols: 3, withHeaderRow: true }).run(),
      },
    },
    ...(
      [
        ["tableRowBefore", "compose.tableRowBefore", "+↑", () => chain()?.addRowBefore().run()],
        ["tableRowAfter", "compose.tableRowAfter", "+↓", () => chain()?.addRowAfter().run()],
        ["tableColBefore", "compose.tableColBefore", "+←", () => chain()?.addColumnBefore().run()],
        ["tableColAfter", "compose.tableColAfter", "+→", () => chain()?.addColumnAfter().run()],
        ["tableDeleteRow", "compose.tableDeleteRow", "−═", () => chain()?.deleteRow().run()],
        ["tableDeleteCol", "compose.tableDeleteCol", "−║", () => chain()?.deleteColumn().run()],
      ] as Array<[string, MessageKey, string, () => unknown]>
    ).map(
      ([id, key, glyph, run]): Item => ({
        kind: "tool",
        tool: { id, labelKey: key, glyph, cssGlyph: true, title: t(key), disabled: !state?.inTable, run: () => void run() },
      }),
    ),
    {
      kind: "tool",
      tool: {
        id: "tableHeaderRow",
        labelKey: "compose.tableHeaderRow",
        glyph: "H",
        cssGlyph: true,
        title: t("compose.tableHeaderRow"),
        active: state?.inTable ? state.headerRow : false,
        disabled: !state?.inTable,
        run: () => void chain()?.toggleHeaderRow().run(),
      },
    },
    {
      kind: "tool",
      tool: {
        id: "tableDelete",
        labelKey: "compose.tableDelete",
        glyph: "✕",
        cssGlyph: true,
        title: t("compose.tableDelete"),
        disabled: !state?.inTable,
        run: () => void chain()?.deleteTable().run(),
      },
    },
  ];

  const onToolbarKey = (ev: KeyboardEvent<HTMLDivElement>): void => {
    const n = items.length;
    let next = -1;
    if (ev.key === "ArrowRight") next = (rove + 1) % n;
    else if (ev.key === "ArrowLeft") next = (rove - 1 + n) % n;
    else if (ev.key === "Home") next = 0;
    else if (ev.key === "End") next = n - 1;
    if (next < 0) return;
    ev.preventDefault();
    setRove(next);
    toolbarRef.current?.querySelectorAll<HTMLElement>(".editor-tool")[next]?.focus();
  };

  // The palette is a small widget of its own: the arrow keys step through the
  // swatches (one tab stop), Escape closes it and gives focus back to its button,
  // and leaving it by any other route closes it.
  const swatches = (): HTMLButtonElement[] =>
    Array.from(paletteRef.current?.querySelectorAll<HTMLButtonElement>("button") ?? []);
  const onPaletteKey = (ev: KeyboardEvent<HTMLDivElement>): void => {
    const n = swatches().length;
    let next = -1;
    if (ev.key === "ArrowRight" || ev.key === "ArrowDown") next = (paletteRove + 1) % n;
    else if (ev.key === "ArrowLeft" || ev.key === "ArrowUp") next = (paletteRove - 1 + n) % n;
    else if (ev.key === "Home") next = 0;
    else if (ev.key === "End") next = n - 1;
    else if (ev.key === "Escape") {
      ev.preventDefault();
      ev.stopPropagation();
      const open = palette;
      setPalette(null);
      const trigger = items.findIndex((i) => i.kind === "tool" && i.tool.palette === open);
      toolbarRef.current?.querySelectorAll<HTMLElement>(".editor-tool")[trigger]?.focus();
      return;
    }
    if (next < 0) return;
    ev.preventDefault();
    setPaletteRove(next);
    swatches()[next]?.focus();
  };
  const onPaletteBlur = (ev: FocusEvent<HTMLDivElement>): void => {
    const to = ev.relatedTarget as Node | null;
    if (to && (paletteRef.current?.contains(to) || toolbarRef.current?.contains(to))) return;
    setPalette(null);
  };
  // Opening a palette moves focus into it (to its first swatch).
  useEffect(() => {
    if (palette) swatches()[0]?.focus();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [palette]);

  const onImageChosen = (files: FileList | null): void => {
    const file = files?.[0];
    if (imgRef.current) imgRef.current.value = "";
    if (file) hooks.current.insertImageFile(file, null);
  };

  // "1." is TEXT to axe (WCAG 2.5.3, label-content-name-mismatch in axe-core
  // 4.14): a name of "Numbered list" does not contain it. Painted from CSS
  // (.editor-tool[data-glyph]::before) it is decoration, not label content. The
  // beta 2 glyphs are painted the same way, since none of them is part of its
  // button's name ("x₂" is not in "Subscript").

  const colours = palette === "highlight" ? HIGHLIGHT_COLOURS : TEXT_COLOURS;
  const paletteLabel = palette === "highlight" ? t("compose.highlightColour") : t("compose.textColour");

  return (
    <div className="editor">
      <div
        ref={toolbarRef}
        className="editor-toolbar"
        role="toolbar"
        // On a narrow screen the bar scrolls sideways inside itself; its keyboard
        // route is its own roving items, which scroll into view when focused.
        data-scroll-x="intentional"
        aria-label={t("compose.formatting")}
        onKeyDown={onToolbarKey}
      >
        {items.map((item, i) => {
          if (item.kind === "select") {
            return (
              <select
                key={item.id}
                className="editor-tool editor-select"
                data-tool={item.id}
                aria-label={t(item.labelKey)}
                value={item.value}
                tabIndex={i === rove ? 0 : -1}
                onFocus={() => setRove(i)}
                onChange={(ev) => item.onChange(ev.target.value)}
              >
                {item.options.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            );
          }
          const tool = item.tool;
          return (
            <button
              key={tool.id}
              type="button"
              className="editor-tool"
              data-tool={tool.id}
              aria-label={t(tool.labelKey)}
              title={tool.title}
              aria-pressed={tool.active === undefined ? undefined : tool.active}
              aria-disabled={tool.disabled ? true : undefined}
              aria-expanded={tool.palette ? palette === tool.palette : undefined}
              aria-controls={tool.palette && palette === tool.palette ? "editor-palette" : undefined}
              data-glyph={tool.cssGlyph ? tool.glyph : undefined}
              style={tool.palette ? ({ "--swatch": tool.swatch ?? "transparent" } as CSSProperties) : undefined}
              tabIndex={i === rove ? 0 : -1}
              onFocus={() => setRove(i)}
              // A mouse press must not take focus from the editor (it would drop
              // the selection the action is about); the keyboard acts on click.
              onMouseDown={(ev) => ev.preventDefault()}
              onClick={tool.disabled ? undefined : tool.run}
            >
              {tool.cssGlyph ? null : tool.glyph}
            </button>
          );
        })}
      </div>
      {palette ? (
        <div
          id="editor-palette"
          ref={paletteRef}
          className="editor-palette"
          role="group"
          aria-label={paletteLabel}
          onKeyDown={onPaletteKey}
          onBlur={onPaletteBlur}
        >
          {colours.map((c, i) => (
            <button
              key={c.hex}
              type="button"
              className="editor-swatch"
              aria-label={t(`compose.colour.${c.id}` as MessageKey)}
              title={t(`compose.colour.${c.id}` as MessageKey)}
              data-colour={c.hex}
              style={{ "--swatch": c.hex } as CSSProperties}
              tabIndex={i === paletteRove ? 0 : -1}
              onFocus={() => setPaletteRove(i)}
              onMouseDown={(ev) => ev.preventDefault()}
              onClick={() => applyColour(palette, c.hex)}
            />
          ))}
          <button
            type="button"
            className="editor-swatch-remove"
            tabIndex={colours.length === paletteRove ? 0 : -1}
            onFocus={() => setPaletteRove(colours.length)}
            onMouseDown={(ev) => ev.preventDefault()}
            onClick={() => applyColour(palette, null)}
          >
            {t("compose.removeColour")}
          </button>
        </div>
      ) : null}
      <EditorContent editor={editor} className="editor-body" />
      <input
        ref={imgRef}
        type="file"
        accept="image/*"
        className="editor-image-input"
        aria-hidden="true"
        tabIndex={-1}
        onChange={(ev) => onImageChosen(ev.target.files)}
      />
    </div>
  );
}
