// SPDX-License-Identifier: AGPL-3.0-only

// shadowNotices (ADR-028 §5, #521) — which accounts in view are shadowed, and
// the two traps a naive version would fall into: claiming a state before
// health has answered, and collapsing several shadowed accounts into one.

import { describe, expect, it } from "vitest";

import { shadowNotices } from "./writebackBanner";
import type { ShadowAccount } from "./writebackBanner";

function account(over: Partial<ShadowAccount> = {}): ShadowAccount {
  return { id: "a1", name: "Personal", writebackEnabled: false, suppressedIntents: 0, ...over };
}

describe("shadowNotices", () => {
  it("is empty when no account is shadowed", () => {
    expect(shadowNotices([account({ writebackEnabled: true })], null)).toEqual([]);
  });

  it("NEGATIVE CONTROL: claims nothing before health has been read", () => {
    // `null` is "we have not asked yet" (useAccounts.ts's rule for `status`),
    // not "writing" and not "shadowed" — a banner built from a guess here
    // would either invent an outage or hide a real one.
    expect(shadowNotices([account({ writebackEnabled: null })], null)).toEqual([]);
  });

  it("names a single shadowed account with its suppressed count", () => {
    const notices = shadowNotices(
      [account({ id: "a1", name: "Personal", writebackEnabled: false, suppressedIntents: 1847 })],
      null,
    );
    expect(notices).toEqual([{ id: "a1", name: "Personal", suppressedIntents: 1847 }]);
  });

  it("reports EVERY shadowed account in an unscoped view, not just the worst one", () => {
    const notices = shadowNotices(
      [
        account({ id: "a1", name: "Personal", writebackEnabled: false, suppressedIntents: 3 }),
        account({ id: "a2", name: "Work", writebackEnabled: true, suppressedIntents: 0 }),
        account({ id: "a3", name: "Old", writebackEnabled: false, suppressedIntents: 9001 }),
      ],
      null,
    );
    expect(notices).toEqual([
      { id: "a1", name: "Personal", suppressedIntents: 3 },
      { id: "a3", name: "Old", suppressedIntents: 9001 },
    ]);
  });

  it("narrows to the scoped account even when another one is also shadowed", () => {
    const notices = shadowNotices(
      [
        account({ id: "a1", name: "Personal", writebackEnabled: false }),
        account({ id: "a2", name: "Work", writebackEnabled: false }),
      ],
      "a2",
    );
    expect(notices).toEqual([{ id: "a2", name: "Work", suppressedIntents: 0 }]);
  });

  it("is empty when scoped to an account that is not shadowed, even if others are", () => {
    const notices = shadowNotices(
      [
        account({ id: "a1", name: "Personal", writebackEnabled: false }),
        account({ id: "a2", name: "Work", writebackEnabled: true }),
      ],
      "a2",
    );
    expect(notices).toEqual([]);
  });

  it("is empty when scoped to an account not present in the list at all", () => {
    expect(shadowNotices([account({ id: "a1" })], "missing")).toEqual([]);
  });
});
