// SPDX-License-Identifier: AGPL-3.0-only

// Tests for createRule's name-collision retry (#292).
//
// This is deliberately tested through MailActions rather than through the pure
// helpers it calls, because THE BOUND IS THE MECHANISM. `nameTakenBy` and
// `bumpRuleName` are tested next door; what can only be observed here is that
// the loop bumps and re-sends, that it stops, and that it does not re-send for
// anything other than a collision. A test of the helpers alone would stay green
// through a loop that never retried and through one that retried forever.
//
// The client and store are minimal fakes: createRule touches exactly
// `store.getState().accountId`, `client.ruleSet` and the two event callbacks,
// and nothing in this path reaches the offline queue or the sync engine (rule
// writes are never queued — see the comment above listRules).

import { describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import type { ActionEvents } from "./actions";
import type { JmapClient } from "./jmap/client";
import type { OfflineActionQueue } from "./queue";
import type { MailStore } from "./store";
import type { SyncEngine } from "./sync";
import type { Rule, SetArgs, SetError, SetResult } from "./jmap/types";

/** The SetError internal/api/rule.go sends for a name collision. */
const nameTaken: SetError = {
  type: "invalidProperties",
  properties: ["name"],
  description: "a rule with that name already exists",
};

const created = (name: string): Rule =>
  ({
    id: "R1",
    name,
    priority: 100,
    conditions: { all: [] },
    actions: [],
    enabled: true,
    systemSuggested: false,
    corrections: 0,
  }) as Rule;

interface Harness {
  actions: MailActions;
  /** The `name` on each Rule/set create, in order — the record of what was
   * actually put on the wire. */
  sent: string[];
  errors: string[];
  infos: string[];
}

/**
 * @param answer decides each attempt: a SetError refuses it, null accepts it.
 */
function harness(answer: (attempt: number, name: string) => SetError | null): Harness {
  const sent: string[] = [];
  const errors: string[] = [];
  const infos: string[] = [];

  const client = {
    ruleSet(args: SetArgs): Promise<SetResult<Rule>> {
      const input = (args.create as Record<string, { name: string }>)["new"];
      const name = input?.name ?? "";
      sent.push(name);
      const err = answer(sent.length, name);
      return Promise.resolve({
        accountId: "A",
        oldState: "1",
        newState: "2",
        created: err ? null : { new: created(name) },
        notCreated: err ? { new: err } : null,
        updated: null,
        notUpdated: null,
        destroyed: null,
        notDestroyed: null,
      } as unknown as SetResult<Rule>);
    },
  } as unknown as JmapClient;

  const store = { getState: () => ({ accountId: "A" }) } as unknown as MailStore;
  const events: ActionEvents = {
    onError: (m) => errors.push(m),
    onInfo: (m) => infos.push(m),
  };

  return {
    actions: new MailActions(
      client,
      store,
      {} as unknown as OfflineActionQueue,
      {} as unknown as SyncEngine,
      events,
    ),
    sent,
    errors,
    infos,
  };
}

const draft = {
  name: "Venmo → Promotions",
  priority: 100,
  conditions: { all: [] },
  actions: [],
  enabled: true,
} as unknown as Omit<Rule, "id" | "systemSuggested" | "corrections">;

describe("createRule — retrying a name the server says is taken", () => {
  it("bumps the name and succeeds on the second attempt", async () => {
    const h = harness((attempt) => (attempt === 1 ? nameTaken : null));
    const result = await h.actions.createRule(draft);

    expect(h.sent).toEqual(["Venmo → Promotions", "Venmo → Promotions (2)"]);
    expect(result?.name).toBe("Venmo → Promotions (2)");
    expect(h.errors).toEqual([]);
    // The rename is REPORTED. A write that succeeds under a different name than
    // the one asked for, silently, is the deniable version of the failure this
    // project keeps meeting.
    expect(h.infos).toHaveLength(1);
    expect(h.infos[0]).toContain("Venmo → Promotions (2)");
  });

  it("says nothing about a rename when there was none", async () => {
    const h = harness(() => null);
    expect((await h.actions.createRule(draft))?.name).toBe("Venmo → Promotions");
    expect(h.sent).toEqual(["Venmo → Promotions"]);
    expect(h.infos).toEqual([]);
  });

  it("gives up after the bound and reports the error, rather than looping", async () => {
    // The server refuses EVERY name. Without a bound this call never returns.
    const h = harness(() => nameTaken);
    const result = await h.actions.createRule(draft);

    expect(result).toBeNull();
    expect(h.sent).toEqual([
      "Venmo → Promotions",
      "Venmo → Promotions (2)",
      "Venmo → Promotions (3)",
    ]);
    // Surfaced exactly as it was before the retry existed: the user is told,
    // and told about the name that actually failed last.
    expect(h.errors).toHaveLength(1);
    expect(h.errors[0]).toContain("a rule with that name already exists");
  });

  // ---- NEGATIVE CONTROL. Without this the retry could be firing on any
  // failure at all, and every one of the assertions above would still pass.
  it("does NOT retry an unrelated invalidProperties error", async () => {
    const badRule: SetError = {
      type: "invalidProperties",
      properties: ["conditions", "actions"],
      description: "rules: invalid regex",
    };
    const h = harness(() => badRule);
    const result = await h.actions.createRule(draft);

    expect(result).toBeNull();
    // ONE attempt. Re-sending a rule the server called malformed would be two
    // more identical refusals and the same message at the end of them.
    expect(h.sent).toEqual(["Venmo → Promotions"]);
    expect(h.errors).toHaveLength(1);
    expect(h.errors[0]).toContain("invalid regex");
  });

  it("does NOT retry a forbidden error", async () => {
    const h = harness(() => ({ type: "forbidden", description: "nope" }));
    expect(await h.actions.createRule(draft)).toBeNull();
    expect(h.sent).toEqual(["Venmo → Promotions"]);
  });
});
