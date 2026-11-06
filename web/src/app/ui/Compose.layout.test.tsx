// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE FIXED-SIZE BODY (#83, ADR-051 §5). The message body fills the composer
// between the fields above and the attachments and action bar below, at a fixed
// size, and scrolls inside itself; the toolbar stays at the top of the body and
// Send stays in view. jsdom does no layout, so this cannot measure pixels (the
// browser lane does: scripts/a11y-check.cjs). What it CAN do is mount the real
// composer with the real stylesheets applied and read the computed style of each
// link in the chain, which is where this broke before: one `display: block` or a
// missing `min-height: 0` anywhere in a flex chain makes a body grow without
// bound instead of scrolling.

import "./editor/jsdomShim";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";
import stylesCss from "../styles.css?raw";
import composeCss from "../compose.css?raw";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const identity = { id: "I1", accountId: "A1", email: "me@example.com", name: "Me", isWildcard: false };
const actions = {
  sendMessage: vi.fn(),
  discardDraft: vi.fn(async () => true),
  saveDraft: vi.fn(async () => null),
  cancelSubmission: vi.fn(async () => true),
  releaseAttachment: vi.fn(),
};
const mailState = { identities: { I1: identity }, mailboxes: {}, emails: {}, composeSnippets: [], session: null };
const client = {
  hasCapability: () => false,
  pgpSigning: () => null,
  pgpEncrypt: () => null,
  pgpKeyExpiryWarningDays: () => null,
  recipientKeyLookup: async () => null,
  contactForAddress: async () => null,
};
vi.mock("../state", () => ({
  useApp: () => ({ actions, client, pushToast: vi.fn(), offerUndo: vi.fn() }),
  useMailState: () => mailState,
}));
vi.mock("./useListVerbs", () => ({ requestPaneLeave: vi.fn() }));

import { Compose } from "./Compose";

let root: Root;
let host: HTMLElement;
let sheet: HTMLStyleElement;

beforeEach(() => {
  // The real stylesheets, applied to the page (textContent, not markup).
  sheet = document.createElement("style");
  sheet.textContent = `${stylesCss}\n${composeCss}`;
  document.head.appendChild(sheet);
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  sheet.remove();
});

async function mountNew(placement: "pane" | "main" | "full"): Promise<void> {
  await act(async () => {
    root.render(<Compose intent={{ kind: "new" }} placement={placement} scope={null} onClose={vi.fn()} />);
  });
  // The editor arrives as a separate chunk; wait for it.
  for (let i = 0; i < 50 && !host.querySelector(".ProseMirror"); i++) {
    await act(async () => {
      await new Promise((r) => setTimeout(r, 20));
    });
  }
}

const css = (el: Element, prop: string): string => getComputedStyle(el).getPropertyValue(prop);
const $ = (sel: string): HTMLElement => {
  const el = host.querySelector<HTMLElement>(sel);
  if (!el) throw new Error(`missing ${sel}`);
  return el;
};

describe("the composer body is a fixed-size, internally scrolling region", () => {
  for (const placement of ["pane", "main", "full"] as const) {
    it(`${placement}: every link in the flex chain lets the editor shrink and scroll`, async () => {
      await mountNew(placement);
      const form = $(".compose-form");
      const field = $(".field-grow");
      const editor = $(".editor");
      const surface = $(".editor-surface");

      // The form is the column; the body field and the editor grow inside it.
      expect(css(form, "display")).toBe("flex");
      expect(css(form, "flex-direction")).toBe("column");
      expect(css(field, "display")).toBe("flex");
      expect(css(field, "flex-direction")).toBe("column");
      expect(css(editor, "display")).toBe("flex");
      expect(css(editor, "flex-direction")).toBe("column");
      expect(css(editor, "flex-grow")).toBe("1");
      // The surface takes what is left and scrolls inside itself.
      expect(css(surface, "flex-grow")).toBe("1");
      expect(css(surface, "overflow-y")).toBe("auto");
      // min-height 0 is what lets a flex child be SMALLER than its content; without
      // it the surface grows to its content and nothing scrolls.
      expect(css($(".editor-body"), "min-height")).toBe("0px");
      expect(css(surface, "min-height")).toBe("0px");
      expect(css(field, "min-height")).toBe("0px");
      // A usable floor on a short window (the form scrolls instead).
      expect(css(editor, "min-height")).toMatch(/^(12rem|192px)$/); // jsdom resolves rem at 16px
    });
  }

  it("the toolbar keeps its height and sits above the surface in the editor", async () => {
    await mountNew("main");
    const editor = $(".editor");
    const children = Array.from(editor.children);
    const toolbar = $(".editor-toolbar");
    expect(children.indexOf(toolbar)).toBe(0);
    expect(children.indexOf($(".editor-body"))).toBeGreaterThan(0);
    // Not part of the scrolling region: the scroller is the surface, a sibling-level
    // descendant of .editor-body, not of the toolbar.
    expect(toolbar.contains($(".editor-surface"))).toBe(false);
    expect(css($(".editor-body"), "overflow-y")).not.toBe("auto");
  });

  it("the action row is pinned to the bottom of the scrolling form, and only the row", async () => {
    await mountNew("main");
    const form = $(".compose-form");
    const actions = $(".compose-actions");
    expect(actions.parentElement).toBe(form);
    expect(actions.contains($('button[type="submit"]'))).toBe(true);
    // After the body in document order, and pinned to the bottom edge of the form.
    expect(Boolean($(".field-grow").compareDocumentPosition(actions) & Node.DOCUMENT_POSITION_FOLLOWING)).toBe(true);
    expect(css(actions, "position")).toBe("sticky");
    expect(css(actions, "bottom")).toBe("0px");
    expect(css(actions, "flex-shrink")).toBe("0");
    // The attachment area, the send line and the encryption switch are NOT stuck: on
    // a short, narrow pane a stuck block taller than the form covered the recipient
    // field (measured at 844x390 in the browser lane).
    for (const sel of [".compose-tiles, .compose-attach, .compose-dropzone", ".compose-sendline"]) {
      const el = host.querySelector(sel);
      if (el) expect(css(el, "position")).not.toBe("sticky");
    }
    expect(host.querySelector(".compose-foot")).toBeNull();
  });

  it("plain mode fills the body field the same way", async () => {
    await mountNew("main");
    const toggle = $(".editor-mode-toggle");
    await act(async () => {
      toggle.click();
    });
    const ta = $("textarea.editor-plain");
    expect(ta.parentElement).toBe($(".field-grow"));
    expect(css(ta, "flex-grow")).toBe("1");
    expect(css(ta, "overflow-y")).toBe("auto");
    expect(css(ta, "resize")).toBe("none");
    expect(css(ta, "min-height")).toMatch(/^(12rem|192px)$/);
  });

  it("control: the assertions read real computed style (an unstyled editor fails them)", async () => {
    sheet.textContent = ""; // no stylesheet at all
    await mountNew("main");
    expect(css($(".editor-surface"), "overflow-y")).not.toBe("auto");
    expect(css($(".compose-actions"), "position")).not.toBe("sticky");
  });
});
