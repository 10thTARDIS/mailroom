// SPDX-License-Identifier: AGPL-3.0-only

// The sync engine's half of the desk (#375), against a fake JMAP client that
// answers from a table and records every batch it was given.
//
// Three things here are worth a test, and they are all about the SHAPE of what
// goes on the wire rather than about the data coming back:
//
//   * the whole desk is ONE request. A dozen areas must not become a dozen round
//     trips, and the only way to know is to count the batches;
//   * `rest` must not fetch drawer contents. A closed drawer draws itself from
//     the summary's count, so a query per drawer would be a page of mail fetched
//     for every row nobody opened;
//   * a section that could not be QUERIED must say so. Against a server that
//     predates the desk filters, `unsupportedFilter` arrives per call and an
//     empty list would read as "no mail here" — the same wrong answer #20
//     shipped when a dropped filter condition returned a superset.
//
// And one about the change loop: with a desk on screen, an Email or Area change
// has to re-run the DESK. Re-running `loadView` instead would refresh a list
// nobody is looking at and leave every visible section stale.

import { describe, expect, it } from "vitest";

import { sectionKey } from "./desk";
import { JmapMethodError } from "./jmap/client";
import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { Area, Email } from "./jmap/types";
import { CHANGE_TYPES, PUSH_TYPES, emptySyncCursor } from "./session";
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
    sentAt: null,
    subject: id,
    preview: "",
    hasAttachment: false,
    from: null,
    messageId: [],
    inReplyTo: [],
    references: [],
    deliveredTo: null,
    identityId: null,
  } as unknown as Email;
}

/** What the fake server does with one call: an answer, or a refusal. */
type Answer = { value?: unknown; error?: JmapMethodError };
type Responder = (call: Call) => Answer;

/**
 * A fake JmapClient that behaves like the real batch endpoint in the one way
 * that matters here: calls run IN ORDER, and an `#ids` result reference reads
 * the answer of the call it names. Without that, an Email/get in these batches
 * could not be distinguished from any other.
 */
class FakeClient {
  readonly requests: Call[][] = [];
  constructor(private readonly table: Record<string, Responder>) {}

  get accountId(): string {
    return ACCOUNT;
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
        const ref = c.args["#ids"] as { resultOf: string } | undefined;
        const source = ref ? byId.get(ref.resultOf) : undefined;
        const ids = ((source?.value as { ids?: string[] } | null)?.ids ?? []) as string[];
        result.value = { list: ids.map(email), notFound: [], state: "e1" } as CallResult["value"];
      }
      byId.set(c.id, result);
      out.push(result);
    }
    return out;
  }

  /** Never reached in these tests, but the engine's type demands it. */
  pushUrl(): string {
    return "/jmap/eventsource/";
  }
}

/** Ids a section's query returns, derived from the filter so each section gets
 * its own answer — which is what proves the ids landed under the right key. */
function idsFor(call: Call): string[] {
  const f = call.args["filter"] as Record<string, unknown>;
  if (f["hasKeyword"]) return ["tray-1", "tray-2"];
  if (f["unsorted"]) return ["unsorted-1"];
  const inArea = f["inArea"] as string | undefined;
  const side = f["onSurface"] === false ? "drawer" : "surface";
  return [`${inArea}-${side}-1`, `${inArea}-${side}-2`];
}

const SUMMARY_ROWS = [
  { areaId: "A1", surface: 2, drawer: 9, unscored: 1, archivedByDrawer: 4 },
  { areaId: "A2", surface: 0, drawer: 3, unscored: 0, archivedByDrawer: 0 },
  { areaId: null, surface: 1, drawer: 2, unscored: 3, archivedByDrawer: 0 },
];

const summaryAnswer: Answer = {
  value: { accountId: ACCOUNT, state: "area-state-1", threshold: 0.4, list: SUMMARY_ROWS },
};

function harness(
  over: Record<string, Responder> = {},
  opts: { maxCalls?: number; areas?: Area[] } = {},
): {
  store: MailStore;
  client: FakeClient;
  engine: SyncEngine;
} {
  const store = new MailStore(null);
  // A session that advertises what this server actually advertises
  // (internal/api/request.go maxCallsInRequest = 64). A fixture with no
  // capabilities at all would exercise the conservative fallback instead of the
  // batch size every real deployment has.
  const session = {
    capabilities: { "urn:ietf:params:jmap:core": { maxCallsInRequest: opts.maxCalls ?? 64 } },
  } as never;
  store.setSession(session, ACCOUNT);
  store.putAreas(opts.areas ?? [area("A2", 1), area("A1", 0)]);
  const client = new FakeClient({
    "Area/getSummary": () => summaryAnswer,
    "Email/query": (c) => {
      const ids = idsFor(c);
      return { value: { ids, total: ids.length + 100, queryState: "q1", position: 0 } };
    },
    ...over,
  });
  const engine = new SyncEngine(client as unknown as JmapClient, store, {
    push: null,
    setTimer: () => 0,
    clearTimer: () => {},
  });
  return { store, client, engine };
}

function names(calls: Call[]): string[] {
  return calls.map((c) => c.name);
}

describe("loading the Review-first desk", () => {
  it("is exactly one request: the counts, the tray, each area, Needs sorting last", async () => {
    const h = harness();
    await h.engine.loadDesk("review", null);

    expect(h.client.requests).toHaveLength(1);
    const calls = h.client.requests[0] as Call[];
    expect(names(calls)).toEqual([
      "Area/getSummary",
      "Email/query", // the all-mail count (#375) — no Email/get, it wants a total
      "Email/query",
      "Email/get", // the tray
      "Email/query",
      "Email/get", // A1 (position 0)
      "Email/query",
      "Email/get", // A2 (position 1)
      "Email/query",
      "Email/get", // Needs sorting, LAST
    ]);
    const filters = calls
      .filter((c) => c.name === "Email/query")
      .map((c) => c.args["filter"] as Record<string, unknown>);
    expect(filters[0]).toEqual({}); // all mail: no mailbox, no scope
    expect(filters[1]?.["hasKeyword"]).toBe("$flagged");
    expect(filters[2]?.["inArea"]).toBe("A1");
    expect(filters[3]?.["inArea"]).toBe("A2");
    expect(filters[4]?.["unsorted"]).toBe(true);
    // Every desk query is on the desk and above the line — except Needs
    // sorting, which takes both sides.
    expect(filters[2]?.["onSurface"]).toBe(true);
    expect(filters[4]?.["onSurface"]).toBeUndefined();
  });

  it("asks the all-mail count for ONE row, never for a limit of zero", async () => {
    // ⚠ THIS SERVER TREATS `limit: 0` AS NO LIMIT (internal/store/jmap_email.go:
    // `if q.Limit > 0` is what appends the LIMIT clause), so a count call
    // written the obvious way would ask it to return the id of every message
    // the user has. One row is the smallest certainly-bounded window; the
    // number wanted is `total`, which Email/query computes either way.
    const h = harness();
    await h.engine.loadDesk("review", null);
    const all = (h.client.requests[0] as Call[])[1] as Call;
    expect(all.name).toBe("Email/query");
    expect(all.args["limit"]).toBe(1);
    expect(all.args["limit"]).not.toBe(0);
    expect(all.args["calculateTotal"]).toBe(true);
  });

  it("asks for a tray page and a list page, and hydrates through a result reference", async () => {
    const h = harness();
    await h.engine.loadDesk("review", null);
    const calls = h.client.requests[0] as Call[];
    const queries = calls.filter((c) => c.name === "Email/query");
    expect(queries[1]?.args["limit"]).toBe(TRAY_PAGE);
    expect(queries[2]?.args["limit"]).toBe(PAGE_SIZE);
    for (const c of calls.filter((x) => x.name === "Email/get")) {
      expect(c.args["#ids"]).toBeTruthy();
    }
  });

  it("lands each section's ids and total under its own key", async () => {
    const h = harness();
    await h.engine.loadDesk("review", null);
    const st = h.store.getState();
    expect(st.sections[sectionKey("tray", null, null)]?.ids).toEqual(["tray-1", "tray-2"]);
    expect(st.sections[sectionKey("surface", "A1", null)]?.ids).toEqual([
      "A1-surface-1",
      "A1-surface-2",
    ]);
    expect(st.sections[sectionKey("surface", "A2", null)]?.ids).toEqual([
      "A2-surface-1",
      "A2-surface-2",
    ]);
    expect(st.sections[sectionKey("surface", null, null)]?.ids).toEqual(["unsorted-1"]);
    // The total is the server's, not the window's — which is what lets the UI
    // say the list it is showing is only part of the section.
    expect(st.sections[sectionKey("surface", "A1", null)]?.total).toBe(102);
    expect(st.sections[sectionKey("surface", "A1", null)]?.loading).toBe(false);
    expect(st.sections[sectionKey("surface", "A1", null)]?.loadedAt).not.toBeNull();
    expect(Object.keys(st.emails)).toContain("A1-surface-1");
  });

  it("stores the counts with the scope and threshold they were taken at", async () => {
    const h = harness();
    await h.engine.loadDesk("review", "ACC7");
    expect(h.store.getState().deskSummary).toEqual({
      scope: "ACC7",
      threshold: 0.4,
      state: "area-state-1",
      rows: SUMMARY_ROWS,
      // Measured in the SAME batch and at the same scope, so the rail's badge
      // and the header sentence describe one moment.
      allMail: 102,
    });
    const summaryCall = (h.client.requests[0] as Call[])[0] as Call;
    expect(summaryCall.args["mailAccountId"]).toBe("ACC7");
    // No threshold: this asks for the STORED line, not a preview.
    expect(summaryCall.args["threshold"]).toBeUndefined();
  });

  it("scopes every section's query — and the all-mail count — to the chosen account", async () => {
    const h = harness();
    await h.engine.loadDesk("review", "ACC7");
    for (const c of (h.client.requests[0] as Call[]).filter((x) => x.name === "Email/query")) {
      expect((c.args["filter"] as Record<string, unknown>)["inAccount"]).toBe("ACC7");
    }
  });

  it("leaves allMail null when that one call is refused, rather than zero", async () => {
    // "All mail is empty" and "we could not count it" are different facts, and
    // only one of them belongs on a badge. The rest of the desk is unaffected:
    // one refused count must not blank a screenful of sections.
    let first = true;
    const h = harness({
      "Email/query": (c) => {
        if (first) {
          first = false;
          return { error: new JmapMethodError(c.id, { type: "unsupportedFilter" }) };
        }
        const ids = idsFor(c);
        return { value: { ids, total: ids.length + 100, queryState: "q1", position: 0 } };
      },
    });
    await h.engine.loadDesk("review", null);
    const st = h.store.getState();
    expect(st.deskSummary?.allMail).toBeNull();
    expect(st.deskSummary?.rows).toEqual(SUMMARY_ROWS);
    expect(st.sections[sectionKey("tray", null, null)]?.ids).toEqual(["tray-1", "tray-2"]);
  });

  it("forgets the other account's sections when the scope changes", async () => {
    const h = harness();
    await h.engine.loadDesk("review", "ACC7");
    expect(Object.keys(h.store.getState().sections).length).toBeGreaterThan(0);
    await h.engine.loadDesk("review", "ACC8");
    for (const key of Object.keys(h.store.getState().sections)) {
      expect(key.startsWith("ACC8/")).toBe(true);
    }
  });

  it("establishes the Email cursor from the first query state", async () => {
    const h = harness();
    await h.engine.loadDesk("review", null);
    expect(h.store.getState().cursor.Email).toBe("q1");
  });

  it("stamps the sync clock", async () => {
    const h = harness();
    expect(h.store.getState().lastSyncAt).toBeNull();
    await h.engine.loadDesk("review", null);
    expect(h.store.getState().lastSyncAt).not.toBeNull();
  });
});

describe("a desk bigger than one request", () => {
  it("splits on query/get pairs rather than being refused whole", async () => {
    // 1 summary + 2 calls per section. With five areas that is 13 calls, which
    // this server would refuse outright at a limit of 7 — and the user would
    // get no desk at all rather than a slow one.
    const areas = [0, 1, 2, 3, 4].map((i) => area(`A${i}`, i));
    const h = harness({}, { maxCalls: 7, areas });
    await h.engine.loadDesk("review", null);

    expect(h.client.requests.length).toBeGreaterThan(1);
    for (const batch of h.client.requests) {
      expect(batch.length).toBeLessThanOrEqual(7);
      // Every Email/get travels with the query it names: a result reference
      // cannot point outside its own batch.
      const ids = new Set(batch.map((c) => c.id));
      for (const c of batch.filter((x) => x.name === "Email/get")) {
        expect(ids.has((c.args["#ids"] as { resultOf: string }).resultOf)).toBe(true);
      }
    }
    // And every section still landed.
    for (const a of areas) {
      expect(h.store.getState().sections[sectionKey("surface", a.id, null)]?.ids).toHaveLength(2);
    }
    expect(h.store.getState().sections[sectionKey("surface", null, null)]?.ids).toEqual([
      "unsorted-1",
    ]);
    expect(h.store.getState().deskSummary?.threshold).toBe(0.4);
  });

  it("NEGATIVE CONTROL: the same desk under a generous limit is one request", async () => {
    const areas = [0, 1, 2, 3, 4].map((i) => area(`A${i}`, i));
    const h = harness({}, { maxCalls: 64, areas });
    await h.engine.loadDesk("review", null);
    expect(h.client.requests).toHaveLength(1);
  });
});

describe("loading Everything else", () => {
  it("is the counts and the tray only — no drawer contents", async () => {
    const h = harness();
    await h.engine.loadDesk("rest", null);
    expect(h.client.requests).toHaveLength(1);
    expect(names(h.client.requests[0] as Call[])).toEqual([
      "Area/getSummary",
      "Email/query", // the all-mail count
      "Email/query",
      "Email/get", // the tray
    ]);
    // A closed drawer draws itself from the summary.
    expect(h.store.getState().sections[sectionKey("drawer", "A1", null)]).toBeUndefined();
    expect(h.store.getState().deskSummary?.rows).toHaveLength(3);
  });

  it("IS the tray page's load (#375): one round trip, tray rows included", async () => {
    // `#/tray` renders from exactly this — the counts, the all-mail total and
    // the tray section — so opening it costs one request however many areas
    // the user has.
    const h = harness();
    await h.engine.loadDesk("rest", "ACC7");
    expect(h.client.requests).toHaveLength(1);
    expect(h.store.getState().sections[sectionKey("tray", null, "ACC7")]?.ids).toEqual([
      "tray-1",
      "tray-2",
    ]);
    expect(h.store.getState().deskSummary?.scope).toBe("ACC7");
  });
});

describe("a section that could not be loaded", () => {
  it("marks only that section, and says so rather than showing nothing", async () => {
    const h = harness({
      "Email/query": (c) => {
        const f = c.args["filter"] as Record<string, unknown>;
        if (f["inArea"] === "A1") {
          return {
            error: new JmapMethodError("q", {
              type: "unsupportedFilter",
              description: "onSurface is not a known filter",
            }),
          };
        }
        const ids = idsFor(c);
        return { value: { ids, total: ids.length + 100, queryState: "q1", position: 0 } };
      },
    });
    await h.engine.loadDesk("review", null);
    const st = h.store.getState();
    const broken = st.sections[sectionKey("surface", "A1", null)];
    // NOT an empty list: against a server without the desk filters, "no mail
    // here" and "I could not ask" look identical, and only one of them is true.
    expect(broken?.error).toContain("onSurface");
    expect(broken?.ids).toEqual([]);
    expect(broken?.loading).toBe(false);
    // Everything else loaded.
    expect(st.sections[sectionKey("surface", "A2", null)]?.error).toBeNull();
    expect(st.sections[sectionKey("surface", "A2", null)]?.ids).toHaveLength(2);
    expect(st.sections[sectionKey("tray", null, null)]?.error).toBeNull();
  });

  it("reports a desk whose counts did not come back, and shows no stale ones", async () => {
    const h = harness({
      "Area/getSummary": () => ({
        error: new JmapMethodError("sum", { type: "unknownMethod" }),
      }),
    });
    await h.engine.loadDesk("review", null);
    expect(h.store.getState().deskSummary).toBeNull();
    expect(h.store.getState().syncError).toBeTruthy();
  });
});

describe("opening a drawer", () => {
  it("needs a desk on screen", async () => {
    const h = harness();
    await h.engine.loadDrawer("A1");
    expect(h.client.requests).toHaveLength(0);
  });

  it("queries below the line and grows by a page", async () => {
    const h = harness();
    await h.engine.loadDesk("rest", null);
    await h.engine.loadDrawer("A1");
    const first = h.client.requests[1] as Call[];
    expect(names(first)).toEqual(["Email/query", "Email/get"]);
    const filter = (first[0] as Call).args["filter"] as Record<string, unknown>;
    expect(filter["inArea"]).toBe("A1");
    expect(filter["onSurface"]).toBe(false);
    expect((first[0] as Call).args["limit"]).toBe(PAGE_SIZE);
    const key = sectionKey("drawer", "A1", null);
    expect(h.store.getState().sections[key]?.ids).toEqual(["A1-drawer-1", "A1-drawer-2"]);

    await h.engine.loadDrawer("A1", { more: true });
    const second = h.client.requests[2] as Call[];
    expect((second[0] as Call).args["limit"]).toBe(PAGE_SIZE * 2);
    expect(h.store.getState().sections[key]?.limit).toBe(PAGE_SIZE * 2);
  });

  it("keeps the grown window when the desk is reloaded", async () => {
    const h = harness();
    await h.engine.loadDesk("rest", null);
    await h.engine.loadDrawer("A1", { more: true });
    await h.engine.refreshDesk();
    // The open drawer was re-read, at the size the user had grown it to.
    const last = h.client.requests[h.client.requests.length - 1] as Call[];
    expect(names(last)).toEqual(["Email/query", "Email/get"]);
    expect((last[0] as Call).args["limit"]).toBe(PAGE_SIZE * 2);
  });

  it("does not re-read a drawer nobody opened", async () => {
    const h = harness();
    await h.engine.loadDesk("rest", null);
    const before = h.client.requests.length;
    await h.engine.refreshDesk();
    // One request for the desk itself, and nothing per closed drawer.
    expect(h.client.requests.length - before).toBe(1);
  });
});

describe("previewing another line", () => {
  it("asks at the given threshold and stores nothing", async () => {
    const h = harness();
    await h.engine.loadDesk("review", null);
    const before = h.store.getState().deskSummary;
    const rows = await h.engine.previewLine(0.9);
    expect(rows).toHaveLength(3);
    const call = (h.client.requests[1] as Call[])[0] as Call;
    expect(call.name).toBe("Area/getSummary");
    expect(call.args["threshold"]).toBe(0.9);
    expect(h.store.getState().deskSummary).toEqual(before);
  });

  it("answers null when the server refuses", async () => {
    const h = harness({
      "Area/getSummary": () => ({ error: new JmapMethodError("sum", { type: "invalidArguments" }) }),
    });
    expect(await h.engine.previewLine(0.9)).toBeNull();
  });
});

describe("the change loop with a desk on screen", () => {
  function changes(over: Record<string, Responder> = {}): ReturnType<typeof harness> {
    const h = harness({
      "Email/changes": () => ({
        value: { created: [], updated: [], destroyed: [], newState: "e1", hasMoreChanges: false },
      }),
      "Mailbox/changes": () => ({
        value: { created: [], updated: [], destroyed: [], newState: "m1", hasMoreChanges: false },
      }),
      "Label/changes": () => ({
        value: { created: [], updated: [], destroyed: [], newState: "l1", hasMoreChanges: false },
      }),
      "Identity/changes": () => ({
        value: { created: [], updated: [], destroyed: [], newState: "i1", hasMoreChanges: false },
      }),
      "EmailSubmission/changes": () => ({
        value: { created: [], updated: [], destroyed: [], newState: "s1", hasMoreChanges: false },
      }),
      "Area/changes": () => ({
        value: { created: [], updated: [], destroyed: [], newState: "a1", hasMoreChanges: false },
      }),
      "DeskPreferences/changes": () => ({
        value: { created: [], updated: [], destroyed: [], newState: "d1", hasMoreChanges: false },
      }),
      "Area/get": () => ({ value: { list: [area("A1", 0)], notFound: [], state: "a2" } }),
      "DeskPreferences/get": () => ({
        value: {
          list: [
            {
              id: ACCOUNT,
              lineThreshold: 0.55,
              drawerDefaultDays: 30,
              trayNagDays: 7,
              drawerSweepEnabled: false,
            },
          ],
          notFound: [],
          state: "d2",
        },
      }),
      "Mailbox/get": () => ({ value: { list: [], notFound: [], state: "m1" } }),
      "Email/get": () => ({ value: { list: [], notFound: [], state: "e1" } }),
      ...over,
    });
    h.store.setCursor({
      ...emptySyncCursor(ACCOUNT),
      Email: "e0",
      Mailbox: "m0",
      Label: "l0",
      Identity: "i0",
      EmailSubmission: "s0",
      Area: "a0",
      DeskPreferences: "d0",
    });
    return h;
  }

  it("polls Area and DeskPreferences alongside the rest", async () => {
    const h = changes();
    await h.engine.poll();
    expect(names(h.client.requests[0] as Call[])).toContain("Area/changes");
    expect(names(h.client.requests[0] as Call[])).toContain("DeskPreferences/changes");
  });

  it("advances the sync clock even when nothing changed", async () => {
    const h = changes();
    h.store.setLastSyncAt(1);
    await h.engine.poll();
    expect(h.store.getState().lastSyncAt).toBeGreaterThan(1);
    // NEGATIVE CONTROL: nothing else ran, so the clock is the poll's doing.
    expect(h.client.requests).toHaveLength(1);
  });

  it("re-runs the DESK on an Email change, not the view", async () => {
    const h = changes({
      "Email/changes": () => ({
        value: { created: [], updated: ["m9"], destroyed: [], newState: "e2", hasMoreChanges: false },
      }),
    });
    await h.engine.loadDesk("review", null);
    const before = h.client.requests.length;
    await h.engine.poll();
    const after = h.client.requests.slice(before).map(names).flat();
    expect(after).toContain("Area/getSummary");
    // loadView would have written the single list; it must not have run.
    expect(h.store.getState().list.key).toBe("");
  });

  it("re-runs the view, not the desk, when no desk is open", async () => {
    const h = changes({
      "Email/changes": () => ({
        value: { created: [], updated: ["m9"], destroyed: [], newState: "e2", hasMoreChanges: false },
      }),
    });
    await h.engine.poll();
    const issued = h.client.requests.map(names).flat();
    expect(issued).not.toContain("Area/getSummary");
    expect(h.store.getState().list.key).toBe("role:inbox");
  });

  it("re-reads the areas — and the desk — when an area moves", async () => {
    const h = changes({
      "Area/changes": () => ({
        value: { created: [], updated: ["A1"], destroyed: ["A2"], newState: "a2", hasMoreChanges: false },
      }),
    });
    await h.engine.loadDesk("review", null);
    const before = h.client.requests.length;
    await h.engine.poll();
    const after = h.client.requests.slice(before).map(names).flat();
    expect(after).toContain("Area/get");
    expect(after).toContain("Area/getSummary");
    // The destroyed area is gone and the re-read list replaced the rest.
    expect(Object.keys(h.store.getState().areas)).toEqual(["A1"]);
    expect(h.store.getState().cursor.Area).toBe("a2");
  });

  it("re-splits the desk when the LINE moves on another device — and only the desk", async () => {
    // The line decides which side of itself every message is on, so a poll that
    // only learned the threshold moved has to re-ask every section. Updating
    // the stored value alone would leave the desk drawn at the old line while
    // claiming the new one.
    const lineMoved: Record<string, Responder> = {
      "DeskPreferences/changes": () => ({
        value: { created: [], updated: [ACCOUNT], destroyed: [], newState: "d2", hasMoreChanges: false },
      }),
    };

    const withDesk = changes(lineMoved);
    await withDesk.engine.loadDesk("review", null);
    const before = withDesk.client.requests.length;
    await withDesk.engine.poll();
    const after = withDesk.client.requests.slice(before).map(names).flat();
    expect(after).toContain("Area/getSummary");
    expect(withDesk.store.getState().deskPreferences?.lineThreshold).toBe(0.55);
    // loadView writes the single list; it must not have run.
    expect(withDesk.store.getState().list.key).toBe("");

    // With no desk on screen there is nothing to re-split: an ordinary view has
    // no idea the line exists, so this must not re-run it either.
    const noDesk = changes(lineMoved);
    await noDesk.engine.poll();
    expect(noDesk.client.requests.map(names).flat()).not.toContain("Area/getSummary");
    expect(noDesk.store.getState().list.key).toBe("");
    expect(noDesk.store.getState().deskPreferences?.lineThreshold).toBe(0.55);
  });

  it("re-reads the desk preferences when the line moves elsewhere", async () => {
    const h = changes({
      "DeskPreferences/changes": () => ({
        value: { created: [], updated: [ACCOUNT], destroyed: [], newState: "d2", hasMoreChanges: false },
      }),
    });
    await h.engine.poll();
    expect(h.store.getState().deskPreferences?.lineThreshold).toBe(0.55);
    expect(h.store.getState().cursor.DeskPreferences).toBe("d2");
  });
});

describe("the reference-data load", () => {
  it("brings the areas and the line back with the mailboxes", async () => {
    const h = harness({
      "Mailbox/get": () => ({ value: { list: [], notFound: [], state: "m1" } }),
      "Label/get": () => ({ value: { list: [], notFound: [], state: "l1" } }),
      "Identity/get": () => ({ value: { list: [], notFound: [], state: "i1" } }),
      "RemoteAllow/get": () => ({ value: { list: [], notFound: [], state: "r1" } }),
      "ComposeSnippet/get": () => ({ value: { list: [], notFound: [], state: "c1" } }),
      "SavedSearch/get": () => ({ value: { list: [], notFound: [], state: "ss1" } }),
      "CategoryTab/get": () => ({ value: { list: [], notFound: [], state: "ct1" } }),
      "Area/get": () => ({ value: { list: [area("A9", 3)], notFound: [], state: "a1" } }),
      "ClientPreferences/get": () => ({
        value: {
          list: [
            {
              id: "CP-WEB",
              family: "web",
              theme: "dark",
              density: "relaxed",
              messageOpens: "focused",
              paneSplitPct: 35,
              textSize: "l",
              rowLayout: "multiLine",
            },
          ],
          notFound: [],
          state: "cp1",
        },
      }),
      "DeskPreferences/get": () => ({
        value: {
          list: [
            {
              id: ACCOUNT,
              lineThreshold: 0.42,
              drawerDefaultDays: 30,
              trayNagDays: 7,
              drawerSweepEnabled: false,
            },
          ],
          notFound: [],
          state: "d1",
        },
      }),
    });
    await h.engine.loadReferenceData();
    expect(h.client.requests).toHaveLength(1);
    const st = h.store.getState();
    // A full replace: the seeded A1/A2 are gone, not merged with A9.
    expect(Object.keys(st.areas)).toEqual(["A9"]);
    expect(st.cursor.Area).toBe("a1");
    expect(st.deskPreferences?.lineThreshold).toBe(0.42);
    expect(st.cursor.DeskPreferences).toBe("d1");
  });

  it("brings this client family's appearance back in the same batch", async () => {
    // The shell applies theme/density/messageOpens/paneSplitPct before the
    // first list is drawn (#375 increment 5), so a separate fetch would mean
    // booting in one appearance and changing to another in front of the user.
    const h = harness({
      "Mailbox/get": () => ({ value: { list: [], notFound: [], state: "m1" } }),
      "Label/get": () => ({ value: { list: [], notFound: [], state: "l1" } }),
      "Identity/get": () => ({ value: { list: [], notFound: [], state: "i1" } }),
      "RemoteAllow/get": () => ({ value: { list: [], notFound: [], state: "r1" } }),
      "ComposeSnippet/get": () => ({ value: { list: [], notFound: [], state: "c1" } }),
      "SavedSearch/get": () => ({ value: { list: [], notFound: [], state: "ss1" } }),
      "CategoryTab/get": () => ({ value: { list: [], notFound: [], state: "ct1" } }),
      "Area/get": () => ({ value: { list: [], notFound: [], state: "a1" } }),
      "DeskPreferences/get": () => ({ value: { list: [], notFound: [], state: "d1" } }),
      "ClientPreferences/get": () => ({
        value: {
          list: [
            {
              id: "CP-WEB",
              family: "web",
              theme: "dark",
              density: "relaxed",
              messageOpens: "focused",
              paneSplitPct: 35,
              textSize: "l",
              rowLayout: "multiLine",
            },
          ],
          notFound: [],
          state: "cp1",
        },
      }),
    });
    await h.engine.loadReferenceData();
    const names = (h.client.requests[0] as Call[]).map((c) => c.name);
    expect(names).toContain("ClientPreferences/get");
    const st = h.store.getState();
    expect(st.clientPreferences["web"]?.theme).toBe("dark");
    // #514: the two reading-comfort fields ride the SAME record. Asserted
    // because the store takes the list as a blind cast — a field the server
    // stopped sending would be `undefined` forever and read perfectly (#155).
    expect(st.clientPreferences["web"]?.textSize).toBe("l");
    expect(st.clientPreferences["web"]?.rowLayout).toBe("multiLine");
    // The cursor IS established here: ClientPreferences is polled like
    // everything else, and without a cursor doPoll skips it silently.
    expect(st.cursor.ClientPreferences).toBe("cp1");
  });
});

describe("rows are threads (#375 increment 5)", () => {
  it("collapses every SECTION query, and never the tray", async () => {
    // RFC 8621 4.4.4. A conversation is one row on the desk; the TRAY is not,
    // because a tray item is the `$flagged` keyword and that is per MESSAGE
    // (#444) — collapsing would hide a flagged reply behind a newer sibling
    // and age the wrong one.
    const h = harness();
    await h.engine.loadDesk("review", null);
    const queries = (h.client.requests[0] as Call[]).filter((c) => c.name === "Email/query");
    // [0] the all-mail COUNT, [1] the tray, [2..] the sections.
    const tray = queries[1] as Call;
    expect((tray.args["filter"] as Record<string, unknown>)["hasKeyword"]).toBe("$flagged");
    expect(tray.args["collapseThreads"]).toBeUndefined();
    const sections = queries.slice(2);
    expect(sections.length).toBeGreaterThan(0);
    for (const c of sections) expect(c.args["collapseThreads"]).toBe(true);
  });

  it("collapses a drawer too", async () => {
    const h = harness();
    await h.engine.loadDesk("rest", null);
    await h.engine.loadDrawer("A1");
    const q = (h.client.requests[1] as Call[])[0] as Call;
    expect(q.name).toBe("Email/query");
    expect(q.args["collapseThreads"]).toBe(true);
  });

  it("leaves the ALL-MAIL count uncollapsed — it counts what #/all lists", async () => {
    // NEGATIVE CONTROL, and a deliberate exception: the rail badge sits above a
    // list (`#/all`) that goes through loadView and is NOT collapsed, so a
    // collapsed count there would print a number the list it links to
    // contradicts.
    const h = harness();
    await h.engine.loadDesk("review", null);
    const all = (h.client.requests[0] as Call[])[1] as Call;
    expect(all.args["limit"]).toBe(1);
    expect(all.args["collapseThreads"]).toBeUndefined();
  });
});

describe("the push subscription", () => {
  it("subscribes to every type this server pushes, ClientPreferences included", async () => {
    // #474 added store.ObjectClientPreferences to internal/api/eventsource.go's
    // eventSourceTypes, so the type this client had to leave out is pushable
    // now and appearance changed on another device arrives at once rather than
    // on the fallback poll.
    //
    // The rest of the assertion is the one that has to keep holding whatever
    // else changes: parseEventTypes answers an UNKNOWN name with a
    // request-level 400, refused WHOLE — so a type named here that the server
    // does not push costs the entire EventSource connection, not one type's
    // freshness. Every name in PUSH_TYPES must be a name this client tracks and
    // that server accepts (session.ts PUSH_TYPES).
    expect(CHANGE_TYPES).toContain("ClientPreferences");
    expect(PUSH_TYPES).toContain("ClientPreferences");
    for (const t of PUSH_TYPES) expect(CHANGE_TYPES).toContain(t);
    expect([...PUSH_TYPES]).toEqual([...CHANGE_TYPES]);
  });

  it("polls ClientPreferences once a cursor exists", async () => {
    const h = harness({
      "Email/changes": () => ({
        value: { created: [], updated: [], destroyed: [], newState: "e1", hasMoreChanges: false },
      }),
      "ClientPreferences/changes": () => ({
        value: { created: [], updated: [], destroyed: [], newState: "cp0", hasMoreChanges: false },
      }),
    });
    h.store.setCursor({ ...emptySyncCursor(ACCOUNT), Email: "e0", ClientPreferences: "cp0" });
    await h.engine.poll();
    expect((h.client.requests[0] as Call[]).map((c) => c.name)).toContain(
      "ClientPreferences/changes",
    );
  });

  it("re-reads the appearance when it moves, and does NOT re-run the desk for it", async () => {
    // Appearance changes nothing about which mail is where. Re-running the desk
    // for it would be a screenful of queries to repaint a colour.
    const h = harness({
      "Email/changes": () => ({
        value: { created: [], updated: [], destroyed: [], newState: "e1", hasMoreChanges: false },
      }),
      "ClientPreferences/changes": () => ({
        value: { created: [], updated: ["CP-WEB"], destroyed: [], newState: "cp2", hasMoreChanges: false },
      }),
      "ClientPreferences/get": () => ({
        value: {
          list: [
            {
              id: "CP-WEB",
              family: "web",
              theme: "light",
              density: "compact",
              messageOpens: "beside",
              paneSplitPct: 44,
              textSize: "l",
              rowLayout: "multiLine",
            },
          ],
          notFound: [],
          state: "cp2",
        },
      }),
    });
    await h.engine.loadDesk("review", null);
    h.store.setCursor({ ...h.store.getState().cursor, Email: "e0", ClientPreferences: "cp0" });
    const before = h.client.requests.length;
    await h.engine.poll();
    const after = h.client.requests.slice(before).map((b) => b.map((c) => c.name)).flat();
    expect(after).toContain("ClientPreferences/get");
    expect(after).not.toContain("Area/getSummary");
    expect(h.store.getState().clientPreferences["web"]?.paneSplitPct).toBe(44);
    expect(h.store.getState().clientPreferences["web"]?.textSize).toBe("l");
    expect(h.store.getState().cursor.ClientPreferences).toBe("cp2");
  });
});
