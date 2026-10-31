// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The chip's key fact follows RecipientKey/lookup, not Contact.keys (#809).
// Contact.keys counts every held key, lapsed ones included, so a recipient whose
// only key expired used to read "key held" directly above an Encrypt line saying
// it could not encrypt. Each case below holds a key on the Contact (the old
// source) and varies only what the lookup says.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Contact, RecipientKeyInfo } from "../../../core/jmap/types";
import {
  LOOKUP_ALICE_SOON,
  LOOKUP_CAROL_EXPIRED,
  LOOKUP_NO_KEY,
} from "../../../core/keyFixtures.testdata";
import { RecipientChips } from "./RecipientChips";
import { chipKeyFact, chipKeyFactFromLookup } from "./chips";
import { sendLine } from "./sendLine";

vi.mock("../../state", () => ({ useApp: () => ({ client: {} }) }));

const heldContact: Contact = {
  id: "c1",
  displayName: "Held",
  addresses: [{ address: "x@example.org", reachedMeAt: null }],
  keys: [{ fingerprint: "AAAA BBBB", kind: "pgp", source: "manual", fetchedAt: "2026-01-01T00:00:00Z" }],
};

let host: HTMLDivElement;
let root: Root;

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

/** The chip's visible text for a recipient whose Contact holds a key and whose
 * lookup answer is `info` (undefined = not loaded yet). */
function chipText(info: RecipientKeyInfo | undefined): string {
  flushSync(() =>
    root.render(
      <RecipientChips
        label="To"
        chips={[{ address: "x@example.org", name: null }]}
        onChips={() => {}}
        factFor={() => chipKeyFactFromLookup(chipKeyFact(heldContact), info)}
        onNeedFact={() => {}}
        keysAvailable={true}
      />,
    ),
  );
  return host.querySelector(".compose-chip")?.textContent ?? "";
}

describe("recipient chip follows the lookup (#809)", () => {
  it("says 'key held' for a usable key", () => {
    expect(chipText(LOOKUP_ALICE_SOON)).toContain("key held");
  });

  it("says 'key expired' for an expired key, in the neutral tone, never 'key held'", () => {
    const text = chipText(LOOKUP_CAROL_EXPIRED);
    expect(text).toContain("key expired");
    expect(text).not.toContain("key held");
    expect(host.querySelector(".compose-chip-lapsed")).not.toBeNull();
    expect(host.querySelector(".compose-chip-key")).toBeNull();
  });

  it("says 'key revoked' for a revoked key", () => {
    const text = chipText({ ...LOOKUP_CAROL_EXPIRED, reason: "revoked", expiredAt: null });
    expect(text).toContain("key revoked");
    expect(text).not.toContain("key held");
  });

  it("keeps saying 'no key' for noKey, and never 'key held'", () => {
    const text = chipText(LOOKUP_NO_KEY);
    expect(text).toContain("no key");
    expect(text).not.toContain("key held");
  });

  it("keeps the Contact-derived chip until the lookup has answered", () => {
    expect(chipText(undefined)).toContain("key held");
  });
});

describe("send line follows the same lookup (#809)", () => {
  // The send line is fed the chips' own fact, as Compose does.
  const line = (name: string, info: RecipientKeyInfo): ReturnType<typeof sendLine> =>
    sendLine(true, [{ name, fact: chipKeyFactFromLookup(chipKeyFact(heldContact), info) }], "me@example.com");

  it("counts a usable key under 'keys held for'", () => {
    expect(line("alice", LOOKUP_ALICE_SOON).parts.map((p) => p.key)).toContain("compose.send.keysHeldFor");
  });

  it("does not count an expired or revoked key under 'keys held for'", () => {
    for (const info of [LOOKUP_CAROL_EXPIRED, { ...LOOKUP_CAROL_EXPIRED, reason: "revoked" as const }]) {
      const keys = line("carol", info).parts.map((p) => p.key);
      expect(keys).not.toContain("compose.send.keysHeldFor");
      expect(keys).toContain("compose.send.noKeyFor");
    }
  });
});
