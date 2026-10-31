// SPDX-License-Identifier: AGPL-3.0-only

// undoRuleApply and an area-filing apply (#708).
//
// A rule that files into an area takes the desk's (or day-one setup's)
// placement labels off the mail it moves, and only the client can hand them
// back: the server keeps no memory of an operation. So the undo must send the
// apply's `removedAreaLabels` as `restoreAreaLabels`, or it takes the rule's
// label off and leaves the mail in neither its old area nor the new one. A
// plain rule's undo must stay exactly the request it always was.

import { describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import type { JmapClient } from "./jmap/client";
import type { OfflineActionQueue } from "./queue";
import type { MailStore } from "./store";
import type { SyncEngine } from "./sync";
import type { RuleApplyResult } from "./jmap/types";

function harness(): { actions: MailActions; sent: Record<string, unknown>[] } {
  const sent: Record<string, unknown>[] = [];
  const client = {
    ruleApply(args: Record<string, unknown>): Promise<RuleApplyResult> {
      sent.push(args);
      return Promise.resolve({ undone: 0, messageIds: [] } as unknown as RuleApplyResult);
    },
  } as unknown as JmapClient;
  const store = { getState: () => ({ accountId: "A" }) } as unknown as MailStore;
  const sync = { refreshView: () => Promise.resolve() } as unknown as SyncEngine;
  return {
    actions: new MailActions(client, store, {} as unknown as OfflineActionQueue, sync, {}),
    sent,
  };
}

describe("undoRuleApply", () => {
  it("hands an area-filing apply's removed labels back as restoreAreaLabels", async () => {
    const h = harness();
    const removed = { M1: [{ labelId: "Lprimary", appliedBy: "desk" as const }] };
    await h.actions.undoRuleApply("R1", ["M1", "M2"], removed);
    expect(h.sent).toEqual([
      { accountId: "A", ruleId: "R1", undo: true, messageIds: ["M1", "M2"], restoreAreaLabels: removed },
    ]);
  });

  it("sends the same request as before for a rule that removed nothing", async () => {
    const h = harness();
    await h.actions.undoRuleApply("R1", ["M1"], {});
    await h.actions.undoRuleApply("R1", ["M2"]);
    expect(h.sent).toEqual([
      { accountId: "A", ruleId: "R1", undo: true, messageIds: ["M1"] },
      { accountId: "A", ruleId: "R1", undo: true, messageIds: ["M2"] },
    ]);
  });
});
