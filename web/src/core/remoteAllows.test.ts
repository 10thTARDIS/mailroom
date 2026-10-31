// SPDX-License-Identifier: AGPL-3.0-only

// Tests for "does this sender's remote content load without asking" (#202).
//
// This is a privacy decision, so the assertions that matter are the NEGATIVES: the
// inputs that must NOT resolve to "load it". A false positive here means a tracking
// pixel fires for a sender the user never approved — the exact outcome the blocking
// default exists to prevent — and it would be invisible, because loaded images look
// like a feature working.

import { describe, expect, it } from "vitest";

import {
  addressDomain,
  isRemoteAllowed,
  matchingRemoteAllow,
  normalizeAddress,
} from "./remoteAllows";

const addr = (value: string) => ({ id: "a" + value, kind: "address", value });
const domain = (value: string) => ({ id: "d" + value, kind: "domain", value });

describe("isRemoteAllowed — fails closed", () => {
  it("blocks when there are no grants at all", () => {
    expect(isRemoteAllowed("news@example.com", [])).toBe(false);
    expect(isRemoteAllowed("news@example.com", null)).toBe(false);
    expect(isRemoteAllowed("news@example.com", undefined)).toBe(false);
  });

  it("blocks when the sender is missing or malformed", () => {
    const allows = [addr("news@example.com"), domain("example.com")];
    expect(isRemoteAllowed(null, allows)).toBe(false);
    expect(isRemoteAllowed(undefined, allows)).toBe(false);
    expect(isRemoteAllowed("", allows)).toBe(false);
    expect(isRemoteAllowed("   ", allows)).toBe(false);
    // No domain part: it can never match a domain grant, and must not match one by
    // accident through an empty-string comparison.
    expect(isRemoteAllowed("not-an-address", allows)).toBe(false);
    expect(isRemoteAllowed("trailing@", allows)).toBe(false);
  });

  it("ignores a grant with an unrecognised kind rather than guessing", () => {
    // Forward compatibility that fails safe: if a future server adds a kind, an
    // older client must not treat it as a match — least of all as a wildcard.
    expect(isRemoteAllowed("news@example.com", [{ id: "x", kind: "all", value: "*" }])).toBe(false);
    expect(isRemoteAllowed("news@example.com", [{ id: "x", kind: "", value: "example.com" }])).toBe(
      false,
    );
  });

  it("ignores an empty grant value", () => {
    expect(isRemoteAllowed("news@example.com", [{ id: "x", kind: "domain", value: "" }])).toBe(
      false,
    );
    expect(isRemoteAllowed("news@example.com", [{ id: "x", kind: "address", value: "  " }])).toBe(
      false,
    );
  });

  it("does not treat a domain grant as a suffix match", () => {
    // THE ONE THAT WOULD HURT. "example.com" must not match
    // "news@notexample.com" or "news@evil-example.com" — a suffix comparison here
    // would hand an attacker every domain ending in an allowed one.
    const allows = [domain("example.com")];
    expect(isRemoteAllowed("news@notexample.com", allows)).toBe(false);
    expect(isRemoteAllowed("news@evil-example.com", allows)).toBe(false);
    expect(isRemoteAllowed("news@example.com.evil.test", allows)).toBe(false);
    // Nor a SUBdomain: mail.example.com is a different sending host, and granting
    // the parent was not a grant of every child.
    expect(isRemoteAllowed("news@mail.example.com", allows)).toBe(false);
  });

  it("does not let an address grant match the whole domain", () => {
    const allows = [addr("news@example.com")];
    expect(isRemoteAllowed("other@example.com", allows)).toBe(false);
  });

  it("does not let a domain grant be satisfied by an address grant's text", () => {
    // A grant stored as an ADDRESS must never match as a domain, even though the
    // string contains one.
    expect(isRemoteAllowed("news@example.com", [domain("news@example.com")])).toBe(false);
  });
});

describe("isRemoteAllowed — the cases that should load", () => {
  it("matches an exact address, case- and whitespace-insensitively", () => {
    const allows = [addr("news@example.com")];
    expect(isRemoteAllowed("news@example.com", allows)).toBe(true);
    expect(isRemoteAllowed("News@Example.com", allows)).toBe(true);
    expect(isRemoteAllowed("  news@example.com  ", allows)).toBe(true);
    // ...and stored the other way round, since the column is citext.
    expect(isRemoteAllowed("news@example.com", [addr("NEWS@EXAMPLE.COM")])).toBe(true);
  });

  it("matches any sender at a granted domain", () => {
    const allows = [domain("example.com")];
    expect(isRemoteAllowed("news@example.com", allows)).toBe(true);
    expect(isRemoteAllowed("someone-else@example.com", allows)).toBe(true);
    expect(isRemoteAllowed("A@EXAMPLE.COM", allows)).toBe(true);
  });

  it("matches the rotating-subdomain case each grant was made for", () => {
    // The reason both kinds exist: one publisher reaches this mailbox from several
    // hosts, so a per-host grant is what an address-only scheme would demand three
    // times over.
    const allows = [domain("mail.vendor.example"), domain("email.vendor-alt.example")];
    expect(isRemoteAllowed("noreply@mail.vendor.example", allows)).toBe(true);
    expect(isRemoteAllowed("noreply@email.vendor-alt.example", allows)).toBe(true);
    expect(isRemoteAllowed("noreply@vendor.example", allows)).toBe(false);
  });
});

describe("matchingRemoteAllow — why images loaded", () => {
  it("returns the grant so the pane can say which one it was", () => {
    const a = addr("news@example.com");
    expect(matchingRemoteAllow("news@example.com", [a])?.id).toBe(a.id);
  });

  it("prefers the exact address over the domain, as the more specific reason", () => {
    const a = addr("news@example.com");
    const d = domain("example.com");
    // Both match; the user is likelier to recognise the address they approved.
    expect(matchingRemoteAllow("news@example.com", [d, a])?.id).toBe(a.id);
  });

  it("returns null when nothing matches, and agrees with isRemoteAllowed", () => {
    const allows = [domain("example.com")];
    for (const from of ["x@other.test", "", null, undefined, "broken"]) {
      expect(matchingRemoteAllow(from, allows)).toBeNull();
      expect(isRemoteAllowed(from, allows)).toBe(false);
    }
  });
});

describe("addressDomain / normalizeAddress", () => {
  it("takes the domain after the LAST @, matching the server's projection", () => {
    // internal/store.AddressDomain uses the last @ too. A disagreement would store
    // grants that never match.
    expect(addressDomain("a@b@example.com")).toBe("example.com");
    expect(addressDomain("plain@example.com")).toBe("example.com");
    expect(addressDomain("UPPER@Example.COM")).toBe("example.com");
  });

  it("has no domain for input that has none", () => {
    expect(addressDomain("no-at-sign")).toBe("");
    expect(addressDomain("trailing@")).toBe("");
    expect(addressDomain("")).toBe("");
  });

  it("normalises to trimmed lowercase", () => {
    expect(normalizeAddress("  Foo@Bar.test ")).toBe("foo@bar.test");
    expect(normalizeAddress(null)).toBe("");
    expect(normalizeAddress(undefined)).toBe("");
  });
});
