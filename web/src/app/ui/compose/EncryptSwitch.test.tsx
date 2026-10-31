// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The Encrypt switch's changed-key refusal (#47, ADR-039) links to the same
// decision the reading pane offers: it names the recipient, shows both keys, and
// a click sends the decision for the NEW key.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { RecipientKeyInfo } from "../../../core/jmap/types";
import {
  CAPTURED_AT,
  LOOKUP_ALICE_SOON,
  LOOKUP_CAROL_EXPIRED,
  LOOKUP_NO_KEY,
} from "../../../core/keyFixtures.testdata";
import { C1_CAPTURED_AT, C1_LOOKUP_CAROL_EXPIRED } from "../../../core/keyHistoryFixtures.testdata";
import type { ComposeRefreshResult } from "../../../core/keyLifecycle";
import { EncryptSwitch } from "./EncryptSwitch";
import type { EncryptOffer } from "./encryptOffer";

const OLD = "0123456789ABCDEF0123456789ABCDEF01234567";
const NEW = "FEDCBA9876543210FEDCBA9876543210FEDCBA98";

const info = (over: Partial<RecipientKeyInfo> = {}): RecipientKeyInfo => ({
  address: "bob@example.com",
  usable: false,
  fingerprint: NEW,
  source: "wkd",
  wkd: "skipped",
  keyChanged: true,
  pinnedFingerprint: OLD,
  ...over,
});

const changedOffer: EncryptOffer = { offered: false, why: "changed", missing: [], changed: ["bob@example.com"] };

let host: HTMLDivElement;
let root: Root;
const onDecideKey = vi.fn(async (..._a: unknown[]) => true);
const onImportKey = vi.fn();
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function render(offer: EncryptOffer, keys: Record<string, RecipientKeyInfo | undefined>): void {
  flushSync(() =>
    root.render(
      <EncryptSwitch offer={offer} checked={false} onChange={() => {}} names={["Bob"]} keys={keys} onDecideKey={onDecideKey} onImportKey={onImportKey} />,
    ),
  );
}

describe("a recipient whose key changed", () => {
  it("shows the refusal, the recipient, both keys and the two decisions", () => {
    render(changedOffer, { "bob@example.com": info() });
    const text = host.textContent ?? "";
    expect(text).toContain("Can't encrypt: the key for bob@example.com has changed");
    expect(text).toContain("The key for bob@example.com is different from the one you last wrote to.");
    expect(text).toContain("0123 4567 89AB CDEF 0123 4567 89AB CDEF 0123 4567");
    expect(text).toContain("FEDC BA98 7654 3210 FEDC BA98 7654 3210 FEDC BA98");
    expect([...host.querySelectorAll("button")].map((b) => b.textContent)).toEqual([
      "Trust this key from now on",
      "Keep the old key",
    ]);
  });

  it("sends the decision for the NEW key", async () => {
    render(changedOffer, { "bob@example.com": info() });
    const keep = [...host.querySelectorAll("button")].find((b) => b.textContent === "Keep the old key")!;
    flushSync(() => keep.click());
    await settle();
    expect(onDecideKey).toHaveBeenCalledWith("bob@example.com", NEW, "keep");
  });

  it("offers no decision it cannot make (an answer missing a fingerprint)", () => {
    render(changedOffer, { "bob@example.com": info({ pinnedFingerprint: null }) });
    expect(host.querySelectorAll("button")).toHaveLength(0);
    expect(host.textContent).toContain("Can't encrypt: the key for bob@example.com has changed");
  });

  it("control: an offered switch draws no decision", () => {
    render({ offered: true, why: "ok", missing: [], changed: [] }, { "bob@example.com": info({ keyChanged: false }) });
    expect(host.querySelectorAll("button")).toHaveLength(0);
    expect(host.querySelector('input[type="checkbox"]')).not.toBeNull();
  });
});

describe("a recipient with no key", () => {
  const missingOffer: EncryptOffer = {
    offered: false,
    why: "missing",
    missing: ["bob@example.com", "eve@example.com"],
    changed: [],
  };

  it("offers 'Import their key...' for each one, named for the screen reader", () => {
    render(missingOffer, {});
    expect(host.textContent).toContain("Can't encrypt: no key for bob@example.com and eve@example.com.");
    const buttons = [...host.querySelectorAll("button")];
    expect(buttons.map((b) => b.textContent)).toEqual(["Import their key…", "Import their key…"]);
    expect(buttons.map((b) => b.getAttribute("aria-label"))).toEqual([
      "Import their key… (bob@example.com)",
      "Import their key… (eve@example.com)",
    ]);
  });

  it("a click opens the import for THAT recipient", () => {
    render(missingOffer, {});
    flushSync(() => (host.querySelectorAll("button")[1] as HTMLButtonElement).click());
    expect(onImportKey).toHaveBeenCalledWith("eve@example.com");
  });

  it("offers nothing to import when the switch is not blocked by a missing key", () => {
    render({ offered: false, why: "pending", missing: [], changed: [] }, {});
    expect(host.querySelectorAll("button")).toHaveLength(0);
  });
});

// #795: recipients' key dates, said by name, from captured lookups.
describe("recipients' key dates", () => {
  const keys = {
    "alice@example.org": LOOKUP_ALICE_SOON,
    "carol@example.org": LOOKUP_CAROL_EXPIRED,
    "nobody@example.net": LOOKUP_NO_KEY,
  };
  const labels = { "alice@example.org": "Alice", "carol@example.org": "Carol", "nobody@example.net": "Nobody" };
  function renderWith(offer: EncryptOffer, recipients: string[]): void {
    flushSync(() =>
      root.render(
        <EncryptSwitch
          offer={offer}
          checked={false}
          onChange={() => {}}
          names={recipients.map((a) => labels[a as keyof typeof labels])}
          keys={keys}
          onDecideKey={onDecideKey}
          onImportKey={onImportKey}
          recipients={recipients}
          labels={labels}
          warnDays={30}
          now={CAPTURED_AT}
        />,
      ),
    );
  }

  it("offered, with a key inside the window: \"Alice's key expires on 31 Oct\"", () => {
    renderWith({ offered: true, why: "ok", missing: [], changed: [] }, ["alice@example.org"]);
    expect(host.querySelector(".compose-key-expiry")?.textContent).toBe("Alice's key expires on 31 Oct.");
    expect(host.querySelector('input[type="checkbox"]')).not.toBeNull();
  });

  it("not offered because the only key expired: says so instead of \"no key\"", () => {
    renderWith({ offered: false, why: "missing", missing: ["carol@example.org"], changed: [] }, ["carol@example.org"]);
    const line = host.querySelector(".compose-encrypt-note")?.textContent ?? "";
    expect(line).toBe("Can't encrypt: Carol's key expired on 21 Oct.");
    expect(line).not.toContain("no key");
  });

  it("an expired key and no key at all: each said once, in its own words", () => {
    renderWith(
      { offered: false, why: "missing", missing: ["carol@example.org", "nobody@example.net"], changed: [] },
      ["carol@example.org", "nobody@example.net"],
    );
    expect(host.querySelector(".compose-encrypt-note")?.textContent).toBe(
      "Can't encrypt: Carol's key expired on 21 Oct. Can't encrypt: no key for Nobody.",
    );
  });
});

// #818: "Can't encrypt: Carol's key expired on …" offers "Check for an updated key".
describe("checking for an updated key from compose", () => {
  const keys = { "carol@example.org": C1_LOOKUP_CAROL_EXPIRED, "nobody@example.net": LOOKUP_NO_KEY };
  const labels = { "carol@example.org": "Carol", "nobody@example.net": "Nobody" };
  const missing: EncryptOffer = {
    offered: false,
    why: "missing",
    missing: ["carol@example.org", "nobody@example.net"],
    changed: [],
  };
  const onRefreshKey = vi.fn(async (_a: string): Promise<ComposeRefreshResult> => ({ kind: "notFound" }));
  function renderWith(withRefresh = true): void {
    flushSync(() =>
      root.render(
        <EncryptSwitch
          offer={missing}
          checked={false}
          onChange={() => {}}
          names={["Carol", "Nobody"]}
          keys={keys}
          onDecideKey={onDecideKey}
          onImportKey={onImportKey}
          recipients={["carol@example.org", "nobody@example.net"]}
          labels={labels}
          warnDays={30}
          now={C1_CAPTURED_AT}
          {...(withRefresh ? { onRefreshKey } : {})}
        />,
      ),
    );
  }
  const refreshButtons = () => [...host.querySelectorAll<HTMLButtonElement>(".compose-key-refresh")];

  it("is offered for the expired key only, named for the screen reader", () => {
    renderWith();
    expect(refreshButtons().map((b) => [b.textContent, b.getAttribute("aria-label")])).toEqual([
      ["Check for an updated key", "Check for an updated key for Carol"],
    ]);
  });

  it("checks that address, is busy meanwhile, then says what it found", async () => {
    let answer: (r: ComposeRefreshResult) => void = () => {};
    onRefreshKey.mockImplementationOnce(() => new Promise((r) => (answer = r)));
    renderWith();
    flushSync(() => refreshButtons()[0]!.click());
    expect(onRefreshKey).toHaveBeenCalledWith("carol@example.org");
    expect(refreshButtons()[0]!.disabled).toBe(true);
    expect(refreshButtons()[0]!.textContent).toBe("Checking for an updated key…");
    flushSync(() => answer({ kind: "renewed", expiresAt: "2028-11-03T18:25:11Z" }));
    // Not settle(): one macrotask was not always enough for the answer to
    // render on a loaded CI runner (run 14871), so wait for the line itself.
    await vi.waitFor(() =>
      expect(host.querySelector(".compose-key-refreshed")?.textContent).toBe("Carol: Updated — now expires 3 Nov 2028"),
    );
    expect(host.querySelector(".compose-key-refreshed")?.getAttribute("role")).toBe("status");
  });

  it("a check refused for being too soon says when to try again", async () => {
    onRefreshKey.mockResolvedValueOnce({ kind: "error", error: "rateLimited" });
    renderWith();
    flushSync(() => refreshButtons()[0]!.click());
    await settle();
    expect(host.querySelector(".compose-key-refreshed")?.textContent).toBe(
      "Carol: Checked recently — try again in a few minutes",
    );
  });

  it("control: without the handler nothing is offered", () => {
    renderWith(false);
    expect(refreshButtons()).toHaveLength(0);
  });
});
