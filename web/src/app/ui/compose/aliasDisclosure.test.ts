// SPDX-License-Identifier: AGPL-3.0-only

// #533: when does sending from an alias disclose the scheme to someone new?
//
// THE SILENT CASES CARRY THIS FILE. A hint that fires on every reply is the
// failure mode the issue names ("a warning shown on every reply is a warning
// nobody reads"), and on a catchall almost every reply goes out as an alias —
// so a rule that cannot stay quiet is indistinguishable from no rule at all.
// The positive cases are two: a forward, and a recipient added to a reply.

import { describe, expect, it } from "vitest";

import { aliasDisclosure } from "./aliasDisclosure";
import type { AliasDisclosureInput } from "./aliasDisclosure";

const BANK = "notices@bank.example";
const ALIAS = "bofa@catchall.example";

function input(over: Partial<AliasDisclosureInput> = {}): AliasDisclosureInput {
  return {
    alias: ALIAS,
    isAlias: true,
    recipients: [BANK],
    parentParticipants: [BANK],
    ownDomains: ["catchall.example"],
    ...over,
  };
}

describe("aliasDisclosure — silent", () => {
  it("says nothing on a plain reply: that correspondent is who used the alias", () => {
    expect(aliasDisclosure(input())).toBeNull();
  });

  it("says nothing when the From is the primary address", () => {
    expect(
      aliasDisclosure(input({ isAlias: false, recipients: ["someone@example.org"], parentParticipants: [] })),
    ).toBeNull();
  });

  it("says nothing for a fresh compose, where the alias is being chosen deliberately", () => {
    expect(
      aliasDisclosure(input({ parentParticipants: null, recipients: ["someone@example.org"] })),
    ).toBeNull();
  });

  it("says nothing about recipients at the user's own domains", () => {
    // The round-trip test address (pgptest@) lives here; warning on it every time
    // would train the operator to ignore the line.
    expect(
      aliasDisclosure(input({ recipients: [BANK, "pgptest@catchall.example"] })),
    ).toBeNull();
  });

  it("says nothing when the alias is unsettled or there are no recipients", () => {
    expect(aliasDisclosure(input({ alias: "  " }))).toBeNull();
    expect(aliasDisclosure(input({ recipients: [] }))).toBeNull();
  });

  it("ignores case and surrounding space when matching a known participant", () => {
    expect(
      aliasDisclosure(input({ recipients: ["  Notices@Bank.Example "], parentParticipants: [BANK] })),
    ).toBeNull();
  });

  it("does not count the alias itself as a new reader when it is on the thread", () => {
    expect(aliasDisclosure(input({ recipients: [BANK, ALIAS.toUpperCase()] }))).toBeNull();
  });
});

describe("aliasDisclosure — speaks up", () => {
  it("names the recipient of a FORWARD, who has never seen this address", () => {
    expect(
      aliasDisclosure(input({ recipients: ["alice@example.org"], parentParticipants: [BANK] })),
    ).toEqual({ alias: ALIAS, newRecipients: ["alice@example.org"] });
  });

  it("names ONLY the recipient added to a reply, not the correspondent who wrote", () => {
    expect(
      aliasDisclosure(input({ recipients: [BANK, "carol@example.org"] })),
    ).toEqual({ alias: ALIAS, newRecipients: ["carol@example.org"] });
  });

  it("counts Cc and Bcc recipients too — the caller passes one list", () => {
    expect(
      aliasDisclosure(input({ recipients: [BANK, "b@example.org", "c@example.net"] })),
    ).toEqual({ alias: ALIAS, newRecipients: ["b@example.org", "c@example.net"] });
  });

  it("lists each new reader once, in the order they appear", () => {
    expect(
      aliasDisclosure(
        input({ recipients: ["dup@example.org", "other@example.org", "DUP@example.org"] }),
      ),
    ).toEqual({ alias: ALIAS, newRecipients: ["dup@example.org", "other@example.org"] });
  });

  it("speaks up for a thread whose To/Cc the client never loaded — conservative, not silent", () => {
    // A reply opens on what the LIST loaded: `from` only. Someone who was on the
    // original Cc can therefore look new. That is a line too many, never a
    // disclosure too few.
    expect(
      aliasDisclosure(input({ recipients: [BANK, "wascc@example.org"], parentParticipants: [BANK] })),
    ).toEqual({ alias: ALIAS, newRecipients: ["wascc@example.org"] });
  });
});
