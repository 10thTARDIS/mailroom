// SPDX-License-Identifier: AGPL-3.0-only

// THE ECHO OF AN EARLIER CHOICE MUST NOT UNDO A LATER ONE (#562).
//
// The accessibility lane once failed at `messageOpens beside: still false after
// 20109ms`: it picked Focused in Settings, then Beside a few seconds later, and
// the pane stayed `off`. The shell applies EVERY `web` ClientPreferences record
// that reaches the store (Shell.tsx applyServerPrefs), and records reach it from
// four reads that run concurrently with this device's own writes:
//
//   * the `/get` beside every `/set` (actions.ts flushClientPreferences);
//   * the `/get` a `/changes` poll follows up with (sync.ts doPoll) — and the
//     push for this device's OWN write is what starts that poll;
//   * the reference-data load, and the id lookup before a first save.
//
// A read the server answered BEFORE a later write committed carries the earlier
// value, and nothing stopped it being applied after that write had already
// landed. These tests replay that as a sequence against a fake server whose
// answers can be HELD, so the order is exact rather than a timing accident:
// the server answers a call at the moment it is asked (its value is fixed
// then); the client only SEES the answer when the test releases it.
//
// Every field the shell applies from the record is covered, not just
// messageOpens: theme, density, text size and rows ride the same record and
// the same apply.

import { describe, expect, it } from "vitest";

import { MailActions, WEB_CLIENT_FAMILY } from "./actions";
import type { ClientPreferencesPatch } from "./actions";
import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { ClientPreferences } from "./jmap/types";
import { StorageActionQueue } from "./queue";
import { emptySyncCursor } from "./session";
import { MemoryStorage } from "./storage";
import { MailStore } from "./store";
import { SyncEngine } from "./sync";

const ACCOUNT = "U1";
const WEB_ID = "CP-WEB";

type Field = keyof ClientPreferencesPatch;

function initialWeb(): ClientPreferences {
  return {
    id: WEB_ID,
    family: "web",
    theme: "system",
    density: "normal",
    messageOpens: "beside",
    paneSplitPct: 40,
    textSize: "m",
    rowLayout: "oneLine",
  };
}

/** A choice, then a DIFFERENT choice for the same field — the user changing
 * their mind. Neither is the initial value, so "B survived" cannot be the
 * initial value showing through. */
const CHANGES: [Field, unknown, unknown][] = [
  ["messageOpens", "focused", "beside"],
  ["theme", "dark", "light"],
  ["density", "compact", "relaxed"],
  ["paneSplitPct", 30, 44],
  ["textSize", "xl", "s"],
  ["rowLayout", "multiLine", "oneLine"],
];

/**
 * A ClientPreferences server with a change log, and a door the test can hold
 * shut. `hold(pred)` arms a hold for the next batch that matches; that batch is
 * ANSWERED immediately (the server's view is fixed at that instant) but
 * delivered only on `release()`.
 */
class FakeServer {
  web: ClientPreferences = initialWeb();
  seq = 0;
  readonly batches: string[][] = [];
  private holdPred: ((calls: Call[]) => boolean) | null = null;
  private releaseHeld: (() => void) | null = null;
  private heldSignal: (() => void) | null = null;

  /** Resolves once a matching batch has been answered and is being held. */
  hold(pred: (calls: Call[]) => boolean): Promise<void> {
    this.holdPred = pred;
    return new Promise((resolve) => {
      this.heldSignal = resolve;
    });
  }

  release(): void {
    const r = this.releaseHeld;
    this.releaseHeld = null;
    r?.();
  }

  /** Another device (or the server itself) changes the web row. */
  changeElsewhere(patch: ClientPreferencesPatch): void {
    this.web = { ...this.web, ...patch } as ClientPreferences;
    this.seq++;
  }

  private state(): string {
    return `cp${this.seq}`;
  }

  private answer(c: Call): CallResult {
    const ok = (value: unknown): CallResult => ({
      id: c.id,
      name: c.name,
      value: value as CallResult["value"],
      error: null,
    });
    switch (c.name) {
      case "ClientPreferences/set": {
        const update = (c.args["update"] ?? {}) as Record<string, ClientPreferencesPatch>;
        const updated: Record<string, null> = {};
        const oldState = this.state();
        for (const [id, patch] of Object.entries(update)) {
          if (id !== WEB_ID) continue;
          this.web = { ...this.web, ...patch } as ClientPreferences;
          this.seq++;
          updated[id] = null;
        }
        return ok({ oldState, newState: this.state(), updated, notUpdated: {} });
      }
      case "ClientPreferences/get":
        return ok({
          list: [
            { ...this.web },
            { ...initialWeb(), id: "CP-PHONE", family: "phone" },
            { ...initialWeb(), id: "CP-DESK", family: "desktop" },
          ],
          notFound: [],
          state: this.state(),
        });
      case "ClientPreferences/changes": {
        const since = Number(String(c.args["sinceState"]).slice(2));
        return ok({
          oldState: c.args["sinceState"],
          newState: this.state(),
          created: [],
          updated: since < this.seq ? [WEB_ID] : [],
          destroyed: [],
          hasMoreChanges: false,
        });
      }
      default:
        // Every other type: nothing ever changes, and the reference-data load
        // reads empty but well-formed lists.
        if (c.name.endsWith("/changes")) {
          return ok({
            oldState: "s0",
            newState: "s0",
            created: [],
            updated: [],
            destroyed: [],
            hasMoreChanges: false,
          });
        }
        return ok({ list: [], notFound: [], state: "s0" });
    }
  }

  client(): JmapClient {
    return {
      accountId: ACCOUNT,
      pushUrl: () => "/jmap/eventsource/",
      request: (calls: Call[]): Promise<CallResult[]> => {
        this.batches.push(calls.map((c) => c.name));
        // Answered NOW, in order — the value each call carries is fixed here.
        const results = calls.map((c) => this.answer(c));
        if (this.holdPred?.(calls)) {
          this.holdPred = null;
          return new Promise((resolve) => {
            this.releaseHeld = () => resolve(results);
            this.heldSignal?.();
            this.heldSignal = null;
          });
        }
        return Promise.resolve(results);
      },
    } as unknown as JmapClient;
  }
}

const isPollRead = (calls: Call[]): boolean =>
  calls.length === 1 && calls[0]?.name === "ClientPreferences/get";
const isWrite = (calls: Call[]): boolean =>
  calls.some((c) => c.name === "ClientPreferences/set");

async function harness() {
  const server = new FakeServer();
  const store = new MailStore(null);
  store.setSession({} as never, ACCOUNT);
  store.setConn("online");
  const client = server.client();
  const sync = new SyncEngine(client, store, { push: null });
  const errors: string[] = [];
  const actions = new MailActions(client, store, new StorageActionQueue(new MemoryStorage()), sync, {
    onError: (m) => errors.push(m),
  });
  // The reference-data load, as the app does it: the row and the cursor.
  await sync.loadReferenceData();
  expect(store.getState().clientPreferences[WEB_CLIENT_FAMILY]?.messageOpens).toBe("beside");
  expect(store.getState().cursor.ClientPreferences).toBe("cp0");
  return { server, store, sync, actions, errors };
}

function stored(store: MailStore, f: Field): unknown {
  return store.getState().clientPreferences[WEB_CLIENT_FAMILY]?.[f];
}

describe("a stale ClientPreferences read after a newer local write (#562)", () => {
  it.each(CHANGES)(
    "%s: the poll that the FIRST write's push started does not undo the second",
    async (field, a, b) => {
      const h = await harness();

      // 1. The user picks A. Its write lands and its own /get says A.
      expect(await h.actions.saveClientPreferences({ [field]: a })).toBe(true);
      expect(stored(h.store, field)).toBe(a);

      // 2. The push for A starts a poll. The server answers its follow-up /get
      //    NOW — while A is the newest thing it holds — but the answer is slow.
      const held = h.server.hold(isPollRead);
      const poll = h.sync.poll();
      await held;

      // 3. The user changes their mind. B's write lands; its own /get says B.
      expect(await h.actions.saveClientPreferences({ [field]: b })).toBe(true);
      expect(h.server.web[field]).toBe(b);
      expect(stored(h.store, field)).toBe(b);

      // 4. The poll's answer — the record as it stood BEFORE B — arrives last.
      h.server.release();
      await poll;

      // The device must still be on B: the shell applies whatever this says.
      expect(stored(h.store, field)).toBe(b);
    },
  );

  it.each(CHANGES)(
    "%s: two quick choices whose answers arrive out of order end on the LATER one",
    async (field, a, b) => {
      const h = await harness();

      // A's write is answered by the server first (A, then B, is the order it
      // stores them in) but its answer is delivered after B's.
      const held = h.server.hold(isWrite);
      const first = h.actions.saveClientPreferences({ [field]: a });
      await held;
      expect(await h.actions.saveClientPreferences({ [field]: b })).toBe(true);
      expect(h.server.web[field]).toBe(b);

      h.server.release();
      expect(await first).toBe(true);

      expect(stored(h.store, field)).toBe(b);
    },
  );
});

describe("a genuinely newer server record still wins (#562 controls)", () => {
  it.each(CHANGES)(
    "%s: a change made on another device, with no local write pending, is applied",
    async (field, a, b) => {
      const h = await harness();
      expect(await h.actions.saveClientPreferences({ [field]: a })).toBe(true);
      expect(stored(h.store, field)).toBe(a);

      // Nothing of ours in flight. Another device picks B; the push polls.
      h.server.changeElsewhere({ [field]: b });
      await h.sync.poll();

      expect(stored(h.store, field)).toBe(b);
    },
  );

  it("a change from another device AFTER the race is still applied — the guard does not pin the field", async () => {
    // The stale read above is ignored for messageOpens; that must not mean
    // every later read is ignored too.
    const h = await harness();
    expect(await h.actions.saveClientPreferences({ messageOpens: "focused" })).toBe(true);
    const held = h.server.hold(isPollRead);
    const poll = h.sync.poll();
    await held;
    expect(await h.actions.saveClientPreferences({ messageOpens: "beside" })).toBe(true);
    h.server.release();
    await poll;
    expect(stored(h.store, "messageOpens")).toBe("beside");

    h.server.changeElsewhere({ messageOpens: "focused" });
    await h.sync.poll();
    expect(stored(h.store, "messageOpens")).toBe("focused");
  });

  it("a stale read keeps the OTHER fields it carries: only the field being written is held back", async () => {
    // Another device moved the theme while this one was changing messageOpens;
    // the stale read is right about the theme and must still deliver it.
    const h = await harness();
    expect(await h.actions.saveClientPreferences({ messageOpens: "focused" })).toBe(true);
    h.server.changeElsewhere({ theme: "dark" });
    const held = h.server.hold(isPollRead);
    const poll = h.sync.poll();
    await held;
    expect(await h.actions.saveClientPreferences({ messageOpens: "beside" })).toBe(true);
    h.server.release();
    await poll;
    expect(stored(h.store, "messageOpens")).toBe("beside");
    expect(stored(h.store, "theme")).toBe("dark");
  });
});

describe("a push that arrives while a poll is running is not lost (#562)", () => {
  // Why the stale value above STAYED for twenty seconds rather than one round
  // trip: the push for the second write arrives while the poll the first write
  // started is still in flight, and poll() handed that caller the in-flight
  // promise — whose /changes had already been read. With push live there is no
  // timer, so nothing asked again until something else changed.
  it("a poll requested mid-poll runs again once the first finishes", async () => {
    const h = await harness();
    const held = h.server.hold(isPollRead);
    h.server.changeElsewhere({ theme: "dark" });
    const first = h.sync.poll();
    await held;

    // The server moves again while that poll's answer is in transit, and the
    // push for it asks for a poll.
    h.server.changeElsewhere({ theme: "light" });
    const second = h.sync.poll();

    h.server.release();
    await first;
    await second;
    expect(stored(h.store, "theme")).toBe("light");
    expect(h.store.getState().cursor.ClientPreferences).toBe(`cp${h.server.seq}`);
  });

  it("NEGATIVE CONTROL: one request, one poll — nothing is re-run for nothing", async () => {
    const h = await harness();
    h.server.changeElsewhere({ theme: "dark" });
    const before = h.server.batches.length;
    await h.sync.poll();
    const polls = h.server.batches
      .slice(before)
      .filter((b) => b.includes("ClientPreferences/changes"));
    expect(polls).toHaveLength(1);
  });
});
