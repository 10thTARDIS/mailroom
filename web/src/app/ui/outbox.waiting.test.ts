// SPDX-License-Identifier: AGPL-3.0-only

// A held submission (#52) is WAITING, not "Sending…" — read from the server's
// own flag (EmailSubmission.mailroomHeld), never from the reason text — and the
// row names the account it goes out on.

import { describe, expect, it } from "vitest";

import { outboxRows } from "./outbox";
import type { EmailSubmission, Identity } from "../../core/jmap/types";

const NOW = Date.parse("2026-10-27T12:00:00Z");
const ACC = "01JB00000000000000000000A1";
const S1 = "01JBQZ3K7WX2N4Y6P8R0S00001";

const identities: Record<string, Identity> = {
  i1: {
    id: "i1", email: "me@example.com", name: "", replyTo: null, bcc: null, textSignature: "",
    htmlSignature: "", mayDelete: true, accountId: ACC, isWildcard: false, enabled: true,
  } as Identity,
};

function sub(over: Partial<EmailSubmission>): EmailSubmission {
  return {
    id: S1, identityId: "i1", emailId: "e1", threadId: null,
    // Past its instant: without the flag this is "sending".
    sendAt: new Date(NOW - 60_000).toISOString(), undoStatus: "pending", mailroomState: "scheduled",
    ...over,
  };
}
const stateOf = (s: EmailSubmission) => outboxRows({ [s.id]: s }, {}, identities, NOW)[0];

describe("a held submission in the Outbox", () => {
  it("is waiting for a reconnect, or for the provider, from the server's flag", () => {
    expect(stateOf(sub({ mailroomHeld: "reconnect" }))).toMatchObject({
      state: { kind: "waiting", why: "reconnect" }, accountId: ACC,
    });
    expect(stateOf(sub({ mailroomHeld: "provider" }))?.state).toEqual({ kind: "waiting", why: "provider" });
  });

  it("is sending without the flag, whatever its stored reply says", () => {
    expect(stateOf(sub({ mailroomHeld: null }))?.state).toEqual({ kind: "sending" });
    expect(stateOf(sub({}))?.state).toEqual({ kind: "sending" });
    expect(
      stateOf(sub({ deliveryStatus: { "*": { smtpReply: "waiting: reconnect this account in Settings and it will be sent" } } }))?.state,
    ).toEqual({ kind: "sending" });
  });

  it("a failure is a failure even if a stale flag came with it", () => {
    expect(stateOf(sub({ mailroomState: "failed", undoStatus: "final", mailroomHeld: "reconnect" }))?.state.kind).toBe("failed");
  });
});
