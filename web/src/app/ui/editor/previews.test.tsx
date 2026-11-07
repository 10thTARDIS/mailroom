// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The picture node view: a cid: picture is DISPLAYED from the preview store (a
// data: URL, which the CSP allows) while the document keeps cid:.

import { afterEach, describe, expect, it } from "vitest";
import { act } from "react";
import { mountEditor } from "./editorTestkit";
import type { Mounted } from "./editorTestkit";
import { createPreviewStore } from "./previewStore";

const PNG = "data:image/png;base64,iVBORw0KGgo=";
let m: Mounted | null = null;
afterEach(async () => {
  await m?.unmount();
  m = null;
});

const imgs = (mm: Mounted): HTMLImageElement[] => Array.from(mm.surface.querySelectorAll("img:not([class])"));

describe("the picture node view", () => {
  it("has no src and shows the alt text until a preview arrives, then shows the data URL", async () => {
    const previews = createPreviewStore();
    m = await mountEditor('<p><img src="cid:a@x" alt="Logo"></p>', { previews });
    expect(imgs(m)).toHaveLength(1);
    expect(imgs(m)[0]!.hasAttribute("src")).toBe(false);
    expect(imgs(m)[0]!.getAttribute("alt")).toBe("Logo");
    await act(async () => previews.set("<A@x>", PNG));
    expect(imgs(m)[0]!.getAttribute("src")).toBe(PNG);
  });

  it("never puts a cid: src in the live DOM, and getHTML still says cid: with no data:", async () => {
    const previews = createPreviewStore();
    m = await mountEditor('<p><img src="cid:a@x" alt="Logo"></p>', { previews });
    await act(async () => previews.set("a@x", PNG));
    // ProseMirror's observer reads DOM changes on a timer: let it run. If the node
    // view's own src change were read back as an edit, the document would now say
    // data: instead of cid:.
    await act(async () => new Promise((r) => setTimeout(r, 60)));
    expect(m.surface.querySelectorAll('[src^="cid:"]')).toHaveLength(0);
    expect(m.html()).toContain('src="cid:a@x"');
    expect(m.html()).not.toContain("data:");
  });

  it("unsubscribes when the picture goes", async () => {
    let subs = 0;
    const inner = createPreviewStore();
    const previews = {
      ...inner,
      subscribe: (fn: (cid: string) => void) => {
        subs += 1;
        const off = inner.subscribe(fn);
        return () => {
          subs -= 1;
          off();
        };
      },
    };
    m = await mountEditor('<p><img src="cid:a@x" alt="Logo"></p>', { previews });
    expect(subs).toBe(1);
    await act(async () => {
      m!.editor.commands.setContent("<p>gone</p>");
    });
    expect(subs).toBe(0);
  });

  it("a data:image picture renders its own src", async () => {
    m = await mountEditor(`<p><img src="${PNG}" alt="x"></p>`, { previews: createPreviewStore() });
    expect(imgs(m)[0]!.getAttribute("src")).toBe(PNG);
  });

  it("without a store a cid: picture still has no src", async () => {
    m = await mountEditor('<p><img src="cid:a@x" alt="Logo"></p>');
    expect(imgs(m)[0]!.hasAttribute("src")).toBe(false);
  });
});
