// SPDX-License-Identifier: AGPL-3.0-only

// A REFUSED QUERY IS NOT "NOTHING MATCHES" (#708).
//
// allMatchingIds backs "Move all from <address> to…" and "select all matching".
// It used to skip a query that came back as a method error, so a server with
// search turned off (Email/query answers `unsupportedFilter` to a text filter)
// produced an empty set, and the sender card said there was no mail from that
// address. It must throw instead, so the caller can say what actually happened.

import { describe, expect, it } from "vitest";

import type { Call, CallResult, JmapClient } from "./jmap/client";
import { JmapMethodError } from "./jmap/client";
import { MailStore } from "./store";
import { BULK_SEARCH_LIMIT, SyncEngine } from "./sync";

const ACCOUNT = "U1";

function engineFor(answer: (c: Call) => Pick<CallResult, "value" | "error">): SyncEngine {
  const store = new MailStore(null);
  store.setSession(
    { capabilities: { "urn:ietf:params:jmap:core": { maxCallsInRequest: 64 } } } as never,
    ACCOUNT,
  );
  store.setConn("online");
  const client = {
    accountId: ACCOUNT,
    request: async (calls: Call[]): Promise<CallResult[]> =>
      calls.map((c) => ({ id: c.id, name: c.name, ...answer(c) }) as CallResult),
  };
  return new SyncEngine(client as unknown as JmapClient, store, {
    push: null,
    setTimer: () => 0,
    clearTimer: () => {},
  });
}

const SENDER = { kind: "search" as const, query: "from:alan@example.com" };

describe("allMatchingIds", () => {
  it("returns the ids and the total of a query that succeeds", async () => {
    const engine = engineFor(() => ({
      value: { ids: ["M1", "M2"], total: 2, queryState: "q1" } as unknown as CallResult["value"],
      error: null,
    }));
    await expect(engine.allMatchingIds(SENDER)).resolves.toEqual({ ids: ["M1", "M2"], total: 2 });
  });

  it("asks for the search index's full limit, so a bulk enumeration is not cut at 500 (#769)", async () => {
    const seen: Call[] = [];
    const engine = engineFor((c) => {
      seen.push(c);
      return { value: { ids: [], total: 0, queryState: "q1" } as unknown as CallResult["value"], error: null };
    });
    await engine.allMatchingIds(SENDER);
    expect(BULK_SEARCH_LIMIT).toBe(5000);
    expect(seen.length).toBeGreaterThan(0);
    for (const c of seen) expect(c.args).toMatchObject({ searchLimit: 5000 });
  });

  it("throws the method error when search is turned off, never an empty set", async () => {
    const engine = engineFor((c) => ({
      value: null as unknown as CallResult["value"],
      error: new JmapMethodError(c.id, { type: "unsupportedFilter", description: "search is disabled" }),
    }));
    await expect(engine.allMatchingIds(SENDER)).rejects.toMatchObject({ type: "unsupportedFilter" });
  });
});
