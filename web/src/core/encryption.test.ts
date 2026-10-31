// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { t } from "../app/i18n";
import { encryptionNotice, recipientKeyNotices } from "./encryption";
import {
  CAPTURED_AT,
  LOOKUP_ALICE_SOON,
  LOOKUP_CAROL_EXPIRED,
  LOOKUP_NO_KEY,
} from "./keyFixtures.testdata";
import { shortFingerprint } from "./pgpSignature";

describe("encryptionNotice (#47 6b)", () => {
  it("says nothing about mail that is not encrypted", () => {
    expect(encryptionNotice({})).toBeNull();
    expect(encryptionNotice({ encrypted: false, decrypted: null })).toBeNull();
  });

  it("says nothing when no decryption was attempted (decrypted: null)", () => {
    expect(encryptionNotice({ encrypted: true, decrypted: null })).toBeNull();
  });

  it("reports a failure with no detail", () => {
    expect(encryptionNotice({ encrypted: true, decrypted: false })).toEqual({ kind: "failed" });
  });

  it("reports a decrypted message", () => {
    expect(encryptionNotice({ encrypted: true, decrypted: true })).toEqual({ kind: "decrypted" });
  });

  it("formats a fingerprint as its long key id", () => {
    expect(shortFingerprint("abcd ef01 2345 6789 aaaa bbbb cccc dddd")).toBe("AAAA BBBB CCCC DDDD");
  });

  it("never claims end-to-end encryption in any string it uses (ADR-024)", () => {
    const keys = [
      "thread.encryption.decrypted",
      "thread.encryption.failed",
      "thread.signature.verified",
      "thread.signature.bad",
      "thread.signature.unknownKey",
      "thread.signature.keyChanged",
    ] as const;
    for (const key of keys) {
      const text = t(key, { fingerprint: "AAAA", address: "a@example.com" });
      // Control: a real string, not the key echoed back for a missing entry.
      expect(text).not.toBe(key);
      expect(text.toLowerCase()).not.toMatch(/end-to-end|only you|nobody else|zero.knowledge/);
    }
  });
});

// Recipients' key dates in compose (#795), from captured RecipientKey/lookup
// answers: a key inside the window is named with its date when Encrypt is
// offered; a lapsed key is named instead of the generic "no key"; an address
// with no key at all stays with the generic sentence.
describe("recipientKeyNotices (#795)", () => {
  const keys = {
    "alice@example.org": LOOKUP_ALICE_SOON,
    "carol@example.org": LOOKUP_CAROL_EXPIRED,
    "nobody@example.net": LOOKUP_NO_KEY,
  };

  it("offered: names the recipient whose key expires inside the server's window", () => {
    const n = recipientKeyNotices({ offered: true, missing: [] }, ["Alice@Example.org"], keys, 30, CAPTURED_AT);
    expect(n.expiring).toEqual([{ kind: "expiresSoon", address: "alice@example.org", at: "2026-10-31T09:50:18Z" }]);
    expect(n.lapsed).toEqual([]);
  });

  it("offered: nothing when the key is outside the window", () => {
    const later = { "alice@example.org": { ...LOOKUP_ALICE_SOON, expiringSoon: false } };
    expect(recipientKeyNotices({ offered: true, missing: [] }, ["alice@example.org"], later, 30, CAPTURED_AT).expiring).toEqual([]);
  });

  it("not offered: an expired key is named with its date; no key at all stays generic", () => {
    const n = recipientKeyNotices(
      { offered: false, missing: ["carol@example.org", "nobody@example.net"] },
      ["carol@example.org", "nobody@example.net"],
      keys,
      30,
      CAPTURED_AT,
    );
    expect(n.lapsed).toEqual([{ kind: "expired", address: "carol@example.org", at: "2026-10-21T09:52:48Z" }]);
    expect(n.otherMissing).toEqual(["nobody@example.net"]);
  });

  it("not offered: a revoked key is named as revoked", () => {
    const revoked = { "carol@example.org": { ...LOOKUP_CAROL_EXPIRED, reason: "revoked" as const, expiredAt: null } };
    const n = recipientKeyNotices({ offered: false, missing: ["carol@example.org"] }, [], revoked, 30, CAPTURED_AT);
    expect(n.lapsed).toEqual([{ kind: "revoked", address: "carol@example.org" }]);
  });
});
