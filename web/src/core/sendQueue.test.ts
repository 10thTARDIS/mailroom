// SPDX-License-Identifier: AGPL-3.0-only

// OFFLINE QUEUED SEND (#62), client half.
//
// The thing under test is not "does the queue hold a send" — it is the two failures
// that make a naive queued send worse than no queued send at all:
//
//   1. The draft is created, then the process dies before the submission. A replay
//      that starts from the top creates a SECOND DRAFT.
//   2. The submission request reaches the server and its RESPONSE is lost. A replay
//      without an idempotency key SENDS THE MESSAGE TWICE.
//
// Both are exercised here against a fake server that records what it was actually
// asked, and both have a negative control — because an assertion like "the draft was
// created once" passes trivially if the replay never happened at all.

import { beforeEach, describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import { JmapAuthError, JmapMethodError, JmapTransportError } from "./jmap/client";
import type { JmapClient } from "./jmap/client";
import type { SetArgs } from "./jmap/types";
import { StorageActionQueue } from "./queue";
import { asQueuedSend, newIdempotencyKey, usableIdempotencyKey, KEY_MAX, KEY_MIN } from "./sendQueue";
import { MemoryStorage } from "./storage";
import { MailStore } from "./store";
import type { SyncEngine } from "./sync";

/**
 * A fake server that behaves like the real one on the points that matter: it
 * assigns draft ids, and it enforces the per-user uniqueness of an idempotency key
 * by returning THE SUBMISSION IT ALREADY MADE (internal/store/submissions.go
 * CreateSubmission). `failNext` is how a lost response is staged — the write
 * happens, then the answer is thrown away.
 */
class FakeServer {
  drafts: Record<string, unknown>[] = [];
  /** Every submission create it was asked for, key included, in order. */
  asked: Record<string, unknown>[] = [];
  /** The submissions that actually exist, by idempotency key. */
  byKey = new Map<string, string>();
  /** Submissions with no key at all — the ones that are deliberately not deduped. */
  keyless = 0;
  failEmailSet: Error | null = null;
  failSubmissionSet: Error | null = null;
  /** A SetError to return instead of creating (the server answering "no"). */
  refuseEmailSet: { type: string; description?: string } | null = null;
  refuseSubmissionSet: { type: string; description?: string } | null = null;
  /** Echo `archiveOnSuccess` on the created submission, as a server that
   * understands Send & archive (#648) does. Off = an older server that ignores it. */
  echoArchive = false;

  /** How many messages this server would actually put on the wire. */
  get sends(): number {
    return this.byKey.size + this.keyless;
  }

  emailSet = async (args: SetArgs): Promise<unknown> => {
    if (this.failEmailSet) {
      const err = this.failEmailSet;
      this.failEmailSet = null;
      throw err;
    }
    const create = (args.create ?? {}) as Record<string, Record<string, unknown>>;
    if (this.refuseEmailSet) {
      return { created: null, notCreated: { d: this.refuseEmailSet }, newState: "s" };
    }
    const obj = create["d"] ?? {};
    this.drafts.push(obj);
    const id = `M${this.drafts.length}`;
    return {
      created: { d: { ...draftShape(id), ...obj, id } },
      notCreated: null,
      newState: "s",
    };
  };

  submissionSet = async (args: SetArgs): Promise<unknown> => {
    const create = (args.create ?? {}) as Record<string, Record<string, unknown>>;
    const obj = create["s"];
    // RECORDED BEFORE THE FAILURE. That is the whole scenario: the server did the
    // work and the client never heard about it.
    if (obj) {
      this.asked.push(obj);
      const key = typeof obj["idempotencyKey"] === "string" ? obj["idempotencyKey"] : "";
      if (this.refuseSubmissionSet) {
        // nothing is created
      } else if (key === "") {
        this.keyless += 1;
      } else if (!this.byKey.has(key)) {
        this.byKey.set(key, `S${this.byKey.size + 1}`);
      }
    }
    if (this.failSubmissionSet) {
      const err = this.failSubmissionSet;
      this.failSubmissionSet = null;
      throw err;
    }
    if (this.refuseSubmissionSet) {
      return { created: null, notCreated: { s: this.refuseSubmissionSet }, newState: "s" };
    }
    const key = typeof obj?.["idempotencyKey"] === "string" ? obj["idempotencyKey"] : "";
    const id = key === "" ? `S${this.keyless}` : (this.byKey.get(key) as string);
    return {
      created: {
        s: {
          id,
          identityId: "I1",
          emailId: String(obj?.["emailId"] ?? ""),
          threadId: null,
          sendAt: "2026-08-01T00:00:00Z",
          undoStatus: "pending",
          ...(this.echoArchive && Array.isArray(obj?.["archiveOnSuccess"])
            ? { archiveOnSuccess: obj["archiveOnSuccess"] }
            : {}),
        },
      },
      notCreated: null,
      newState: "s",
    };
  };

  asClient(): JmapClient {
    return {
      emailSet: this.emailSet,
      submissionSet: this.submissionSet,
      request: async () => [],
    } as unknown as JmapClient;
  }
}

function draftShape(id: string): Record<string, unknown> {
  return {
    id,
    blobId: "b",
    threadId: "t",
    mailboxIds: {},
    keywords: { $draft: true },
    size: 1,
    receivedAt: "2026-08-01T00:00:00Z",
    sentAt: null,
    preview: "p",
    hasAttachment: false,
    messageId: [],
    inReplyTo: [],
    references: [],
    deliveredTo: null,
    identityId: null,
    suggestedLabels: null,
  };
}

const noSync = { poll: async () => {}, refreshView: async () => {} } as unknown as SyncEngine;

interface Harness {
  server: FakeServer;
  store: MailStore;
  storage: MemoryStorage;
  queue: StorageActionQueue;
  actions: MailActions;
  errors: string[];
  infos: string[];
}

/** A harness. `storage` is passed in to build a SECOND one over the same durable
 * bytes — which is what a page reload is. */
function harness(storage = new MemoryStorage(), server = new FakeServer()): Harness {
  const store = new MailStore(null);
  store.setSession({ } as never, "A1");
  const queue = new StorageActionQueue(storage);
  const errors: string[] = [];
  const infos: string[] = [];
  const actions = new MailActions(server.asClient(), store, queue, noSync, {
    onError: (m) => errors.push(m),
    onInfo: (m) => infos.push(m),
  });
  return { server, store, storage, queue, actions, errors, infos };
}

function send(h: Harness, subject = "the invoice"): ReturnType<MailActions["sendMessage"]> {
  return h.actions.sendMessage({
    from: { name: null, email: "me@example.com" },
    to: [{ name: null, email: "friend@example.net" }],
    subject,
    text: "the body the user typed",
  });
}

describe("a send the server cannot be told about", () => {
  it("is queued rather than lost, and says so", async () => {
    const h = harness();
    h.server.failEmailSet = new JmapTransportError("network down");

    const out = await send(h);
    expect(out?.queued).toBe(true);
    // NOT reported as sent, and not reported as a failure.
    expect(h.errors).toEqual([]);

    const pending = await h.queue.list();
    expect(pending).toHaveLength(1);
    expect(pending[0]?.entity).toBe("EmailSubmission");
    expect(pending[0]?.label).toContain("the invoice");

    // THE MESSAGE ITSELF IS IN THE QUEUE. Before this change the composed text
    // existed only in the DOM of a window that was about to close.
    const held = asQueuedSend(pending[0]?.payload ?? null);
    expect(held?.draft).toMatchObject({ subject: "the invoice" });
    expect(JSON.stringify(held?.draft)).toContain("the body the user typed");
    expect(held?.draftId).toBeNull();
    expect(held?.submit).toBe(true);
  });

  it("survives a reload and then sends exactly once", async () => {
    const storage = new MemoryStorage();
    const first = harness(storage);
    first.server.failEmailSet = new JmapTransportError("network down");
    await send(first);

    // THE RELOAD: a new store, a new queue object, a new runner — the same bytes.
    const server = new FakeServer();
    const second = harness(storage, server);
    await second.actions.resume();
    await second.actions.drain();

    expect(server.drafts).toHaveLength(1);
    expect(server.sends).toBe(1);
    expect(await second.queue.list()).toEqual([]);
    expect(second.infos.some((m) => m.includes("the invoice"))).toBe(true);
  });

  it("keeps a draft-only save as a draft, and never submits it", async () => {
    const h = harness();
    h.server.failEmailSet = new JmapTransportError("network down");
    const out = await h.actions.sendMessage({
      from: { name: null, email: "me@example.com" },
      to: [{ name: null, email: "friend@example.net" }],
      subject: "half written",
      text: "…",
      draftOnly: true,
    });
    expect(out?.queued).toBe(true);
    expect(asQueuedSend((await h.queue.list())[0]?.payload ?? null)?.submit).toBe(false);

    await h.actions.drain();
    expect(h.server.drafts).toHaveLength(1);
    // The one assertion that matters for a draft: NOTHING WAS SENT.
    expect(h.server.sends).toBe(0);
    expect(h.server.asked).toEqual([]);
  });
});

describe("a lost response to the submission", () => {
  it("does not send the message twice", async () => {
    const h = harness();
    // The draft is created. The submission request ARRIVES AND IS ACTED ON, and the
    // response is lost — the exact failure the idempotency key exists for.
    h.server.failSubmissionSet = new JmapTransportError("connection reset");

    const out = await send(h);
    expect(out?.queued).toBe(true);
    expect(out?.emailId).toBe("M1");
    expect(h.server.sends).toBe(1); // the server already did it

    await h.actions.drain();

    // ONE message on the wire, from two submission requests.
    expect(h.server.asked).toHaveLength(2);
    expect(h.server.sends).toBe(1);
    // And the second request carried THE SAME KEY — which is the mechanism, so it
    // is asserted rather than inferred from the count.
    expect(h.server.asked[1]?.["idempotencyKey"]).toBe(h.server.asked[0]?.["idempotencyKey"]);
    expect(await h.queue.list()).toEqual([]);
  });

  it("does not create a second draft, because the draft id was persisted first", async () => {
    const h = harness();
    h.server.failSubmissionSet = new JmapTransportError("connection reset");
    await send(h);

    // Persisted DURABLY, not just in memory: read it back out of storage.
    const reloaded = new StorageActionQueue(h.storage);
    expect(asQueuedSend((await reloaded.list())[0]?.payload ?? null)?.draftId).toBe("M1");

    await h.actions.drain();
    expect(h.server.drafts).toHaveLength(1);
  });

  // THE CASE THE TEST ABOVE DOES NOT COVER, and a probe caught the gap: there, the
  // draft id was written by sendMessage on its way to queueing. Here BOTH phases
  // happen inside the runner — the send was queued knowing nothing, its first replay
  // creates the draft and then loses the submission — so the id has to be persisted
  // by flushSend itself, mid-replay, or the next replay starts from the top.
  it("persists the draft id mid-replay, so a second replay does not make a second draft", async () => {
    const h = harness();
    h.server.failEmailSet = new JmapTransportError("network down");
    await send(h);
    expect(asQueuedSend((await h.queue.list())[0]?.payload ?? null)?.draftId).toBeNull();

    h.server.failSubmissionSet = new JmapTransportError("connection reset");
    await h.actions.drain(); // replay 1: draft created, submission lost
    expect(h.server.drafts).toHaveLength(1);

    await h.actions.drain(); // replay 2
    expect(h.server.drafts).toHaveLength(1);
    expect(h.server.sends).toBe(1);
    expect(await h.queue.list()).toEqual([]);
  });

  it("NEGATIVE CONTROL: without the persisted draft id, the replay does make a second draft", async () => {
    const h = harness();
    h.server.failSubmissionSet = new JmapTransportError("connection reset");
    await send(h);

    // Undo exactly the write under test — keep everything else — and the defect it
    // prevents comes back. Without this, "drafts === 1" above could be passing
    // because the replay never ran.
    const entry = (await h.queue.list())[0];
    const held = asQueuedSend(entry?.payload ?? null);
    await h.queue.setPayload(entry?.queueId as string, {
      ...(held as unknown as Record<string, unknown>),
      draftId: null,
    });

    await h.actions.drain();
    expect(h.server.drafts).toHaveLength(2);
    // Even then, the KEY still holds the line on what matters: one message sent.
    expect(h.server.sends).toBe(1);
  });

  it("NEGATIVE CONTROL: a keyless replay of the same send would go out twice", async () => {
    // Proof that the fake server is capable of double-sending at all, so the
    // single-send assertions above are the key's doing and not the fake's.
    const server = new FakeServer();
    for (let i = 0; i < 2; i++) {
      await server.submissionSet({ accountId: "A1", create: { s: { emailId: "M1" } } });
    }
    expect(server.sends).toBe(2);
  });
});

describe("a server that answers and says no", () => {
  it("is not queued — the user hears it while the window is still open", async () => {
    const h = harness();
    h.server.refuseEmailSet = {
      type: "forbiddenFrom",
      description: "you have no verified identity for that From address",
    };

    const out = await send(h);
    expect(out).toBeNull();
    expect(await h.queue.list()).toEqual([]);
    expect(h.errors.join(" ")).toContain("verified identity");
  });

  it("flags a refused SUBMISSION so the composer can tell it from a send (#692)", async () => {
    const h = harness();
    h.server.refuseSubmissionSet = { type: "noRecipients", description: "no recipients" };

    const out = await send(h);
    expect(out).toMatchObject({ submissionId: null, refused: true });
    expect(out?.emailId).not.toBeNull(); // the send-created draft stays on the server
    expect(out?.queued).toBeUndefined();
    expect(h.errors.join(" ")).toContain("no recipients");
    expect(await h.queue.list()).toEqual([]);
    expect(h.server.sends).toBe(0);
  });

  it("drops a queued send the server later refuses, instead of retrying it forever", async () => {
    const h = harness();
    h.server.failEmailSet = new JmapTransportError("network down");
    await send(h);

    h.server.refuseSubmissionSet = { type: "overQuota", description: "you are over your quota" };
    await h.actions.drain();

    expect(await h.queue.list()).toEqual([]);
    expect(h.errors.join(" ")).toContain("over your quota");
    expect(h.errors.join(" ")).toContain("the invoice");
    expect(h.server.sends).toBe(0);
  });

  it("keeps the send queued when the SESSION lapsed, which is not a refusal", async () => {
    const h = harness();
    h.server.failEmailSet = new JmapTransportError("network down");
    await send(h);

    h.server.failEmailSet = new JmapAuthError(401);
    await h.actions.drain();

    // Still there: signing in drains it. Losing a composed message because a token
    // expired would be the same bug in a different coat.
    expect(await h.queue.list()).toHaveLength(1);
    expect(h.store.getState().conn).toBe("unauthenticated");
    expect(h.errors).toEqual([]);
  });

  it("drops a send whose whole method the server rejected", async () => {
    const h = harness();
    h.server.failEmailSet = new JmapTransportError("network down");
    await send(h);

    h.server.failEmailSet = new JmapMethodError("set", {
      type: "unknownMethod",
      description: "no such method",
    });
    await h.actions.drain();
    expect(await h.queue.list()).toEqual([]);
    expect(h.errors.join(" ")).toContain("no such method");
  });
});

describe("the idempotency key", () => {
  it("is fresh for every send and inside the bound the server enforces", () => {
    const keys = new Set<string>();
    for (let i = 0; i < 500; i++) {
      const k = newIdempotencyKey();
      expect(usableIdempotencyKey(k)).toBe(true);
      expect(k.length).toBeGreaterThanOrEqual(KEY_MIN);
      expect(k.length).toBeLessThanOrEqual(KEY_MAX);
      keys.add(k);
    }
    // A COLLISION IS LOST MAIL, not a retry: the server would read the second send
    // as a replay of the first and suppress it.
    expect(keys.size).toBe(500);
  });

  it("is required for a payload to be replayable at all", () => {
    const base = { draft: { subject: "s" }, draftId: null, submit: true, subject: "s" };
    expect(asQueuedSend({ ...base, idempotencyKey: "x" })).toBeNull(); // too short
    expect(asQueuedSend({ ...base, idempotencyKey: "a".repeat(KEY_MAX + 1) })).toBeNull();
    expect(asQueuedSend({ ...base })).toBeNull(); // absent
    expect(asQueuedSend({ ...base, idempotencyKey: "send-abcdefgh" })).not.toBeNull();
    // A payload that is not a send at all — an ordinary flag patch — must not be
    // read as one.
    expect(asQueuedSend({ "keywords/$seen": true })).toBeNull();
    expect(asQueuedSend(null)).toBeNull();
  });

  it("makes an unreadable queue entry a discard, not a stuck queue", async () => {
    const h = harness();
    await h.queue.enqueue({
      accountId: "A1",
      entity: "EmailSubmission",
      op: "create",
      targetId: "d",
      payload: { garbage: true },
      label: "Send “corrupt”",
    });
    await h.actions.drain();
    // Gone, and reported. A send that could never succeed must not sit at the head
    // of the queue blocking the ones behind it.
    expect(await h.queue.list()).toEqual([]);
    expect(h.errors.join(" ")).toContain("corrupt");
  });
});

describe("nothing about the online path changed", () => {
  it("sends directly, in two round trips, and reports the submission id", async () => {
    const h = harness();
    const out = await send(h);
    expect(out?.queued).toBeUndefined();
    expect(out?.emailId).toBe("M1");
    expect(out?.submissionId).toBe("S1");
    expect(await h.queue.list()).toEqual([]);
    expect(h.server.sends).toBe(1);
    // The direct send carries the key too — which is what makes the queued retry
    // above resolve to the same submission rather than a second one.
    expect(usableIdempotencyKey(String(h.server.asked[0]?.["idempotencyKey"]))).toBe(true);
  });
});

describe("a queued send does not disturb the queued patches beside it", () => {
  it("drains both, and each by its own path", async () => {
    const h = harness();
    h.server.failEmailSet = new JmapTransportError("network down");
    await send(h);
    // A flag patch enqueued while offline, alongside the send.
    await h.actions.setSeen(["M9"], true);
    expect(await h.queue.list()).toHaveLength(2);

    let setCalls = 0;
    const client = h.server.asClient() as unknown as {
      request: (calls: unknown[]) => Promise<unknown[]>;
    };
    client.request = async () => {
      setCalls += 1;
      return [{ value: { notUpdated: null, notDestroyed: null, newState: "s" }, error: null }];
    };
    // The runner holds the client from construction, so rebuild the actions over the
    // same queue rather than mutating a live one.
    const actions = new MailActions(
      client as unknown as JmapClient,
      h.store,
      h.queue,
      noSync,
      { onError: (m) => h.errors.push(m), onInfo: (m) => h.infos.push(m) },
    );
    await actions.drain();

    expect(h.server.drafts).toHaveLength(1);
    expect(h.server.sends).toBe(1);
    expect(setCalls).toBe(1); // the patch went through the batched Email/set, once
    expect(await h.queue.list()).toEqual([]);
  });
});

// SEND & ARCHIVE (#648): the archive is the SERVER's, performed only once the
// message has actually been sent. The client's whole part is to pass the
// request along — on an immediate send AND on a held one — and to promise the
// archive only when the server has echoed that it understood.
describe("Send & archive", () => {
  function sendArchiving(h: Harness): ReturnType<MailActions["sendMessage"]> {
    return h.actions.sendMessage({
      from: { name: null, email: "me@example.com" },
      to: [{ name: null, email: "friend@example.net" }],
      subject: "the reply",
      text: "thanks",
      archiveOnSuccess: ["P1"],
    });
  }

  it("asks the server to archive on success, and reports it pending when the server echoes it", async () => {
    const h = harness();
    h.server.echoArchive = true;
    const out = await sendArchiving(h);
    expect(h.server.asked[0]?.["archiveOnSuccess"]).toEqual(["P1"]);
    expect(out?.archivePending).toBe(true);
  });

  // THE CONTROL: a server that ignores the key must not have the client announce
  // an archive nobody will perform.
  it("does not promise an archive the server did not acknowledge", async () => {
    const h = harness();
    const out = await sendArchiving(h);
    expect(h.server.asked[0]?.["archiveOnSuccess"]).toEqual(["P1"]);
    expect(out?.archivePending).toBe(false);
  });

  it("does not ask for an archive on a plain send", async () => {
    const h = harness();
    await send(h);
    expect(h.server.asked[0]).not.toHaveProperty("archiveOnSuccess");
  });

  it("holds the archive request WITH an offline send, so it too waits for the send", async () => {
    const h = harness();
    h.server.failEmailSet = new JmapTransportError("network down");
    const out = await sendArchiving(h);
    expect(out?.queued).toBe(true);
    expect(asQueuedSend((await h.queue.list())[0]?.payload ?? null)?.archiveOnSuccess).toEqual(["P1"]);

    await h.actions.drain();
    expect(h.server.sends).toBe(1);
    expect(h.server.asked[0]?.["archiveOnSuccess"]).toEqual(["P1"]);
  });

  it("drops a malformed archive list from a stored payload rather than sending it", () => {
    const base = { draft: { subject: "s" }, draftId: null, submit: true, subject: "s", idempotencyKey: "send-abcdefgh" };
    expect(asQueuedSend({ ...base, archiveOnSuccess: [1, 2] } as never)?.archiveOnSuccess).toBeUndefined();
    expect(asQueuedSend({ ...base, archiveOnSuccess: [] } as never)?.archiveOnSuccess).toBeUndefined();
    expect(asQueuedSend({ ...base, archiveOnSuccess: ["P1"] } as never)?.archiveOnSuccess).toEqual(["P1"]);
  });
});
