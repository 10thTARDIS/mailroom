// SPDX-License-Identifier: AGPL-3.0-only

// AN ARCHIVED MESSAGE MUST NOT COME BACK (#685).
//
// Production 1.81.6: archive a message while reading it, the pane advances, and
// a couple of seconds later the archived row REAPPEARS in the list, stays for a
// few seconds, and goes again. Two things put it back, and both are modelled
// here end to end through the real SyncEngine and MailStore:
//
//   1. THE RETIREMENT GAP. The local move overlay is what hides the row, and
//      putEmails retires it the moment the server's Email says "archive" — which
//      the poll after the Email/set reads. The list's ids still come from a
//      query issued BEFORE the archive, and without the overlay nothing filters
//      them: the row is back until the re-query lands.
//   2. THE STALE RESPONSE. A load whose request left before the archive was
//      committed answers AFTER that retirement, with the message still in it.
//      On the desk it is the single-flight load in progress (#665), which is
//      still the latest generation when it lands, so the guard applies it.
//
// The transport is a tiny server: a request is EVALUATED when it is sent (a
// snapshot of the mailbox at that instant) and ANSWERED when the test says so,
// in whatever order the test wants. That is the whole race: when a query was
// asked is not when its answer arrives.

import { describe, expect, it } from "vitest";

import { sectionKey } from "./desk";
import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { Email, Mailbox } from "./jmap/types";
import { MailStore, sectionIds, visibleIds } from "./store";
import { SyncEngine } from "./sync";

const ACCOUNT = "U1";
const INBOX = "mb-inbox";
const ARCHIVE = "mb-archive";

function mailbox(id: string, role: "inbox" | "archive"): Mailbox {
  return { id, accountId: "A1", name: role, parentId: null, role, sortOrder: 0 } as unknown as Mailbox;
}

type Where = "inbox" | "archive";

/** The server: where each message is, newest first, and which changed. */
class World {
  where: Record<string, Where> = {};
  order: string[] = [];
  state = 0;
  changed = new Set<string>();

  email(id: string): Email {
    return {
      id,
      blobId: "b",
      threadId: `t-${id}`,
      mailboxIds: { [this.where[id] === "archive" ? ARCHIVE : INBOX]: true },
      keywords: {},
      size: 1,
      receivedAt: `2026-10-0${this.order.indexOf(id) + 1}T00:00:00Z`,
    } as unknown as Email;
  }
  inbox(): string[] {
    return this.order.filter((id) => this.where[id] === "inbox");
  }
  move(id: string, to: Where): void {
    this.where[id] = to;
    this.changed.add(id);
    this.state += 1;
  }
}

interface Parked {
  calls: Call[];
  answer: CallResult[];
  resolve(): void;
}

class ServerClient {
  readonly parked: Parked[] = [];
  constructor(private readonly world: World) {}

  get accountId(): string {
    return ACCOUNT;
  }
  pushUrl(): string {
    return "/jmap/eventsource/";
  }

  /** EVALUATED NOW, answered later — see the header. */
  request(calls: Call[]): Promise<CallResult[]> {
    const w = this.world;
    const answer: CallResult[] = calls.map((c) => {
      const ok = (value: unknown): CallResult => ({ id: c.id, name: c.name, error: null, value }) as never;
      switch (c.name) {
        case "Area/getSummary":
          return ok({ accountId: ACCOUNT, state: "a", threshold: 0.4, list: [] });
        case "Email/query": {
          const ids = c.id === "all" ? [] : w.inbox();
          return ok({ ids, total: ids.length, queryState: `s${w.state}`, position: 0 });
        }
        case "Email/get": {
          const explicit = c.args["ids"] as string[] | undefined;
          const ids = explicit ?? w.inbox();
          return ok({ list: ids.map((id) => w.email(id)), notFound: [], state: `s${w.state}` });
        }
        case "Email/changes": {
          const updated = [...w.changed];
          w.changed.clear();
          return ok({ oldState: c.args["sinceState"], newState: `s${w.state}`, created: [], updated, destroyed: [] });
        }
        case "Mailbox/get":
          return ok({ list: [mailbox(INBOX, "inbox"), mailbox(ARCHIVE, "archive")], state: "m" });
        default:
          return ok({});
      }
    });
    return new Promise<CallResult[]>((res) => {
      this.parked.push({ calls, answer, resolve: () => res(answer) });
    });
  }

  /** The parked request whose calls include `name`, oldest first. */
  find(name: string, from = 0): Parked {
    const p = this.parked.slice(from).find((x) => x.calls.some((c) => c.name === name));
    if (!p) throw new Error(`no parked request with ${name}`);
    return p;
  }
}

/** Lets queued microtasks and the resulting store writes run. */
const settle = () => new Promise<void>((r) => setTimeout(r, 0));

function harness() {
  const world = new World();
  for (const id of ["Y", "X", "W"]) {
    world.where[id] = "inbox";
    world.order.push(id);
  }
  const store = new MailStore(null);
  store.setSession(
    { capabilities: { "urn:ietf:params:jmap:core": { maxCallsInRequest: 64 } } } as never,
    ACCOUNT,
  );
  store.putMailboxes([mailbox(INBOX, "inbox"), mailbox(ARCHIVE, "archive")], true);
  const client = new ServerClient(world);
  const engine = new SyncEngine(client as unknown as JmapClient, store, {
    push: null,
    setTimer: () => 0,
    clearTimer: () => {},
  });
  return { world, store, client, engine };
}

/**
 * Archives X exactly as MailActions.archive + drain do, minus the durable
 * queue: the overlay first, then the Email/set reaching the server (the world
 * moves), then the poll flushChunk fires once the set has answered.
 */
async function archiveX(h: ReturnType<typeof harness>): Promise<{ poll: Promise<void> }> {
  h.store.applyMove(["X"], "archive");
  h.world.move("X", "archive");
  const poll = h.engine.poll();
  await settle();
  // The poll's Email/changes, then its follow-up Email/get (+ Mailbox/get).
  h.client.find("Email/changes").resolve();
  await settle();
  const follow = h.client.parked.find(
    (p) => p.calls.some((c) => c.name === "Email/get" && Array.isArray(c.args["ids"])),
  );
  if (!follow) throw new Error("the poll sent no follow-up Email/get");
  follow.resolve();
  await settle();
  // Wrapped: an async function returning a promise would wait for it, and the
  // poll does not finish until the desk refresh the test has yet to answer.
  return { poll };
}

describe("a load sent before an archive cannot bring the message back (#685)", () => {
  it("the desk: neither the retirement gap nor the in-flight load shows X again", async () => {
    const h = harness();
    const tray = sectionKey("tray", null, null);
    const visible = () => sectionIds(h.store.getState(), tray, { move: "inbox", snoozed: null });

    const first = h.engine.loadDesk("rest", null);
    await settle();
    h.client.parked[0]?.resolve();
    await first;
    expect(visible()).toEqual(["W", "X", "Y"]);

    // A refresh already in flight when the user archives — the one the open
    // started (marking it read is a write, and a write is a poll and a desk
    // load). Its request left with X still in the inbox.
    const inFlight = h.engine.loadDesk("rest", null);
    await settle();
    const stale = h.client.parked[1];
    if (!stale) throw new Error("no in-flight desk load");

    // From the archive on, every store write is watched: X must never be in
    // what the desk draws again, not even for one notification.
    const seen: string[][] = [];
    const unsubscribe = h.store.subscribe(() => seen.push(visible()));
    const before = h.client.parked.length;
    const { poll } = await archiveX(h);

    // The poll retired the overlay and asked for a desk refresh, which the
    // single flight queued behind the load in progress.
    expect(h.store.getState().overlay["X"]).toBeUndefined();
    expect(visible()).toEqual(["W", "Y"]);

    stale.resolve();
    await settle();
    expect(visible()).toEqual(["W", "Y"]);

    // The trailing load was sent after the archive and answers without X.
    const trailing = h.client.find("Area/getSummary", before);
    trailing.resolve();
    await Promise.all([inFlight, poll]);
    await settle();
    unsubscribe();

    expect(visible()).toEqual(["W", "Y"]);
    // Raw ids, in the order the server answered them.
    expect(h.store.getState().sections[tray]?.ids).toEqual(["Y", "W"]);
    expect(seen.length).toBeGreaterThan(0);
    for (const ids of seen) expect(ids).not.toContain("X");
  });

  it("the ordinary list: the retirement gap and a stale answer landing last both stay hidden", async () => {
    const h = harness();
    const visible = () => visibleIds(h.store.getState(), "inbox", null);

    const first = h.engine.loadView({ kind: "role", role: "inbox" });
    await settle();
    h.client.parked[0]?.resolve();
    await first;
    expect(visible()).toEqual(["W", "X", "Y"]);

    // A refresh in flight when the user archives.
    const inFlight = h.engine.refreshView();
    await settle();
    const stale = h.client.parked[1];
    if (!stale) throw new Error("no in-flight list load");

    const seen: string[][] = [];
    const unsubscribe = h.store.subscribe(() => seen.push(visible()));
    const before = h.client.parked.length;
    const { poll } = await archiveX(h);
    expect(h.store.getState().overlay["X"]).toBeUndefined();
    expect(visible()).toEqual(["W", "Y"]);

    // Ordinary loads are NOT single-flight: the poll's refresh went out at
    // once. Answered in the worse order — the fresh one first, the stale one
    // last — the stale one would otherwise be the list until the next change.
    const fresh = h.client.parked.slice(before).find((p) => p.calls.some((c) => c.name === "Email/query"));
    if (!fresh) throw new Error("the poll did not refresh the list");
    fresh.resolve();
    await settle();
    expect(visible()).toEqual(["W", "Y"]);
    stale.resolve();
    await Promise.all([inFlight, poll]);
    await settle();
    unsubscribe();

    expect(visible()).toEqual(["W", "Y"]);
    for (const ids of seen) expect(ids).not.toContain("X");
  });

  it("a move the server refused still brings the row back", async () => {
    // The negative control for the guard: hiding is only right for a move that
    // HAPPENED. A refused archive is rolled back (revertMove) and the row has to
    // show again from the list it never left.
    const h = harness();
    const tray = sectionKey("tray", null, null);
    const visible = () => sectionIds(h.store.getState(), tray, { move: "inbox", snoozed: null });
    const first = h.engine.loadDesk("rest", null);
    await settle();
    h.client.parked[0]?.resolve();
    await first;

    h.store.applyMove(["X"], "archive");
    expect(visible()).toEqual(["W", "Y"]);
    h.store.revertMove(["X"]);
    expect(visible()).toEqual(["W", "X", "Y"]);
  });

  it("a list queried after the archive settled is trusted, including a move back", async () => {
    // Archive, settle, then undo from another device: a query issued after the
    // archive settled is evidence about X, and X in it must show.
    const h = harness();
    const tray = sectionKey("tray", null, null);
    const visible = () => sectionIds(h.store.getState(), tray, { move: "inbox", snoozed: null });
    const first = h.engine.loadDesk("rest", null);
    await settle();
    h.client.parked[0]?.resolve();
    await first;

    const before = h.client.parked.length;
    const { poll } = await archiveX(h);
    h.client.find("Area/getSummary", before).resolve();
    await poll;
    expect(visible()).toEqual(["W", "Y"]);

    h.world.move("X", "inbox");
    const again = h.engine.loadDesk("rest", null);
    await settle();
    h.client.parked[h.client.parked.length - 1]?.resolve();
    await again;
    expect(visible()).toEqual(["W", "X", "Y"]);
  });
});
