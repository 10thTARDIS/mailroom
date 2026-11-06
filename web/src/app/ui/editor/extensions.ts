// SPDX-License-Identifier: AGPL-3.0-only

// The editor's schema and behaviour (ADR-051). Beta 1 kept exactly the features
// the execCommand editor had (bold, italic, underline, two kinds of list, links,
// inline pictures) plus the block quote a reply quote needs. Beta 2 adds the new
// formatting: strike-through, subscript, superscript, inline code, code blocks,
// text and highlight colour, font family and size (each only from the fixed
// lists in ./styleValues.ts) and paragraph indentation. Everything else
// StarterKit ships stays switched off (headings, rules, gap cursor), so the schema,
// and therefore what a paste can become, is no wider than what the toolbar offers.

import { Extension } from "@tiptap/core";
import type { Editor } from "@tiptap/core";
import { FileHandler } from "@tiptap/extension-file-handler";
import Image from "@tiptap/extension-image";
import Subscript from "@tiptap/extension-subscript";
import Superscript from "@tiptap/extension-superscript";
import StarterKit from "@tiptap/starter-kit";
import type { EditorProps } from "@tiptap/pm/view";
import { Indent } from "./indent";
import { countDroppedImages } from "./paste";
import { TEXT_STYLE_EXTENSIONS } from "./textStyles";
import { isAllowedLinkUri, isInlineImageSrc } from "./uri";

/** The only inline style an image carries. It is a constant, never read from
 * pasted markup, so a hostile `style` on an `<img>` goes nowhere. */
const IMAGE_STYLE = "max-width:100%";

/** Raster image types that go through the upload path when pasted or dropped as
 * files. SVG is left out: it is a document, not a picture. */
export const IMAGE_FILE_TYPES = ["image/png", "image/jpeg", "image/gif", "image/webp"];

export interface EditorHooks {
  /** Uploads a dropped or pasted image file and inserts it as a cid: image (at pos, or at the selection when null). */
  insertImageFile: (file: File, pos: number | null) => void;
  /** Called once per paste that dropped pictures from the web. */
  remoteImagesDropped: (count: number) => void;
}

/** StarterKit with the Link mark cut down to `href`. A pasted anchor's class,
 * target, rel and title are not parsed (so they cannot be kept) and not rendered:
 * the server decides rel and target when the message is sent. Done by wrapping the
 * kit's own link extension, which avoids depending on its package directly. */
const Kit = StarterKit.extend({
  addExtensions() {
    return (this.parent?.() ?? []).map((ext) =>
      ext.name === "link"
        ? ext.extend({
            addAttributes() {
              return {
                href: {
                  default: null,
                  parseHTML: (el: HTMLElement) => el.getAttribute("href"),
                  renderHTML: (attrs: Record<string, unknown>) => (attrs["href"] ? { href: attrs["href"] } : {}),
                },
              };
            },
          })
        : ext,
    );
  },
});

export function buildExtensions(hooks: EditorHooks) {
  return [
    Kit.configure({
      heading: false,
      // A code block keeps its text and nothing else: no `language-` class is
      // read from pasted markup or written back (the output is inline styles
      // only, no classes).
      codeBlock: { languageClassPrefix: null },
      horizontalRule: false,
      gapcursor: false,
      trailingNode: false,
      link: {
        // No link is made by typing or pasting a bare URL, as before; only the
        // toolbar button (or an existing link in a draft) makes one.
        autolink: false,
        linkOnPaste: false,
        openOnClick: false,
        isAllowedUri: (url) => isAllowedLinkUri(url),
        // Only href is kept (see Kit above); nothing else is rendered either, so
        // the library's default target and rel are switched off. The server
        // decides rel and target when the message is sent.
        HTMLAttributes: { target: null, rel: null, class: null },
      },
    }),
    Subscript,
    Superscript,
    ...TEXT_STYLE_EXTENSIONS,
    Indent,
    Image.extend({
      // EVERY parse path (paste, drop, the seed, insertContent) goes through this
      // rule, so a picture that is not inline is refused wherever it came from, not
      // only on paste. The server's sanitiser strips them on send as well.
      parseHTML() {
        return [
          {
            tag: "img[src]",
            getAttrs: (dom) => (isInlineImageSrc((dom as HTMLElement).getAttribute("src")) ? null : false),
          },
        ];
      },
      // Only src, alt and the fixed style. width, height and title are not kept
      // (title is hover text, not an alternative to alt), so pasted markup cannot
      // size or label a picture.
      addAttributes() {
        return {
          src: { default: null },
          alt: { default: null },
          style: {
            default: IMAGE_STYLE,
            parseHTML: () => IMAGE_STYLE,
            renderHTML: () => ({ style: IMAGE_STYLE }),
          },
        };
      },
    }).configure({ inline: true, allowBase64: true, HTMLAttributes: {} }),
    FileHandler.configure({
      allowedMimeTypes: IMAGE_FILE_TYPES,
      onDrop: (_editor: Editor, files: File[], pos: number) => {
        for (const f of files) hooks.insertImageFile(f, pos);
      },
      onPaste: (_editor: Editor, files: File[], html?: string) => {
        // A copy from a web page carries the picture as a file AND as markup that
        // points at the remote original. The markup path (which drops the remote
        // picture and says so) owns that paste; only a bare image, such as a
        // screenshot, is uploaded.
        if (html && html.length > 0) return;
        for (const f of files) hooks.insertImageFile(f, null);
      },
    }),
    // Ctrl/Cmd+Enter SENDS from the composer (Compose.tsx's form handler). The
    // hard-break extension binds the same chord to a line break, so it is claimed
    // here, ahead of it, and the keystroke is left to the form.
    Extension.create({
      name: "sendChordPassthrough",
      priority: 1000,
      addKeyboardShortcuts() {
        return { "Mod-Enter": () => true };
      },
    }),
  ];
}

/** The ProseMirror props shared by every editor instance. */
export function buildEditorProps(
  hooks: EditorHooks,
  attributes: Record<string, string>,
): EditorProps {
  return {
    attributes,
    // Rich paste keeps formatting the schema knows and drops the rest. Pictures
    // that are not inline are refused by the Image parse rule; this only counts
    // them in the pasted markup (parsed in an inert document) so the user is told.
    // Mod-Shift-V is ProseMirror's own "paste as plain text".
    transformPastedHTML: (html) => {
      const dropped = countDroppedImages(html);
      if (dropped > 0) hooks.remoteImagesDropped(dropped);
      return html;
    },
    handleDOMEvents: {
      // A paste during IME composition is not handled by ProseMirror (it returns
      // early while composing), so the browser would paste natively, as markup,
      // into the live surface. Refuse it.
      paste: (view, event) => {
        if (!view.composing) return false;
        event.preventDefault();
        return true;
      },
      // Tab leaves the editor, as it did when this was a plain contentEditable
      // (operator decision, ADR-051 beta 2): the body never traps the keyboard.
      // The list-item extension would otherwise take Tab to nest an item; nesting
      // is on Mod-] and Mod-[ and the toolbar instead (./indent.ts). Returning true
      // tells ProseMirror not to handle the key; it does NOT cancel it, so the
      // browser moves focus.
      keydown: (_view, event) => event.key === "Tab" && !event.ctrlKey && !event.metaKey && !event.altKey,
    },
  };
}
