// SPDX-License-Identifier: AGPL-3.0-only

// Kept apart from RichTextEditor so the composer can read the editor's document
// without importing the editor (which is a separate chunk, ADR-051 §6).

import type { Editor } from "@tiptap/core";

/** The editor's HTML for the draft. An empty paragraph is written `<p><br></p>`:
 * mail clients draw a bare `<p></p>` with no height, which would lose the blank
 * lines the writer typed. (The serializer's output escapes all text, so a literal
 * empty-paragraph tag can only be an empty paragraph.) */
export function editorHtml(editor: Editor): string {
  return editor.getHTML().replace(/<p(\s[^>]*)?><\/p>/g, "<p$1><br></p>");
}
