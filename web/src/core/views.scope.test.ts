// SPDX-License-Identifier: AGPL-3.0-only

// THE ACCOUNT SCOPE, AND ALL MAIL (#375 increment 4).
//
// What is worth pinning here is not that a key is copied into an object. It is
// the claim the account switcher makes: that choosing one account narrows
// EVERY list, and that it does so by intersecting the question the view already
// asked rather than by replacing it.
//
// The old behaviour was the opposite and was documented as a limit of the query
// layer: a per-account view was the SEARCH `account:<id>`, the server has no
// filter operators, and so "this account's starred mail" could not be one
// query. `inAccount` is structured and flat, so it can — and these tests are
// what stops that regressing into a filter that is silently dropped for one
// kind, which would show another account's mail under this account's name.

import { describe, expect, it } from "vitest";

import { planFor, viewKey } from "./views";
import type { View } from "./views";
import type { Label, Mailbox } from "./jmap/types";

const ACC = "01JBQZ3K7WX2N4Y6P8R0S1T2AC";
const AREA = "01JBQZ3K7WX2N4Y6P8R0S1T2AR";
const LBL = "01JBQZ3K7WX2N4Y6P8R0S1T2LB";
const LOCAL = "01JBQZ3K7WX2N4Y6P8R0S1T2LC";

/** Two accounts' worth of mailboxes: Email/query has no filter term for the
 * Mailbox's own accountId (#488 added that field to Mailbox, not to the
 * filter grammar), so a unified role view is still N queries and the scope
 * has to narrow every one of them. */
const mailboxes: Mailbox[] = [
  { id: "mb-a", name: "INBOX", role: "inbox" } as Mailbox,
  { id: "mb-b", name: "INBOX", role: "inbox" } as Mailbox,
  { id: "mb-arch", name: "Archive", role: "archive" } as Mailbox,
];

const labels: Record<string, Label> = {
  [LBL]: { id: LBL, name: "Work", keyword: "MailroomLabel-work" } as Label,
  // A LOCAL-ONLY label has no keyword atom to match on.
  [LOCAL]: { id: LOCAL, name: "Client Work", keyword: null } as Label,
};

/** Every kind the switcher is specified to narrow, unscoped and scoped. */
const SCOPED_KINDS: { name: string; view: View }[] = [
  { name: "role", view: { kind: "role", role: "inbox" } },
  { name: "starred", view: { kind: "starred" } },
  { name: "priority", view: { kind: "priority" } },
  { name: "snoozed", view: { kind: "snoozed" } },
  { name: "unread", view: { kind: "unread" } },
  { name: "label", view: { kind: "label", labelId: LBL } },
  { name: "tab", view: { kind: "tab", tab: "promotions" } },
  { name: "all", view: { kind: "all" } },
  // Not in the spec's list, and scoped anyway: `#/inbox/sort` is a list route,
  // the switcher puts `acct` on it, and the rail prints its count from the
  // SCOPED summary — so an unscoped queue would sit under a badge counting
  // something else.
  { name: "needsCategory", view: { kind: "needsCategory" } },
];

describe("the account scope narrows every structured view", () => {
  it("adds inAccount to EVERY filter of the plan, and to none without a scope", () => {
    for (const { name, view } of SCOPED_KINDS) {
      const plain = planFor(view, mailboxes, labels);
      for (const f of plain.filters) expect(f.inAccount, `${name} unscoped`).toBeUndefined();

      const scoped = planFor({ ...view, accountId: ACC } as View, mailboxes, labels);
      expect(scoped.filters.length, `${name} filter count`).toBe(plain.filters.length);
      for (const f of scoped.filters) expect(f.inAccount, `${name} scoped`).toBe(ACC);
    }
  });

  it("keeps everything else the view asked for — it intersects, it does not replace", () => {
    // The failure this guards is the one that looks like it works: a scope that
    // REPLACED the filter would return that account's whole mailbox under the
    // heading of a label, a tab or the priority inbox.
    const starred = planFor({ kind: "starred", accountId: ACC }, mailboxes, labels);
    expect(starred.filters).toEqual([{ hasKeyword: "$flagged", inAccount: ACC }]);

    const tab = planFor({ kind: "tab", tab: "promotions", accountId: ACC }, mailboxes, labels);
    for (const f of tab.filters) {
      expect(f.inTab).toBe("promotions");
      expect(f.isSnoozed).toBe(false);
      expect(["mb-a", "mb-b"]).toContain(f.inMailbox);
    }

    const label = planFor({ kind: "label", labelId: LBL, accountId: ACC }, mailboxes, labels);
    // The text half and the structured scope in ONE flat filter: the server
    // runs the operator against the index and ANDs `inAccount` in SQL.
    expect(label.filters).toEqual([{ query: "label:Work", inAccount: ACC }]);
    expect(label.needsSearch).toBe(true);

    const unread = planFor({ kind: "unread", accountId: ACC }, mailboxes, labels);
    expect(unread.filters).toEqual([{ notKeyword: "$seen", inAccount: ACC }]);

    const snoozed = planFor({ kind: "snoozed", accountId: ACC }, mailboxes, labels);
    expect(snoozed.filters).toEqual([{ isSnoozed: true, inAccount: ACC }]);
    expect(snoozed.snoozed).toBe(true);

    const inbox = planFor({ kind: "role", role: "inbox", accountId: ACC }, mailboxes, labels);
    // One query per inbox mailbox still — the client cannot tell which belongs
    // to the chosen account, and the intersection answers that for it.
    expect(inbox.filters).toHaveLength(2);
    for (const f of inbox.filters) expect(f.isSnoozed).toBe(false);
  });

  it("leaves a folder, a search and the legacy account view unscoped", () => {
    // A mailbox already belongs to one account; a search carries its own
    // `account:` operator; the legacy view IS the operator.
    expect(planFor({ kind: "mailbox", mailboxId: "mb-arch" }, mailboxes, labels).filters).toEqual([
      { inMailbox: "mb-arch" },
    ]);
    expect(planFor({ kind: "search", query: "invoice" }, mailboxes, labels).filters).toEqual([
      { query: "invoice" },
    ]);
    expect(planFor({ kind: "account", accountId: ACC }, mailboxes, labels).filters).toEqual([
      { query: `account:${ACC}` },
    ]);
  });

  it("makes the scope part of the view's IDENTITY", () => {
    // state.tsx keeps the View object stable while its key is unchanged and
    // reloads the list when it moves. A key that ignored the scope would leave
    // the previous account's mail on screen after a switch, with no request
    // made and nothing to say so.
    for (const { name, view } of SCOPED_KINDS) {
      expect(viewKey({ ...view, accountId: ACC } as View), name).not.toBe(viewKey(view));
    }
  });
});

describe("all mail", () => {
  it("is ONE query with no mailbox condition — the server excludes only trash", () => {
    const plan = planFor({ kind: "all" }, mailboxes, labels);
    expect(plan.filters).toEqual([{}]);
    expect(plan.role).toBeNull();
    // It does NOT filter the snooze: sleeping mail is still mail, and this is
    // the list that promises to hold all of it.
    expect(plan.snoozed).toBeNull();
    expect(plan.needsSearch).toBe(false);
    // No sort: the plan takes the client's default (returnedAt desc), which is
    // the order store.ts re-sorts every list into. Asking the server for a
    // different one would put the page boundary and the rows out of step.
    expect(plan.sort).toBeUndefined();
  });

  it("narrows by AREA through the area's own claim, not through its label", () => {
    const plan = planFor({ kind: "all", areaId: AREA }, mailboxes, labels);
    expect(plan.filters).toEqual([{ inArea: AREA }]);
    expect(plan.needsSearch).toBe(false);
  });

  it("narrows by LABEL structurally, with no dependency on the search index", () => {
    // `hasKeyword` on a label atom resolves through `label_keyword_map` and
    // matches `message_labels` server-side (internal/store keywordPredicate) —
    // the note in views.ts saying it does not is stale.
    const plan = planFor({ kind: "all", labelId: LBL }, mailboxes, labels);
    expect(plan.filters).toEqual([{ hasKeyword: "MailroomLabel-work" }]);
    expect(plan.needsSearch).toBe(false);
  });

  it("falls back to the operator for a LOCAL-ONLY label, which has no atom", () => {
    const plan = planFor({ kind: "all", labelId: LOCAL }, mailboxes, labels);
    expect(plan.filters).toEqual([{ query: 'label:"Client Work"' }]);
    expect(plan.needsSearch).toBe(true);
  });

  it("asks NOTHING for a label it has never heard of, rather than everything", () => {
    // The failure mode being excluded: an empty filter would list the whole
    // account under a heading naming one label.
    const plan = planFor({ kind: "all", labelId: "01JBQZ3K7WX2N4Y6P8R0S1T2XX" }, mailboxes, labels);
    expect(plan.filters).toEqual([]);
  });

  it("combines every narrowing into the one flat filter", () => {
    const plan = planFor(
      { kind: "all", areaId: AREA, labelId: LBL, accountId: ACC },
      mailboxes,
      labels,
    );
    expect(plan.filters).toEqual([
      { inArea: AREA, hasKeyword: "MailroomLabel-work", inAccount: ACC },
    ]);
  });
});
