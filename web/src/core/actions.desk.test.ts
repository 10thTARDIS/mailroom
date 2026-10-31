// SPDX-License-Identifier: AGPL-3.0-only

// The desk's two mutations (#375): moving the LINE, and archiving a whole
// DRAWER.
//
// Both are the kind of write this project has been bitten by before, and each
// fails in its own way:
//
//   * the line decides what the user SEES. Written optimistically it would
//     redraw the desk at a threshold the server may have rounded or refused, so
//     the store is written from the server's own answer and a refusal must leave
//     it exactly as it was.
//   * archive-all moves thousands of messages. It must go out in bounded chunks
//     rather than as a client loop of single sets, it must ROLL BACK what the
//     server refused instead of leaving those rows looking archived, and when it
//     hits its cap it must SAY SO — a partial sweep presented as a finished one
//     is the "a bounded result reads as a total" failure this codebase keeps a
//     rule about.

import { describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import { JmapTransportError } from "./jmap/client";
import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { DeskPreferences, Mailbox, QueryArgs, QueryResult } from "./jmap/types";
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

function prefs(over: Partial<DeskPreferences> = {}): DeskPreferences {
  return {
    id: ACCOUNT,
    lineThreshold: 0.5,
    drawerDefaultDays: 30,
    trayNagDays: 7,
    // An established desk: migration 00050 backfilled every login that already
    // had preferences, so null here would be a login that has never seen the desk.
    setupCompletedAt: "2026-01-01T00:00:00Z",
    drawerSweepEnabled: false,
    ...over,
  };
}

interface Harness {
  store: MailStore;
  actions: MailActions;
  errors: string[];
  /** Every batch the client was asked to send. */
  requests: Call[][];
  /** Every Email/query the drawer collector issued. */
  queries: QueryArgs[];
  refreshes: number;
  queue: StorageActionQueue;
  /** Replace the set of ids the fake server refuses to update. */
  refuse: (ids: string[]) => void;
}

interface Options {
  /** How many messages the drawer holds, and what the server reports as its
   * total (which may exceed what it will page out). */
  drawerIds?: string[];
  drawerTotal?: number;
  /** Ids the server refuses to update. */
  notUpdated?: string[];
  /** Throw a transport error on the Nth batch (0-based). */
  throwOnRequest?: number;
  /** DeskPreferences/set outcome. */
  setOutcome?: "ok" | "refuse" | "silent" | "throw";
  /** What DeskPreferences/get answers with afterwards. */
  serverThreshold?: number;
  desk?: { tab: "review" | "rest"; scope: string | null } | null;
  /** Advertised maxCallsInRequest, or undefined for a session that omits it. */
  maxCalls?: number;
  conn?: "online" | "offline";
}

function harness(opts: Options = {}): Harness {
  const store = new MailStore(null);
  const session = opts.maxCalls
    ? ({ capabilities: { "urn:ietf:params:jmap:core": { maxCallsInRequest: opts.maxCalls } } } as never)
    : ({} as never);
  store.setSession(session, ACCOUNT);
  store.setConn(opts.conn ?? "online");
  store.putMailboxes([mailbox("MB-A", "archive"), mailbox("MB-I", "inbox")]);

  const errors: string[] = [];
  const requests: Call[][] = [];
  const queries: QueryArgs[] = [];
  let refreshes = 0;
  const drawerIds = opts.drawerIds ?? [];

  const client = {
    emailQuery: async (args: QueryArgs): Promise<QueryResult> => {
      queries.push(args);
      const position = args.position ?? 0;
      const limit = args.limit ?? 500;
      return {
        accountId: ACCOUNT,
        queryState: "q1",
        canCalculateChanges: false,
        position,
        total: opts.drawerTotal ?? drawerIds.length,
        ids: drawerIds.slice(position, position + limit),
      };
    },
    request: async (calls: Call[]): Promise<CallResult[]> => {
      const index = requests.length;
      requests.push(calls);
      if (opts.throwOnRequest === index) throw new JmapTransportError("server down");
      return calls.map((c): CallResult => {
        if (c.name === "DeskPreferences/set") {
          const id = Object.keys((c.args["update"] as Record<string, unknown>) ?? {})[0] as string;
          if (opts.setOutcome === "refuse") {
            return {
              id: c.id,
              name: c.name,
              value: {
                notUpdated: { [id]: { type: "invalidProperties", description: "nope" } },
                updated: null,
                newState: "d2",
              },
              error: null,
            };
          }
          if (opts.setOutcome === "silent") {
            return {
              id: c.id,
              name: c.name,
              value: { updated: {}, notUpdated: {}, newState: "d2" },
              error: null,
            };
          }
          return {
            id: c.id,
            name: c.name,
            value: { updated: { [id]: null }, notUpdated: {}, newState: "d2" },
            error: null,
          };
        }
        if (c.name === "DeskPreferences/get") {
          return {
            id: c.id,
            name: c.name,
            value: {
              list: [prefs({ lineThreshold: opts.serverThreshold ?? 0.5 })],
              notFound: [],
              state: "d2",
            },
            error: null,
          };
        }
        // Email/set: everything updates except the ids the server refuses.
        const update = (c.args["update"] as Record<string, unknown>) ?? {};
        const updated: Record<string, null> = {};
        const notUpdated: Record<string, unknown> = {};
        for (const id of Object.keys(update)) {
          if (opts.notUpdated?.includes(id)) {
            notUpdated[id] = { type: "notFound", description: "gone" };
          } else {
            updated[id] = null;
          }
        }
        return {
          id: c.id,
          name: c.name,
          value: { updated, notUpdated, newState: "e2" },
          error: null,
        };
      });
    },
  } as unknown as JmapClient;

  const sync = {
    currentDesk: opts.desk === undefined ? { tab: "rest", scope: null } : opts.desk,
    refreshDesk: async () => {
      refreshes += 1;
    },
    poll: async () => {},
    refreshView: async () => {},
  } as unknown as SyncEngine;

  const queue = new StorageActionQueue(new MemoryStorage());
  const actions = new MailActions(client, store, queue, sync, { onError: (m) => errors.push(m) });
  return {
    store,
    actions,
    errors,
    requests,
    queries,
    queue,
    // Lets a test change WHICH ids the server refuses part-way through, which is
    // what an undo test needs: the sweep has to succeed before the restore can
    // be refused.
    refuse: (list: string[]) => {
      opts.notUpdated = list;
    },
    get refreshes() {
      return refreshes;
    },
  } as Harness;
}

function ids(n: number, from = 0): string[] {
  return Array.from({ length: n }, (_, i) => `m${from + i}`);
}

describe("archiving a whole drawer", () => {
  it("sends 450 messages as 200 / 200 / 50 in one batch, and queues nothing", async () => {
    const h = harness({ drawerIds: ids(450) });
    const res = await h.actions.archiveDrawer("A1");

    expect(res).toEqual({ archived: 450, failed: 0, bound: false, ids: ids(450) });
    const sets = h.requests.flat().filter((c) => c.name === "Email/set");
    expect(sets.map((c) => Object.keys(c.args["update"] as object).length)).toEqual([200, 200, 50]);
    // Under the default calls-per-request, three chunks are ONE round trip.
    expect(h.requests).toHaveLength(1);
    // The patch is the same role move `archive` builds.
    expect(Object.values((sets[0] as Call).args["update"] as Record<string, unknown>)[0]).toEqual({
      "mailboxIds/MB-A": true,
    });
    // NOT the offline queue: that is one action per message, which is exactly
    // the client loop this exists to avoid.
    expect(await h.queue.list()).toHaveLength(0);
  });

  it("applies the move locally so the drawer empties at once", async () => {
    const h = harness({ drawerIds: ids(3) });
    await h.actions.archiveDrawer("A1");
    expect(h.store.getState().overlay["m0"]?.move).toBe("archive");
    expect(h.store.getState().overlay["m2"]?.move).toBe("archive");
  });

  it("rolls back what the server refused and counts it as failed", async () => {
    const h = harness({ drawerIds: ids(450), notUpdated: ["m5", "m300"] });
    const res = await h.actions.archiveDrawer("A1");
    expect(res.archived).toBe(448);
    expect(res.failed).toBe(2);
    expect(res.bound).toBe(false);
    // THE IDS IT REPORTS ARE THE ONES THAT MOVED. The two the server refused
    // are absent, so an undo built from this list never asks the server to put
    // back a message that never left.
    expect(res.ids).toHaveLength(448);
    expect(res.ids).not.toContain("m5");
    expect(res.ids).not.toContain("m300");
    // Those two are back in the drawer rather than sitting there looking
    // archived until someone reloads.
    expect(h.store.getState().overlay["m5"]?.move ?? null).toBeNull();
    expect(h.store.getState().overlay["m300"]?.move ?? null).toBeNull();
    expect(h.store.getState().overlay["m6"]?.move).toBe("archive");
  });

  it("stops at the cap and REPORTS that it is a bound", async () => {
    const h = harness({ drawerIds: ids(6000), drawerTotal: 6000, maxCalls: 64 });
    const res = await h.actions.archiveDrawer("A1");
    expect(res.bound).toBe(true);
    expect(res.archived).toBe(5000);
    expect(res.failed).toBe(0);
    // The 5001st is untouched: a cap that quietly archived everything would be
    // no cap at all.
    expect(h.store.getState().overlay["m5000"]).toBeUndefined();
  });

  it("NEGATIVE CONTROL: a drawer under the cap is not reported as bounded", async () => {
    const h = harness({ drawerIds: ids(10), drawerTotal: 10 });
    expect((await h.actions.archiveDrawer("A1")).bound).toBe(false);
  });

  it("pages the drawer with position until the server runs out", async () => {
    const h = harness({ drawerIds: ids(1100), drawerTotal: 1100, maxCalls: 64 });
    await h.actions.archiveDrawer("A1");
    expect(h.queries.map((q) => q.position)).toEqual([0, 500, 1000]);
    expect(h.queries[0]?.limit).toBe(500);
    // The drawer's own filter, scoped as the desk is.
    expect(h.queries[0]?.filter).toEqual({
      inArea: "A1",
      onDesk: true,
      onSurface: false,
      isSnoozed: false,
    });
  });

  it("splits into several requests when the server takes few calls per batch", async () => {
    const h = harness({ drawerIds: ids(1000), maxCalls: 2 });
    await h.actions.archiveDrawer("A1");
    // 5 chunks of 200, two calls per request.
    expect(h.requests).toHaveLength(3);
    expect(h.requests.map((r) => r.length)).toEqual([2, 2, 1]);
  });

  it("takes back the unsent half when the connection drops mid-sweep", async () => {
    const h = harness({ drawerIds: ids(1000), maxCalls: 2, throwOnRequest: 1 });
    const res = await h.actions.archiveDrawer("A1");
    // The first request's 400 landed; the remaining 600 did not.
    expect(res.archived).toBe(400);
    expect(res.failed).toBe(600);
    expect(h.store.getState().overlay["m999"]?.move ?? null).toBeNull();
    expect(h.store.getState().overlay["m0"]?.move).toBe("archive");
    expect(h.errors.join(" ")).toContain("unreachable");
  });

  it("refuses when there is no desk on screen", async () => {
    const h = harness({ drawerIds: ids(5), desk: null });
    expect(await h.actions.archiveDrawer("A1")).toEqual({
      archived: 0,
      failed: 0,
      bound: false,
      ids: [],
    });
    expect(h.requests).toHaveLength(0);
    expect(h.queries).toHaveLength(0);
    expect(h.errors.join(" ")).toContain("desk");
  });

  it("refuses when the client is offline rather than pretending to sweep", async () => {
    const h = harness({ drawerIds: ids(5), conn: "offline" });
    expect((await h.actions.archiveDrawer("A1")).archived).toBe(0);
    expect(h.queries).toHaveLength(0);
    expect(h.errors.join(" ")).toContain("connection");
  });

  it("refuses when the account has no Archive folder", async () => {
    const h = harness({ drawerIds: ids(5) });
    h.store.putMailboxes([mailbox("MB-I", "inbox")], true);
    expect((await h.actions.archiveDrawer("A1")).archived).toBe(0);
    expect(h.errors.join(" ")).toContain("Archive");
  });

  it("does nothing at all for an empty drawer", async () => {
    const h = harness({ drawerIds: [] });
    expect(await h.actions.archiveDrawer("A1")).toEqual({
      archived: 0,
      failed: 0,
      bound: false,
      ids: [],
    });
    expect(h.requests).toHaveLength(0);
  });
});

describe("undoing an archive (#375 increment 6)", () => {
  it("restores to the INBOX by naming that mailbox's id, so the server moves by role", async () => {
    const h = harness();
    expect(await h.actions.restoreToInbox(["m1", "m2"])).toBe(true);
    // The same shape `archive` uses, pointed the other way: the patch names a
    // mailbox and the server reads only its ROLE (internal/api/email.go
    // setMailbox), which is what restores every account's copy.
    const queued = await h.queue.list();
    expect(queued.map((a) => a.targetId)).toEqual(["m1", "m2"]);
    expect(queued[0]?.payload).toEqual({ "mailboxIds/MB-I": true });
    expect(queued[0]?.entity).toBe("Email");
    expect(queued[0]?.op).toBe("update");
  });

  it("moves the overlay to inbox at once, so the row comes back before the round trip", async () => {
    const h = harness();
    await h.actions.restoreToInbox(["m1"]);
    expect(h.store.getState().overlay["m1"]?.move).toBe("inbox");
  });

  it("goes through the QUEUE by default, so an undo works with no connection", async () => {
    const h = harness({ conn: "offline" });
    expect(await h.actions.restoreToInbox(["m1"])).toBe(true);
    expect(await h.queue.list()).toHaveLength(1);
    expect(h.errors).toEqual([]);
  });

  it("refuses, and says why, when the account has no Inbox", async () => {
    const h = harness();
    h.store.putMailboxes([mailbox("MB-A", "archive")], true);
    expect(await h.actions.restoreToInbox(["m1"])).toBe(false);
    expect(await h.queue.list()).toHaveLength(0);
    expect(h.errors.join(" ")).toContain("Inbox");
  });

  it("does nothing for an empty list", async () => {
    const h = harness();
    expect(await h.actions.restoreToInbox([])).toBe(false);
    expect(await h.queue.list()).toHaveLength(0);
    expect(h.requests).toHaveLength(0);
  });
});

describe("undoing a whole archive-all (#375 increment 6)", () => {
  it("puts a 450-message sweep back as 200 / 200 / 50, and queues nothing", async () => {
    const h = harness({ drawerIds: ids(450) });
    const swept = await h.actions.archiveDrawer("A1");
    const before = h.requests.length;

    const ok = await h.actions.restoreToInbox(swept.ids, { direct: true });

    // EVERY CHUNK, NOT JUST THE FIRST — asserted BEFORE the return value on
    // purpose. An undo that sent one chunk of 200 and reported failure would
    // fail the boolean too, and a suite whose first red line is `expected false
    // to be true` names the symptom rather than the defect. This says which
    // messages were actually asked about.
    const sets = h.requests
      .slice(before)
      .flat()
      .filter((c) => c.name === "Email/set");
    expect(sets.map((c) => Object.keys(c.args["update"] as object).length)).toEqual([200, 200, 50]);
    const restored = sets.flatMap((c) => Object.keys(c.args["update"] as object));
    expect(restored).toEqual(swept.ids);
    // The inbox move, by role, exactly as the queued form sends it.
    expect(Object.values((sets[0] as Call).args["update"] as Record<string, unknown>)[0]).toEqual({
      "mailboxIds/MB-I": true,
    });
    expect(ok).toBe(true);
    // NOT the offline queue: 450 durable rows and 450 replays is the client
    // loop archiveDrawer exists to avoid, in reverse.
    expect(await h.queue.list()).toHaveLength(0);
  });

  // ⚠ THIS IS THE OPTIMISTIC-APPLY TEST, NOT THE CHUNKING TEST, and the
  // difference was found by probing it: an inverse crippled to send only its
  // first chunk still passes here, because `applyMove` is called over the WHOLE
  // set before a single request goes out. That is correct behaviour — the rows
  // have to come back at once — but it means the overlay says nothing about how
  // much was sent. What the server was actually asked is the test above.
  it("draws the whole sweep back at once, before any round trip", async () => {
    const h = harness({ drawerIds: ids(450) });
    const swept = await h.actions.archiveDrawer("A1");
    await h.actions.restoreToInbox(swept.ids, { direct: true });
    for (const id of ["m0", "m199", "m200", "m449"]) {
      expect(h.store.getState().overlay[id]?.move).toBe("inbox");
    }
  });

  it("reports failure when the server refuses part of it", async () => {
    const h = harness({ drawerIds: ids(450), notUpdated: ["m300"] });
    const swept = await h.actions.archiveDrawer("A1");
    // m300 never left, so it is not in the list to put back — and the 449 that
    // did are all restored, across all three chunks.
    const before = h.requests.length;
    const ok = await h.actions.restoreToInbox(swept.ids, { direct: true });
    const asked = h.requests
      .slice(before)
      .flat()
      .filter((c) => c.name === "Email/set")
      .flatMap((c) => Object.keys(c.args["update"] as object));
    expect(asked).toEqual(swept.ids);
    expect(asked).toHaveLength(449);
    expect(ok).toBe(true);

    // Now refuse one on the way BACK.
    const h2 = harness({ drawerIds: ids(450) });
    const swept2 = await h2.actions.archiveDrawer("A1");
    h2.refuse(["m7"]);
    expect(await h2.actions.restoreToInbox(swept2.ids, { direct: true })).toBe(false);
    // The refused one carries NO local move at all: revertMove drops the intent
    // rather than restoring the previous one, so what renders is the server's
    // own Email — which is still in Archive, because that is the message the
    // server just declined to move. The client claims nothing about it.
    expect(h2.store.getState().overlay["m7"]?.move ?? null).toBeNull();
    // Its neighbours in the same chunk are unaffected: a per-id refusal rolls
    // back per id, not per batch.
    expect(h2.store.getState().overlay["m8"]?.move).toBe("inbox");
    // And it SAYS so: a partly-undone sweep that reported nothing would leave
    // the user believing everything came back.
    expect(h2.errors.join(" ")).toContain("1 message could not be put back");
  });

  it("refuses to run without a connection rather than pretending", async () => {
    const h = harness({ drawerIds: ids(10) });
    const swept = await h.actions.archiveDrawer("A1");
    h.store.setConn("offline");
    expect(await h.actions.restoreToInbox(swept.ids, { direct: true })).toBe(false);
    expect(h.errors.join(" ")).toContain("connection");
  });
});

describe("moving the line", () => {
  it("writes the SERVER's value, not the one we asked for", async () => {
    // lineThreshold is a float32 server-side: what comes back is what the desk
    // is actually being drawn at.
    const h = harness({ setOutcome: "ok", serverThreshold: 0.6 });
    h.store.setDeskPreferences(prefs({ lineThreshold: 0.5 }));
    expect(await h.actions.setLineThreshold(0.61)).toBe(true);
    expect(h.store.getState().deskPreferences?.lineThreshold).toBe(0.6);
    // Set and re-read in ONE batch, in that order.
    expect((h.requests[0] as Call[]).map((c) => c.name)).toEqual([
      "DeskPreferences/set",
      "DeskPreferences/get",
    ]);
    expect((h.requests[0] as Call[])[0]?.args["update"]).toEqual({
      [ACCOUNT]: { lineThreshold: 0.61 },
    });
    // Every section is on one side of the line or the other, so all of them
    // have to be re-asked.
    expect(h.refreshes).toBe(1);
  });

  it("leaves the line where it was when the server refuses, and says why", async () => {
    const h = harness({ setOutcome: "refuse" });
    h.store.setDeskPreferences(prefs({ lineThreshold: 0.5 }));
    expect(await h.actions.setLineThreshold(0.9)).toBe(false);
    expect(h.store.getState().deskPreferences?.lineThreshold).toBe(0.5);
    expect(h.errors.join(" ")).toContain("nope");
    expect(h.refreshes).toBe(0);
  });

  it("does not believe a set the server never confirmed", async () => {
    const h = harness({ setOutcome: "silent", serverThreshold: 0.9 });
    h.store.setDeskPreferences(prefs({ lineThreshold: 0.5 }));
    expect(await h.actions.setLineThreshold(0.9)).toBe(false);
    expect(h.store.getState().deskPreferences?.lineThreshold).toBe(0.5);
    expect(h.errors.join(" ")).toBeTruthy();
  });

  it("keeps the line when the server cannot be reached", async () => {
    const h = harness({ throwOnRequest: 0 });
    h.store.setDeskPreferences(prefs({ lineThreshold: 0.5 }));
    expect(await h.actions.setLineThreshold(0.9)).toBe(false);
    expect(h.store.getState().deskPreferences?.lineThreshold).toBe(0.5);
    expect(h.errors.join(" ")).toContain("unreachable");
  });

  it("rejects a threshold outside 0..1 without sending anything", async () => {
    const h = harness();
    h.store.setDeskPreferences(prefs({ lineThreshold: 0.5 }));
    for (const bad of [-0.01, 1.01, Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(await h.actions.setLineThreshold(bad)).toBe(false);
    }
    expect(h.requests).toHaveLength(0);
    expect(h.errors).toHaveLength(4);
    expect(h.store.getState().deskPreferences?.lineThreshold).toBe(0.5);
  });

  it("accepts both ends of the range", async () => {
    // NEGATIVE CONTROL for the check above: the same path with a valid value
    // does reach the server.
    const h = harness({ setOutcome: "ok", serverThreshold: 0 });
    h.store.setDeskPreferences(prefs());
    expect(await h.actions.setLineThreshold(0)).toBe(true);
    expect(await h.actions.setLineThreshold(1)).toBe(true);
    expect(h.requests).toHaveLength(2);
  });

  it("can move the line before the preferences have ever been read", async () => {
    // The record's id is the account id, so the slider works on first paint.
    const h = harness({ setOutcome: "ok", serverThreshold: 0.7 });
    expect(h.store.getState().deskPreferences).toBeNull();
    expect(await h.actions.setLineThreshold(0.7)).toBe(true);
    expect((h.requests[0] as Call[])[0]?.args["update"]).toEqual({
      [ACCOUNT]: { lineThreshold: 0.7 },
    });
  });
});
