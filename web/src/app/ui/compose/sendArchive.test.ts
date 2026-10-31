// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import {
  PANE_LEAVE_TTL_MS,
  archiveOnSuccessFor,
  offersSendAndArchive,
  paneLeaveVerdict,
  paneToLeaveAfterSend,
  parentInInbox,
  queuedToastKey,
  sentToastKey,
} from "./sendArchive";

describe("offersSendAndArchive", () => {
  it("offers it for a reply, reply-all or forward — every intent with a parent", () => {
    expect(offersSendAndArchive("reply", true)).toBe(true);
    expect(offersSendAndArchive("replyAll", true)).toBe(true);
    expect(offersSendAndArchive("forward", true)).toBe(true);
  });

  it("does not offer it for a brand-new message", () => {
    expect(offersSendAndArchive("new", true)).toBe(false);
  });

  // A resumed draft carries no memory of what it replied to (Compose.tsx's
  // `parent` is only ever set for reply/replyAll/forward), so it is treated
  // exactly like `new` rather than guessed at.
  it("does not offer it for a resumed draft", () => {
    expect(offersSendAndArchive("draft", true)).toBe(false);
  });
});

// The client no longer archives or restores anything itself (#648): the server
// archives once the message is SENT. What remains client-side is what to ask
// for and what the toast may honestly claim.
describe("archiveOnSuccessFor", () => {
  it("asks for the parent when Send & archive was used", () => {
    expect(archiveOnSuccessFor(true, "P1")).toEqual(["P1"]);
  });

  it("asks for nothing on a plain Send, or with no parent to archive", () => {
    expect(archiveOnSuccessFor(false, "P1")).toBeUndefined();
    expect(archiveOnSuccessFor(true, null)).toBeUndefined();
  });
});

describe("the send toast never says 'archived'", () => {
  // The message is still in its undo window: only a FUTURE archive can be
  // promised, and only on the server's own echo of the request.
  it("promises the archive only when the server acknowledged it", () => {
    expect(sentToastKey(true)).toBe("compose.sentWillArchive");
    expect(sentToastKey(false)).toBe("compose.sent");
  });

  it("says the same for a send held offline", () => {
    expect(queuedToastKey(true)).toBe("compose.queuedWillArchive");
    expect(queuedToastKey(false)).toBe("compose.queued");
  });
});

// #648 review: the button is only an "archive" when there is an Inbox copy to
// clear. Without this, Send & archive on a reply to your own message from Sent
// moved the sent original out of Sent, and an undone send then "restored" it
// into the Inbox, where it had never been.
describe("offersSendAndArchive only for a parent in the Inbox", () => {
  it("withholds it when the parent is not in the Inbox, whatever the intent", () => {
    expect(offersSendAndArchive("reply", false)).toBe(false);
    expect(offersSendAndArchive("replyAll", false)).toBe(false);
    expect(offersSendAndArchive("forward", false)).toBe(false);
  });
});

describe("parentInInbox", () => {
  const boxes = {
    in: { role: "inbox" },
    sent: { role: "sent" },
    arc: { role: "archive" },
    lbl: { role: null },
  };

  it("is true when the server lists an inbox-role folder among its mailboxes", () => {
    expect(parentInInbox({ mailboxIds: { in: true, lbl: true }, localMove: null }, boxes)).toBe(true);
  });

  it("is false for a message in Sent, Archive or a plain folder", () => {
    expect(parentInInbox({ mailboxIds: { sent: true }, localMove: null }, boxes)).toBe(false);
    expect(parentInInbox({ mailboxIds: { arc: true }, localMove: null }, boxes)).toBe(false);
    expect(parentInInbox({ mailboxIds: { lbl: true }, localMove: null }, boxes)).toBe(false);
  });

  it("lets a pending local move decide over the server's mailboxIds", () => {
    expect(parentInInbox({ mailboxIds: { in: true }, localMove: "archive" }, boxes)).toBe(false);
    expect(parentInInbox({ mailboxIds: { arc: true }, localMove: "inbox" }, boxes)).toBe(true);
  });

  it("is false with no parent, or before mailboxes have loaded", () => {
    expect(parentInInbox(null, boxes)).toBe(false);
    expect(parentInInbox({ mailboxIds: { in: true }, localMove: null }, {})).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// #648: after Send & archive the reading pane stayed open on the conversation
// the send had just closed out. It now leaves it the way `e` does.

describe("paneToLeaveAfterSend", () => {
  const parent = { id: "P1", threadId: "T1" };

  it("leaves the pane when it shows the parent", () => {
    expect(paneToLeaveAfterSend({ archiving: true, parent, open: { id: "P1", threadId: "T1" } })).toBe("P1");
  });

  it("leaves the pane when it shows another message of the parent's conversation", () => {
    expect(paneToLeaveAfterSend({ archiving: true, parent, open: { id: "M2", threadId: "T1" } })).toBe("M2");
  });

  // THE NEGATIVE CONTROLS. Each of these is a send that must not touch the pane.
  it("does nothing after a plain Send, even on the parent's own pane", () => {
    expect(paneToLeaveAfterSend({ archiving: false, parent, open: { id: "P1", threadId: "T1" } })).toBeNull();
  });

  it("does nothing when the open pane is a different conversation", () => {
    expect(paneToLeaveAfterSend({ archiving: true, parent, open: { id: "X9", threadId: "T9" } })).toBeNull();
  });

  it("does nothing when no message is open", () => {
    expect(paneToLeaveAfterSend({ archiving: true, parent, open: { id: null, threadId: null } })).toBeNull();
  });

  it("does nothing without a parent (a new message or a resumed draft)", () => {
    expect(paneToLeaveAfterSend({ archiving: true, parent: null, open: { id: "P1", threadId: "T1" } })).toBeNull();
  });

  // An unknown conversation is not a match: a null threadId on both sides must
  // not read as "the same thread".
  it("does not match two unknown conversations to each other", () => {
    expect(
      paneToLeaveAfterSend({ archiving: true, parent: { id: "P1", threadId: null }, open: { id: "X9", threadId: null } }),
    ).toBeNull();
  });
});

describe("paneLeaveVerdict", () => {
  const note = { openId: "P1", at: 1_000 };

  it("has nothing to do without a note", () => {
    expect(paneLeaveVerdict(null, { openId: "P1", composing: false, at: 1_000 })).toBe("none");
  });

  // Acting while the composer is still in the URL would carry the composer
  // along with the pane, and closing it afterwards would go back.
  it("waits while the composer is still open", () => {
    expect(paneLeaveVerdict(note, { openId: "P1", composing: true, at: 1_001 })).toBe("wait");
  });

  it("leaves once the composer has closed onto the same message", () => {
    expect(paneLeaveVerdict(note, { openId: "P1", composing: false, at: 1_050 })).toBe("leave");
  });

  it("drops the note when the pane is on another message by then", () => {
    expect(paneLeaveVerdict(note, { openId: "X9", composing: false, at: 1_050 })).toBe("drop");
    expect(paneLeaveVerdict(note, { openId: null, composing: false, at: 1_050 })).toBe("drop");
  });

  // A note written where no list surface was mounted must not fire later, when
  // the user happens to open that message again.
  it("drops a stale note", () => {
    expect(paneLeaveVerdict(note, { openId: "P1", composing: false, at: 1_000 + PANE_LEAVE_TTL_MS + 1 })).toBe("drop");
  });
});
