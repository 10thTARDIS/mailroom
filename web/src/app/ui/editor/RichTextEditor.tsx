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
import type { JSX, KeyboardEvent } from "react";
import { EditorContent, useEditor, useEditorState } from "@tiptap/react";
import type { Editor } from "@tiptap/core";
import { t, type MessageKey } from "../../i18n";
import { buildEditorProps, buildExtensions } from "./extensions";
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
function chord(key: string, shift = false): string {
  return isMac() ? `${shift ? "⇧" : ""}⌘${key}` : `Ctrl+${shift ? "Shift+" : ""}${key}`;
}

/** The editor's HTML for the draft. An empty paragraph is written `<p><br></p>`:
 * mail clients draw a bare `<p></p>` with no height, which would lose the blank
 * lines the writer typed. (The serializer's output escapes all text, so a literal
 * empty-paragraph tag can only be an empty paragraph.) */
export function editorHtml(editor: Editor): string {
  return editor.getHTML().replace(/<p(\s[^>]*)?><\/p>/g, "<p$1><br></p>");
}

interface Tool {
  id: string;
  labelKey: MessageKey;
  title: string;
  glyph: string;
  cssGlyph?: boolean;
  active?: boolean;
  run: () => void;
}

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
  const [rove, setRove] = useState(0);

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
    selector: ({ editor: ed }) => ({
      bold: ed?.isActive("bold") ?? false,
      italic: ed?.isActive("italic") ?? false,
      underline: ed?.isActive("underline") ?? false,
      bulletList: ed?.isActive("bulletList") ?? false,
      orderedList: ed?.isActive("orderedList") ?? false,
    }),
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
  const tools: Tool[] = [
    {
      id: "bold",
      labelKey: "compose.bold",
      glyph: "B",
      title: `${t("compose.bold")} (${chord("B")})`,
      active: state?.bold,
      run: () => void chain()?.toggleBold().run(),
    },
    {
      id: "italic",
      labelKey: "compose.italic",
      glyph: "I",
      title: `${t("compose.italic")} (${chord("I")})`,
      active: state?.italic,
      run: () => void chain()?.toggleItalic().run(),
    },
    {
      id: "underline",
      labelKey: "compose.underline",
      glyph: "U",
      title: `${t("compose.underline")} (${chord("U")})`,
      active: state?.underline,
      run: () => void chain()?.toggleUnderline().run(),
    },
    {
      id: "bulletList",
      labelKey: "compose.bulletList",
      glyph: "•",
      title: `${t("compose.bulletList")} (${chord("8", true)})`,
      active: state?.bulletList,
      run: () => void chain()?.toggleBulletList().run(),
    },
    {
      id: "orderedList",
      labelKey: "compose.numberList",
      glyph: "1.",
      cssGlyph: true,
      title: `${t("compose.numberList")} (${chord("7", true)})`,
      active: state?.orderedList,
      run: () => void chain()?.toggleOrderedList().run(),
    },
    {
      id: "link",
      labelKey: "compose.link",
      glyph: "🔗",
      title: t("compose.link"),
      run: addLink,
    },
    {
      id: "image",
      labelKey: "compose.insertImage",
      glyph: "🖼",
      title: t("compose.insertImage"),
      run: () => imgRef.current?.click(),
    },
  ];

  const onToolbarKey = (ev: KeyboardEvent<HTMLDivElement>): void => {
    const n = tools.length;
    let next = -1;
    if (ev.key === "ArrowRight") next = (rove + 1) % n;
    else if (ev.key === "ArrowLeft") next = (rove - 1 + n) % n;
    else if (ev.key === "Home") next = 0;
    else if (ev.key === "End") next = n - 1;
    if (next < 0) return;
    ev.preventDefault();
    setRove(next);
    toolbarRef.current?.querySelectorAll<HTMLButtonElement>("button.editor-tool")[next]?.focus();
  };

  const onImageChosen = (files: FileList | null): void => {
    const file = files?.[0];
    if (imgRef.current) imgRef.current.value = "";
    if (file) hooks.current.insertImageFile(file, null);
  };

  // "1." is TEXT to axe (WCAG 2.5.3, label-content-name-mismatch in axe-core
  // 4.14): a name of "Numbered list" does not contain it. Painted from CSS
  // (.editor-tool[data-glyph]::before) it is decoration, not label content.

  return (
    <div className="editor">
      <div
        ref={toolbarRef}
        className="editor-toolbar"
        role="toolbar"
        aria-label={t("compose.formatting")}
        onKeyDown={onToolbarKey}
      >
        {tools.map((tool, i) => (
          <button
            key={tool.id}
            type="button"
            className="editor-tool"
            aria-label={t(tool.labelKey)}
            title={tool.title}
            aria-pressed={tool.active === undefined ? undefined : tool.active}
            data-glyph={tool.cssGlyph ? tool.glyph : undefined}
            tabIndex={i === rove ? 0 : -1}
            onFocus={() => setRove(i)}
            // A mouse press must not take focus from the editor (it would drop
            // the selection the action is about); the keyboard acts on click.
            onMouseDown={(ev) => ev.preventDefault()}
            onClick={tool.run}
          >
            {tool.cssGlyph ? null : tool.glyph}
          </button>
        ))}
      </div>
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
