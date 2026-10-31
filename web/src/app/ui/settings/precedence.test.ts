// SPDX-License-Identifier: AGPL-3.0-only

// The one assertion that has to hold is the FIRST entry of each group: that is
// the address the user's mail goes out as, and it is the whole reason this row
// prints an order rather than a list. A test that only checked the set would pass
// with the order reversed.

import { describe, expect, it } from "vitest";

import { identityPrecedence } from "./precedence";
import type { Identity } from "../../../core/jmap/types";

const ACC_A = "01JBQZ3K7WX2N4Y6P8R0S1T2A0";
const ACC_B = "01JBQZ3K7WX2N4Y6P8R0S1T2B0";

/** An Identity in the shape the server serves, with only the fields precedence
 * reads varied — the rest are the neutral values a real record carries, so the
 * fixture cannot pass by omitting the field under test. */
function identity(over: Partial<Identity> & Pick<Identity, "id" | "email" | "accountId">): Identity {
  return {
    name: "",
    replyTo: null,
    bcc: null,
    textSignature: "",
    htmlSignature: "",
    mayDelete: true,
    isWildcard: false,
    enabled: true,
    ...over,
  };
}

describe("identityPrecedence", () => {
  it("puts a concrete enabled identity above a catch-all", () => {
    const groups = identityPrecedence([
      identity({ id: "i-star", email: "*@example.com", accountId: ACC_A, isWildcard: true }),
      identity({ id: "i-you", email: "you@example.com", accountId: ACC_A }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0]?.entries.map((e) => e.email)).toEqual([
      "you@example.com",
      "*@example.com",
    ]);
    expect(groups[0]?.entries.map((e) => e.reason)).toEqual(["primary", "catch-all"]);
  });

  it("puts a disabled identity last, even a concrete one, and even under a catch-all", () => {
    // The direction that matters: a disabled concrete address cannot send, so
    // ranking it above a working catch-all would draw a precedence the server
    // would not honour.
    const groups = identityPrecedence([
      identity({ id: "i-off", email: "aaa@example.com", accountId: ACC_A, enabled: false }),
      identity({ id: "i-star", email: "*@example.com", accountId: ACC_A, isWildcard: true }),
      identity({ id: "i-you", email: "zzz@example.com", accountId: ACC_A }),
    ]);
    expect(groups[0]?.entries.map((e) => e.email)).toEqual([
      "zzz@example.com",
      "*@example.com",
      "aaa@example.com",
    ]);
    expect(groups[0]?.entries.map((e) => e.reason)).toEqual([
      "primary",
      "catch-all",
      "disabled",
    ]);
  });

  it("calls a disabled wildcard disabled, not catch-all", () => {
    const groups = identityPrecedence([
      identity({
        id: "i",
        email: "*@example.com",
        accountId: ACC_A,
        isWildcard: true,
        enabled: false,
      }),
    ]);
    expect(groups[0]?.entries[0]?.reason).toBe("disabled");
  });

  it("groups by account, oldest account first", () => {
    const groups = identityPrecedence([
      identity({ id: "b1", email: "b@example.org", accountId: ACC_B }),
      identity({ id: "a1", email: "a@example.com", accountId: ACC_A }),
    ]);
    expect(groups.map((g) => g.accountId)).toEqual([ACC_A, ACC_B]);
    expect(groups.map((g) => g.entries.length)).toEqual([1, 1]);
  });

  it("breaks ties inside a rank on the address, so the order is stable", () => {
    const groups = identityPrecedence([
      identity({ id: "2", email: "zoe@example.com", accountId: ACC_A }),
      identity({ id: "1", email: "abe@example.com", accountId: ACC_A }),
    ]);
    expect(groups[0]?.entries.map((e) => e.email)).toEqual([
      "abe@example.com",
      "zoe@example.com",
    ]);
  });

  it("reads isWildcard, not the address — a star-prefixed address is not a catch-all by itself", () => {
    // The negative control for the authority question. Were this sniffing the
    // string, the entry below would be ranked as a catch-all and the test would
    // see "catch-all" here.
    const groups = identityPrecedence([
      identity({ id: "i", email: "*not-a-wildcard@example.com", accountId: ACC_A }),
    ]);
    expect(groups[0]?.entries[0]?.reason).toBe("primary");
  });

  it("leaves out an identity with no account rather than filing it under nothing", () => {
    const groups = identityPrecedence([
      identity({ id: "orphan", email: "nowhere@example.com", accountId: "" }),
      identity({ id: "ok", email: "you@example.com", accountId: ACC_A }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0]?.entries.map((e) => e.id)).toEqual(["ok"]);
  });

  it("takes the store's keyed record as well as an array", () => {
    const record = {
      a1: identity({ id: "a1", email: "a@example.com", accountId: ACC_A }),
      a2: identity({ id: "a2", email: "*@example.com", accountId: ACC_A, isWildcard: true }),
    };
    expect(identityPrecedence(record)[0]?.entries.map((e) => e.reason)).toEqual([
      "primary",
      "catch-all",
    ]);
  });

  it("is empty for no identities, which is a state and not an error", () => {
    expect(identityPrecedence([])).toEqual([]);
    expect(identityPrecedence({})).toEqual([]);
  });
});
