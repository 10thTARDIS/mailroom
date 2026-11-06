// SPDX-License-Identifier: AGPL-3.0-only

// Shared by the editor's tests: mount the real editor in jsdom, drive it with
// real DOM events (a keydown, a paste), and read its document back. Nothing here
// mocks Tiptap or ProseMirror; the point of the suites is the real parser.

import "./jsdomShim";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";
import type { Editor } from "@tiptap/core";
import RichTextEditor from "./RichTextEditor";
import type { RichTextEditorProps } from "./RichTextEditor";
import { editorHtml } from "./RichTextEditor";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

export interface Mounted {
  host: HTMLElement;
  editor: Editor;
  changes: string[];
  notices: string[];
  uploads: File[];
  /** The element the user types into. */
  surface: HTMLElement;
  /** The document as the draft would carry it. */
  html: () => string;
  unmount: () => Promise<void>;
}

export async function mountEditor(
  initialHTML = "",
  extra: Partial<RichTextEditorProps> = {},
): Promise<Mounted> {
  const host = document.createElement("div");
  document.body.appendChild(host);
  const root: Root = createRoot(host);
  const changes: string[] = [];
  const notices: string[] = [];
  const uploads: File[] = [];
  let editor: Editor | null = null;
  await act(async () => {
    root.render(
      <RichTextEditor
        initialHTML={initialHTML}
        onChange={(h) => changes.push(h)}
        onImageFile={async (f) => {
          uploads.push(f);
          return `img-${uploads.length}@inline.mailroom`;
        }}
        onNotice={(m) => notices.push(m)}
        onReady={(e) => {
          editor = e;
        }}
        label="Message"
        {...extra}
      />,
    );
  });
  if (!editor) throw new Error("editor did not mount");
  const ed = editor as Editor;
  const surface = host.querySelector<HTMLElement>(".ProseMirror");
  if (!surface) throw new Error("no editing surface");
  return {
    host,
    editor: ed,
    changes,
    notices,
    uploads,
    surface,
    html: () => editorHtml(ed),
    unmount: async () => {
      await act(async () => root.unmount());
      host.remove();
    },
  };
}

export interface PasteData {
  html?: string;
  text?: string;
  files?: File[];
}

/** A real `paste` event with a clipboard carrying `data`, sent to the surface. */
export async function paste(m: Mounted, data: PasteData): Promise<Event> {
  const ev = new Event("paste", { bubbles: true, cancelable: true });
  const types: string[] = [];
  if (data.html !== undefined) types.push("text/html");
  if (data.text !== undefined) types.push("text/plain");
  Object.defineProperty(ev, "clipboardData", {
    value: {
      types,
      files: data.files ?? [],
      items: [],
      getData: (t: string) => (t === "text/html" ? data.html ?? "" : t === "text/plain" ? data.text ?? "" : ""),
    },
  });
  await act(async () => {
    m.surface.dispatchEvent(ev);
  });
  return ev;
}

export interface KeyInit {
  key: string;
  keyCode?: number;
  ctrlKey?: boolean;
  shiftKey?: boolean;
  metaKey?: boolean;
}

/** A keydown on the surface; the event, so a test can see if it was cancelled. */
export async function press(m: Mounted, init: KeyInit): Promise<KeyboardEvent> {
  const ev = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, ...init });
  await act(async () => {
    m.surface.dispatchEvent(ev);
  });
  return ev;
}

export function button(m: Mounted, name: string): HTMLButtonElement {
  const b = m.host.querySelector<HTMLButtonElement>(`button[aria-label="${name}"]`);
  if (!b) throw new Error(`no toolbar button ${name}`);
  return b;
}

export async function click(el: HTMLElement): Promise<void> {
  await act(async () => {
    el.click();
  });
}
