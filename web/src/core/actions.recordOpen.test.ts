// SPDX-License-Identifier: AGPL-3.0-only

// recordOpen (#842): one Triage/open per call with the session's account, and a
// failure is swallowed — the open signal is training data, never a user-facing
// error.

import { describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import type { JmapClient } from "./jmap/client";
import type { OfflineActionQueue } from "./queue";
import type { MailStore } from "./store";
import type { SyncEngine } from "./sync";

function harness(fail: boolean) {
  const sent: { accountId: string; emailIds: string[] }[] = [];
  const errors: string[] = [];
  const client = {
    triageOpen(args: { accountId: string; emailIds: string[] }) {
      sent.push(args);
      return fail ? Promise.reject(new Error("offline")) : Promise.resolve({ opened: args.emailIds });
    },
  } as unknown as JmapClient;
  const store = { getState: () => ({ accountId: "A" }) } as unknown as MailStore;
  const actions = new MailActions(client, store, {} as OfflineActionQueue, {} as SyncEngine, {
    onError: (m) => errors.push(m),
  });
  return { actions, sent, errors };
}

describe("recordOpen", () => {
  it("sends one Triage/open for the message", async () => {
    const h = harness(false);
    await h.actions.recordOpen("m1");
    expect(h.sent).toEqual([{ accountId: "A", emailIds: ["m1"] }]);
    expect(h.errors).toEqual([]);
  });

  it("swallows a failure without surfacing an error", async () => {
    const h = harness(true);
    await expect(h.actions.recordOpen("m1")).resolves.toBeUndefined();
    expect(h.sent).toHaveLength(1);
    expect(h.errors).toEqual([]);
  });
});
