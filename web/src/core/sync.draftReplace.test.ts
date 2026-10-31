// SPDX-License-Identifier: AGPL-3.0-only

// A draft being typed into, seen from the list (#610 M1 and M4).
//
// Every autosave is a create AND a destroy in one Email/set, so the change poll
// receives both in one Email/changes response. The measured defect: the destroy
// was applied at once, while the created id only reached the list after an
// Email/get AND a query — so the draft's row was absent for two round trips on
// every save, and a "Loading…" line appeared under the list each time.
//
// There is no DOM here, and there does not need to be: what the list draws is
// `list.ids`, so the test records EVERY state the store passes through during a
// poll and asserts on the worst one. A final-state assertion alone would pass
// against the bug — the row always did come back.

import { describe, expect, it } from "vitest";

import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { Email, Mailbox } from "./jmap/types";
import { emptySyncCursor } from "./session";
import { MailStore } from "./store";
import { SyncEngine } from "./sync";

const ACCOUNT = "U1";
const DRAFTS: Mailbox = {
  id: "MB-D",
  accountId: ACCOUNT,
  name: "Drafts",
  parentId: null,
  role: "drafts",
  sortOrder: 1,
  totalEmails: 0,
  unreadEmails: 0,
  totalThreads: 0,
  unreadThreads: 0,
  isSubscribed: true,
} as Mailbox;
const INBOX: Mailbox = { ...DRAFTS, id: "MB-I", name: "Inbox", role: "inbox" } as Mailbox;

function email(id: string, receivedAt: string): Email {
  return {
    id,
    blobId: "b",
    threadId: `t-${id}`,
    mailboxIds: { [DRAFTS.id]: true },
    keywords: { $draft: true },
    size: 1,
    receivedAt,
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

/**
 * The server's side, as a mutable mailbox: the query answers from `rows`, the
 * change poll from `pending`. `save(old, next)` is what an autosave does to it —
 * one create and one destroy, landed together, exactly as the change_log
 * measured on the instance (same transaction, same millisecond).
 */
class FakeServer {
  rows: Email[];
  pending: { created: string[]; destroyed: string[] } = { created: [], destroyed: [] };
  failQuery = false;
  readonly requests: Call[][] = [];

  constructor(rows: Email[]) {
    this.rows = rows;
  }

  save(old: string, next: Email): void {
    this.rows = [next, ...this.rows.filter((e) => e.id !== old)];
    this.pending = { created: [next.id], destroyed: [old] };
  }

  get accountId(): string {
    return ACCOUNT;
  }

  async request(calls: Call[]): Promise<CallResult[]> {
    this.requests.push(calls);
    const byId = new Map<string, CallResult>();
    const out: CallResult[] = [];
    for (const c of calls) {
      let value: unknown = {};
      if (c.name === "Email/changes") {
        value = { ...this.pending, updated: [], newState: "e2", hasMoreChanges: false };
        this.pending = { created: [], destroyed: [] };
      } else if (c.name === "Email/query") {
        if (this.failQuery) throw new Error("the network went away");
        const ids = [...this.rows]
          .sort((a, b) => b.receivedAt.localeCompare(a.receivedAt))
          .map((e) => e.id);
        value = { ids, total: ids.length, queryState: "q2", position: 0 };
      } else if (c.name === "Email/get") {
        const ref = c.args["#ids"] as { resultOf: string } | undefined;
        const want = ref
          ? (((byId.get(ref.resultOf)?.value as { ids?: string[] } | null)?.ids ?? []) as string[])
          : ((c.args["ids"] as string[] | undefined) ?? []);
        const list = this.rows.filter((e) => want.includes(e.id));
        value = { list, notFound: want.filter((id) => !list.some((e) => e.id === id)), state: "e2" };
      } else if (c.name === "Mailbox/get") {
        value = { list: [DRAFTS, INBOX], notFound: [], state: "m1" };
      }
      const result: CallResult = { id: c.id, name: c.name, value: value as CallResult["value"], error: null };
      byId.set(c.id, result);
      out.push(result);
    }
    return out;
  }

  pushUrl(): string {
    return "/jmap/eventsource/";
  }
}

async function onDrafts(rows: Email[]): Promise<{
  store: MailStore;
  server: FakeServer;
  engine: SyncEngine;
  /** every `list` the store held, in order, from the moment this is called */
  seen: { ids: string[]; loading: boolean }[];
}> {
  const store = new MailStore(null);
  store.setSession(
    { capabilities: { "urn:ietf:params:jmap:core": { maxCallsInRequest: 64 } } } as never,
    ACCOUNT,
  );
  store.setConn("online");
  store.putMailboxes([DRAFTS, INBOX], true);
  const server = new FakeServer(rows);
  const engine = new SyncEngine(server as unknown as JmapClient, store, {
    push: null,
    setTimer: () => 0,
    clearTimer: () => {},
  });
  await engine.loadView({ kind: "role", role: "drafts" });
  store.setCursor({ ...emptySyncCursor(ACCOUNT), Email: "e1" });
  const seen: { ids: string[]; loading: boolean }[] = [];
  store.subscribe(() => {
    const l = store.getState().list;
    seen.push({ ids: [...l.ids], loading: l.loading });
  });
  return { store, server, engine, seen };
}

const T0 = "2026-09-18T00:40:00Z";
const T1 = "2026-09-18T00:57:14Z";
const T2 = "2026-09-18T00:57:30Z";

describe("an autosave, seen from the list (#610)", () => {
  it("M1: the draft's row is never absent — every state holds the old id or the new one", async () => {
    const h = await onDrafts([email("other", T0), email("draft-1", T1)]);
    expect(h.store.getState().list.ids).toEqual(["draft-1", "other"]);

    h.server.save("draft-1", email("draft-2", T2));
    await h.engine.poll();

    expect(h.seen.length).toBeGreaterThan(0);
    const holes = h.seen.filter((s) => !s.ids.includes("draft-1") && !s.ids.includes("draft-2"));
    expect(holes).toEqual([]);
    // And it did land: the old revision is gone and the new one is in its place.
    expect(h.store.getState().list.ids).toEqual(["draft-2", "other"]);
    expect(h.store.getState().emails["draft-1"]).toBeUndefined();
  });

  it("NEGATIVE CONTROL: a poll with no destroy produces no such state either", async () => {
    // Proves the recorder can see the list at all: this poll moves nothing out.
    const h = await onDrafts([email("other", T0), email("draft-1", T1)]);
    h.server.rows = [email("new-mail", T2), ...h.server.rows];
    h.server.pending = { created: ["new-mail"], destroyed: [] };
    await h.engine.poll();
    expect(h.seen.length).toBeGreaterThan(0);
    expect(h.seen.every((s) => s.ids.includes("draft-1"))).toBe(true);
    expect(h.store.getState().list.ids).toEqual(["new-mail", "draft-1", "other"]);
  });

  it("a message destroyed ELSEWHERE still leaves — once the list has been re-run", async () => {
    // The price of M1 is that a destroy with no replacement lingers for the
    // refresh; it must still go, from the list and from the store.
    const h = await onDrafts([email("other", T0), email("doomed", T1)]);
    h.server.rows = h.server.rows.filter((e) => e.id !== "doomed");
    h.server.pending = { created: [], destroyed: ["doomed"] };
    await h.engine.poll();
    expect(h.store.getState().list.ids).toEqual(["other"]);
    expect(h.store.getState().emails["doomed"]).toBeUndefined();
  });

  it("a destroyed message still leaves the STORE when the re-run fails", async () => {
    // The removal is in a `finally`: without it, a failed refresh would keep a
    // message the server has already destroyed, indefinitely.
    const h = await onDrafts([email("other", T0), email("doomed", T1)]);
    h.server.rows = h.server.rows.filter((e) => e.id !== "doomed");
    h.server.pending = { created: [], destroyed: ["doomed"] };
    h.server.failQuery = true;
    await h.engine.poll();
    expect(h.store.getState().emails["doomed"]).toBeUndefined();
    expect(h.store.getState().list.ids).not.toContain("doomed");
  });

  it("M4: the refresh a save causes never raises `loading` under a list that is on screen", async () => {
    const h = await onDrafts([email("other", T0), email("draft-1", T1)]);
    h.server.save("draft-1", email("draft-2", T2));
    await h.engine.poll();
    expect(h.seen.some((s) => s.loading)).toBe(false);
  });

  it("NEGATIVE CONTROL: opening a view still says it is loading", async () => {
    // Quiet is for a REFRESH of what is already there. A fresh view has nothing
    // on screen yet, and "Loading…" is the honest thing to draw.
    const h = await onDrafts([email("draft-1", T1)]);
    await h.engine.loadView({ kind: "role", role: "inbox" });
    expect(h.seen.some((s) => s.loading)).toBe(true);
  });
});
