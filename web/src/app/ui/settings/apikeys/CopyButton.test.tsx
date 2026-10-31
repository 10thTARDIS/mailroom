// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE COPY BUTTON ON A PLAIN-HTTP ORIGIN (#867). navigator.clipboard exists only
// in a secure context, and a dev instance can be plain http on a LAN, so the
// button used to say "Couldn't copy" every time. It now falls back to selecting a
// hidden textarea and execCommand("copy"), and says "Copied." only when that
// reports success. The textarea must live INSIDE the button's own wrapper: the
// token dialog is modal and everything outside it is inert.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { CopyButton } from "./SetupGuide";

let host: HTMLDivElement;
let root: Root;
const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));
const status = (): string => host.querySelector('[role="status"]')?.textContent ?? "";

function setClipboard(value: unknown): void {
  Object.defineProperty(navigator, "clipboard", { value, configurable: true });
}
function setExec(fn: unknown): void {
  Object.defineProperty(document, "execCommand", { value: fn, configurable: true, writable: true });
}

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  flushSync(() => root.render(<CopyButton text="secret-token" label="Copy" />));
});

afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
  setClipboard(undefined);
  vi.restoreAllMocks();
});

const click = async (): Promise<void> => {
  host.querySelector("button")!.click();
  await settle();
};

describe("CopyButton", () => {
  it("uses the Clipboard API when the browser has it", async () => {
    const writeText = vi.fn(async () => {});
    setClipboard({ writeText });
    const exec = vi.fn(() => true);
    setExec(exec);
    await click();
    expect(writeText).toHaveBeenCalledWith("secret-token");
    expect(exec).not.toHaveBeenCalled();
    expect(status()).toBe("Copied.");
  });

  it("falls back to a selection copy inside its own wrapper when there is no Clipboard API", async () => {
    setClipboard(undefined);
    let seenInside = false;
    let seenValue = "";
    const exec = vi.fn((cmd: string) => {
      const ta = host.querySelector("textarea");
      seenInside = ta !== null && host.querySelector(".copy-control")!.contains(ta) && ta.parentElement !== document.body;
      seenValue = ta?.value ?? "";
      return cmd === "copy";
    });
    setExec(exec);
    const button = host.querySelector("button")!;
    button.focus();
    await click();
    expect(exec).toHaveBeenCalledWith("copy");
    expect(seenInside).toBe(true);
    expect(seenValue).toBe("secret-token");
    expect(host.querySelector("textarea")).toBeNull();
    expect(document.activeElement).toBe(button);
    expect(status()).toBe("Copied.");
  });

  it("falls back when the Clipboard API rejects", async () => {
    setClipboard({ writeText: vi.fn(async () => { throw new Error("denied"); }) });
    setExec(vi.fn(() => true));
    await click();
    expect(status()).toBe("Copied.");
  });

  it("says it could not copy when the selection copy reports failure or throws", async () => {
    setClipboard(undefined);
    setExec(vi.fn(() => false));
    await click();
    expect(status()).toContain("Couldn't copy");
    expect(host.querySelector("textarea")).toBeNull();

    setExec(vi.fn(() => { throw new Error("nope"); }));
    await click();
    expect(status()).toContain("Couldn't copy");
    expect(host.querySelector("textarea")).toBeNull();
  });
});
