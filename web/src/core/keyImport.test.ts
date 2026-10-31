// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { JmapMethodError, JmapTransportError } from "./jmap/client";
import { importKeyErrorOf, keyFileToText } from "./keyImport";

describe("importKeyErrorOf", () => {
  it("passes each of the server's refusals through as itself", () => {
    for (const type of [
      "secretKeyRefused",
      "keyDoesNotNameAddress",
      "noEncryptionSubkey",
      "keyExpiredOrRevoked",
      "invalidKey",
      "tooLarge",
      "ownAddress",
    ]) {
      expect(importKeyErrorOf(new JmapMethodError("c", { type }))).toBe(type);
    }
  });

  it("never invents a cause: an unknown method error is 'failed'", () => {
    expect(importKeyErrorOf(new JmapMethodError("c", { type: "serverFail" }))).toBe("failed");
    expect(importKeyErrorOf(new Error("boom"))).toBe("failed");
  });

  it("an unreachable server is 'unavailable'", () => {
    expect(importKeyErrorOf(new JmapTransportError("down"))).toBe("unavailable");
  });
});

describe("keyFileToText", () => {
  const enc = (s: string) => new TextEncoder().encode(s);

  it("keeps an armored key as the text it is", () => {
    const armored = "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nabc\n-----END PGP PUBLIC KEY BLOCK-----\n";
    expect(keyFileToText(enc(`\n${armored}`))).toBe(armored.trim());
  });

  it("sends a binary key as base64, byte for byte", () => {
    const bin = new Uint8Array([0x99, 0x01, 0x0d, 0x04, 0xff, 0x00, 0x80]);
    const out = keyFileToText(bin);
    expect([...atob(out)].map((c) => c.charCodeAt(0))).toEqual([...bin]);
  });

  it("handles a key larger than one chunk", () => {
    const bin = new Uint8Array(100_000).map((_, i) => (i * 7) & 0xff);
    expect(atob(keyFileToText(bin)).length).toBe(100_000);
  });
});
