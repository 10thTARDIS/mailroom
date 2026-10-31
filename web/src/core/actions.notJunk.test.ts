// SPDX-License-Identifier: AGPL-3.0-only

// "Not junk" (#528, spec §6) through the offline queue.
//
// The test that earns its place is the REFUSAL one. The rescue is shown as an
// optimistic move to the inbox, and the queue undoes a refused action by patch
// KEY. If the patch did not name a mailbox, a refusal would revert a keyword
// that was never set and leave the move behind: the message out of Junk, in no
// list, until a reload — a rescue that loses the message it was rescuing.

import { describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { Mailbox } from "./jmap/types";
import { KEYWORD_JUNK } from "./jmap/types";
import { StorageActionQueue } from "./queue";
import { MemoryStorage } from "./storage";
import { MailStore } from "./store";
import type { SyncEngine } from "./sync";

const ACCOUNT = "U1";

function mailbox(id: string, role: Mailbox["role"]): Mailbox {
  return {
    id,
    accountId: ACCOUNT,
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

function harness(opts: { refuse?: string[]; mailboxes?: Mailbox[] } = {}) {
  const store = new MailStore(null);
  store.setSession({} as never, ACCOUNT);
  store.setConn("online");
  store.putMailboxes(opts.mailboxes ?? [mailbox("MB-I", "inbox"), mailbox("MB-J", "junk")]);

  const errors: string[] = [];
  const sets: Call[] = [];
  const client = {
    request: async (calls: Call[]): Promise<CallResult[]> =>
      calls.map((c): CallResult => {
        sets.push(c);
        const update = (c.args["update"] as Record<string, unknown>) ?? {};
        const updated: Record<string, null> = {};
        const notUpdated: Record<string, unknown> = {};
        for (const id of Object.keys(update)) {
          if (opts.refuse?.includes(id)) {
            notUpdated[id] = { type: "invalidPatch", description: "refused for the test" };
          } else {
            updated[id] = null;
          }
        }
        return { id: c.id, name: c.name, value: { updated, notUpdated, newState: "e2" }, error: null };
      }),
  } as unknown as JmapClient;
  const sync = { poll: async () => {}, refreshDesk: async () => {} } as unknown as SyncEngine;
  const queue = new StorageActionQueue(new MemoryStorage());
  const actions = new MailActions(client, store, queue, sync, { onError: (m) => errors.push(m) });
  return { store, actions, queue, errors, sets };
}

describe("MailActions.notJunk (#528)", () => {
  it("queues one Email update per message: clear the verdict AND name the inbox", async () => {
    const h = harness();
    // Queue first, drain never reached: inspect exactly what would go out.
    const drain = h.actions.drain.bind(h.actions);
    h.actions.drain = async () => {};
    expect(await h.actions.notJunk(["m1", "m2"])).toBe(true);
    h.actions.drain = drain;

    const queued = await h.queue.list();
    expect(queued.map((a) => a.targetId)).toEqual(["m1", "m2"]);
    expect(queued[0]?.entity).toBe("Email");
    expect(queued[0]?.op).toBe("update");
    expect(queued[0]?.payload).toEqual({ [`keywords/${KEYWORD_JUNK}`]: null, "mailboxIds/MB-I": true });
  });

  it("moves the row to the inbox at once, before the round trip", async () => {
    const h = harness();
    await h.actions.notJunk(["m1"]);
    expect(h.store.getState().overlay["m1"]?.move).toBe("inbox");
  });

  it("sends the patch in an Email/set and clears the queue when the server accepts", async () => {
    const h = harness();
    await h.actions.notJunk(["m1"]);
    await h.actions.drain();
    const set = h.sets.find((c) => c.name === "Email/set");
    expect((set?.args["update"] as Record<string, unknown>)["m1"]).toEqual({
      [`keywords/${KEYWORD_JUNK}`]: null,
      "mailboxIds/MB-I": true,
    });
    expect(await h.queue.list()).toHaveLength(0);
    expect(h.errors).toEqual([]);
  });

  it("ROLLS THE MOVE BACK when the server refuses, so the message reappears in Junk", async () => {
    const h = harness({ refuse: ["m1"] });
    await h.actions.notJunk(["m1", "m2"]);
    expect(h.store.getState().overlay["m1"]?.move).toBe("inbox");

    await h.actions.drain();

    // The refused one has NO pending move left — not a move with an empty keyword
    // map, which is what a patch without a mailbox key leaves behind.
    expect(h.store.getState().overlay["m1"]).toBeUndefined();
    // The accepted one keeps its optimistic move until the server's Email agrees.
    expect(h.store.getState().overlay["m2"]?.move).toBe("inbox");
    expect(h.errors.join(" ")).toContain("Not junk");
    expect(await h.queue.list()).toHaveLength(0);
  });

  it("refuses, and says why, when the account has no Inbox", async () => {
    const h = harness({ mailboxes: [mailbox("MB-J", "junk")] });
    expect(await h.actions.notJunk(["m1"])).toBe(false);
    expect(await h.queue.list()).toHaveLength(0);
    expect(h.store.getState().overlay["m1"]).toBeUndefined();
    expect(h.errors.join(" ")).toContain("Inbox");
  });

  it("does nothing for an empty list", async () => {
    const h = harness();
    expect(await h.actions.notJunk([])).toBe(false);
    expect(await h.queue.list()).toHaveLength(0);
  });
});
