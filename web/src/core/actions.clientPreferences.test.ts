// SPDX-License-Identifier: AGPL-3.0-only

// SAVING THIS DEVICE'S APPEARANCE (#375 increment 5).
//
// Four things here can go wrong quietly, which is why each is a test rather
// than a comment:
//
//   * the SHAPE on the wire. `clientPreferencesSet` refuses `create` outright
//     ("the client families are a fixed set; update one instead of creating a
//     row"), so a client that guessed at create would have a settings page that
//     silently never persisted anything. It must be `update`, keyed on the id
//     the server itself handed back from `/get`.
//   * a REFUSAL must leave the device on the value the user chose. Snapping the
//     UI back to the server's copy would make a rejected write look like the
//     user never clicked, and a device is supposed to keep working when the
//     server says no (or is not there at all).
//   * the DEBOUNCE. The splitter fires on every pointermove; without
//     coalescing, one drag across the window is hundreds of `set` calls.
//   * the id is FETCHED when it is not known. A save can precede the
//     reference-data load, and inventing a create the server would refuse is
//     the failure mode this exists to avoid.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { MailActions, WEB_CLIENT_FAMILY } from "./actions";
import { JmapTransportError } from "./jmap/client";
import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { ClientPreferences } from "./jmap/types";
import { StorageActionQueue } from "./queue";
import { MemoryStorage } from "./storage";
import { MailStore } from "./store";
import type { SyncEngine } from "./sync";

const ACCOUNT = "U1";
const WEB_ID = "CP-WEB";

function webPrefs(over: Partial<ClientPreferences> = {}): ClientPreferences {
  return {
    id: WEB_ID,
    family: "web",
    theme: "system",
    density: "normal",
    messageOpens: "beside",
    paneSplitPct: 40,
    textSize: "m",
    rowLayout: "oneLine",
    ...over,
  };
}

interface Options {
  /** Seed the store with the web row, as the reference-data load would. */
  seeded?: boolean;
  setOutcome?: "ok" | "refuse" | "silent" | "throw";
  /** What the `/get` beside the set answers with. */
  serverPrefs?: ClientPreferences;
}

function harness(opts: Options = {}) {
  const store = new MailStore(null);
  store.setSession({} as never, ACCOUNT);
  store.setConn("online");
  if (opts.seeded !== false) {
    store.putClientPreferences([webPrefs()], store.beginClientPreferencesRead());
  }

  const errors: string[] = [];
  const requests: Call[][] = [];

  const client = {
    request: async (calls: Call[]): Promise<CallResult[]> => {
      requests.push(calls);
      if (opts.setOutcome === "throw" && calls.some((c) => c.name === "ClientPreferences/set")) {
        throw new JmapTransportError("server down");
      }
      return calls.map((c): CallResult => {
        if (c.name === "ClientPreferences/set") {
          const id = Object.keys((c.args["update"] as Record<string, unknown>) ?? {})[0] as string;
          if (opts.setOutcome === "refuse") {
            return {
              id: c.id,
              name: c.name,
              value: {
                updated: null,
                notUpdated: { [id]: { type: "invalidProperties", description: "density must be one of compact, normal, relaxed" } },
                newState: "cp1",
              },
              error: null,
            };
          }
          if (opts.setOutcome === "silent") {
            return {
              id: c.id,
              name: c.name,
              value: { updated: {}, notUpdated: {}, newState: "cp1" },
              error: null,
            };
          }
          return {
            id: c.id,
            name: c.name,
            value: { updated: { [id]: null }, notUpdated: {}, newState: "cp2" },
            error: null,
          };
        }
        // ClientPreferences/get — all three families, web first.
        return {
          id: c.id,
          name: c.name,
          value: {
            list: [
              opts.serverPrefs ?? webPrefs({ theme: "dark" }),
              { ...webPrefs(), id: "CP-PHONE", family: "phone" },
              { ...webPrefs(), id: "CP-DESK", family: "desktop" },
            ],
            notFound: [],
            state: "cp2",
          },
          error: null,
        };
      });
    },
  } as unknown as JmapClient;

  const sync = { refreshDesk: async () => {}, currentDesk: null } as unknown as SyncEngine;
  const queue = new StorageActionQueue(new MemoryStorage());
  const actions = new MailActions(client, store, queue, sync, {
    onError: (m) => errors.push(m),
  });
  return { store, actions, errors, requests };
}

describe("saveClientPreferences", () => {
  it("sends an UPDATE keyed on the server's own id, never a create", async () => {
    const h = harness();
    expect(await h.actions.saveClientPreferences({ theme: "dark" })).toBe(true);

    const batch = h.requests[0] as Call[];
    expect(batch.map((c) => c.name)).toEqual([
      "ClientPreferences/set",
      // The server's copy, read AFTER the set: `updated` carries a bare null,
      // so it says nothing about what is now stored.
      "ClientPreferences/get",
    ]);
    const args = (batch[0] as Call).args;
    expect(args["update"]).toEqual({ [WEB_ID]: { theme: "dark" } });
    expect(args["create"]).toBeUndefined();
    expect(args["destroy"]).toBeUndefined();
  });

  it("sends only the properties it was given", async () => {
    const h = harness();
    await h.actions.saveClientPreferences({ paneSplitPct: 37 });
    const update = ((h.requests[0] as Call[])[0] as Call).args["update"] as Record<string, unknown>;
    expect(update[WEB_ID]).toEqual({ paneSplitPct: 37 });
  });

  it("stores what the SERVER says it now holds, not what was sent", async () => {
    const h = harness({ serverPrefs: webPrefs({ theme: "light", paneSplitPct: 55 }) });
    await h.actions.saveClientPreferences({ theme: "dark" });
    const stored = h.store.getState().clientPreferences[WEB_CLIENT_FAMILY];
    expect(stored?.theme).toBe("light");
    expect(stored?.paneSplitPct).toBe(55);
    // ...and the other families it returned are kept too.
    expect(Object.keys(h.store.getState().clientPreferences).sort()).toEqual([
      "desktop",
      "phone",
      "web",
    ]);
  });

  it("reports a REFUSAL and leaves the stored value alone", async () => {
    // The device keeps the value the user chose — the local apply already
    // happened, and this must not write the server's copy over it.
    const h = harness({ setOutcome: "refuse" });
    expect(await h.actions.saveClientPreferences({ density: "relaxed" })).toBe(false);
    expect(h.errors).toHaveLength(1);
    expect(h.errors[0]).toContain("density must be one of");
    expect(h.store.getState().clientPreferences[WEB_CLIENT_FAMILY]?.density).toBe("normal");
  });

  it("refuses to believe a set the server did not name", async () => {
    const h = harness({ setOutcome: "silent" });
    expect(await h.actions.saveClientPreferences({ theme: "dark" })).toBe(false);
    expect(h.errors[0]).toContain("did not say it changed");
    expect(h.store.getState().clientPreferences[WEB_CLIENT_FAMILY]?.theme).toBe("system");
  });

  it("reports an unreachable server and stores nothing", async () => {
    const h = harness({ setOutcome: "throw" });
    expect(await h.actions.saveClientPreferences({ theme: "dark" })).toBe(false);
    expect(h.errors[0]).toContain("unreachable");
    expect(h.store.getState().clientPreferences[WEB_CLIENT_FAMILY]?.theme).toBe("system");
  });

  it("asks the server for the web row's id when it has not read one", async () => {
    // A save can precede the reference-data load. `/get` MATERIALISES the row
    // server-side, so asking is always enough — and `create` would be refused.
    const h = harness({ seeded: false });
    expect(await h.actions.saveClientPreferences({ theme: "dark" })).toBe(true);
    expect(h.requests).toHaveLength(2);
    expect((h.requests[0] as Call[]).map((c) => c.name)).toEqual(["ClientPreferences/get"]);
    const update = ((h.requests[1] as Call[])[0] as Call).args["update"] as Record<string, unknown>;
    expect(Object.keys(update)).toEqual([WEB_ID]);
  });

  it("writes nothing at all for an empty patch", async () => {
    const h = harness();
    expect(await h.actions.saveClientPreferences({})).toBe(true);
    expect(h.requests).toHaveLength(0);
  });
});

describe("the splitter's debounce", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("coalesces two drags into ONE set, carrying the LAST value", async () => {
    const h = harness();
    const first = h.actions.saveClientPreferences({ paneSplitPct: 30 }, { debounceMs: 500 });
    const second = h.actions.saveClientPreferences({ paneSplitPct: 44 }, { debounceMs: 500 });
    // Nothing has gone out yet: a drag in progress is not a decision.
    expect(h.requests).toHaveLength(0);

    await vi.advanceTimersByTimeAsync(500);
    expect(await first).toBe(true);
    expect(await second).toBe(true);

    expect(h.requests).toHaveLength(1);
    const update = ((h.requests[0] as Call[])[0] as Call).args["update"] as Record<string, unknown>;
    expect(update[WEB_ID]).toEqual({ paneSplitPct: 44 });
  });

  it("merges DIFFERENT properties rather than dropping the earlier one", async () => {
    const h = harness();
    void h.actions.saveClientPreferences({ theme: "dark" }, { debounceMs: 500 });
    void h.actions.saveClientPreferences({ paneSplitPct: 44 }, { debounceMs: 500 });
    await vi.advanceTimersByTimeAsync(500);
    const update = ((h.requests[0] as Call[])[0] as Call).args["update"] as Record<string, unknown>;
    expect(update[WEB_ID]).toEqual({ theme: "dark", paneSplitPct: 44 });
  });

  it("NEGATIVE CONTROL: two writes far enough apart are two sets", async () => {
    // Proves the single request above is the debounce doing something, not the
    // harness only ever recording one batch.
    const h = harness();
    void h.actions.saveClientPreferences({ paneSplitPct: 30 }, { debounceMs: 500 });
    await vi.advanceTimersByTimeAsync(500);
    void h.actions.saveClientPreferences({ paneSplitPct: 44 }, { debounceMs: 500 });
    await vi.advanceTimersByTimeAsync(500);
    expect(h.requests).toHaveLength(2);
  });

  it("does not debounce a write that did not ask to be debounced", async () => {
    const h = harness();
    await h.actions.saveClientPreferences({ theme: "dark" });
    expect(h.requests).toHaveLength(1);
  });
});
