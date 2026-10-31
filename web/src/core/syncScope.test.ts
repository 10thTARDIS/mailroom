// SPDX-License-Identifier: AGPL-3.0-only

// SELECTIVE SYNC, client half (#62).
//
// The thing worth testing here is not that a setter sets. It is that the client
// never CLAIMS a folder is frozen unless the server agreed, and never claims it is
// live once the server has frozen it. Both directions are claims about mail:
//
//   * saying "paused" too early hides new mail that is in fact still arriving;
//   * saying nothing after a pause presents a snapshot as current mail — it will
//     list messages the sender has since deleted.
//
// So the store is written from the SERVER's answer, not optimistically, and a
// refusal must leave the previous state alone.

import { beforeEach, describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import { JmapTransportError } from "./jmap/client";
import type { JmapClient } from "./jmap/client";
import type { Mailbox, SetArgs } from "./jmap/types";
import { StorageActionQueue } from "./queue";
import { MemoryStorage } from "./storage";
import { MailStore } from "./store";
import type { SyncEngine } from "./sync";

function mailbox(id: string, over: Partial<Mailbox> = {}): Mailbox {
  return {
    id,
    accountId: "ACC1",
    name: "Bulk",
    parentId: null,
    role: null,
    sortOrder: 10,
    totalEmails: 3,
    unreadEmails: 1,
    totalThreads: 3,
    unreadThreads: 1,
    isSubscribed: true,
    ...over,
  } as Mailbox;
}

const noSync = { poll: async () => {}, refreshView: async () => {} } as unknown as SyncEngine;

interface Harness {
  store: MailStore;
  actions: MailActions;
  errors: string[];
  /** Every Mailbox/set the client issued. */
  sent: SetArgs[];
}

/** `outcome` decides what the fake server does with the set. */
function harness(outcome: "ok" | "refuse" | "throw" = "ok"): Harness {
  const store = new MailStore(null);
  store.setSession({} as never, "A1");
  const errors: string[] = [];
  const sent: SetArgs[] = [];
  const client = {
    mailboxSet: async (args: SetArgs) => {
      sent.push(args);
      if (outcome === "throw") throw new JmapTransportError("server down");
      const id = Object.keys(args.update ?? {})[0] as string;
      if (outcome === "refuse") {
        return {
          notUpdated: { [id]: { type: "invalidProperties", description: "nope" } },
          newState: "s",
        };
      }
      return { updated: { [id]: null }, notUpdated: null, newState: "s" };
    },
    request: async () => [],
  } as unknown as JmapClient;
  const actions = new MailActions(client, store, new StorageActionQueue(new MemoryStorage()), noSync, {
    onError: (m) => errors.push(m),
  });
  return { store, actions, errors, sent };
}

describe("pausing a folder", () => {
  let h: Harness;
  beforeEach(() => {
    h = harness();
    h.store.putMailboxes([mailbox("M1")]);
  });

  it("records the new scope once the server accepts it", async () => {
    expect(await h.actions.setMailboxSyncScope("M1", "off")).toBe(true);
    expect(h.store.getState().mailboxes["M1"]?.syncScope).toBe("off");
    expect(h.errors).toEqual([]);
    // Exactly the one property the server accepts, and nothing else — a patch
    // mentioning any other property is refused WHOLE server-side.
    expect(h.sent).toHaveLength(1);
    expect(h.sent[0]?.update).toEqual({ M1: { syncScope: "off" } });
  });

  it("goes back to full through the same call", async () => {
    await h.actions.setMailboxSyncScope("M1", "off");
    expect(await h.actions.setMailboxSyncScope("M1", "full")).toBe(true);
    expect(h.store.getState().mailboxes["M1"]?.syncScope).toBe("full");
  });

  it("treats a mailbox with no syncScope as live", () => {
    // An older server does not send the property. Absent must read as "full", not
    // as "paused" — the failure would tell the user their mail is not syncing when
    // it is.
    expect(h.store.getState().mailboxes["M1"]?.syncScope).toBeUndefined();
  });
});

describe("a folder is never claimed to be paused unless it is", () => {
  it("keeps the old scope when the server refuses", async () => {
    const h = harness("refuse");
    h.store.putMailboxes([mailbox("M1")]);

    expect(await h.actions.setMailboxSyncScope("M1", "off")).toBe(false);
    // NOT "off". Marking the folder paused here would hide arriving mail behind a
    // label the server never agreed to.
    expect(h.store.getState().mailboxes["M1"]?.syncScope).toBeUndefined();
    expect(h.errors.join(" ")).toContain("nope");
  });

  it("keeps the old scope when the server cannot be reached", async () => {
    const h = harness("throw");
    h.store.putMailboxes([mailbox("M1", { syncScope: "off" })]);

    // Resuming while offline must not report success: the folder is still frozen
    // server-side, and saying otherwise would present a snapshot as live mail.
    expect(await h.actions.setMailboxSyncScope("M1", "full")).toBe(false);
    expect(h.store.getState().mailboxes["M1"]?.syncScope).toBe("off");
    expect(h.errors.join(" ")).toContain("unreachable");
  });

  it("NEGATIVE CONTROL: the same call against a working server does change it", async () => {
    // Proves the two assertions above are the refusal's doing and not a setter
    // that never writes at all.
    const h = harness();
    h.store.putMailboxes([mailbox("M1", { syncScope: "off" })]);
    expect(await h.actions.setMailboxSyncScope("M1", "full")).toBe(true);
    expect(h.store.getState().mailboxes["M1"]?.syncScope).toBe("full");
  });
});

describe("the store applier", () => {
  it("does not invent a mailbox it has never seen", () => {
    const h = harness();
    h.store.applyMailboxSyncScope("nope", "off");
    expect(h.store.getState().mailboxes["nope"]).toBeUndefined();
  });

  it("leaves every other field of the mailbox alone", () => {
    const h = harness();
    h.store.putMailboxes([mailbox("M1", { name: "Bulk", unreadEmails: 7 })]);
    h.store.applyMailboxSyncScope("M1", "off");
    const m = h.store.getState().mailboxes["M1"];
    expect(m?.name).toBe("Bulk");
    expect(m?.unreadEmails).toBe(7);
    expect(m?.syncScope).toBe("off");
  });

  it("does not disturb the other mailboxes", () => {
    const h = harness();
    h.store.putMailboxes([mailbox("M1"), mailbox("M2", { name: "Other" })]);
    h.store.applyMailboxSyncScope("M1", "off");
    expect(h.store.getState().mailboxes["M2"]?.syncScope).toBeUndefined();
    expect(Object.keys(h.store.getState().mailboxes).sort()).toEqual(["M1", "M2"]);
  });
});
