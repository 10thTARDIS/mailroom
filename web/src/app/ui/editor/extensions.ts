// SPDX-License-Identifier: AGPL-3.0-only

// The editor's schema and behaviour (ADR-051). Beta 1 keeps exactly the features
// the execCommand editor had (bold, italic, underline, two kinds of list, links,
// inline pictures) plus the block quote a reply quote needs; everything else
// StarterKit ships is switched off, so the schema, and therefore what a paste can
// become, is no wider than what the toolbar offers. Beta 2 turns more on.

import { Extension } from "@tiptap/core";
import type { Editor } from "@tiptap/core";
import { FileHandler } from "@tiptap/extension-file-handler";
import Image from "@tiptap/extension-image";
import StarterKit from "@tiptap/starter-kit";
import type { EditorProps } from "@tiptap/pm/view";
import { dropRemoteImages } from "./paste";
import { isAllowedLinkUri } from "./uri";

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

export function buildExtensions(hooks: EditorHooks) {
  return [
    StarterKit.configure({
      heading: false,
      code: false,
      codeBlock: false,
      strike: false,
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
        // Bare anchors, as the old editor made: rel and target are applied when
        // the message is sent (the server decides them).
        HTMLAttributes: { target: null, rel: null },
      },
    }),
    Image.extend({
      addAttributes() {
        return {
          ...this.parent?.(),
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
    // Rich paste keeps formatting the schema knows and drops the rest; the one
    // thing it also drops is a picture that is not inline (ADR-051 §4). Mod-Shift-V
    // is ProseMirror's own "paste as plain text".
    transformPasted: (slice) => {
      const { slice: kept, dropped } = dropRemoteImages(slice);
      if (dropped > 0) hooks.remoteImagesDropped(dropped);
      return kept;
    },
    handleDOMEvents: {
      // Tab leaves the editor, as it did when this was a plain contentEditable.
      // (The list-item extension would otherwise take Tab to indent; indenting
      // arrives with the new formatting in beta 2.) Returning true tells
      // ProseMirror not to handle the key; it does NOT cancel it, so the browser
      // moves focus.
      keydown: (_view, event) => event.key === "Tab" && !event.ctrlKey && !event.metaKey && !event.altKey,
    },
  };
}
