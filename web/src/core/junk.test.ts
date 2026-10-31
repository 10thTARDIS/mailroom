// SPDX-License-Identifier: AGPL-3.0-only

// "Not junk" (#528): which messages are in Junk, and what rescuing one sends.
//
// The case that matters most is the first: a message the provider filed into
// the Junk FOLDER without tagging it. That is every junk message on the provider
// this project runs against (measured, see core/junk.ts), and a check that only
// looked at the `$junk` keyword passed every other test here while offering the
// control for none of them.

import { beforeEach, describe, expect, it } from "vitest";

import { isInJunk, notJunkPatch } from "./junk";
import type { Email, Mailbox } from "./jmap/types";
import { KEYWORD_JUNK } from "./jmap/types";
import { MailStore } from "./store";

function mailbox(id: string, role: Mailbox["role"]): Mailbox {
  return {
    id,
    accountId: "U1",
    name: role ?? id,
    parentId: null,
    role,
    sortOrder: 1,
    totalEmails: 0,
    unreadEmails: 0,
    totalThreads: 0,
    unreadThreads: 0,
    isSubscribed: true,
  };
}

function email(id: string, over: Partial<Email> = {}): Email {
  return {
    id,
    blobId: "b",
    threadId: `t-${id}`,
    mailboxIds: {},
    keywords: {},
    size: 1,
    receivedAt: "2026-07-01T00:00:00Z",
    sentAt: null,
    subject: id,
    preview: "",
    hasAttachment: false,
    from: null,
    messageId: [],
    inReplyTo: [],
    references: [],
    deliveredTo: null,
    identityId: null,
    ...over,
  } as unknown as Email;
}

let store: MailStore;
beforeEach(() => {
  store = new MailStore(null);
  store.putMailboxes([mailbox("MB-I", "inbox"), mailbox("MB-J", "junk"), mailbox("MB-F", null)]);
});

describe("isInJunk", () => {
  it("is TRUE for a message in the Junk folder that carries no junk keyword (the Migadu shape)", () => {
    store.putEmails([email("m1", { mailboxIds: { "MB-J": true } })]);
    expect(isInJunk(store.getState(), "m1")).toBe(true);
  });

  it("is true for a message the provider TAGGED as junk, wherever it sits", () => {
    store.putEmails([email("m1", { mailboxIds: { "MB-I": true }, keywords: { [KEYWORD_JUNK]: true } })]);
    expect(isInJunk(store.getState(), "m1")).toBe(true);
  });

  it("is false for ordinary mail, including mail in a folder with no role", () => {
    store.putEmails([
      email("inbox", { mailboxIds: { "MB-I": true } }),
      email("folder", { mailboxIds: { "MB-F": true } }),
    ]);
    expect(isInJunk(store.getState(), "inbox")).toBe(false);
    expect(isInJunk(store.getState(), "folder")).toBe(false);
  });

  it("is true when ANY copy is in Junk — the server moves every account's copy by role", () => {
    store.putEmails([email("m1", { mailboxIds: { "MB-F": true, "MB-J": true } })]);
    expect(isInJunk(store.getState(), "m1")).toBe(true);
  });

  it("is false the moment the user has rescued it, before the server agrees", () => {
    // Without this the pane would keep offering "Not junk" for the message it just moved.
    store.putEmails([email("m1", { mailboxIds: { "MB-J": true } })]);
    store.applyMove(["m1"], "inbox");
    expect(isInJunk(store.getState(), "m1")).toBe(false);
  });

  it("is false for a message the client does not hold", () => {
    expect(isInJunk(store.getState(), "nope")).toBe(false);
  });
});

describe("notJunkPatch", () => {
  it("removes the verdict AND names the inbox, so a refusal can roll the move back", () => {
    expect(notJunkPatch("MB-I")).toEqual({
      [`keywords/${KEYWORD_JUNK}`]: null,
      "mailboxIds/MB-I": true,
    });
  });
});
