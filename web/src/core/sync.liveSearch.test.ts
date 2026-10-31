// SPDX-License-Identifier: AGPL-3.0-only

// LIVE SEARCH AT THE REQUEST LAYER (#724): one list load per pause in typing,
// so the loads overlap, and what is on screen must always be the NEWEST
// search's answer — never an older one that happened to arrive later — without
// the list blanking between them.

import { describe, expect, it } from "vitest";

import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { Email } from "./jmap/types";
import { MailStore } from "./store";
import { SyncEngine } from "./sync";
import type { View } from "./views";
import { viewKey } from "./views";

const ACCOUNT = "U1";

function email(id: string, receivedAt: string): Email {
  return {
    id,
    blobId: "b-" + id,
    threadId: "t-" + id,
    mailboxIds: {},
    keywords: {},
    size: 1,
    receivedAt,
    subject: id,
    preview: "",
    hasAttachment: false,
  } as Email;
}

interface Pending {
  calls: Call[];
  signal: AbortSignal | undefined;
  answer: (ids: string[], extra?: Record<string, unknown>) => void;
}

/** A client whose every request waits until the test answers it, in any order. */
function harness() {
  const store = new MailStore(null);
  store.setSession(
    { capabilities: { "urn:ietf:params:jmap:core": { maxCallsInRequest: 64 } } } as never,
    ACCOUNT,
  );
  store.setConn("online");
  const pending: Pending[] = [];
  const emails: Record<string, Email> = {};
  const client = {
    accountId: ACCOUNT,
    request: (calls: Call[], _using?: unknown, signal?: AbortSignal): Promise<CallResult[]> =>
      new Promise((resolve) => {
        pending.push({
          calls,
          signal,
          answer: (ids, extra = {}) =>
            resolve(
              calls.map((c) =>
                c.name === "Email/query"
                  ? ({ id: c.id, name: c.name, value: { ids, total: ids.length, queryState: "s1", ...extra } } as unknown as CallResult)
                  : ({ id: c.id, name: c.name, value: { list: ids.map((i) => emails[i]).filter(Boolean) } } as unknown as CallResult),
              ),
            ),
        });
      }),
    searchSnippetGet: async () => ({ list: [] }),
  };
  const engine = new SyncEngine(client as unknown as JmapClient, store, {
    push: null,
    setTimer: () => 0,
    clearTimer: () => {},
  });
  return { store, engine, pending, emails };
}

const flush = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

const INV: View = { kind: "search", query: "inv", prefix: true };
const INVOICE: View = { kind: "search", query: "invoice" };

describe("loadView under live search", () => {
  it("a stale response never overwrites a newer one, whatever order they arrive in", async () => {
    const h = harness();
    h.emails["A1"] = email("A1", "2026-10-01T00:00:00Z");
    h.emails["B1"] = email("B1", "2026-10-02T00:00:00Z");

    const first = h.engine.loadView(INV);
    const second = h.engine.loadView(INVOICE);
    expect(h.pending.length).toBe(2);
    // The older request was cancelled when the newer one started.
    expect(h.pending[0]?.signal?.aborted).toBe(true);
    expect(h.pending[1]?.signal?.aborted).toBe(false);

    // OUT OF ORDER: the newer answer lands first, then the older one.
    h.pending[1]?.answer(["B1"]);
    await second;
    h.pending[0]?.answer(["A1"]);
    await first;
    await flush();

    const list = h.store.getState().list;
    expect(list.key).toBe(viewKey(INVOICE));
    expect(list.ids).toEqual(["B1"]);
    expect(list.loading).toBe(false);
  });

  it("in order too: the older answer, arriving first, is dropped once a newer load has started", async () => {
    const h = harness();
    h.emails["A1"] = email("A1", "2026-10-01T00:00:00Z");
    h.emails["B1"] = email("B1", "2026-10-02T00:00:00Z");
    const first = h.engine.loadView(INV);
    const second = h.engine.loadView(INVOICE);
    h.pending[0]?.answer(["A1"]);
    await first;
    expect(h.store.getState().list.ids).not.toContain("A1");
    h.pending[1]?.answer(["B1"]);
    await second;
    expect(h.store.getState().list.ids).toEqual(["B1"]);
  });

  it("keeps the previous search's rows on screen while the next one loads (no flicker to empty)", async () => {
    const h = harness();
    h.emails["A1"] = email("A1", "2026-10-01T00:00:00Z");
    h.emails["B1"] = email("B1", "2026-10-02T00:00:00Z");
    const first = h.engine.loadView(INV);
    h.pending[0]?.answer(["A1"]);
    await first;

    const second = h.engine.loadView(INVOICE);
    // Between the keystroke's request and its answer: the new key, the OLD rows.
    const mid = h.store.getState().list;
    expect(mid.key).toBe(viewKey(INVOICE));
    expect(mid.ids).toEqual(["A1"]);
    expect(mid.loading).toBe(true);
    h.pending[1]?.answer(["B1"]);
    await second;
    expect(h.store.getState().list.ids).toEqual(["B1"]);

    // Leaving search for somewhere else is a different list: it starts empty.
    void h.engine.loadView({ kind: "role", role: "inbox" });
    expect(h.store.getState().list.ids).toEqual([]);
  });

  it("asks for the prefix and the relevance order, and keeps the server's order", async () => {
    const h = harness();
    h.emails["OLD"] = email("OLD", "2020-01-01T00:00:00Z");
    h.emails["NEW"] = email("NEW", "2026-10-02T00:00:00Z");
    const view: View = { kind: "search", query: "invo", prefix: true, sort: "relevance" };
    const load = h.engine.loadView(view);
    const q = h.pending[0]?.calls.find((c) => c.name === "Email/query");
    expect(q?.args["filter"]).toEqual({ query: "invo", queryPrefix: true });
    expect(q?.args["sort"]).toEqual([{ property: "relevance" }]);
    // Best match first, though it is the OLDER message: not re-sorted by date.
    h.pending[0]?.answer(["OLD", "NEW"]);
    await load;
    expect(h.store.getState().list.ids).toEqual(["OLD", "NEW"]);
  });

  it("records the index's bound when the search was capped, and clears it when it was not", async () => {
    const h = harness();
    h.emails["A1"] = email("A1", "2026-10-01T00:00:00Z");
    const capped = h.engine.loadView(INVOICE);
    h.pending[0]?.answer(["A1"], { searchCapped: 500, total: 500 });
    await capped;
    expect(h.store.getState().list.searchCapped).toBe(500);
    expect(h.store.getState().list.total).toBe(500);

    const plain = h.engine.loadView(INV);
    h.pending[1]?.answer(["A1"]);
    await plain;
    expect(h.store.getState().list.searchCapped).toBeNull();
  });
});
