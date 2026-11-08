// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// A DESTROYED EDITOR IS NOT READY. Tiptap can hand a component an instance it has
// already destroyed while it rebuilds one, and reading it throws; that crashed the
// reply composer when its editor was rebuilt as it opened (the accessibility lane
// caught it as "Cannot read properties of null (reading 'cached')").

import { describe, expect, it } from "vitest";
import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import { editorHtml, liveEditor } from "./editorHtml";

describe("liveEditor", () => {
  it("passes a live editor through", () => {
    const ed = new Editor({ extensions: [StarterKit], content: "<p>hi</p>" });
    expect(liveEditor(ed)).toBe(ed);
    expect(editorHtml(ed)).toContain("hi");
    ed.destroy();
  });

  it("refuses a destroyed one, which would throw if read", () => {
    const ed = new Editor({ extensions: [StarterKit], content: "<p>hi</p>" });
    ed.destroy();
    expect(liveEditor(ed)).toBeNull();
    // the hazard the guard exists for: reading it really throws
    expect(() => editorHtml(ed)).toThrow();
  });

  it("refuses null and undefined", () => {
    expect(liveEditor(null)).toBeNull();
    expect(liveEditor(undefined)).toBeNull();
  });
});
