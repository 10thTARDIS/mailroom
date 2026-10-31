// SPDX-License-Identifier: AGPL-3.0-only

// Desk loads are single-flight with one trailing re-run, generation-guarded and
// abortable (#665). In practice many full desk loads ran in a few minutes,
// each slower than the last because they overlapped, and the response that
// arrived LAST won whether or not it was the newest question.
//
// The transport here is controllable: every request parks until the test
// resolves it by hand, in whatever order the test wants, and records the
// AbortSignal it was given.

import { describe, expect, it } from "vitest";

import { sectionKey } from "./desk";
import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { Email } from "./jmap/types";
import { MailStore } from "./store";
import { SyncEngine, TRAY_PAGE } from "./sync";

const ACCOUNT = "U1";

function email(id: string): Email {
  return { id, blobId: "b", threadId: `t-${id}`, mailboxIds: {}, keywords: {}, size: 1 } as unknown as Email;
}

interface Pending {
  calls: Call[];
  signal: AbortSignal | undefined;
  /** Answer the whole batch: the tray section gets `ids`. */
  resolve(ids: string[]): void;
  reject(err: unknown): void;
}

class ControlledClient {
  readonly pending: Pending[] = [];
  constructor(private readonly honourAbort = true) {}

  get accountId(): string {
    return ACCOUNT;
  }
  pushUrl(): string {
    return "/jmap/eventsource/";
  }

  request(calls: Call[], _using?: readonly string[], signal?: AbortSignal): Promise<CallResult[]> {
    return new Promise<CallResult[]>((res, rej) => {
      const p: Pending = {
        calls,
        signal,
        reject: rej,
        resolve: (ids) => {
          const out: CallResult[] = calls.map((c) => {
            if (c.name === "Area/getSummary") {
              return { id: c.id, name: c.name, error: null, value: { accountId: ACCOUNT, state: "s", threshold: 0.4, list: [] } } as never;
            }
            if (c.name === "Email/query") {
              const isAll = c.id === "all";
              return {
                id: c.id,
                name: c.name,
                error: null,
                value: { ids: isAll ? [] : ids, total: ids.length, queryState: "q1", position: 0 },
              } as never;
            }
            return { id: c.id, name: c.name, error: null, value: { list: ids.map(email), notFound: [], state: "e1" } } as never;
          });
          res(out);
        },
      };
      if (signal && this.honourAbort) {
        signal.addEventListener("abort", () => rej(new DOMException("aborted", "AbortError")));
      }
      this.pending.push(p);
    });
  }
}

function harness(honourAbort = true) {
  const store = new MailStore(null);
  store.setSession(
    { capabilities: { "urn:ietf:params:jmap:core": { maxCallsInRequest: 64 } } } as never,
    ACCOUNT,
  );
  const client = new ControlledClient(honourAbort);
  const engine = new SyncEngine(client as unknown as JmapClient, store, {
    push: null,
    setTimer: () => 0,
    clearTimer: () => {},
  });
  return { store, client, engine };
}

/** Lets queued microtasks run so a load that is going to send its request has. */
const settle = () => new Promise<void>((r) => setTimeout(r, 0));

/** The limit the (single) tray query of a parked request asked for. */
function trayLimit(p: Pending): unknown {
  return p.calls.filter((c) => c.name === "Email/query" && c.id !== "all")[0]?.args["limit"];
}

describe("desk loads are single-flight (#665)", () => {
  it("two calls during one flight send two requests in total", async () => {
    const h = harness();
    const a = h.engine.loadDesk("rest", null);
    await settle();
    const b = h.engine.loadDesk("rest", null);
    await settle();
    expect(h.client.pending).toHaveLength(1);
    h.client.pending[0]?.resolve(["x"]);
    await settle();
    expect(h.client.pending).toHaveLength(2);
    h.client.pending[1]?.resolve(["x"]);
    await Promise.all([a, b]);
    expect(h.client.pending).toHaveLength(2);
  });

  it("five calls during one flight still send exactly two", async () => {
    const h = harness();
    const all = [h.engine.loadDesk("rest", null)];
    await settle();
    for (let i = 0; i < 4; i++) all.push(h.engine.loadDesk("rest", null));
    await settle();
    h.client.pending[0]?.resolve(["x"]);
    await settle();
    h.client.pending[1]?.resolve(["x"]);
    await Promise.all(all);
    await settle();
    expect(h.client.pending).toHaveLength(2);
  });
});

describe("a stale response cannot overwrite a newer one (#665)", () => {
  it("an older response arriving after a newer one is dropped", async () => {
    // Abort is NOT honoured by this transport, so the superseded request still
    // answers — late — which is the case the generation guard exists for.
    const h = harness(false);
    const first = h.engine.loadDesk("rest", "ACC1");
    await settle();
    const second = h.engine.loadDesk("rest", "ACC2");
    await settle();
    expect(h.client.pending).toHaveLength(2);

    h.client.pending[1]?.resolve(["new-1"]);
    await second;
    h.client.pending[0]?.resolve(["old-1"]);
    await first;

    const st = h.store.getState();
    expect(st.sections[sectionKey("tray", null, "ACC2")]?.ids).toEqual(["new-1"]);
    expect(st.sections[sectionKey("tray", null, "ACC1")]).toBeUndefined();
  });

  it("the same holds when the same desk is asked for twice and answers cross", async () => {
    const h = harness(false);
    const first = h.engine.loadDesk("rest", null);
    await settle();
    const second = h.engine.loadDesk("rest", "ACC2");
    await settle();
    const third = h.engine.loadDesk("rest", null);
    await settle();
    // `first` and `third` are the same desk, but `third` is the newer question.
    h.client.pending[2]?.resolve(["third"]);
    await third;
    h.client.pending[0]?.resolve(["first"]);
    h.client.pending[1]?.resolve(["second"]);
    await Promise.all([first, second]);
    const st = h.store.getState();
    expect(st.sections[sectionKey("tray", null, null)]?.ids).toEqual(["third"]);
  });
});

describe("a superseded load is aborted (#665)", () => {
  it("a scope change aborts the old request and surfaces no error", async () => {
    const h = harness();
    const old = h.engine.loadDesk("rest", "ACC1");
    await settle();
    const first = h.client.pending[0] as Pending;
    expect(first.signal?.aborted).toBe(false);

    const next = h.engine.loadDesk("rest", "ACC2");
    await settle();
    expect(first.signal?.aborted).toBe(true);
    h.client.pending[1]?.resolve(["fresh"]);
    await Promise.all([old, next]);

    const st = h.store.getState();
    expect(st.syncError).toBeNull();
    expect(Object.values(st.sections).every((s) => s.error == null)).toBe(true);
    expect(st.sections[sectionKey("tray", null, "ACC2")]?.ids).toEqual(["fresh"]);
    expect(h.client.pending[1]?.signal?.aborted).toBe(false);
  });

  it("a mere refresh of the same desk does not abort the request in flight", async () => {
    const h = harness();
    const a = h.engine.loadDesk("rest", null);
    await settle();
    const b = h.engine.loadDesk("rest", null);
    await settle();
    expect(h.client.pending[0]?.signal?.aborted).toBe(false);
    h.client.pending[0]?.resolve(["x"]);
    await settle();
    h.client.pending[1]?.resolve(["x"]);
    await Promise.all([a, b]);
  });
});

describe("a burst of refreshes walks the open drawers once more, not once per caller (#665)", () => {
  it("five refreshDesk calls during one flight reload each open drawer at most twice", async () => {
    const h = harness();
    h.store.putAreas([
      { id: "A1", name: "One", position: 1, labelId: "L1" },
      { id: "A2", name: "Two", position: 2, labelId: "L2" },
    ] as never);
    // Both drawers are open: they have been loaded once.
    for (const id of ["A1", "A2"]) h.store.setSection({ key: sectionKey("drawer", id, null), loadedAt: 1 });
    const drawers: string[] = [];
    (h.engine as unknown as { loadDrawer(id: string): Promise<void> }).loadDrawer = async (id: string) => {
      drawers.push(id);
    };
    const first = h.engine.loadDesk("rest", null);
    await settle();
    const burst = Array.from({ length: 5 }, () => h.engine.refreshDesk());
    await settle();
    h.client.pending[0]?.resolve(["x"]);
    await settle();
    h.client.pending[1]?.resolve(["x"]);
    await Promise.all([first, ...burst]);
    await settle();
    // One pass, plus at most one trailing pass, for the whole burst: not five.
    expect(drawers.length).toBeGreaterThanOrEqual(2);
    expect(drawers.length).toBeLessThanOrEqual(4);
  });
});

describe("'show more' survives being coalesced (#665)", () => {
  const trayKey = sectionKey("tray", null, null);

  it("a more request made during a flight grows the section in the trailing load", async () => {
    const h = harness();
    const a = h.engine.loadDesk("rest", null);
    await settle();
    const b = h.engine.loadDesk("rest", null, { more: trayKey });
    // A plain refresh arriving after it must not erase the request.
    const c = h.engine.loadDesk("rest", null);
    await settle();
    expect(trayLimit(h.client.pending[0] as Pending)).toBe(TRAY_PAGE);
    h.client.pending[0]?.resolve(["x"]);
    await settle();
    expect(h.client.pending).toHaveLength(2);
    expect(trayLimit(h.client.pending[1] as Pending)).toBe(TRAY_PAGE * 2);
    h.client.pending[1]?.resolve(["x"]);
    await Promise.all([a, b, c]);
    expect(h.store.getState().sections[trayKey]?.limit).toBe(TRAY_PAGE * 2);
  });
});
