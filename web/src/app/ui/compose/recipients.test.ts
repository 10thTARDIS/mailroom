// SPDX-License-Identifier: AGPL-3.0-only

// Who a reply goes to (#646). Pure-function tests, like the rest of compose's
// decisions (see from.test.ts): no DOM test harness is installed here.

import { describe, expect, it } from "vitest";

import { isOwnAddress, replyRecipients, type ReplyParent } from "./recipients";
import type { EmailAddress, Identity } from "../../../core/jmap/types";

const ACC = "01JBQZ3K7WX2N4Y6P8R0S1T2A0";

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

const me = identity({ id: "i-me", email: "me@example.com" });
const catchAll = identity({ id: "i-wild", email: "*@alias.example", isWildcard: true });
const retired = identity({ id: "i-old", email: "old@example.com", enabled: false });
const IDS = [me, catchAll, retired];

const a = (email: string, name: string | null = null): EmailAddress => ({ email, name });
const addrs = (list: { address: string }[]): string[] => list.map((c) => c.address);

describe("isOwnAddress", () => {
  it("matches a concrete identity, case-insensitively", () => {
    expect(isOwnAddress("Me@Example.com", IDS)).toBe(true);
    expect(isOwnAddress("someone@example.com", IDS)).toBe(false);
  });

  it("matches EVERY local part at a catch-all's domain", () => {
    expect(isOwnAddress("bank@alias.example", IDS)).toBe(true);
    expect(isOwnAddress("anything-at-all@ALIAS.example", IDS)).toBe(true);
    // ...and only that domain: a subdomain or lookalike is someone else.
    expect(isOwnAddress("x@mail.alias.example", IDS)).toBe(false);
    expect(isOwnAddress("x@alias.example.evil", IDS)).toBe(false);
  });

  it("counts a disabled identity: unable to send as it is not 'not mine'", () => {
    expect(isOwnAddress("old@example.com", IDS)).toBe(true);
  });
});

describe("replyRecipients — Reply", () => {
  it("goes to Reply-To when there is one, not to From (RFC 5322 §3.6.2)", () => {
    const p: ReplyParent = {
      from: [a("alice@list.example", "Alice")],
      replyTo: [a("list@list.example", "The List")],
      to: [a("list@list.example")],
    };
    const r = replyRecipients(p, IDS, "reply");
    expect(addrs(r.to)).toEqual(["list@list.example"]);
    expect(r.to[0]?.name).toBe("The List");
    expect(r.cc).toEqual([]);
  });

  it("goes to From when there is no Reply-To", () => {
    const r = replyRecipients({ from: [a("bob@example.org", "Bob")], replyTo: null }, IDS, "reply");
    expect(addrs(r.to)).toEqual(["bob@example.org"]);
    expect(r.to[0]?.name).toBe("Bob");
  });

  it("treats an EMPTY Reply-To as absent", () => {
    const r = replyRecipients({ from: [a("bob@example.org")], replyTo: [] }, IDS, "reply");
    expect(addrs(r.to)).toEqual(["bob@example.org"]);
  });

  it("uses every Reply-To address, not just the first", () => {
    const r = replyRecipients(
      { from: [a("x@example.org")], replyTo: [a("r1@example.org"), a("r2@example.org")] },
      IDS,
      "reply",
    );
    expect(addrs(r.to)).toEqual(["r1@example.org", "r2@example.org"]);
  });

  it("does NOT add the other recipients (that is Reply all)", () => {
    const r = replyRecipients(
      { from: [a("bob@example.org")], to: [a("me@example.com"), a("carol@example.org")], cc: [a("dan@example.org")] },
      IDS,
      "reply",
    );
    expect(addrs(r.to)).toEqual(["bob@example.org"]);
    expect(r.cc).toEqual([]);
  });

  it("continues a message the user SENT, to its recipients, not to themselves", () => {
    const r = replyRecipients(
      { from: [a("me@example.com")], to: [a("carol@example.org", "Carol")] },
      IDS,
      "reply",
    );
    expect(addrs(r.to)).toEqual(["carol@example.org"]);
  });

  it("treats mail sent from one of the user's catch-all aliases as theirs too", () => {
    const r = replyRecipients(
      { from: [a("bank@alias.example")], to: [a("support@bank.example")] },
      IDS,
      "reply",
    );
    expect(addrs(r.to)).toEqual(["support@bank.example"]);
  });

  it("honours a Reply-To the user set on their own message", () => {
    const r = replyRecipients(
      { from: [a("me@example.com")], replyTo: [a("team@example.org")], to: [a("carol@example.org")] },
      IDS,
      "reply",
    );
    expect(addrs(r.to)).toEqual(["team@example.org"]);
  });
});

describe("replyRecipients — Reply all", () => {
  it("adds the parent's To to To and its Cc to Cc", () => {
    const r = replyRecipients(
      {
        from: [a("bob@example.org")],
        to: [a("carol@example.org")],
        cc: [a("dan@example.org")],
      },
      IDS,
      "replyAll",
    );
    expect(addrs(r.to)).toEqual(["bob@example.org", "carol@example.org"]);
    expect(addrs(r.cc)).toEqual(["dan@example.org"]);
  });

  it("removes the user's own addresses, concrete AND every catch-all alias", () => {
    const r = replyRecipients(
      {
        from: [a("bob@example.org")],
        to: [a("bank@alias.example"), a("carol@example.org"), a("ME@example.com")],
        cc: [a("other-alias@alias.example"), a("dan@example.org"), a("old@example.com")],
      },
      IDS,
      "replyAll",
    );
    expect(addrs(r.to)).toEqual(["bob@example.org", "carol@example.org"]);
    expect(addrs(r.cc)).toEqual(["dan@example.org"]);
  });

  it("collapses duplicates across the headers, keeping the first occurrence", () => {
    const r = replyRecipients(
      {
        from: [a("bob@example.org", "Bob")],
        to: [a("BOB@example.org", "bob (again)"), a("carol@example.org")],
        cc: [a("carol@example.org"), a("dan@example.org"), a("dan@example.org")],
      },
      IDS,
      "replyAll",
    );
    expect(addrs(r.to)).toEqual(["bob@example.org", "carol@example.org"]);
    expect(r.to[0]?.name).toBe("Bob");
    expect(addrs(r.cc)).toEqual(["dan@example.org"]);
  });

  it("on a mailing list, addresses the list via Reply-To and keeps the rest", () => {
    const r = replyRecipients(
      {
        from: [a("alice@list.example")],
        replyTo: [a("list@list.example")],
        to: [a("list@list.example")],
        cc: [a("carol@example.org")],
      },
      IDS,
      "replyAll",
    );
    expect(addrs(r.to)).toEqual(["list@list.example"]);
    expect(addrs(r.cc)).toEqual(["carol@example.org"]);
  });
});

describe("replyRecipients — never addressed to nobody", () => {
  it("a note to self replies to self rather than opening empty", () => {
    const r = replyRecipients({ from: [a("me@example.com")], to: [a("me@example.com")] }, IDS, "reply");
    expect(addrs(r.to)).toEqual(["me@example.com"]);
  });

  it("a reply-all to a note to self that Cc'd someone goes to them", () => {
    const r = replyRecipients(
      { from: [a("me@example.com")], to: [a("me@example.com")], cc: [a("carol@example.org")] },
      IDS,
      "replyAll",
    );
    expect(addrs(r.to)).toEqual(["carol@example.org"]);
    expect(r.cc).toEqual([]);
  });

  it("the user's own Bcc-only send falls back to its From", () => {
    const r = replyRecipients({ from: [a("me@example.com")], to: [] }, IDS, "reply");
    expect(addrs(r.to)).toEqual(["me@example.com"]);
  });

  it("drops blank addresses rather than making empty chips", () => {
    const r = replyRecipients({ from: [a("  "), a("bob@example.org")] }, IDS, "reply");
    expect(addrs(r.to)).toEqual(["bob@example.org"]);
  });
});
