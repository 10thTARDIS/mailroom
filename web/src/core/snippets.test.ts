// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { automaticSignature, automaticSignatureBody, offeredSignatures } from "./snippets";
import type { ComposeSnippet, Id, Identity } from "./jmap/types";

const WORK = "01IDWORK" as Id;
const SHOP = "01IDSHOP" as Id;

function identity(over: Partial<Identity> = {}): Identity {
  return {
    id: WORK,
    email: "work@example.com",
    name: "Work",
    htmlSignature: "",
    textSignature: "",
    ...over,
  } as Identity;
}

function sig(over: Partial<ComposeSnippet> & { name: string }): ComposeSnippet {
  return {
    id: `id-${over.name}` as Id,
    kind: "signature",
    bodyHtml: "",
    bodyText: over.name,
    identityId: null,
    isDefault: false,
    ...over,
  } as ComposeSnippet;
}

describe("automaticSignature", () => {
  it("inserts nothing when no signature is marked default", () => {
    const all = [sig({ name: "work", identityId: WORK }), sig({ name: "general" })];
    expect(automaticSignature(all, WORK)).toBeNull();
  });

  it("prefers the default bound to this identity over the unbound default", () => {
    const all = [
      sig({ name: "general", isDefault: true }),
      sig({ name: "work", identityId: WORK, isDefault: true }),
    ];
    expect(automaticSignature(all, WORK)?.name).toBe("work");
  });

  it("falls back to the unbound default when nothing is bound to this identity", () => {
    const all = [
      sig({ name: "general", isDefault: true }),
      sig({ name: "shop", identityId: SHOP, isDefault: true }),
    ];
    expect(automaticSignature(all, WORK)?.name).toBe("general");
  });

  // THE ONE THAT MATTERS: a signature bound to another address must never be inserted.
  // Putting the wrong name at the bottom of a message is the failure this whole
  // per-identity mechanism exists to prevent.
  it("never uses a signature bound to a different identity", () => {
    const all = [sig({ name: "shop", identityId: SHOP, isDefault: true })];
    expect(automaticSignature(all, WORK)).toBeNull();
  });

  it("uses only the unbound default when the identity is unknown", () => {
    const all = [
      sig({ name: "shop", identityId: SHOP, isDefault: true }),
      sig({ name: "general", isDefault: true }),
    ];
    expect(automaticSignature(all, null)?.name).toBe("general");
    expect(automaticSignature(all, undefined)?.name).toBe("general");
  });

  it("ignores templates, which have no identity and are not sign-offs", () => {
    const all = [
      { ...sig({ name: "reply-template", isDefault: true }), kind: "template" } as ComposeSnippet,
    ];
    expect(automaticSignature(all, WORK)).toBeNull();
  });
});

describe("automaticSignatureBody", () => {
  // The identity's own htmlSignature/textSignature are RFC 8621 fields, so one can
  // already be set from before this feature or by another JMAP client. Ignoring it would
  // look like Mailroom had broken a signature the user configured.
  it("falls back to the identity's own signature when no snippet is default", () => {
    const body = automaticSignatureBody([], identity({ htmlSignature: "<p>from JMAP</p>" }));
    expect(body).toEqual({ html: "<p>from JMAP</p>", text: "" });
  });

  it("prefers a default snippet over the identity's own signature", () => {
    const all = [sig({ name: "mine", identityId: WORK, isDefault: true, bodyText: "mine" })];
    expect(automaticSignatureBody(all, identity({ htmlSignature: "<p>old</p>" }))).toEqual({
      html: "",
      text: "mine",
    });
  });

  it("is null when neither exists, and treats a whitespace-only identity signature as none", () => {
    expect(automaticSignatureBody([], identity())).toBeNull();
    expect(automaticSignatureBody([], identity({ htmlSignature: "   " }))).toBeNull();
    expect(automaticSignatureBody([], undefined)).toBeNull();
  });

  it("does not fall back past a signature bound to another address", () => {
    // The SHOP snippet must not be used, and the WORK identity's own signature must be.
    const all = [sig({ name: "shop", identityId: SHOP, isDefault: true })];
    expect(automaticSignatureBody(all, identity({ textSignature: "work sig" }))).toEqual({
      html: "",
      text: "work sig",
    });
  });
});

describe("offeredSignatures", () => {
  it("offers this identity's and the unbound ones, and hides another identity's", () => {
    const all = [
      sig({ name: "work", identityId: WORK }),
      sig({ name: "shop", identityId: SHOP }),
      sig({ name: "general" }),
    ];
    expect(offeredSignatures(all, WORK).map((s) => s.name)).toEqual(["work", "general"]);
  });

  it("puts bound before unbound, default before the rest, then sorts by name", () => {
    const all = [
      sig({ name: "zeta", identityId: WORK }),
      sig({ name: "alpha", identityId: WORK, isDefault: true }),
      sig({ name: "beta", identityId: WORK }),
      sig({ name: "unbound-default", isDefault: true }),
      sig({ name: "unbound-other" }),
    ];
    expect(offeredSignatures(all, WORK).map((s) => s.name)).toEqual([
      "alpha",
      "beta",
      "zeta",
      "unbound-default",
      "unbound-other",
    ]);
  });

  it("excludes templates", () => {
    const all = [
      sig({ name: "work", identityId: WORK }),
      { ...sig({ name: "tpl" }), kind: "template" } as ComposeSnippet,
    ];
    expect(offeredSignatures(all, WORK).map((s) => s.name)).toEqual(["work"]);
  });

  it("offers only unbound signatures when there is no identity yet", () => {
    const all = [sig({ name: "work", identityId: WORK }), sig({ name: "general" })];
    expect(offeredSignatures(all, null).map((s) => s.name)).toEqual(["general"]);
  });
});
