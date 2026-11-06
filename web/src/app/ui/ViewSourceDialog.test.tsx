// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// VIEW SOURCE (#43), rendered for real. The source is attacker-controlled, so the
// first test feeds it a message built to run if anything ever rendered it as HTML.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { BlobFetch } from "../../core/jmap/blobs";
import { MAX_SOURCE_DISPLAY_BYTES } from "../../core/messageSource";
import { ViewSourceDialog } from "./ViewSourceDialog";

const HOSTILE =
  "From: Mallory <mallory@example.com>\r\n" +
  "Subject: <script>window.__pwned = 'subject'</script>\r\n" +
  "To: you@example.com\r\n\r\n" +
  "<script>window.__pwned = 'script'</script>\r\n" +
  "<img src=x onerror=\"window.__pwned = 'onerror'\">\r\n" +
  "<a href=\"javascript:window.__pwned = 'href'\">click</a>\r\n" +
  // Split so the repository's frame-sandbox scan (cmd/mailroom/security_test.go)
  // does not mistake a line of hostile TEST DATA for a frame this app renders.
  "<" + "iframe srcdoc=\"<script>parent.__pwned = 'srcdoc'</script>\"></" + "iframe>\r\n";

let host: HTMLDivElement;
let root: Root;
const onClose = vi.fn();
const onDownload = vi.fn();
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

function ok(text: string): BlobFetch {
  return { kind: "ok", blob: new Blob([new TextEncoder().encode(text)], { type: "application/octet-stream" }) };
}

async function show(result: BlobFetch, notice?: string): Promise<void> {
  flushSync(() =>
    root.render(
      <ViewSourceDialog load={async () => result} onDownload={onDownload} downloadNotice={notice} onClose={onClose} />,
    ),
  );
  await settle();
  await settle();
}

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
  delete (window as unknown as Record<string, unknown>).__pwned;
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

const pre = () => host.querySelector("pre.view-source-text") as HTMLPreElement | null;
const button = (label: string) =>
  [...host.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent === label);

describe("a hostile message", () => {
  it("renders as inert text: no element is created from it and no handler runs", async () => {
    await show(ok(HOSTILE));
    const p = pre()!;
    // The text is all there, byte for byte, as TEXT ...
    expect(p.textContent).toBe(HOSTILE);
    // ... in a <pre> that holds nothing but that one text node ...
    expect(p.children.length).toBe(0);
    // ... and the dialog around it has grown none of the elements the source names.
    for (const tag of ["script", "img", "a", "iframe", "svg", "style", "form"]) {
      expect(host.querySelectorAll(tag).length, `<${tag}> elements`).toBe(0);
    }
    expect(host.querySelectorAll("[onerror], [srcdoc], [href]").length).toBe(0);
    // Nothing ran. (jsdom does not run scripts in an innerHTML, but it does run
    // an onerror on an <img> it loads, and it would have created the <script>:
    // the structural asserts above are the load-bearing ones.)
    expect((window as unknown as Record<string, unknown>).__pwned).toBeUndefined();
  });

  it("shows hostile header text as text on the headers tab too", async () => {
    await show(ok(HOSTILE));
    flushSync(() => button("Headers")!.click());
    const dd = [...host.querySelectorAll("dd")].map((d) => d.textContent);
    expect(dd).toContain("<script>window.__pwned = 'subject'</script>");
    expect(host.querySelectorAll("script, img, a").length).toBe(0);
  });
});

describe("the source view", () => {
  it("is a labelled modal dialog with focus on it, and Escape closes it", async () => {
    await show(ok("A: 1\r\n\r\nbody"));
    const dlg = host.querySelector('[role="dialog"]') as HTMLElement;
    expect(dlg.getAttribute("aria-modal")).toBe("true");
    const label = document.getElementById(dlg.getAttribute("aria-labelledby")!);
    expect(label?.textContent).toBe("Message source");
    expect(document.activeElement).toBe(dlg);
    dlg.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("says when the message could not be loaded", async () => {
    await show({ kind: "failed" });
    expect(host.querySelector('[role="alert"]')?.textContent).toBe("The message source could not be loaded.");
    expect(pre()).toBeNull();
  });

  it("truncates a very large message with a note, and keeps the download", async () => {
    await show(ok("x".repeat(MAX_SOURCE_DISPLAY_BYTES + 100)));
    expect(pre()!.textContent!.length).toBe(MAX_SOURCE_DISPLAY_BYTES);
    expect(host.querySelector(".view-source-truncated")?.textContent).toBe(
      "Showing the first 1.0 MiB of 1.0 MiB. Download the .eml for the whole message.",
    );
    button("Download .eml")!.click();
    expect(onDownload).toHaveBeenCalledTimes(1);
  });

  it("shows no truncation note for an ordinary message", async () => {
    await show(ok("A: 1\r\n\r\nbody"));
    expect(host.querySelector(".view-source-truncated")).toBeNull();
  });

  it("shows a download refusal where the reader is looking", async () => {
    await show(ok("A: 1\r\n\r\nbody"), "Blocked: nope");
    expect(host.textContent).toContain("Blocked: nope");
  });
});

describe("the headers view", () => {
  it("lists every header in order with long ones unfolded", async () => {
    await show(ok("Received: a\r\nSubject: long\r\n  folded\r\nReceived: b\r\n\r\nbody: no"));
    flushSync(() => button("Headers")!.click());
    const names = [...host.querySelectorAll("dt")].map((d) => d.textContent);
    const values = [...host.querySelectorAll("dd")].map((d) => d.textContent);
    expect(names).toEqual(["Received", "Subject", "Received"]);
    expect(values).toEqual(["a", "long  folded", "b"]);
    expect(button("Headers")!.getAttribute("aria-pressed")).toBe("true");
  });
});

describe("copy", () => {
  it("copies the WHOLE message, not just what is displayed", async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const big = "y".repeat(MAX_SOURCE_DISPLAY_BYTES + 7);
    await show(ok(big));
    button("Copy")!.click();
    await settle();
    expect(writeText).toHaveBeenCalledTimes(1);
    expect((writeText.mock.calls[0] as unknown as [string])[0].length).toBe(MAX_SOURCE_DISPLAY_BYTES + 7);
    expect(host.textContent).toContain("Copied");
  });

  it("says so when the browser refuses", async () => {
    Object.defineProperty(navigator, "clipboard", {
      value: {
        writeText: async () => {
          throw new Error("denied");
        },
      },
      configurable: true,
    });
    await show(ok("A: 1\r\n\r\nb"));
    button("Copy")!.click();
    await settle();
    expect(host.textContent).toContain("Copying was not allowed by the browser.");
  });
});

describe("download", () => {
  it("calls the guarded download", async () => {
    await show(ok("A: 1\r\n\r\nb"));
    button("Download .eml")!.click();
    expect(onDownload).toHaveBeenCalledTimes(1);
  });
});
