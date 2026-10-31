// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";
import { currentTab, planFor, tabToView, viewFromHash, viewKey, viewToHash } from "./views";
import type { View } from "./views";
import type { Mailbox } from "./jmap/types";

// The category-tab view (#228). What is worth pinning is not that a filter is
// built — it is the three decisions that make a tab a NARROWING of the inbox
// rather than a fourth kind of container.

const mailboxes: Mailbox[] = [
  { id: "mb-a", name: "INBOX", role: "inbox", totalEmails: 0, unreadEmails: 0 } as Mailbox,
  { id: "mb-b", name: "INBOX", role: "inbox", totalEmails: 0, unreadEmails: 0 } as Mailbox,
  { id: "mb-arch", name: "Archive", role: "archive", totalEmails: 0, unreadEmails: 0 } as Mailbox,
];

describe("the category-tab view", () => {
  it("narrows the INBOX, one query per account, and never leaves it", () => {
    const plan = planFor({ kind: "tab", tab: "promotions" }, mailboxes, {});
    // One filter per inbox mailbox — the server has no OR, so a unified inbox is
    // N queries the client merges.
    expect(plan.filters).toHaveLength(2);
    for (const f of plan.filters) {
      expect(f.inTab).toBe("promotions");
      // SCOPED TO THE INBOX. A "Promotions" that also listed archived or sent
      // mail would be neither a folder nor a label but a third thing.
      expect(["mb-a", "mb-b"]).toContain(f.inMailbox);
    }
    expect(plan.role).toBe("inbox");
    expect(plan.needsSearch).toBe(false);
  });

  it("excludes snoozed mail, like the inbox it narrows", () => {
    const plan = planFor({ kind: "tab", tab: "updates" }, mailboxes, {});
    expect(plan.snoozed).toBe(false);
    for (const f of plan.filters) expect(f.isSnoozed).toBe(false);
  });

  it("passes the tab the user clicked, not an expanded set", () => {
    // The server applies the enabled-fold — asking for primary also returns mail
    // from tabs that are switched off. If the client expanded it here, that logic
    // would exist twice and the two would drift.
    const plan = planFor({ kind: "tab", tab: "primary" }, mailboxes, {});
    for (const f of plan.filters) expect(f.inTab).toBe("primary");
  });

  it("round-trips through the URL hash", () => {
    for (const tab of ["primary", "social", "promotions", "updates", "forums"] as const) {
      const v: View = { kind: "tab", tab };
      expect(viewToHash(v)).toBe(`#/tab/${tab}`);
      expect(viewFromHash(viewToHash(v))).toEqual(v);
      expect(viewKey(v)).toBe(`tab:${tab}`);
    }
  });

  it("falls back to the inbox for a tab outside the vocabulary", () => {
    // A pasted or stale link must not produce a view the server would refuse
    // with invalidArguments — the vocabulary is closed, so anything else is a
    // typo.
    expect(viewFromHash("#/tab/inbox")).toEqual({ kind: "role", role: "inbox" });
    expect(viewFromHash("#/tab/")).toEqual({ kind: "role", role: "inbox" });
    expect(viewFromHash("#/tab/PROMOTIONS")).toEqual({ kind: "role", role: "inbox" });
  });

  // ⚠ THE BUG DOGFOODING FOUND, in under an hour. The strip first routed Primary
  // to the plain inbox view, on the reasoning that Primary "already collects
  // everything no other tab claimed". The inbox view carries NO inTab filter, so
  // it lists every message — and every mail appeared under Primary AND under its
  // real tab at the same time. The strip looked completely broken.
  //
  // The inbox and Primary answer different questions. "Inbox" is all of it;
  // "Primary" is what no other enabled tab claimed, plus whatever folded in from
  // a disabled one. Only the server can answer the second.
  it("routes EVERY tab through inTab, Primary included", () => {
    for (const tab of ["primary", "social", "promotions", "updates", "forums"] as const) {
      const v = tabToView(tab);
      expect(v).toEqual({ kind: "tab", tab });
      const plan = planFor(v, mailboxes, {});
      expect(plan.filters.length).toBeGreaterThan(0);
      for (const f of plan.filters) expect(f.inTab).toBe(tab);
    }
  });

  it("highlights no tab on the plain inbox, because that view is not filtered", () => {
    // Highlighting Primary on the unfiltered inbox would claim the list is
    // narrowed when it is not — the same lie the routing bug told, just in the
    // chrome instead of the data.
    expect(currentTab({ kind: "role", role: "inbox" })).toBeNull();
    expect(currentTab({ kind: "role", role: "archive" })).toBeNull();
    expect(currentTab({ kind: "search", query: "x" })).toBeNull();
    expect(currentTab({ kind: "tab", tab: "updates" })).toBe("updates");
  });

  it("has no inbox mailbox, no queries — not one unscoped query", () => {
    // The degenerate case worth pinning: with no inbox to narrow, the plan is
    // EMPTY. A fallback to a single filter carrying only `inTab` would quietly
    // widen the view to the whole mailbox, archive and sent included.
    const plan = planFor({ kind: "tab", tab: "forums" }, [], {});
    expect(plan.filters).toHaveLength(0);
  });
});
