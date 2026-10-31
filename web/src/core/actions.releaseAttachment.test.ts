// SPDX-License-Identifier: AGPL-3.0-only

// Tests for releaseAttachment (#396/#412): the client half of lifting — or
// restoring — a malware quarantine on one projected attachment part.
//
// The client, sync engine and store are minimal fakes, following
// actions.createRule.test.ts's pattern: releaseAttachment touches exactly
// `store.getState().accountId`, `client.emailReleaseAttachment`,
// `sync.loadThread` and the error callback — nothing here reaches the offline
// queue, which is why it is not queued at all (see the method's own comment).

import { describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import type { ActionEvents } from "./actions";
import type { JmapClient } from "./jmap/client";
import type { OfflineActionQueue } from "./queue";
import type { MailStore } from "./store";
import type { SyncEngine } from "./sync";

interface ReleaseArgs {
  accountId: string;
  id: string;
  partId: string;
  revoke?: boolean;
}

interface Harness {
  actions: MailActions;
  /** Every `Email/releaseAttachment` call this test observed, in order — the
   * record of what actually went on the wire. */
  sent: ReleaseArgs[];
  refreshedThreads: string[];
  errors: string[];
}

/** @param answer what `client.emailReleaseAttachment` resolves (or throws). */
function harness(answer: () => unknown): Harness {
  const sent: ReleaseArgs[] = [];
  const refreshedThreads: string[] = [];
  const errors: string[] = [];

  const client = {
    emailReleaseAttachment(args: ReleaseArgs) {
      sent.push(args);
      return Promise.resolve(answer());
    },
  } as unknown as JmapClient;

  const sync = {
    loadThread(threadId: string) {
      refreshedThreads.push(threadId);
      return Promise.resolve();
    },
  } as unknown as SyncEngine;

  const store = { getState: () => ({ accountId: "A" }) } as unknown as MailStore;
  const events: ActionEvents = { onError: (m) => errors.push(m) };

  return {
    actions: new MailActions(client, store, {} as unknown as OfflineActionQueue, sync, events),
    sent,
    refreshedThreads,
    errors,
  };
}

const releaseReply = (affected: string[]) => ({
  accountId: "A",
  newState: "2",
  released: true,
  affectedEmailIds: affected,
});

describe("releaseAttachment", () => {
  it("sends Email/releaseAttachment with the MESSAGE id and partId — not the thread id", async () => {
    const h = harness(() => releaseReply(["E1"]));

    const ok = await h.actions.releaseAttachment("T1", "E1", "2");

    expect(ok).toBe(true);
    expect(h.sent).toEqual([{ accountId: "A", id: "E1", partId: "2" }]);
    expect(h.sent[0]?.id).not.toBe("T1");
  });

  it("refreshes the whole THREAD afterwards, not just the message asked about — the release can affect other copies of the same bytes", async () => {
    const h = harness(() => releaseReply(["E1", "E9"]));
    await h.actions.releaseAttachment("T1", "E1", "2");
    expect(h.refreshedThreads).toEqual(["T1"]);
  });

  it("omits revoke on an ordinary release", async () => {
    const h = harness(() => releaseReply(["E1"]));
    await h.actions.releaseAttachment("T1", "E1", "2");
    expect(h.sent[0]).not.toHaveProperty("revoke");
  });

  it("passes revoke through when undoing a release", async () => {
    const h = harness(() => ({ accountId: "A", newState: "3", released: false, affectedEmailIds: ["E1"] }));
    await h.actions.releaseAttachment("T1", "E1", "2", { revoke: true });
    expect(h.sent).toEqual([{ accountId: "A", id: "E1", partId: "2", revoke: true }]);
  });

  it("surfaces a failure via the error toast and does not refresh the thread", async () => {
    const sent: ReleaseArgs[] = [];
    const client = {
      emailReleaseAttachment(args: ReleaseArgs) {
        sent.push(args);
        return Promise.reject(new Error("boom"));
      },
    } as unknown as JmapClient;
    const refreshedThreads: string[] = [];
    const sync = {
      loadThread(threadId: string) {
        refreshedThreads.push(threadId);
        return Promise.resolve();
      },
    } as unknown as SyncEngine;
    const errors: string[] = [];
    const store = { getState: () => ({ accountId: "A" }) } as unknown as MailStore;
    const actions = new MailActions(client, store, {} as unknown as OfflineActionQueue, sync, {
      onError: (m) => errors.push(m),
    });

    const ok = await actions.releaseAttachment("T1", "E1", "2");

    expect(ok).toBe(false);
    expect(sent).toEqual([{ accountId: "A", id: "E1", partId: "2" }]);
    expect(refreshedThreads).toEqual([]);
    expect(errors).toHaveLength(1);
    expect(errors[0]).toContain("Could not release that file");
  });
});
