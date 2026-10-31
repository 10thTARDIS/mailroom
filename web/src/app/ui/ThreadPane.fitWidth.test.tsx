// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// A WIDE NEWSLETTER IS SCALED TO A PHONE'S SCREEN (#801). The sandboxed frame's
// hash-pinned reporter says how wide its document naturally is; on a phone, when
// that is more than the frame may take, HtmlBody lays the frame out at the
// document's own width and scales it down to fit, so the newsletter has nothing to
// scroll inside its box. jsdom does no layout, so the width the card allows is
// stood in for (`avail`), and the reporter's message is posted by hand — from the
// frame's own window, because the parent accepts nothing else. The real frame is
// measured in a browser by scripts/a11y-check.cjs.
//
// What must hold: scale only on a phone, only when the document is wider than the
// room, never for a message that reports nothing, a report from anywhere but the
// frame is ignored, and the sandbox and CSP are exactly what they were.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

vi.mock("../state", () => ({
  useApp: () => ({
    client: { fetchInlineImage: vi.fn(), imageProxyEnabled: () => false, blobUrl: () => "", fetchProxiedImage: vi.fn() },
    actions: new Proxy({}, { get: () => vi.fn(async () => true) }),
  }),
  useMailState: () => ({ remoteAllows: {} }),
}));
vi.mock("./shellContext", () => ({ useShell: () => ({ openViewer: vi.fn() }) }));

import { HtmlBody } from "./ThreadPane";
import { MESSAGE_SANDBOX } from "./messageDocument";

let host: HTMLDivElement;
let root: Root;
let avail = 375;
let phone = true;
const realRO = globalThis.ResizeObserver;
const realMatchMedia = window.matchMedia;
const realClient = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  globalThis.ResizeObserver = class {
    observe(): void {}
    disconnect(): void {}
    unobserve(): void {}
  } as unknown as typeof ResizeObserver;
  window.matchMedia = ((q: string) => ({
    matches: phone,
    media: q,
    addEventListener: () => {},
    removeEventListener: () => {},
  })) as unknown as typeof window.matchMedia;
  // The frame's parent is the box around it: it is what the room is measured from.
  Object.defineProperty(HTMLElement.prototype, "clientWidth", {
    configurable: true,
    get(this: HTMLElement) {
      return this.classList.contains("message-html-box") ? avail : 0;
    },
  });
});

afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
  globalThis.ResizeObserver = realRO;
  window.matchMedia = realMatchMedia;
  if (realClient) Object.defineProperty(HTMLElement.prototype, "clientWidth", realClient);
});

function mount(): HTMLIFrameElement {
  flushSync(() =>
    root.render(
      <HtmlBody
        html="<p>hi</p>"
        title="A newsletter"
        htmlParts={undefined}
        attachments={undefined}
        scan={null}
        senderAddress="news@example.com"
        messageId="M1"
      />,
    ),
  );
  return host.querySelector("iframe") as HTMLIFrameElement;
}

/** What the reporter posts, delivered as if from `source`. */
function report(frame: HTMLIFrameElement, data: unknown, source: Window | null = frame.contentWindow): void {
  flushSync(() => {
    window.dispatchEvent(new MessageEvent("message", { data, source }));
  });
}

describe("a wide newsletter on a phone (#801)", () => {
  it("is laid out at its own width and scaled to fit the room", () => {
    const frame = mount();
    report(frame, { type: "mailroom-height", height: 900, width: 750 });
    expect(frame.style.width).toBe("750px");
    expect(frame.style.transform).toBe(`scale(${375 / 750})`);
    expect(frame.style.height).toBe("900px");
    // The box holds the room the scaled frame takes up, not the frame's own height.
    const box = host.querySelector<HTMLElement>(".message-html-box");
    expect(box?.style.height).toBe(`${Math.ceil((900 + 2) * 0.5)}px`);
  });

  it("leaves a document that fits alone", () => {
    const frame = mount();
    report(frame, { type: "mailroom-height", height: 900, width: 375 });
    expect(frame.style.transform).toBe("");
    expect(frame.style.width).toBe("");
  });

  it("leaves a message that reports no width alone", () => {
    const frame = mount();
    report(frame, { type: "mailroom-height", height: 900 });
    expect(frame.style.transform).toBe("");
    expect(frame.style.height).toBe("900px");
  });

  it("does not scale off a phone", () => {
    phone = false;
    try {
      const frame = mount();
      report(frame, { type: "mailroom-height", height: 900, width: 750 });
      expect(frame.style.transform).toBe("");
      expect(frame.style.width).toBe("");
    } finally {
      phone = true;
    }
  });

  it("does not scale past the point where the text would be unreadable", () => {
    const frame = mount();
    report(frame, { type: "mailroom-height", height: 900, width: 3000 });
    expect(frame.style.transform).toBe("");
  });

  it("ignores a report that does not come from its own frame", () => {
    const frame = mount();
    report(frame, { type: "mailroom-height", height: 900, width: 750 }, window);
    expect(frame.style.transform).toBe("");
    expect(frame.style.height).toBe("");
  });

  it("keeps the sandbox and the policy exactly as strict", () => {
    const frame = mount();
    expect(frame.getAttribute("sandbox")).toBe(MESSAGE_SANDBOX);
    expect(frame.getAttribute("sandbox")).not.toContain("allow-same-origin");
    expect(frame.getAttribute("title")).toBe("A newsletter");
    const doc = frame.getAttribute("srcdoc") ?? "";
    expect(doc).toContain("default-src 'none'");
    expect(doc).toContain("img-src data:");
  });
});
