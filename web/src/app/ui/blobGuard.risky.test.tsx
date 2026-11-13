// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE QUESTION BEFORE A RISKY DOWNLOAD (ADR-056, operator decision Q81).
//
// "The dialog appeared" is also what a test that renders the wrong thing sees, so
// the controls are explicit: an ordinary file downloads with no question, and a
// risky one downloads only after "Download anyway". Cancel is observed as: no
// probe, no anchor click.

import axe from "axe-core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { BlobProbe } from "../../core/jmap/blobs";
import type { EmailBodyPart } from "../../core/jmap/types";

const probeBlob = vi.fn(async (_url: string): Promise<BlobProbe> => ({ kind: "ok" }));
const client = {
  probeBlob,
  fetchPart: vi.fn(),
  blobUrl: (blobId: string) => `/jmap/download/A/${blobId}`,
  previewUrl: (blobId: string) => `/jmap/preview/A/${blobId}`,
};
vi.mock("../state", () => ({
  useApp: () => ({ client, actions: {} }),
  useMailState: () => ({ remoteAllows: {} }),
}));
vi.mock("./shellContext", () => ({
  useShell: () => ({ openViewer: vi.fn() }),
}));

import { AttachmentList } from "./ThreadPane";

function part(partId: string, type: string, name: string, riskyType?: string | null): EmailBodyPart {
  return {
    partId,
    blobId: `B.${partId}`,
    size: 2048,
    name,
    type,
    charset: null,
    disposition: "attachment",
    cid: null,
    riskyType,
  };
}

let host: HTMLDivElement;
let root: Root;
let clicked: string[];
const userClick = (el: HTMLElement): void => {
  el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
};
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));
const dialog = (): HTMLElement | null => host.querySelector<HTMLElement>('[role="alertdialog"]');
const button = (label: string): HTMLButtonElement =>
  Array.from(host.querySelectorAll("button")).find((b) => b.textContent === label) as HTMLButtonElement;

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  probeBlob.mockClear();
  clicked = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    clicked.push(this.getAttribute("href") ?? "");
  });
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
  vi.restoreAllMocks();
});

function render(p: EmailBodyPart): HTMLAnchorElement {
  flushSync(() => {
    root.render(<AttachmentList messageId="M1" threadId="T1" attachments={[p]} scan={null} />);
  });
  const link = host.querySelector<HTMLAnchorElement>("a.attachment");
  expect(link).not.toBeNull();
  return link as HTMLAnchorElement;
}

describe("a risky download asks first (ADR-056)", () => {
  it("shows the dialog for a risky file, with Cancel focused, and downloads nothing yet", async () => {
    userClick(render(part("2", "application/octet-stream", "setup.exe", "program")));
    await settle();
    expect(dialog()).not.toBeNull();
    expect(dialog()?.textContent).toContain("This is a program file. Only open it if you expected it from someone you trust.");
    expect(document.activeElement).toBe(button("Cancel"));
    expect(button("Download anyway")).toBeDefined();
    expect(probeBlob).not.toHaveBeenCalled();
    expect(clicked).toEqual([]);
  });

  it("downloads nothing on Cancel, and Escape cancels too", async () => {
    userClick(render(part("2", "application/octet-stream", "setup.exe", "program")));
    await settle();
    userClick(button("Cancel"));
    await settle();
    expect(dialog()).toBeNull();
    expect(probeBlob).not.toHaveBeenCalled();
    expect(clicked).toEqual([]);

    userClick(host.querySelector("a.attachment") as HTMLElement);
    await settle();
    expect(dialog()).not.toBeNull();
    dialog()!.parentElement!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await settle();
    expect(dialog()).toBeNull();
    expect(clicked).toEqual([]);
  });

  it("Escape cancels the question WITHOUT also reaching the shell, which would close the open message", async () => {
    // The shell's Escape is a window-level listener; React's root sits inside
    // window, so an unstopped keystroke arrives there after the dialog has acted.
    const reachedShell = vi.fn();
    window.addEventListener("keydown", reachedShell);
    try {
      userClick(render(part("2", "application/octet-stream", "setup.exe", "program")));
      await settle();
      dialog()!.parentElement!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
      await settle();
      expect(dialog()).toBeNull();
      expect(reachedShell).not.toHaveBeenCalled();

      // CONTROL: the listener does see an Escape that nothing stops, so
      // "not called" above is not a listener that could never fire.
      host.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
      expect(reachedShell).toHaveBeenCalledTimes(1);
    } finally {
      window.removeEventListener("keydown", reachedShell);
    }
  });

  it("downloads on Download anyway", async () => {
    userClick(render(part("2", "application/octet-stream", "setup.exe", "program")));
    await settle();
    userClick(button("Download anyway"));
    await settle();
    expect(dialog()).toBeNull();
    expect(probeBlob).toHaveBeenCalledTimes(1);
    expect(clicked).toEqual(["/jmap/download/A/B.2"]);
  });

  it("CONTROL: an ordinary file downloads at once, with no question", async () => {
    userClick(render(part("2", "application/zip", "archive.zip", null)));
    await settle();
    expect(dialog()).toBeNull();
    expect(clicked).toEqual(["/jmap/download/A/B.2"]);
  });

  it("CONTROL: a PDF and an image (no riskyType) do not ask", async () => {
    const cases: [string, string][] = [
      ["application/pdf", "report.pdf"],
      ["image/png", "photo.png"],
    ];
    for (const [type, name] of cases) {
      flushSync(() => {
        root.render(<AttachmentList messageId="M1" threadId="T1" attachments={[part("2", type, name)]} scan={null} />);
      });
      const dl = host.querySelector<HTMLAnchorElement>("a.attachment-download, a.attachment");
      userClick(dl as HTMLElement);
      await settle();
      expect(dialog(), name).toBeNull();
    }
  });

  it("uses a different sentence for a disguised file and the generic one for an unknown reason", async () => {
    userClick(render(part("2", "application/zip", "invoice.pdf.exe", "disguised")));
    await settle();
    expect(dialog()?.textContent).toContain("not what its name suggests");
    userClick(button("Cancel"));
    userClick(render(part("2", "application/octet-stream", "x.bin", "something-new")));
    await settle();
    expect(dialog()?.textContent).toContain("This type of file can run programs");
  });

  it("is accessible under axe, and the control proves axe can see a defect", async () => {
    userClick(render(part("2", "application/octet-stream", "setup.exe", "program")));
    await settle();
    const clean = await axe.run(dialog()!.parentElement!, { rules: { "color-contrast": { enabled: false } } });
    expect(clean.violations.map((v) => v.id)).toEqual([]);

    const bad = document.createElement("div");
    const unnamed = document.createElement("button");
    unnamed.type = "button";
    bad.appendChild(unnamed);
    document.body.appendChild(bad);
    const res = await axe.run(bad);
    bad.remove();
    expect(res.violations.map((v) => v.id)).toContain("button-name");
  });
});
