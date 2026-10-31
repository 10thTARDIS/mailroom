// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE IMPORT-A-KEY DIALOG (#47, ADR-038), rendered for real: nothing is stored
// until the user has SEEN the fingerprint (in fours) and the user ID, each
// refusal is a plain sentence naming what to do, and the key that is imported is
// the key that was checked.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { ImportKeyOutcome } from "../../core/keyImport";
import { ImportKeyDialog } from "./ImportKeyDialog";

const FPR = "0123456789ABCDEF0123456789ABCDEF01234567";
const KEY = "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nabc\n-----END PGP PUBLIC KEY BLOCK-----";

let host: HTMLDivElement;
let root: Root;
const check = vi.fn(async (..._a: unknown[]): Promise<ImportKeyOutcome> => ({
  ok: true,
  fingerprint: FPR,
  userId: "Bob Example <bob@example.com>",
}));
const onImport = vi.fn(async (..._a: unknown[]): Promise<ImportKeyOutcome> => ({
  ok: true,
  fingerprint: FPR,
  userId: "Bob Example <bob@example.com>",
}));
const onImported = vi.fn();
const onClose = vi.fn();
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
  flushSync(() =>
    root.render(
      <ImportKeyDialog
        address="bob@example.com"
        check={check}
        onImport={onImport}
        onImported={onImported}
        onClose={onClose}
      />,
    ),
  );
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

const text = () => host.textContent ?? "";
const textarea = () => host.querySelector("textarea") as HTMLTextAreaElement;
const button = (label: string) =>
  [...host.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent === label);

function paste(value: string): void {
  const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
  flushSync(() => {
    setter.call(textarea(), value);
    textarea().dispatchEvent(new Event("input", { bubbles: true }));
  });
}

describe("the dialog", () => {
  it("is a labelled modal dialog with a labelled text box and file picker, focus in the text box", () => {
    const dlg = host.querySelector('[role="dialog"]')!;
    expect(dlg.getAttribute("aria-modal")).toBe("true");
    expect(dlg.getAttribute("aria-label")).toBe("Import a key for bob@example.com");
    expect(host.querySelector('label[for="' + textarea().id + '"]')?.textContent).toBe("Public key");
    const file = host.querySelector('input[type="file"]') as HTMLInputElement;
    expect(host.querySelector('label[for="' + file.id + '"]')).not.toBeNull();
    expect(file.accept).toContain(".asc");
    expect(document.activeElement).toBe(textarea());
  });

  it("shows the fingerprint in fours and the user ID BEFORE anything is stored", async () => {
    paste(KEY);
    flushSync(() => button("Check this key")!.click());
    await settle();
    expect(check).toHaveBeenCalledWith(KEY);
    expect(onImport).not.toHaveBeenCalled();
    expect(text()).toContain("0123 4567 89AB CDEF 0123 4567 89AB CDEF 0123 4567");
    expect(text()).toContain("Bob Example <bob@example.com>");
    expect(text()).toContain("Is this the right key?");
    expect(textarea()).toBeNull();
  });

  it("imports exactly the key that was checked, then reports and closes", async () => {
    paste(KEY);
    flushSync(() => button("Check this key")!.click());
    await settle();
    flushSync(() => button("Import this key")!.click());
    await settle();
    expect(onImport).toHaveBeenCalledWith(KEY);
    expect(onImported).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("'choose a different key' goes back to the box without storing anything", async () => {
    paste(KEY);
    flushSync(() => button("Check this key")!.click());
    await settle();
    flushSync(() => button("Choose a different key")!.click());
    expect(textarea().value).toBe(KEY);
    expect(onImport).not.toHaveBeenCalled();
  });

  it("Escape and Cancel close it", () => {
    flushSync(() => button("Cancel")!.click());
    expect(onClose).toHaveBeenCalledTimes(1);
    flushSync(() => {
      host.querySelector(".modal-backdrop")!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    });
    expect(onClose).toHaveBeenCalledTimes(2);
  });
});

describe("refusals, in plain words, shown as alerts", () => {
  const cases: [ImportKeyOutcome, string][] = [
    [{ ok: false, error: "secretKeyRefused" }, "That is a secret key. Mailroom did not keep it."],
    [{ ok: false, error: "keyDoesNotNameAddress" }, "That key doesn't name bob@example.com"],
    [{ ok: false, error: "noEncryptionSubkey" }, "it has no encryption subkey"],
    [{ ok: false, error: "keyExpiredOrRevoked" }, "expired or been revoked"],
    [{ ok: false, error: "invalidKey" }, "single OpenPGP public key"],
    [{ ok: false, error: "tooLarge" }, "256 KiB"],
    [{ ok: false, error: "ownAddress" }, "one of your own addresses"],
    [{ ok: false, error: "unavailable" }, "Couldn't reach the server"],
    [{ ok: false, error: "empty" }, "Paste a key or choose a file first."],
  ];
  for (const [outcome, want] of cases) {
    it(`${outcome.ok ? "" : outcome.error}`, async () => {
      check.mockResolvedValueOnce(outcome);
      paste(KEY);
      flushSync(() => button("Check this key")!.click());
      await settle();
      expect(host.querySelector('[role="alert"]')?.textContent).toContain(want);
      // Still on step 1, nothing stored, the pasted text kept for another try.
      expect(onImport).not.toHaveBeenCalled();
      expect(textarea().value).toBe(KEY);
    });
  }

  it("a refusal at the confirming step is shown there and does not close", async () => {
    paste(KEY);
    flushSync(() => button("Check this key")!.click());
    await settle();
    onImport.mockResolvedValueOnce({ ok: false, error: "ownAddress" });
    flushSync(() => button("Import this key")!.click());
    await settle();
    expect(host.querySelector('[role="alert"]')?.textContent).toContain("one of your own addresses");
    expect(onClose).not.toHaveBeenCalled();
    expect(onImported).not.toHaveBeenCalled();
  });
});

describe("the file picker", () => {
  async function choose(file: File): Promise<void> {
    const input = host.querySelector('input[type="file"]') as HTMLInputElement;
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    flushSync(() => {
      input.dispatchEvent(new Event("change", { bubbles: true }));
    });
    await settle();
    await settle();
  }

  it("reads an armored file client-side into the text box", async () => {
    await choose(new File([KEY], "bob.asc", { type: "application/pgp-keys" }));
    expect(textarea().value).toBe(KEY);
    expect(check).not.toHaveBeenCalled(); // reading a file is not importing it
  });

  it("reads a binary file as base64", async () => {
    await choose(new File([new Uint8Array([0x99, 0x01, 0xff, 0x00])], "bob.pgp"));
    expect(atob(textarea().value).length).toBe(4);
  });

  it("refuses a file far over the cap without reading it", async () => {
    const big = new File([new Uint8Array(600 * 1024)], "huge.asc");
    await choose(big);
    expect(host.querySelector('[role="alert"]')?.textContent).toContain("256 KiB");
    expect(textarea().value).toBe("");
  });
});
