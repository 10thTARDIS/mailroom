// SPDX-License-Identifier: AGPL-3.0-only

// The desk in ONE pass (#665): against a server whose desk capability says
// `getDesk: true`, a desk load is ONE `Area/getDesk` (the counts and every
// Review-first section, from one resolution of every message's area) and ONE
// Email/get for all of its ids — not `Area/getSummary` plus an `Email/query`
// per section, each of which made the server resolve every area again.
//
// What matters, and is tested here:
//   * the wire shape — one getDesk, no per-section Email/query, the tray and
//     the all-mail count unchanged beside it;
//   * that the STORE ends up exactly where the per-section path leaves it, so
//     nothing downstream (DeskRoute, the header sentence) can tell which path
//     loaded it;
//   * the 500-id bound: windows that do not fit one Email/get are hydrated in
//     chunks the server takes;
//   * the fallback: no flag, or a server that refuses the method, gets the
//     per-section batch it always got.

import { describe, expect, it } from "vitest";

import { sectionKey } from "./desk";
import { JmapMethodError } from "./jmap/client";
import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { Area, Email } from "./jmap/types";
import { MailStore } from "./store";
import { PAGE_SIZE, SyncEngine, TRAY_PAGE } from "./sync";

const ACCOUNT = "U1";

function area(id: string, position: number): Area {
  return {
    id,
    name: id,
    position,
    labelId: `L${id}`,
    tabs: [],
    labelIds: [],
    autoArchive: true,
    autoArchiveDays: null,
    hiddenFromRail: false,
  };
}

function email(id: string): Email {
  return {
    id,
    blobId: "b",
    threadId: `t-${id}`,
    mailboxIds: {},
    keywords: {},
    size: 1,
    receivedAt: "2026-07-01T00:00:00Z",
  } as unknown as Email;
}

const SUMMARY_ROWS = [
  { areaId: "A1", surface: 2, drawer: 9, unscored: 1, archivedByDrawer: 4 },
  { areaId: "A2", surface: 0, drawer: 3, unscored: 0, archivedByDrawer: 0 },
  { areaId: null, surface: 1, drawer: 2, unscored: 3, archivedByDrawer: 0 },
];

/** A section's ids, the same answer whichever path asks — keyed on what the
 * section IS, so a mix-up of sections shows as wrong ids under a key. */
function sectionIds(areaId: string | null, n = 2): string[] {
  const base = areaId === null ? "unsorted" : `${areaId}-surface`;
  return Array.from({ length: n }, (_, i) => `${base}-${i + 1}`);
}

/** Evaluates the one result-reference shape each path uses: `/ids`, and
 * `/sections/*\/ids` (RFC 8620 §3.7's `*`, mapped and flattened). */
function resolveRef(value: unknown, path: string): string[] {
  const v = value as { ids?: string[]; sections?: { ids: string[] }[] } | null;
  if (path === "/ids") return v?.ids ?? [];
  if (path === "/sections/*/ids") return (v?.sections ?? []).flatMap((s) => s.ids);
  throw new Error(`unsupported path ${path}`);
}

type Answer = { value?: unknown; error?: JmapMethodError };
type Responder = (call: Call) => Answer;

class FakeClient {
  readonly requests: Call[][] = [];
  constructor(private readonly table: Record<string, Responder>) {}

  get accountId(): string {
    return ACCOUNT;
  }
  pushUrl(): string {
    return "/jmap/eventsource/";
  }

  async request(calls: Call[]): Promise<CallResult[]> {
    this.requests.push(calls);
    const byId = new Map<string, CallResult>();
    const out: CallResult[] = [];
    for (const c of calls) {
      const answer = this.table[c.name]?.(c) ?? { value: {} };
      const result: CallResult = {
        id: c.id,
        name: c.name,
        value: (answer.value ?? null) as CallResult["value"],
        error: answer.error ?? null,
      };
      if (c.name === "Email/get" && !answer.error) {
        const ref = c.args["#ids"] as { resultOf: string; path: string } | undefined;
        let ids: string[];
        if (ref) {
          const source = byId.get(ref.resultOf);
          if (!source || source.error) {
            result.error = new JmapMethodError(c.id, { type: "invalidResultReference" });
            result.value = null;
            byId.set(c.id, result);
            out.push(result);
            continue;
          }
          ids = resolveRef(source.value, ref.path);
        } else {
          ids = c.args["ids"] as string[];
        }
        result.value = { list: ids.map(email), notFound: [], state: "e1" } as CallResult["value"];
      }
      byId.set(c.id, result);
      out.push(result);
    }
    return out;
  }
}

/** The server: Area/getDesk answers each requested section with sectionIds,
 * windowed by its limit; the per-section path's Email/query answers the same
 * ids for the same section. Totals are the same on both paths. */
function server(over: Record<string, Responder> = {}): Record<string, Responder> {
  return {
    "Area/getSummary": () => ({
      value: { accountId: ACCOUNT, state: "area-state-1", threshold: 0.4, list: SUMMARY_ROWS },
    }),
    "Area/getDesk": (c) => {
      const asked = (c.args["sections"] as { areaId: string | null; limit: number }[]) ?? [];
      return {
        value: {
          accountId: ACCOUNT,
          state: "area-state-1",
          queryState: "q1",
          threshold: 0.4,
          list: SUMMARY_ROWS,
          sections: asked.map((s) => ({
            areaId: s.areaId,
            position: 0,
            total: 102,
            ids: sectionIds(s.areaId).slice(0, s.limit),
          })),
          notFound: [],
        },
      };
    },
    "Email/query": (c) => {
      const f = c.args["filter"] as Record<string, unknown>;
      if (c.id === "all") return { value: { ids: ["x"], total: 102, queryState: "q1", position: 0 } };
      if (f["hasKeyword"]) return { value: { ids: ["tray-1", "tray-2"], total: 102, queryState: "q1", position: 0 } };
      const id = f["unsorted"] ? null : (f["inArea"] as string);
      return { value: { ids: sectionIds(id), total: 102, queryState: "q1", position: 0 } };
    },
    ...over,
  };
}

function harness(
  over: Record<string, Responder> = {},
  opts: { snapshot?: boolean; maxObjects?: number; maxCalls?: number; areas?: Area[] } = {},
) {
  const store = new MailStore(null);
  const core: Record<string, number> = { maxCallsInRequest: opts.maxCalls ?? 64 };
  core["maxObjectsInGet"] = opts.maxObjects ?? 500;
  const capabilities: Record<string, unknown> = { "urn:ietf:params:jmap:core": core };
  // A server that predates #665 advertises the desk URN WITHOUT the flag.
  capabilities["urn:mailroom:desk"] = opts.snapshot === false ? {} : { getDesk: true };
  store.setSession({ capabilities } as never, ACCOUNT);
  store.putAreas(opts.areas ?? [area("A2", 1), area("A1", 0)]);
  const client = new FakeClient(server(over));
  const engine = new SyncEngine(client as unknown as JmapClient, store, {
    push: null,
    setTimer: () => 0,
    clearTimer: () => {},
  });
  return { store, client, engine };
}

const names = (calls: Call[]) => calls.map((c) => c.name);

describe("a desk load against a server with Area/getDesk", () => {
  it("is ONE request: getDesk and one Email/get, then the all-mail count and the tray", async () => {
    const h = harness();
    await h.engine.loadDesk("review", null);

    expect(h.client.requests).toHaveLength(1);
    const calls = h.client.requests[0] as Call[];
    expect(names(calls)).toEqual([
      "Area/getDesk",
      "Email/get", // every section's ids, through /sections/*/ids
      "Email/query", // the all-mail count
      "Email/query",
      "Email/get", // the tray, as before
    ]);
    // NO per-section Email/query, and no separate summary.
    const queries = calls.filter((c) => c.name === "Email/query");
    expect(queries.map((c) => c.id)).toEqual(["all", "q0"]);
    expect((queries[1]?.args["filter"] as Record<string, unknown>)["hasKeyword"]).toBe("$flagged");
    expect(queries[1]?.args["limit"]).toBe(TRAY_PAGE);
    expect(queries[1]?.args["collapseThreads"]).toBeUndefined();
    expect(names(calls)).not.toContain("Area/getSummary");

    const desk = calls[0] as Call;
    // Every area in position order, then Needs sorting — the order drawn.
    expect(desk.args["sections"]).toEqual([
      { areaId: "A1", limit: PAGE_SIZE },
      { areaId: "A2", limit: PAGE_SIZE },
      { areaId: null, limit: PAGE_SIZE },
    ]);
    // The STORED line: no threshold, exactly as Area/getSummary was asked.
    expect(desk.args["threshold"]).toBeUndefined();
    expect(desk.args["mailAccountId"]).toBeUndefined();
    const get = calls[1] as Call;
    expect(get.args["#ids"]).toEqual({ resultOf: "desk", name: "Area/getDesk", path: "/sections/*/ids" });
  });

  it("leaves the store EXACTLY where the per-section path leaves it", async () => {
    // DeskRoute and the header sentence read sections and deskSummary; if
    // the two paths differed there, they would show different desks.
    const strip = (h: ReturnType<typeof harness>) => {
      const st = h.store.getState();
      const sections = Object.fromEntries(
        Object.entries(st.sections).map(([k, s]) => [k, { ...s, loadedAt: s.loadedAt === null ? null : "set" }]),
      );
      return { sections, deskSummary: st.deskSummary, emails: Object.keys(st.emails).sort(), cursor: st.cursor.Email };
    };
    for (const scope of [null, "ACC7"]) {
      const one = harness();
      const per = harness({}, { snapshot: false });
      await one.engine.loadDesk("review", scope);
      await per.engine.loadDesk("review", scope);
      expect(names(per.client.requests[0] as Call[])).toContain("Area/getSummary");
      expect(strip(one)).toEqual(strip(per));
      expect(strip(one).sections[sectionKey("surface", "A1", scope)]?.ids).toEqual(["A1-surface-1", "A1-surface-2"]);
    }
  });

  it("scopes getDesk with mailAccountId, and the tray and count with inAccount", async () => {
    const h = harness();
    await h.engine.loadDesk("review", "ACC7");
    const calls = h.client.requests[0] as Call[];
    expect(calls[0]?.args["mailAccountId"]).toBe("ACC7");
    for (const c of calls.filter((x) => x.name === "Email/query")) {
      expect((c.args["filter"] as Record<string, unknown>)["inAccount"]).toBe("ACC7");
    }
    expect(h.store.getState().deskSummary?.scope).toBe("ACC7");
  });

  it("grows only the section named by `more`", async () => {
    const h = harness();
    await h.engine.loadDesk("review", null);
    await h.engine.loadDesk("review", null, { more: sectionKey("surface", "A2", null) });
    const desk = (h.client.requests[1] as Call[])[0] as Call;
    expect(desk.args["sections"]).toEqual([
      { areaId: "A1", limit: PAGE_SIZE },
      { areaId: "A2", limit: 2 * PAGE_SIZE },
      { areaId: null, limit: PAGE_SIZE },
    ]);
    expect(h.store.getState().sections[sectionKey("surface", "A2", null)]?.limit).toBe(2 * PAGE_SIZE);
  });

  it("stamps each section with the move epoch taken before the request left (#685)", async () => {
    // A move that settles while the request is out advances the epoch; the
    // ids that come back predate it and must carry the epoch from BEFORE.
    let epochNow = 7;
    const base = server();
    const h = harness({
      "Area/getDesk": (c) => {
        epochNow = 99;
        return base["Area/getDesk"]!(c);
      },
    });
    h.store.moveEpoch = () => epochNow;
    await h.engine.loadDesk("review", null);
    expect(epochNow).toBe(99);
    for (const a of ["A1", "A2", null]) {
      expect(h.store.getState().sections[sectionKey("surface", a, null)]?.epoch).toBe(7);
    }
  });

  it("Everything else asks getDesk for the counts alone, and fetches no section", async () => {
    const h = harness();
    await h.engine.loadDesk("rest", null);
    const calls = h.client.requests[0] as Call[];
    expect(names(calls)).toEqual(["Area/getDesk", "Email/query", "Email/query", "Email/get"]);
    expect(calls[0]?.args["sections"]).toEqual([]);
    expect(h.store.getState().deskSummary?.rows).toEqual(SUMMARY_ROWS);
    expect(h.store.getState().deskSummary?.allMail).toBe(102);
  });
});

describe("windows that do not fit one Email/get (maxObjectsInGet)", () => {
  it("hydrates in chunks the server takes, after getDesk", async () => {
    // Three sections of 50 against a server that takes 60 ids a /get: one
    // Email/get over /sections/*/ids would ask for 150 and be refused.
    const h = harness({}, { maxObjects: 60 });
    await h.engine.loadDesk("review", null);
    expect(h.client.requests).toHaveLength(2);
    expect(names(h.client.requests[0] as Call[])).toEqual([
      "Area/getDesk",
      "Email/query",
      "Email/query",
      "Email/get", // the tray only
    ]);
    for (const c of h.client.requests[1] as Call[]) {
      expect(c.name).toBe("Email/get");
      expect((c.args["ids"] as string[]).length).toBeLessThanOrEqual(60);
    }
    const st = h.store.getState();
    for (const a of ["A1", "A2", null]) {
      const s = st.sections[sectionKey("surface", a, null)];
      expect(s?.error).toBeNull();
      for (const id of s?.ids ?? []) expect(st.emails[id]).toBeDefined();
    }
  });

  it("NEGATIVE CONTROL: windows that fit are one request", async () => {
    const h = harness({}, { maxObjects: 150 });
    await h.engine.loadDesk("review", null);
    expect(h.client.requests).toHaveLength(1);
  });
});

describe("falling back to the per-section path", () => {
  it("a server without the getDesk flag gets Area/getSummary and a query per section", async () => {
    const h = harness({}, { snapshot: false });
    await h.engine.loadDesk("review", null);
    const calls = h.client.requests[0] as Call[];
    expect(names(calls)).not.toContain("Area/getDesk");
    expect(names(calls)[0]).toBe("Area/getSummary");
    expect(calls.filter((c) => c.name === "Email/query")).toHaveLength(5); // all, tray, A1, A2, unsorted
  });

  it("a server that refuses the method is asked the old way, now and from then on", async () => {
    const h = harness({
      "Area/getDesk": (c) => ({ error: new JmapMethodError(c.id, { type: "unknownMethod" }) }),
    });
    await h.engine.loadDesk("review", null);
    expect(h.client.requests).toHaveLength(2);
    expect(names(h.client.requests[1] as Call[])[0]).toBe("Area/getSummary");
    const st = h.store.getState();
    expect(st.sections[sectionKey("surface", "A1", null)]?.ids).toEqual(["A1-surface-1", "A1-surface-2"]);
    expect(st.deskSummary?.rows).toEqual(SUMMARY_ROWS);
    expect(st.syncError).toBeNull();

    await h.engine.loadDesk("review", null);
    expect(h.client.requests).toHaveLength(3);
    expect(names(h.client.requests[2] as Call[])).not.toContain("Area/getDesk");
  });
});

describe("a desk that did not fully come back", () => {
  it("an area destroyed elsewhere says so; the rest of the desk loads", async () => {
    const h = harness({
      "Area/getDesk": (c) => {
        const asked = c.args["sections"] as { areaId: string | null; limit: number }[];
        return {
          value: {
            accountId: ACCOUNT,
            state: "area-state-2",
            queryState: "q1",
            threshold: 0.4,
            list: SUMMARY_ROWS,
            sections: asked
              .filter((s) => s.areaId !== "A2")
              .map((s) => ({ areaId: s.areaId, position: 0, total: 2, ids: sectionIds(s.areaId) })),
            notFound: ["A2"],
          },
        };
      },
    });
    await h.engine.loadDesk("review", null);
    const st = h.store.getState();
    expect(st.sections[sectionKey("surface", "A2", null)]?.error).toMatch(/no longer exists/);
    expect(st.sections[sectionKey("surface", "A2", null)]?.loading).toBe(false);
    expect(st.sections[sectionKey("surface", "A1", null)]?.ids).toEqual(["A1-surface-1", "A1-surface-2"]);
    expect(st.sections[sectionKey("surface", null, null)]?.ids).toEqual(["unsorted-1", "unsorted-2"]);
  });

  it("a refused getDesk marks every section and the counts, not just one", async () => {
    const h = harness({
      "Area/getDesk": (c) => ({ error: new JmapMethodError(c.id, { type: "serverFail", description: "boom" }) }),
    });
    await h.engine.loadDesk("review", null);
    const st = h.store.getState();
    expect(st.deskSummary).toBeNull();
    for (const a of ["A1", "A2", null]) {
      const s = st.sections[sectionKey("surface", a, null)];
      expect(s?.loading).toBe(false);
      expect(s?.error).toMatch(/boom/);
    }
    // The tray is its own query and still landed.
    expect(st.sections[sectionKey("tray", null, null)]?.ids).toEqual(["tray-1", "tray-2"]);
    expect(st.syncError).toMatch(/boom/);
  });
});
