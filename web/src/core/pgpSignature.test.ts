// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { groupFingerprint, shortFingerprint, signatureNotice } from "./pgpSignature";
import { SIGNATURE_KEY_EXPIRED, SIGNATURE_PLAIN_BAD } from "./keyFixtures.testdata";

const A = "0123456789ABCDEF0123456789ABCDEF01234567";
const B = "FEDCBA9876543210FEDCBA9876543210FEDCBA98";

describe("signatureNotice (#47, ADR-039)", () => {
  it("says nothing for an unsigned message", () => {
    expect(signatureNotice(null)).toBeNull();
    expect(signatureNotice(undefined)).toBeNull();
  });

  it("verified names the key", () => {
    expect(signatureNotice({ state: "verified", fingerprint: A })).toEqual({
      state: "verified",
      fingerprint: "89AB CDEF 0123 4567",
    });
  });

  it("a verified that names no key is not shown as verified", () => {
    expect(signatureNotice({ state: "verified" })).toEqual({ state: "unknownKey", fingerprint: null });
  });

  it("bad and unknownKey", () => {
    expect(signatureNotice({ state: "bad" })).toEqual({ state: "bad" });
    expect(signatureNotice({ state: "unknownKey", fingerprint: A })).toEqual({
      state: "unknownKey",
      fingerprint: "89AB CDEF 0123 4567",
    });
    expect(signatureNotice({ state: "unknownKey" })).toEqual({ state: "unknownKey", fingerprint: null });
  });

  it("keyChanged carries both full fingerprints and the address", () => {
    expect(
      signatureNotice({
        state: "keyChanged",
        address: "alice@example.com",
        fingerprint: B,
        previousFingerprint: A,
      }),
    ).toEqual({
      state: "keyChanged",
      address: "alice@example.com",
      fingerprint: B,
      previousFingerprint: A,
      refused: false,
    });
    expect(
      signatureNotice({
        state: "keyChanged",
        address: "alice@example.com",
        fingerprint: B,
        previousFingerprint: A,
        refused: true,
      }),
    ).toMatchObject({ refused: true });
  });

  it("a keyChanged that cannot be acted on is the weaker unknown state", () => {
    expect(signatureNotice({ state: "keyChanged", fingerprint: B })).toEqual({
      state: "unknownKey",
      fingerprint: "7654 3210 FEDC BA98",
    });
  });

  it("a state from a newer server is never good news", () => {
    expect(signatureNotice({ state: "somethingNew" as never })).toEqual({ state: "bad" });
  });

  it("groups fingerprints in fours", () => {
    expect(groupFingerprint(A)).toBe("0123 4567 89AB CDEF 0123 4567 89AB CDEF 0123 4567");
    expect(groupFingerprint("ab cd")).toBe("ABCD");
    expect(shortFingerprint("")).toBe("");
  });
});

describe("the address an unknown key was judged against", () => {
  it("is carried when the server named it, so a key can be imported for it", () => {
    expect(signatureNotice({ state: "unknownKey", address: "a@example.com" })).toEqual({
      state: "unknownKey",
      fingerprint: null,
      address: "a@example.com",
    });
  });
});

// The keyExpired reason (#795/#796), from the JSON the server produced: its
// own state, never verified; any other bad (or an unknown reason) stays bad.
describe("a signature by a key that has since expired", () => {
  it("is its own notice with the expiry date and whether it was valid when signed", () => {
    expect(signatureNotice(SIGNATURE_KEY_EXPIRED)).toEqual({
      state: "keyExpired",
      keyExpiredAt: "2026-10-11T09:49:12Z",
      validWhenSigned: true,
    });
    expect(signatureNotice({ ...SIGNATURE_KEY_EXPIRED, validWhenSigned: false })).toMatchObject({ validWhenSigned: false });
  });

  it("a bad without the reason, or with one this client does not know, is plain bad", () => {
    expect(signatureNotice(SIGNATURE_PLAIN_BAD)).toEqual({ state: "bad" });
    expect(signatureNotice({ ...SIGNATURE_KEY_EXPIRED, reason: "somethingElse" as never })).toEqual({ state: "bad" });
  });

  it("the reason never softens a state other than bad", () => {
    expect(signatureNotice({ state: "unknownKey", reason: "keyExpired" })).toEqual({ state: "unknownKey", fingerprint: null });
  });
});
