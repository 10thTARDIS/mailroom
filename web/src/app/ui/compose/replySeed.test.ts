// SPDX-License-Identifier: AGPL-3.0-only

// What a reply/reply-all composer opens with (#646). `replyRecipients` itself
// is pinned in recipients.test.ts; this only tests the seam that decides
// WHETHER to call it, and with which mode.

import { describe, expect, it } from "vitest";

import { initialReplyFields } from "./replySeed";
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
const IDS = [me];

const a = (email: string): EmailAddress => ({ email, name: null });
const addrs = (list: { address: string }[]): string[] => list.map((c) => c.address);

describe("initialReplyFields", () => {
  it("is empty for a non-reply intent (new, forward, draft)", () => {
    expect(initialReplyFields(null, { from: [a("bob@example.org")] }, IDS)).toEqual({
      to: [],
      cc: [],
    });
  });

  it("is empty when the parent has not resolved yet", () => {
    expect(initialReplyFields("reply", null, IDS)).toEqual({ to: [], cc: [] });
    expect(initialReplyFields("replyAll", null, IDS)).toEqual({ to: [], cc: [] });
  });

  it("a plain reply seeds To and leaves Cc empty", () => {
    const seed = initialReplyFields(
      "reply",
      { from: [a("bob@example.org")], to: [a("me@example.com")], cc: [a("carol@example.org")] },
      IDS,
    );
    expect(addrs(seed.to)).toEqual(["bob@example.org"]);
    expect(seed.cc).toEqual([]);
  });

  it("a reply-all seeds Cc from the parent's Cc too", () => {
    const seed = initialReplyFields(
      "replyAll",
      { from: [a("bob@example.org")], to: [a("me@example.com")], cc: [a("carol@example.org")] },
      IDS,
    );
    expect(addrs(seed.to)).toEqual(["bob@example.org"]);
    expect(addrs(seed.cc)).toEqual(["carol@example.org"]);
  });
});
