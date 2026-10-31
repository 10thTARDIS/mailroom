// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import type { RecipientKeyInfo } from "../../../core/jmap/types";
import { addressesToLookUp, encryptOffer, type EncryptOfferInput } from "./encryptOffer";

const usable = (address: string): RecipientKeyInfo => ({
  address,
  usable: true,
  fingerprint: "AAAA",
  source: "wkd",
  wkd: "found",
  keyChanged: false,
  pinnedFingerprint: null,
});
const unusable = (address: string): RecipientKeyInfo => ({
  ...usable(address),
  usable: false,
  fingerprint: null,
  source: null,
  wkd: "none",
});
const changed = (address: string): RecipientKeyInfo => ({
  ...usable(address),
  usable: false,
  keyChanged: true,
  pinnedFingerprint: "BBBB",
});

const base = (over: Partial<EncryptOfferInput> = {}): EncryptOfferInput => ({
  serverCanEncrypt: true,
  ownKey: true,
  ownAddress: "me@example.com",
  to: ["bob@example.com"],
  cc: [],
  bcc: [],
  keys: { "bob@example.com": usable("bob@example.com") },
  ...over,
});

describe("encryptOffer", () => {
  it("is offered when every To/Cc recipient has a usable key", () => {
    const o = encryptOffer(
      base({ cc: ["carol@example.com"], keys: { "bob@example.com": usable("bob@example.com"), "carol@example.com": usable("carol@example.com") } }),
    );
    expect(o).toEqual({ offered: true, why: "ok", missing: [], changed: [] });
  });

  it("names the recipients without a key, and only them", () => {
    const o = encryptOffer(
      base({
        to: ["bob@example.com", "NoKey@Example.com"],
        keys: { "bob@example.com": usable("bob@example.com"), "nokey@example.com": unusable("nokey@example.com") },
      }),
    );
    expect(o.offered).toBe(false);
    expect(o.why).toBe("missing");
    expect(o.missing).toEqual(["nokey@example.com"]);
  });

  it("refuses Bcc outright, before looking at keys", () => {
    const o = encryptOffer(base({ bcc: ["hidden@example.com"] }));
    expect(o).toMatchObject({ offered: false, why: "bcc" });
  });

  it("treats a recipient the lookup has not answered for as pending, never as a yes", () => {
    const o = encryptOffer(base({ keys: {} }));
    expect(o).toMatchObject({ offered: false, why: "pending" });
  });

  it("does not offer a changed key, and names it", () => {
    const o = encryptOffer(base({ keys: { "bob@example.com": changed("bob@example.com") } }));
    expect(o).toMatchObject({ offered: false, why: "changed", changed: ["bob@example.com"] });
  });

  it("needs the server capability and the sender's own key", () => {
    expect(encryptOffer(base({ serverCanEncrypt: false })).why).toBe("unsupported");
    expect(encryptOffer(base({ serverCanEncrypt: null })).why).toBe("unsupported");
    expect(encryptOffer(base({ ownKey: false })).why).toBe("noOwnKey");
    expect(encryptOffer(base({ ownKey: null })).why).toBe("noOwnKey");
  });

  it("needs a recipient, and a message to oneself needs no lookup", () => {
    expect(encryptOffer(base({ to: [] })).why).toBe("noRecipients");
    expect(encryptOffer(base({ to: ["me@example.com"], keys: {} })).offered).toBe(true);
  });
});

describe("addressesToLookUp", () => {
  it("asks about To and Cc only, deduplicated, without the sender, never Bcc", () => {
    expect(
      addressesToLookUp(["Bob@example.com", "me@example.com"], ["bob@example.com", "c@example.com"], "ME@example.com"),
    ).toEqual(["bob@example.com", "c@example.com"]);
  });
});
