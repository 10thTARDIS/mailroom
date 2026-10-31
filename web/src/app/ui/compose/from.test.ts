// SPDX-License-Identifier: AGPL-3.0-only

// The From sentence and the Change list (#448).
//
// Tested at the pure-function boundary, like every other web test in this repo
// (no React Testing Library / jsdom is installed — see
// attachmentScanPolicy.test.ts's note). These two functions ARE the decisions
// the FromLine makes: which address a reply goes out as, what word explains it,
// and in what order the alternatives are offered.

import { describe, expect, it } from "vitest";

import {
  aliasNote,
  defaultIdentity,
  fromSentence,
  identityChoices,
  primaryIdentity,
  replyIdentity,
  resumedIdentity,
} from "./from";
import type { Identity } from "../../../core/jmap/types";

const ACC = "01JBQZ3K7WX2N4Y6P8R0S1T2A0";

/** An Identity in the shape the server serves. Only the fields these functions
 * read are varied; the rest carry the neutral values a real record has, so a
 * fixture cannot pass by omitting the field under test. */
function identity(over: Partial<Identity> & Pick<Identity, "id" | "email">): Identity {
  return {
    name: "",
    replyTo: null,
    bcc: null,
    textSignature: "",
    htmlSignature: "",
    mayDelete: true,
    accountId: ACC,
    isWildcard: false,
    enabled: true,
    ...over,
  };
}

const personal = identity({ id: "i-personal", email: "personal@example.com" });
const house = identity({ id: "i-house", email: "house@example.com" });
const wild = identity({ id: "i-wild", email: "*@example.net", isWildcard: true });
const off = identity({ id: "i-off", email: "old@example.com", enabled: false });

describe("primaryIdentity", () => {
  it("is the first enabled concrete identity in the server's order", () => {
    expect(primaryIdentity([personal, house])?.id).toBe("i-personal");
    expect(primaryIdentity([house, personal])?.id).toBe("i-house");
  });

  // The control: without it, "first" could be satisfied by any of the three.
  it("skips a catch-all and a disabled identity", () => {
    expect(primaryIdentity([off, wild, house])?.id).toBe("i-house");
  });

  it("is undefined when nothing concrete can send", () => {
    expect(primaryIdentity([off, wild])).toBeUndefined();
  });
});

describe("replyIdentity", () => {
  it("takes the identity the parent was delivered to", () => {
    expect(replyIdentity([personal, house], { identityId: "i-house" })?.id).toBe("i-house");
  });

  it("falls back to the concrete identity matching deliveredTo", () => {
    const got = replyIdentity([personal, house], { deliveredTo: "HOUSE@Example.com" });
    expect(got?.id).toBe("i-house");
  });

  it("is undefined for a fresh message and for an unknown delivery", () => {
    expect(replyIdentity([personal, house], null)).toBeUndefined();
    expect(replyIdentity([personal, house], { deliveredTo: "nobody@example.org" })).toBeUndefined();
  });

  it("does not match a wildcard identity against deliveredTo", () => {
    // `*@example.net` is not an address; answering with it would put a From on
    // the wire the server refuses (spec §5 — the From is a security boundary).
    expect(replyIdentity([wild], { deliveredTo: "anything@example.net" })).toBeUndefined();
  });
});

describe("defaultIdentity", () => {
  it("prefers the reply target over the primary", () => {
    expect(defaultIdentity([personal, house], { identityId: "i-house" })?.id).toBe("i-house");
  });

  it("is the primary for a fresh message", () => {
    expect(defaultIdentity([personal, house], null)?.id).toBe("i-personal");
  });

  it("falls back to an enabled catch-all when nothing concrete can send", () => {
    expect(defaultIdentity([off, wild], null)?.id).toBe("i-wild");
  });

  it("still answers when every identity is disabled, so the composer can say so", () => {
    expect(defaultIdentity([off], null)?.id).toBe("i-off");
  });
});

describe("fromSentence", () => {
  it("says 'replying here' for the address the parent was delivered to", () => {
    expect(fromSentence(house, { identityId: "i-house" }, [personal, house])).toEqual({
      address: "house@example.com",
      reason: "replyingHere",
    });
  });

  it("says 'primary' for a fresh message from the primary address", () => {
    expect(fromSentence(personal, null, [personal, house])).toEqual({
      address: "personal@example.com",
      reason: "primary",
    });
  });

  it("prints the typed local part for a catch-all, never the wildcard form", () => {
    expect(fromSentence(wild, null, [personal, wild], " shop@example.net ")).toEqual({
      address: "shop@example.net",
      reason: "catchAll",
    });
    // Nothing typed yet: the domain, which is what the old control showed too.
    expect(fromSentence(wild, null, [personal, wild])).toEqual({
      address: "@example.net",
      reason: "catchAll",
    });
  });

  // The direction that matters: `disabled` outranks every other reason, because
  // it is the one word whose being wrong lets a user press Send on an address
  // the server will refuse.
  it("says 'disabled' even for the identity the parent was delivered to", () => {
    const disabledReplyTarget = identity({ id: "i-off", email: "old@example.com", enabled: false });
    expect(
      fromSentence(disabledReplyTarget, { identityId: "i-off" }, [personal, disabledReplyTarget])
        ?.reason,
    ).toBe("disabled");
  });

  it("is null with no identity at all", () => {
    expect(fromSentence(undefined, null, [])).toBeNull();
  });
});

describe("identityChoices", () => {
  it("orders enabled concrete, then catch-all, then disabled", () => {
    const rows = identityChoices([off, wild, personal, house], null);
    expect(rows.map((r) => r.id)).toEqual(["i-personal", "i-house", "i-wild", "i-off"]);
    expect(rows.map((r) => r.reason)).toEqual(["primary", "primary", "catchAll", "disabled"]);
  });

  // ⚠ THE CONTROL FOR THE GROUPING. A sort on `enabled` alone — or on
  // `[!isWildcard, enabled]` — puts the disabled CONCRETE address above the
  // catch-all that can actually send. The order above is the only one that ranks
  // "can I click this and send" descending.
  it("ranks a disabled concrete address BELOW an enabled catch-all", () => {
    const rows = identityChoices([off, wild], null);
    expect(rows.map((r) => r.id)).toEqual(["i-wild", "i-off"]);
  });

  it("leads with the reply target inside the concrete group", () => {
    const rows = identityChoices([personal, house], { identityId: "i-house" });
    expect(rows.map((r) => r.id)).toEqual(["i-house", "i-personal"]);
    expect(rows.map((r) => r.reason)).toEqual(["replyingHere", "primary"]);
  });

  it("filters on the address and on the display name, case-insensitively", () => {
    const named = identity({ id: "i-named", email: "z@example.com", name: "Workshop" });
    expect(identityChoices([personal, house, named], null, "HOUSE").map((r) => r.id)).toEqual([
      "i-house",
    ]);
    expect(identityChoices([personal, house, named], null, "worksh").map((r) => r.id)).toEqual([
      "i-named",
    ]);
    expect(identityChoices([personal, house], null, "nothing")).toEqual([]);
  });

  it("keeps the server's order inside a group rather than sorting by address", () => {
    // "primary" means a POSITION (there is no flag on Identity), so re-sorting
    // here would make it mean "alphabetically first" and disagree with
    // fromSentence.
    const zed = identity({ id: "i-zed", email: "aaa@example.com" });
    const rows = identityChoices([house, zed], null);
    expect(rows.map((r) => r.id)).toEqual(["i-house", "i-zed"]);
  });
});

describe("aliasNote", () => {
  it("is present for a non-primary alias, naming it", () => {
    expect(aliasNote(house, personal)).toEqual({ alias: "house@example.com" });
  });

  it("is absent on the primary address", () => {
    expect(aliasNote(personal, personal)).toBeNull();
  });

  it("is absent when there is no primary to compare with", () => {
    expect(aliasNote(wild, undefined)).toBeNull();
  });

  it("names the domain for a catch-all", () => {
    expect(aliasNote(wild, personal)).toEqual({ alias: "example.net" });
  });
});

// ---------------------------------------------------------------------------
// #599: RESUMING A DRAFT MUST NOT CHANGE WHO IT IS FROM.
//
// Found on delivered mail, not by reading: two drafts composed from two
// different aliases were refused by the provider, re-sent 96 minutes later, and
// BOTH went out as the canonical mailbox. Two aliases landing on one address is
// a default being re-applied.
//
// The old code matched the saved From against CONCRETE identities only, so on a
// catch-all an alias matched nothing and fell through to the primary.
//
// ⚠ THE OVER-MATCHING CASES ARE THE ONES THAT KEEP THIS HONEST. A wildcard that
// claimed every unmatched address would "fix" every assertion about aliases and
// then fail at the provider instead of on screen, which is worse than the bug.
describe("resumedIdentity — #599", () => {
  const catchall = identity({ id: "i-cat", email: "*@example.net", isWildcard: true });
  const house = identity({ id: "i-house", email: "house@example.com" });
  const ids = [house, catchall];

  it("resolves a DERIVED ALIAS to the wildcard that owns its domain", () => {
    expect(resumedIdentity(ids, "mortgage@example.net")?.id).toBe("i-cat");
  });

  it("still prefers a concrete identity that matches exactly", () => {
    expect(resumedIdentity(ids, "house@example.com")?.id).toBe("i-house");
  });

  it("ignores case and surrounding space, as the old exact match did", () => {
    expect(resumedIdentity(ids, "  HOUSE@example.com ")?.id).toBe("i-house");
    expect(resumedIdentity(ids, " Mortgage@ExAmPlE.net ")?.id).toBe("i-cat");
  });

  it("does NOT claim an address on a domain no identity owns", () => {
    // The over-match that would be worse than the bug: this account cannot send
    // as it, so claiming it moves the failure from the screen to the provider.
    expect(resumedIdentity(ids, "someone@elsewhere.example")).toBeUndefined();
  });

  it("does NOT match a domain that merely ENDS with the wildcard's", () => {
    // `evilexample.net` ends with `example.net` as a string. A suffix test
    // would take it; the domain is compared whole.
    expect(resumedIdentity(ids, "x@evilexample.net")).toBeUndefined();
    expect(resumedIdentity(ids, "x@sub.example.net")).toBeUndefined();
  });

  it("returns nothing for an empty or malformed From, rather than guessing", () => {
    expect(resumedIdentity(ids, "")).toBeUndefined();
    expect(resumedIdentity(ids, "   ")).toBeUndefined();
    expect(resumedIdentity(ids, null)).toBeUndefined();
    expect(resumedIdentity(ids, undefined)).toBeUndefined();
    expect(resumedIdentity(ids, "not-an-address")).toBeUndefined();
  });

  it("returns nothing when the account has no catch-all at all", () => {
    expect(resumedIdentity([house], "mortgage@example.net")).toBeUndefined();
  });
});
