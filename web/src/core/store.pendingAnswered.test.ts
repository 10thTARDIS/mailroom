// SPDX-License-Identifier: AGPL-3.0-only

// "Change not yet confirmed by the server" must mean exactly that. The
// `answered` overlay flag (a message left the needs-a-category queue) is a local
// queue note that only a fresh query of that queue retires, so it must not make
// a row look unconfirmed. In practice, relabelled-and-archived messages
// all carried the pending dot after every write had been applied.

import { describe, expect, it } from "vitest";

import type { Email } from "./jmap/types";
import { MailStore, effectiveEmail } from "./store";

function email(id: string): Email {
  return {
    id,
    blobId: "b",
    threadId: "t",
    mailboxIds: { inbox: true },
    keywords: {},
    size: 1,
    receivedAt: "2026-10-01T00:00:00Z",
    subject: id,
    preview: "",
    hasAttachment: false,
  } as unknown as Email;
}

describe("EffectiveEmail.pending", () => {
  it("is false for a message whose only overlay entry is `answered`", () => {
    const store = new MailStore(null);
    store.putEmails([email("m1")]);
    store.applyAnswered(["m1"]);
    const state = store.getState();
    // The flag is really there (otherwise this test proves nothing)...
    expect(state.overlay["m1"]?.answered).toBe(true);
    // ...and it does not make the row read as unconfirmed.
    expect(effectiveEmail(state, "m1")?.pending).toBe(false);
  });

  it("is still true while a server-confirmable intent is outstanding (control)", () => {
    const store = new MailStore(null);
    store.putEmails([email("m1")]);
    store.applyAnswered(["m1"]);
    store.applyKeyword(["m1"], "$seen", true);
    expect(effectiveEmail(store.getState(), "m1")?.pending).toBe(true);
  });
});
