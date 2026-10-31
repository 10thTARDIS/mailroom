// SPDX-License-Identifier: AGPL-3.0-only

// THE OUTBOX SURVIVES A RELOAD (#653).
//
// The bootstrap fetched mailboxes, labels, identities, areas and the rest, and
// never a single EmailSubmission. Submissions reached the store only from this
// tab's own sends and from `/changes` — which needs a cursor only a send in
// this tab ever set. So after a reload the Outbox and the rail's count were
// empty: on a real instance three failed, undismissed sends were in the
// database and nowhere on screen.
//
// The fake server below answers the way internal/api does: EmailSubmission/query
// returns a capped page with the real `total`, and EmailSubmission/get resolves
// its `#ids` back-reference against the query answered earlier in the batch.

import { describe, expect, it } from "vitest";

import type { Call, CallResult, JmapClient } from "./jmap/client";
import { JmapMethodError } from "./jmap/client";
import type { EmailSubmission } from "./jmap/types";
import { MailStore } from "./store";
import { SyncEngine } from "./sync";
import { outboxCount } from "../app/ui/outbox";

const ACCOUNT = "U1";
const SESSION = { capabilities: { "urn:ietf:params:jmap:core": { maxCallsInRequest: 64 } } };

function failed(id: string): EmailSubmission {
  return {
    id,
    identityId: "I1",
    emailId: `e-${id}`,
    threadId: null,
    sendAt: "2026-09-20T00:00:00Z",
    undoStatus: "final",
    mailroomState: "failed",
    dismissedAt: null,
  };
}

class OutboxServer {
  /** Every batch, in order. */
  readonly batches: Call[][] = [];
  /** Every submission id an EmailSubmission/get returned, across all batches. */
  readonly served: string[] = [];

  /**
   * @param outbox what `inOutbox` selects, newest first
   * @param pageCap the server's maximum page (it reports `limit` when it applies it)
   * @param opts.unknown answer EmailSubmission/query as a server without it would
   * @param opts.shrink report a total larger than the rows it will ever return
   */
  constructor(
    private readonly outbox: EmailSubmission[],
    private readonly pageCap = 500,
    private readonly opts: { unknown?: boolean; shrink?: number; malformed?: boolean } = {},
  ) {}

  get accountId(): string {
    return ACCOUNT;
  }

  async connect() {
    return SESSION;
  }

  async request(calls: Call[]): Promise<CallResult[]> {
    this.batches.push(calls);
    const answered = new Map<string, unknown>();
    return calls.map((c) => {
      let value: unknown = { list: [], notFound: [], state: `${c.name}-state` };
      if (c.name === "EmailSubmission/query") {
        if (this.opts.unknown) {
          return {
            id: c.id,
            name: c.name,
            value: null,
            error: new JmapMethodError(c.id, { type: "unknownMethod" }),
          };
        }
        if (this.opts.malformed) {
          // Well-formed JSON, not a query answer: no ids, no total — what a fake
          // that answers every unfamiliar method with an empty list sends.
          value = { list: [], notFound: [], state: "s0" };
          answered.set(c.id, value);
          return { id: c.id, name: c.name, value: value as CallResult["value"], error: null };
        }
        const position = (c.args["position"] as number | undefined) ?? 0;
        const ids = this.outbox.slice(position, position + this.pageCap).map((s) => s.id);
        value = {
          accountId: ACCOUNT,
          queryState: "sub-7",
          canCalculateChanges: false,
          position,
          total: this.outbox.length + (this.opts.shrink ?? 0),
          ids,
          limit: this.pageCap,
        };
      } else if (c.name === "EmailSubmission/get") {
        if (this.opts.malformed) {
          // And the get answers as those fakes did: an empty, successful list.
          answered.set(c.id, value);
          return { id: c.id, name: c.name, value: value as CallResult["value"], error: null };
        }
        const ref = c.args["#ids"] as { resultOf: string } | undefined;
        const q = ref ? (answered.get(ref.resultOf) as { ids?: string[] } | undefined) : undefined;
        const ids = q?.ids;
        if (!Array.isArray(ids)) {
          // The real server: a path that does not resolve is invalidResultReference.
          return {
            id: c.id,
            name: c.name,
            value: null,
            error: new JmapMethodError(c.id, { type: "invalidResultReference" }),
          };
        }
        const list = this.outbox.filter((s) => ids.includes(s.id));
        this.served.push(...list.map((s) => s.id));
        value = { list, notFound: [], state: "sub-8" };
      }
      answered.set(c.id, value);
      return { id: c.id, name: c.name, value: value as CallResult["value"], error: null };
    });
  }

  /** Every EmailSubmission/query this server was asked, as its position. */
  queryPositions(): number[] {
    return this.batches
      .flat()
      .filter((c) => c.name === "EmailSubmission/query")
      .map((c) => (c.args["position"] as number | undefined) ?? 0);
  }
}

function engineFor(server: OutboxServer): { store: MailStore; engine: SyncEngine } {
  const store = new MailStore(null);
  const engine = new SyncEngine(server as unknown as JmapClient, store, {
    push: null,
    setTimer: () => 0,
    clearTimer: () => {},
  });
  return { store, engine };
}

describe("the Outbox after a reload (#653)", () => {
  it("is loaded by the bootstrap: a reload onto the Outbox shows its failed sends", async () => {
    const rows = [failed("S3"), failed("S2"), failed("S1")];
    const server = new OutboxServer(rows);
    const { store, engine } = engineFor(server);

    await engine.start({ kind: "outbox" });

    const st = store.getState();
    expect(Object.keys(st.submissions).sort()).toEqual(["S1", "S2", "S3"]);
    // What the rail badge and the Outbox list are computed from.
    expect(outboxCount(st.submissions)).toBe(3);
  });

  it("asks for exactly the Outbox, and hydrates it through the query's ids", async () => {
    const server = new OutboxServer([failed("S1")]);
    const { engine } = engineFor(server);

    await engine.start({ kind: "outbox" });

    const first = server.batches[0] ?? [];
    const query = first.find((c) => c.name === "EmailSubmission/query");
    const get = first.find((c) => c.name === "EmailSubmission/get");
    expect(query?.args["filter"]).toEqual({ inOutbox: true });
    expect(query?.args["calculateTotal"]).toBe(true);
    expect(get?.args["#ids"]).toEqual({ resultOf: query?.id, name: "EmailSubmission/query", path: "/ids" });
    // NOT `ids: null`, which the server caps at the 100 most recent, silently.
    expect(first.some((c) => c.name === "EmailSubmission/get" && c.args["ids"] === null)).toBe(false);
  });

  it("establishes the EmailSubmission cursor, so the change loop follows the Outbox", async () => {
    const server = new OutboxServer([failed("S1")]);
    const { store, engine } = engineFor(server);

    await engine.start({ kind: "outbox" });

    // The QUERY's state: a submission made between the query and the get is
    // then replayed by /changes rather than stepped over.
    expect(store.getState().cursor.EmailSubmission).toBe("sub-7");
  });

  it("does not move a cursor this tab already holds", async () => {
    const server = new OutboxServer([failed("S1")]);
    const { store, engine } = engineFor(server);
    store.setSession(SESSION as never, ACCOUNT);
    store.setCursor({ ...store.getState().cursor, EmailSubmission: "sub-9" });

    await engine.loadReferenceData();

    expect(store.getState().cursor.EmailSubmission).toBe("sub-9");
    expect(Object.keys(store.getState().submissions)).toEqual(["S1"]);
  });

  it("pages past the server's cap, fetching every row exactly once", async () => {
    const rows = ["S5", "S4", "S3", "S2", "S1"].map(failed);
    const server = new OutboxServer(rows, 2);
    const { store, engine } = engineFor(server);

    await engine.start({ kind: "outbox" });

    expect(server.queryPositions()).toEqual([0, 2, 4]);
    expect([...server.served].sort()).toEqual(["S1", "S2", "S3", "S4", "S5"]);
    expect(outboxCount(store.getState().submissions)).toBe(5);
  });

  it("stops paging when a page comes back empty, rather than asking forever", async () => {
    // The total says 5 more than there are: the result shrank while being read.
    const server = new OutboxServer(["S2", "S1"].map(failed), 1, { shrink: 5 });
    const { store, engine } = engineFor(server);

    await engine.start({ kind: "outbox" });

    expect(server.queryPositions()).toEqual([0, 1, 2]);
    expect(outboxCount(store.getState().submissions)).toBe(2);
  });

  it("on a server without EmailSubmission/query, loads everything else and sets no cursor", async () => {
    const server = new OutboxServer([failed("S1")], 500, { unknown: true });
    const { store, engine } = engineFor(server);

    await engine.start({ kind: "outbox" });

    const st = store.getState();
    expect(st.submissions).toEqual({});
    // A cursor with nothing behind it would make the empty Outbox permanent:
    // the change loop would only ever report what moved AFTER it.
    expect(st.cursor.EmailSubmission).toBeFalsy();
    expect(st.cursor.Mailbox).toBe("Mailbox/get-state");
    expect(st.conn).toBe("online");
  });
});

// The first version of the bootstrap threw on a query answer with no `ids`, and
// the throw took the WHOLE reference-data batch with it — mailboxes, labels,
// the desk — because it is one try block. Twenty-four unrelated tests whose
// fakes answer every unknown method with an empty list caught it.
describe("a malformed Outbox answer", () => {
  it("costs the Outbox only, never the rest of the reference data", async () => {
    const server = new OutboxServer([failed("S1")], 500, { malformed: true });
    const { store, engine } = engineFor(server);

    await engine.start({ kind: "outbox" });

    const st = store.getState();
    expect(st.cursor.Mailbox).toBe("Mailbox/get-state");
    expect(st.cursor.ClientPreferences).toBe("ClientPreferences/get-state");
    expect(st.cursor.EmailSubmission).toBeFalsy();
    expect(st.conn).toBe("online");
  });
});
