// SPDX-License-Identifier: AGPL-3.0-only

// Recipient chips: parsing pasted lists, and the one key fact a chip prints
// (#448).
//
// The `chipKeyFact` tests are the load-bearing ones. Three of the four inputs
// look alike and mean different things, and getting any of them wrong prints a
// claim about a stranger's cryptographic state that nobody made:
//
//   undefined  the lookup has not answered              → say nothing
//   null       the lookup answered: no such contact     → "no key"
//   {keys: []} the server answered: no keys             → "no key"
//   {}         the server COULD NOT answer (keys absent) → say nothing

import { describe, expect, it } from "vitest";

import { addChips, chipKeyFact, chipLabel, looksLikeAddress, parseChip, parseChips } from "./chips";
import type { Contact } from "../../../core/jmap/types";

function contact(over: Partial<Contact> = {}): Contact {
  return {
    id: "c1",
    displayName: "A friend",
    addresses: [{ address: "friend@example.com", reachedMeAt: null }],
    ...over,
  };
}

describe("chipKeyFact", () => {
  it("is 'held' when the contact has a key", () => {
    expect(
      chipKeyFact(
        contact({
          keys: [{ fingerprint: "AAAA BBBB", kind: "pgp", source: "wkd", fetchedAt: "2026-01-01T00:00:00Z" }],
        }),
      ),
    ).toBe("held");
  });

  it("is 'none' when the server answered with an empty key list", () => {
    expect(chipKeyFact(contact({ keys: [] }))).toBe("none");
  });

  // ⚠ THE #176 PAIR. `keys` absent means the server was not asked or cannot
  // answer; treating it as an empty list would print "no key" for every
  // correspondent on a server that has never been able to say.
  it("is 'unknown' when the contact carries no keys property at all", () => {
    expect(chipKeyFact(contact())).toBe("unknown");
  });

  it("is 'unknown' before the lookup has answered, and 'none' on a miss", () => {
    expect(chipKeyFact(undefined)).toBe("unknown");
    expect(chipKeyFact(null)).toBe("none");
  });

  it("distinguishes all four inputs — no two collapse", () => {
    const facts = [
      chipKeyFact(undefined),
      chipKeyFact(null),
      chipKeyFact(contact()),
      chipKeyFact(contact({ keys: [] })),
    ];
    expect(facts).toEqual(["unknown", "none", "unknown", "none"]);
  });
});

describe("parseChip", () => {
  it("takes a display name out of angle brackets", () => {
    expect(parseChip("Ada Lovelace <ada@example.com>")).toEqual({
      address: "ada@example.com",
      name: "Ada Lovelace",
    });
  });

  it("strips the quotes RFC 5322 puts round a name", () => {
    expect(parseChip('"Lovelace, Ada" <ada@example.com>')).toEqual({
      address: "ada@example.com",
      name: "Lovelace, Ada",
    });
  });

  it("reads a bare address, with no name", () => {
    expect(parseChip("  ada@example.com , ")).toEqual({ address: "ada@example.com", name: null });
  });

  it("is null for nothing at all", () => {
    expect(parseChip("   ")).toBeNull();
  });
});

describe("parseChips", () => {
  it("turns a pasted comma list into chips and keeps the unfinished tail", () => {
    const { chips, rest } = parseChips("a@example.com, b@example.com, c@ex");
    expect(chips.map((c) => c.address)).toEqual(["a@example.com", "b@example.com"]);
    expect(rest).toBe("c@ex");
  });

  it("chips everything when the input ends in a separator", () => {
    const { chips, rest } = parseChips("a@example.com; b@example.com;");
    expect(chips.map((c) => c.address)).toEqual(["a@example.com", "b@example.com"]);
    expect(rest).toBe("");
  });

  it("splits on newlines, which is what a paste out of a mail header carries", () => {
    const { chips } = parseChips("a@example.com\nAda <ada@example.com>\n");
    expect(chips).toEqual([
      { address: "a@example.com", name: null },
      { address: "ada@example.com", name: "Ada" },
    ]);
  });

  // ⚠ THE CONTROL FOR THE WHITESPACE FALLBACK. A display name has spaces in it,
  // so splitting on whitespace unconditionally would turn one recipient into
  // three broken ones.
  it("does not split a display name on its spaces", () => {
    const { chips } = parseChips("Ada Lovelace <ada@example.com>,");
    expect(chips).toEqual([{ address: "ada@example.com", name: "Ada Lovelace" }]);
  });

  it("does split a whitespace-separated list of bare addresses", () => {
    const { chips } = parseChips("a@example.com b@example.com,");
    expect(chips.map((c) => c.address)).toEqual(["a@example.com", "b@example.com"]);
  });
});

describe("addChips", () => {
  it("does not add an address that is already there, whatever its case", () => {
    const out = addChips([{ address: "a@example.com", name: "First" }], [
      { address: "A@Example.com", name: "Second" },
      { address: "b@example.com", name: null },
    ]);
    expect(out).toEqual([
      { address: "a@example.com", name: "First" },
      { address: "b@example.com", name: null },
    ]);
  });

  it("drops a duplicate inside the added batch too", () => {
    const out = addChips([], [
      { address: "a@example.com", name: null },
      { address: "a@example.com", name: null },
    ]);
    expect(out).toHaveLength(1);
  });
});

describe("chipLabel and looksLikeAddress", () => {
  it("prefers the name, falling back to the address", () => {
    expect(chipLabel({ address: "a@example.com", name: "Ada" })).toBe("Ada");
    expect(chipLabel({ address: "a@example.com", name: "  " })).toBe("a@example.com");
    expect(chipLabel({ address: "a@example.com", name: null })).toBe("a@example.com");
  });

  it("accepts a plain address and rejects a fragment", () => {
    expect(looksLikeAddress("a@example.com")).toBe(true);
    expect(looksLikeAddress("a@")).toBe(false);
    expect(looksLikeAddress("@example.com")).toBe(false);
    expect(looksLikeAddress("a@b@c")).toBe(false);
    expect(looksLikeAddress("a b@example.com")).toBe(false);
  });
});
