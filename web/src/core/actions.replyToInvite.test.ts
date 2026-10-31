// SPDX-License-Identifier: AGPL-3.0-only

// Answering a calendar invitation (#725): the client half. One call to
// Email/replyToInvite naming the MESSAGE, the submission it returns put in the
// store (so the Outbox sees it like any send), the thread reloaded so the card
// reads the server's record — and NOT queued offline: an answer replayed after
// the event moved would answer a question nobody is asking any more.

import { describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import type { ActionEvents } from "./actions";
import type { JmapClient } from "./jmap/client";
import { JmapTransportError } from "./jmap/client";
import type { OfflineActionQueue } from "./queue";
import type { MailStore } from "./store";
import type { SyncEngine } from "./sync";

function harness(answer: () => unknown) {
  const sent: unknown[] = [];
  const reloads: [string, string | undefined][] = [];
  const put: unknown[] = [];
  const errors: string[] = [];
  const queued: unknown[] = [];
  const cancels: unknown[] = [];
  const client = {
    emailReplyToInvite(args: unknown) {
      sent.push(args);
      return Promise.resolve().then(answer);
    },
    submissionSet(args: unknown) {
      cancels.push(args);
      return Promise.resolve({ updated: { S1: null } });
    },
  } as unknown as JmapClient;
  const sync = {
    loadThread(threadId: string, openedId?: string) {
      reloads.push([threadId, openedId]);
      return Promise.resolve();
    },
    poll: () => Promise.resolve(),
  } as unknown as SyncEngine;
  const store = {
    getState: () => ({ accountId: "A" }),
    putSubmissions: (s: unknown[]) => put.push(...s),
  } as unknown as MailStore;
  const queue = { enqueue: (a: unknown) => queued.push(a) } as unknown as OfflineActionQueue;
  const events: ActionEvents = { onError: (m) => errors.push(m), onInfo: () => undefined };
  return { actions: new MailActions(client, store, queue, sync, events), sent, reloads, put, errors, queued, cancels };
}

describe("replyToInvite", () => {
  it("sends one call naming the message and the answer, stores the submission, reloads the thread", async () => {
    const sub = { id: "S1", sendAt: "2026-10-17T12:00:10Z", undoStatus: "pending" };
    const h = harness(() => ({ accountId: "A", emailId: "D1", partstat: "ACCEPTED", submission: sub, newState: "9" }));
    const res = await h.actions.replyToInvite("T1", "M1", "accepted");
    expect(h.sent).toEqual([{ accountId: "A", emailId: "M1", partstat: "accepted" }]);
    expect(h.put).toEqual([sub]);
    expect(h.reloads).toEqual([["T1", "M1"]]);
    expect(res).toEqual({ submissionId: "S1", sendAt: "2026-10-17T12:00:10Z" });
  });

  it("a refusal is said in the server's words, nothing is reloaded or queued", async () => {
    const h = harness(() => {
      throw new Error("This invitation does not name a valid organiser address");
    });
    expect(await h.actions.replyToInvite("T1", "M1", "declined")).toBeNull();
    expect(h.errors.join(" ")).toContain("does not name a valid organiser");
    expect(h.reloads).toEqual([]);
    expect(h.queued).toEqual([]);
  });

  it("is NOT queued offline", async () => {
    const h = harness(() => {
      throw new JmapTransportError("offline");
    });
    expect(await h.actions.replyToInvite("T1", "M1", "accepted")).toBeNull();
    expect(h.queued).toEqual([]);
    expect(h.errors.length).toBe(1);
  });
});

describe("withdrawInviteReply", () => {
  it("cancels the submission and reloads the thread so the card stops saying 'You accepted'", async () => {
    const h = harness(() => null);
    expect(await h.actions.withdrawInviteReply("T1", "M1", "S1")).toBe(true);
    expect(h.cancels).toEqual([{ accountId: "A", update: { S1: { undoStatus: "canceled" } } }]);
    expect(h.reloads).toEqual([["T1", "M1"]]);
  });
});
