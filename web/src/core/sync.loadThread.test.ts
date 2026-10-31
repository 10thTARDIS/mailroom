// SPDX-License-Identifier: AGPL-3.0-only

// A MESSAGE OPENED FROM TRASH GETS ITS BODY (#630).
//
// Thread/get leaves trashed messages out of a conversation (internal/store
// GetThreads), and the pane's Email/get asks only for the ids Thread/get
// returned. So a trashed message opened from Trash, or from an `is:trashed`
// search, was never fetched with its body. The fake server below answers the
// way the real one does: the conversation omits the trashed message, and
// Email/get by id still returns it.

import { describe, expect, it } from "vitest";

import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { Email } from "./jmap/types";
import { MailStore } from "./store";
import { SyncEngine } from "./sync";

const ACCOUNT = "U1";

function email(id: string): Email {
  return {
    id,
    blobId: "b",
    threadId: "T",
    mailboxIds: { MB: true },
    keywords: {},
    size: 1,
    receivedAt: "2026-09-19T00:00:00Z",
    subject: id,
    preview: "",
    bodyValues: { "1": { value: `the body of ${id}` } },
  } as unknown as Email;
}

class ThreadServer {
  /** Every set of ids an Email/get asked for, in order. */
  readonly emailGets: string[][] = [];

  /** @param members what Thread/get reports: the conversation WITHOUT its trashed messages. */
  constructor(
    private readonly members: string[],
    private readonly rows: Email[],
  ) {}

  get accountId(): string {
    return ACCOUNT;
  }

  private get(ids: string[]) {
    this.emailGets.push([...ids]);
    const list = this.rows.filter((e) => ids.includes(e.id));
    return { list, notFound: ids.filter((id) => !list.some((e) => e.id === id)), state: "e1" };
  }

  async request(calls: Call[]): Promise<CallResult[]> {
    return calls.map((c) => {
      let value: unknown = {};
      if (c.name === "Thread/get") {
        // A conversation whose every message is trashed is not reported at all.
        value =
          this.members.length > 0
            ? { list: [{ id: "T", emailIds: this.members }], notFound: [], state: "t1" }
            : { list: [], notFound: ["T"], state: "t1" };
      } else if (c.name === "Email/get") {
        value = this.get(this.members); // the #ids back-reference to Thread/get
      }
      return { id: c.id, name: c.name, value: value as CallResult["value"], error: null };
    });
  }

  async emailGet(args: { ids: string[] }) {
    return this.get(args.ids);
  }
}

function engineFor(server: ThreadServer): { store: MailStore; engine: SyncEngine } {
  const store = new MailStore(null);
  store.setSession(
    { capabilities: { "urn:ietf:params:jmap:core": { maxCallsInRequest: 64 } } } as never,
    ACCOUNT,
  );
  store.setConn("online");
  const engine = new SyncEngine(server as unknown as JmapClient, store, {
    push: null,
    setTimer: () => 0,
    clearTimer: () => {},
  });
  return { store, engine };
}

function bodyOf(store: MailStore, id: string): string | undefined {
  return (store.getState().emails[id] as Email | undefined)?.bodyValues?.["1"]?.value;
}

describe("loadThread with the opened message (#630)", () => {
  it("fetches a trashed message the conversation left out, with its body", async () => {
    const server = new ThreadServer(["live"], [email("live"), email("trashed")]);
    const { store, engine } = engineFor(server);

    await engine.loadThread("T", "trashed");

    expect(bodyOf(store, "trashed")).toBe("the body of trashed");
    expect(bodyOf(store, "live")).toBe("the body of live");
    expect(server.emailGets).toEqual([["live"], ["trashed"]]);
  });

  it("fetches it when every message in the conversation is trashed", async () => {
    const server = new ThreadServer([], [email("trashed")]);
    const { store, engine } = engineFor(server);

    await engine.loadThread("T", "trashed");

    expect(bodyOf(store, "trashed")).toBe("the body of trashed");
  });

  it("asks nothing more when the opened message is in the conversation", async () => {
    const server = new ThreadServer(["a", "b"], [email("a"), email("b")]);
    const { engine } = engineFor(server);

    await engine.loadThread("T", "b");

    expect(server.emailGets).toEqual([["a", "b"]]);
  });

  it("asks nothing more when no opened message is named", async () => {
    const server = new ThreadServer(["live"], [email("live"), email("trashed")]);
    const { store, engine } = engineFor(server);

    await engine.loadThread("T");

    expect(server.emailGets).toEqual([["live"]]);
    expect(bodyOf(store, "trashed")).toBeUndefined();
  });
});

// ONE DECRYPT PER OPEN (#47). Opening a message mounts the list route and the
// reading pane, and both used to ask for the thread's body: two identical
// Email/get calls, which for an encrypted message are two decryptions and two
// `body` audit rows a few milliseconds apart.
describe("opening a message asks for its body once (#47)", () => {
  it("two open-initiated loads of one thread issue ONE Email/get", async () => {
    const server = new ThreadServer(["a", "b"], [email("a"), email("b")]);
    const { store, engine } = engineFor(server);

    // The route's effect and the pane's effect, in the same tick.
    await Promise.all([
      engine.loadThread("T", undefined, { open: true }),
      engine.loadThread("T", "b", { open: true }),
    ]);

    expect(server.emailGets).toEqual([["a", "b"]]);
    expect(bodyOf(store, "b")).toBe("the body of b");
  });

  it("the second caller's opened message is still fetched when the shared batch left it out", async () => {
    const server = new ThreadServer(["live"], [email("live"), email("trashed")]);
    const { store, engine } = engineFor(server);

    await Promise.all([
      engine.loadThread("T", undefined, { open: true }),
      engine.loadThread("T", "trashed", { open: true }),
    ]);

    expect(server.emailGets).toEqual([["live"], ["trashed"]]);
    expect(bodyOf(store, "trashed")).toBe("the body of trashed");
  });

  it("shares nothing once the request has settled: the next open asks again", async () => {
    const server = new ThreadServer(["a"], [email("a")]);
    const { engine } = engineFor(server);

    await engine.loadThread("T", "a", { open: true });
    await engine.loadThread("T", "a", { open: true });

    expect(server.emailGets).toEqual([["a"], ["a"]]);
  });

  it("a reload after a mutation never joins an open load in flight", async () => {
    const server = new ThreadServer(["a"], [email("a")]);
    const { engine } = engineFor(server);

    await Promise.all([
      engine.loadThread("T", "a", { open: true }),
      engine.loadThread("T", "a"), // e.g. releaseAttachment's reload
    ]);

    expect(server.emailGets).toEqual([["a"], ["a"]]);
  });
});
