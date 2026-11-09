// SPDX-License-Identifier: AGPL-3.0-only

// A rule's message-kind binding travels on Rule/set unchanged: createRule sends
// `kind` as given (a kind, or null for "any"), updateRule patches it, and every
// kind the editor offers has a label.

import { describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import type { ActionEvents } from "./actions";
import type { JmapClient } from "./jmap/client";
import type { OfflineActionQueue } from "./queue";
import type { MailStore } from "./store";
import type { SyncEngine } from "./sync";
import { MESSAGE_KINDS } from "./jmap/types";
import type { Rule, SetArgs, SetResult } from "./jmap/types";
import { t } from "../app/i18n";
import type { MessageKey } from "../app/i18n";

function harness(): { actions: MailActions; sent: SetArgs[] } {
  const sent: SetArgs[] = [];
  const client = {
    ruleSet(args: SetArgs): Promise<SetResult<Rule>> {
      sent.push(args);
      return Promise.resolve({
        accountId: "A",
        oldState: "1",
        newState: "2",
        created: { new: { id: "R1" } as Rule },
        notCreated: null,
        updated: { R1: null },
        notUpdated: null,
        destroyed: null,
        notDestroyed: null,
      } as unknown as SetResult<Rule>);
    },
  } as unknown as JmapClient;
  const store = { getState: () => ({ accountId: "A" }) } as unknown as MailStore;
  const events: ActionEvents = {};
  return {
    actions: new MailActions(
      client,
      store,
      {} as unknown as OfflineActionQueue,
      {} as unknown as SyncEngine,
      events,
    ),
    sent,
  };
}

const base = {
  name: "Shop promotions",
  priority: 100,
  conditions: { all: [] },
  actions: [],
  enabled: true,
};

describe("a rule's message kind", () => {
  it("is sent on create, as a kind or as null for any", async () => {
    const h = harness();
    await h.actions.createRule({ ...base, kind: "promotional" } as unknown as Omit<Rule, "id" | "systemSuggested" | "corrections">);
    await h.actions.createRule({ ...base, kind: null } as unknown as Omit<Rule, "id" | "systemSuggested" | "corrections">);
    const kinds = h.sent.map((a) => (a.create as Record<string, { kind?: unknown }>)["new"]?.kind);
    expect(kinds).toEqual(["promotional", null]);
  });

  it("is patched on update, and null clears it", async () => {
    const h = harness();
    await h.actions.updateRule("R1", { kind: "list" });
    await h.actions.updateRule("R1", { kind: null });
    const patches = h.sent.map((a) => (a.update as Record<string, { kind?: unknown }>)["R1"]);
    expect(patches).toEqual([{ kind: "list" }, { kind: null }]);
  });

  it("has a label for every kind the editor offers", () => {
    expect(MESSAGE_KINDS.length).toBe(7);
    for (const k of MESSAGE_KINDS) {
      const key = `rules.kind.${k}` as MessageKey;
      expect(t(key)).not.toBe(key);
    }
  });
});
