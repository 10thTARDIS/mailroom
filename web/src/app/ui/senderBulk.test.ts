// SPDX-License-Identifier: AGPL-3.0-only

// A sender's mail moved to Archive, Junk or Trash in one act (#59, #53).
//
// Real MailStore; the network (loadEmails) and the three moves are stand-ins
// that record what they were asked. What is pinned: unloaded mail is read
// first, mail already in the folder is counted and NOT moved again, the
// unreadable is counted, the move is the ordinary action, one Undo with the
// right inverse, and where "the rest" starts for each kind of target.

import { describe, expect, it, vi } from "vitest";

import type { Email, Id, Mailbox } from "../../core/jmap/types";
import { MailStore } from "../../core/store";
import { bulkText, moveSenderMail, restStart } from "./senderBulk";

function mailbox(id: string, role: Mailbox["role"]): Mailbox {
  return { id, accountId: "A1", name: id, parentId: null, role, sortOrder: 0, totalEmails: 0, unreadEmails: 0 } as Mailbox;
}
function email(id: string, mailboxIds: Id[]): Email {
  return {
    id,
    blobId: "b" + id,
    threadId: "t" + id,
    mailboxIds: Object.fromEntries(mailboxIds.map((m) => [m, true])),
    keywords: {},
    labelIds: {},
    size: 1,
    receivedAt: "2026-10-01T00:00:00Z",
    subject: id,
    preview: "",
    hasAttachment: false,
  } as unknown as Email;
}

function harness(loaded: Email[], onServer: Email[] = []) {
  const store = new MailStore(null);
  store.putMailboxes([mailbox("INBOX", "inbox"), mailbox("ARCH", "archive"), mailbox("JUNK", "junk"), mailbox("BIN", "trash")]);
  store.putEmails(loaded);
  const calls: Record<string, Id[][]> = { archive: [], moveToJunk: [], trash: [], restoreToInbox: [], untrash: [] };
  const rec = (name: string) =>
    vi.fn(async (ids: Id[]) => {
      calls[name]!.push(ids);
      return true;
    });
  const actions = {
    relabel: vi.fn(),
    archive: rec("archive"),
    moveToJunk: rec("moveToJunk"),
    trash: vi.fn(async (ids: Id[]) => void calls["trash"]!.push(ids)),
    restoreToInbox: rec("restoreToInbox"),
    untrash: rec("untrash"),
  };
  const loadEmails = vi.fn(async (ids: Id[]) => {
    const have = new Map(onServer.map((e) => [e.id, e]));
    store.putEmails(ids.flatMap((id) => (have.has(id) ? [have.get(id) as Email] : [])));
    return { notFound: ids.filter((id) => !have.has(id)), failed: false };
  });
  const undos: Array<{ label: string; inverse: () => Promise<boolean> }> = [];
  const toasts: string[] = [];
  const deps = {
    store,
    sync: { loadEmails },
    actions,
    offerUndo: (label: string, inverse: () => Promise<boolean>) => void undos.push({ label, inverse }),
    pushToast: (t: { message: string }) => (toasts.push(t.message), 1),
  };
  return { store, calls, loadEmails, undos, toasts, deps };
}

describe("moveSenderMail to a folder (#59, #53)", () => {
  it("archives what is not archived, counts what already is, reads the unloaded, one Undo to the Inbox", async () => {
    const h = harness(
      [email("m1", ["INBOX"]), email("m2", ["ARCH"])],
      [email("m3", ["INBOX"])], // on the server, not loaded yet
    );
    const out = await moveSenderMail(h.deps as never, ["m1", "m2", "m3", "gone"], { kind: "archive" });
    expect(h.loadEmails).toHaveBeenCalledWith(["m3", "gone"]);
    expect(out).toMatchObject({ looked: 4, moved: 2, alreadyThere: 1, unreadable: 1, truncated: false });
    expect(h.calls["archive"]).toEqual([["m1", "m3"]]);
    expect(h.undos).toHaveLength(1);
    expect(h.undos[0]!.label).toBe(
      "Moved 2 messages to Archive. 1 already in Archive. 1 could not be read and were skipped.",
    );
    await h.undos[0]!.inverse();
    expect(h.calls["restoreToInbox"]).toEqual([["m1", "m3"]]);
  });

  it("Junk is the ordinary move to Junk, and its Undo puts the mail back in the Inbox", async () => {
    const h = harness([email("m1", ["INBOX"]), email("m2", ["ARCH"])]);
    await moveSenderMail(h.deps as never, ["m1", "m2"], { kind: "junk" });
    expect(h.calls["moveToJunk"]).toEqual([["m1", "m2"]]);
    await h.undos[0]!.inverse();
    expect(h.calls["restoreToInbox"]).toEqual([["m1", "m2"]]);
  });

  it("Trash's Undo restores from the Trash (to Archive, as the server does), not to the Inbox", async () => {
    const h = harness([email("m1", ["INBOX"])]);
    await moveSenderMail(h.deps as never, ["m1"], { kind: "trash" });
    expect(h.calls["trash"]).toEqual([["m1"]]);
    await h.undos[0]!.inverse();
    expect(h.calls["untrash"]).toEqual([["m1"]]);
    expect(h.calls["restoreToInbox"]).toEqual([]);
  });

  it("nothing to move says so and offers no Undo", async () => {
    const h = harness([email("m1", ["ARCH"])]);
    const out = await moveSenderMail(h.deps as never, ["m1"], { kind: "archive" });
    expect(out?.moved).toBe(0);
    expect(h.calls["archive"]).toEqual([]);
    expect(h.undos).toHaveLength(0);
    expect(h.toasts).toEqual(["Nothing to move to Archive. 1 already in Archive."]);
  });

  it("a capped set says how many were not touched", async () => {
    const h = harness([email("m1", ["INBOX"])]);
    const out = await moveSenderMail(h.deps as never, ["m1"], { kind: "archive" }, 1200);
    expect(out).toMatchObject({ truncated: true, total: 1200 });
    expect(bulkText(out!, "Archive")).toBe("Moved 1 of 1,200 messages to Archive. 1,199 more were not touched.");
  });
});

describe("restStart", () => {
  it("continues after the page for targets that leave mail in the search, restarts at 0 for Junk and Trash", () => {
    expect(restStart({ kind: "area", areaId: "A" }, 1000, 1000)).toBe(2000);
    expect(restStart({ kind: "archive" }, 0, 1000)).toBe(1000);
    expect(restStart({ kind: "junk" }, 0, 1000)).toBe(0);
    expect(restStart({ kind: "trash" }, 1000, 1000)).toBe(0);
  });
});
