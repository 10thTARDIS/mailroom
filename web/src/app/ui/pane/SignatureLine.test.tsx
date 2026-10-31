// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE SIGNATURE LINE (#47, ADR-039), rendered for real: each state says its own
// plain sentence, a changed key shows both fingerprints grouped in fours with the
// two decisions and a comparison, a click sends exactly the decision named, and a
// failed decision leaves the question where it was.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { PgpSignature } from "../../../core/jmap/types";
import { SIGNATURE_KEY_EXPIRED } from "../../../core/keyFixtures.testdata";

const decideContactKey = vi.fn(async (..._args: unknown[]) => true);
const importContactKey = vi.fn(async (..._args: unknown[]) => ({ ok: true as const, fingerprint: "AB".repeat(20), userId: "Alice <alice@example.com>" }));

vi.mock("../../state", () => ({
  useApp: () => ({ actions: { decideContactKey, importContactKey } }),
}));

import { SignatureLine } from "./SignatureLine";

const OLD = "0123456789ABCDEF0123456789ABCDEF01234567";
const NEW = "FEDCBA9876543210FEDCBA9876543210FEDCBA98";

let host: HTMLDivElement;
let root: Root;
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
  decideContactKey.mockResolvedValue(true);
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function render(sig: PgpSignature | null | undefined): void {
  flushSync(() => root.render(<SignatureLine email={{ threadId: "T1", pgpSignature: sig }} />));
}

const text = () => host.textContent ?? "";
const buttons = () => [...host.querySelectorAll<HTMLButtonElement>("button")];
const button = (label: string) => buttons().find((b) => b.textContent === label);

describe("an unsigned message", () => {
  it("draws nothing at all", () => {
    render(null);
    expect(host.childElementCount).toBe(0);
    render(undefined);
    expect(host.childElementCount).toBe(0);
  });
});

describe("the plain states", () => {
  it("verified says which key, as a status, and offers no decision", () => {
    render({ state: "verified", address: "alice@example.com", fingerprint: OLD });
    expect(host.querySelector('[role="status"]')).not.toBeNull();
    expect(text()).toContain("checks out against key 89AB CDEF 0123 4567");
    expect(buttons()).toHaveLength(0);
  });

  it("bad says it does not check out", () => {
    render({ state: "bad" });
    expect(text()).toContain("signature doesn't check out");
    expect(text()).not.toMatch(/checks out against/);
    expect(buttons()).toHaveLength(0);
  });

  it("unknownKey says the key has not been seen from this sender", () => {
    render({ state: "unknownKey", fingerprint: NEW });
    expect(text()).toContain("key you haven't seen from this sender");
    expect(text()).toContain("7654 3210 FEDC BA98");
    // No address from the server: nothing to import a key FOR.
    expect(buttons()).toHaveLength(0);
  });

  it("each state reads differently, so none is conveyed by colour alone", () => {
    const states: PgpSignature[] = [
      { state: "verified", fingerprint: OLD },
      { state: "bad" },
      { state: "unknownKey" },
      { state: "keyChanged", address: "a@example.com", fingerprint: NEW, previousFingerprint: OLD },
    ];
    const seen = new Set<string>();
    for (const s of states) {
      render(s);
      seen.add(text());
    }
    expect(seen.size).toBe(states.length);
  });
});

describe("a changed key", () => {
  const changed: PgpSignature = {
    state: "keyChanged",
    address: "alice@example.com",
    fingerprint: NEW,
    previousFingerprint: OLD,
  };

  it("says NEW, shows both full fingerprints in fours, and what it can mean", () => {
    render(changed);
    expect(text()).toContain("Signed with a NEW key, different from the one you had for alice@example.com.");
    expect(text()).toContain("0123 4567 89AB CDEF 0123 4567 89AB CDEF 0123 4567");
    expect(text()).toContain("FEDC BA98 7654 3210 FEDC BA98 7654 3210 FEDC BA98");
    expect(text()).toContain("changed keys, or that someone else is pretending");
    expect(host.querySelector('[role="group"]')).not.toBeNull();
    expect(host.querySelector('[role="status"]')).not.toBeNull();
    expect(buttons().map((b) => b.textContent)).toEqual([
      "Trust this key from now on",
      "Keep the old key",
      "Import a key for this sender…",
    ]);
    expect(host.querySelector("details summary")?.textContent).toBe("Compare the keys");
  });

  it("trust sends the NEW key's fingerprint with 'trust', and reloads this thread", async () => {
    render(changed);
    flushSync(() => button("Trust this key from now on")!.click());
    await settle();
    expect(decideContactKey).toHaveBeenCalledWith("alice@example.com", NEW, "trust", "T1");
  });

  it("keep sends the same key with 'keep'", async () => {
    render(changed);
    flushSync(() => button("Keep the old key")!.click());
    await settle();
    expect(decideContactKey).toHaveBeenCalledWith("alice@example.com", NEW, "keep", "T1");
  });

  it("a failed decision leaves the question and both buttons where they were", async () => {
    decideContactKey.mockResolvedValueOnce(false);
    render(changed);
    flushSync(() => button("Keep the old key")!.click());
    await settle();
    expect(buttons()).toHaveLength(3); // trust, keep, import
    expect(buttons().every((b) => !b.disabled)).toBe(true);
  });

  it("after 'keep' the line says so and offers only the way back", () => {
    render({ ...changed, refused: true });
    expect(text()).toContain("You chose to keep the old key");
    expect(buttons().map((b) => b.textContent)).toEqual(["Trust this key after all", "Import a key for this sender…"]);
  });
});

describe("importing a key for this sender", () => {
  const unknown: PgpSignature = { state: "unknownKey", address: "alice@example.com", fingerprint: NEW };
  const changed: PgpSignature = {
    state: "keyChanged",
    address: "alice@example.com",
    fingerprint: NEW,
    previousFingerprint: OLD,
  };

  it("is offered on an unknown key and on a changed key, and not on verified or bad", () => {
    render(unknown);
    expect(button("Import a key for this sender…")).toBeDefined();
    render(changed);
    expect(button("Import a key for this sender…")).toBeDefined();
    render({ state: "verified", address: "alice@example.com", fingerprint: OLD });
    expect(button("Import a key for this sender…")).toBeUndefined();
    render({ state: "bad", address: "alice@example.com" });
    expect(button("Import a key for this sender…")).toBeUndefined();
  });

  it("opens the dialog for the sender; checking is a dry run, importing reloads this thread", async () => {
    render(unknown);
    flushSync(() => button("Import a key for this sender…")!.click());
    expect(host.querySelector('[role="dialog"]')?.getAttribute("aria-label")).toBe(
      "Import a key for alice@example.com",
    );
    const ta = host.querySelector("textarea") as HTMLTextAreaElement;
    const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
    flushSync(() => {
      setter.call(ta, "-----BEGIN PGP PUBLIC KEY BLOCK-----\nx\n-----END PGP PUBLIC KEY BLOCK-----");
      ta.dispatchEvent(new Event("input", { bubbles: true }));
    });
    flushSync(() => button("Check this key")!.click());
    await settle();
    expect(importContactKey).toHaveBeenLastCalledWith("alice@example.com", expect.any(String), { dryRun: true });
    flushSync(() => button("Import this key")!.click());
    await settle();
    expect(importContactKey).toHaveBeenLastCalledWith("alice@example.com", expect.any(String), { threadId: "T1" });
    expect(host.querySelector('[role="dialog"]')).toBeNull();
  });
});

// #795: a signature by a key that has since expired (the server's JSON).
describe("a signature by an expired key", () => {
  it("says it was signed with a key that expired, and that it was valid then; neutral, not bad or verified", () => {
    render(SIGNATURE_KEY_EXPIRED);
    const line = host.querySelector(".signature-line") as HTMLElement;
    expect(line.textContent).toBe("Signed with a key that expired on 11 Oct 2026; it was valid when this was signed.");
    expect(line.classList.contains("signature-line-expired")).toBe(true);
    expect(line.classList.contains("signature-line-bad")).toBe(false);
    expect(line.classList.contains("signature-line-verified")).toBe(false);
    expect(line.getAttribute("role")).toBe("status");
    expect(text()).not.toMatch(/may have been changed|checks out/);
    expect(buttons()).toHaveLength(0);
  });

  it("without validWhenSigned it only states the expiry", () => {
    render({ ...SIGNATURE_KEY_EXPIRED, validWhenSigned: false });
    expect(text()).toBe("Signed with a key that expired on 11 Oct 2026.");
  });
});
