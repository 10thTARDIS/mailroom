// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import {
  INBOX_ROUTE,
  SETTINGS_ROWS,
  escapeTarget,
  formatRoute,
  isListRoute,
  legacyRedirect,
  listRouteOf,
  parseRoute,
  routeKey,
  viewForRoute,
  viewRoute,
} from "./routes";
import type { Route } from "./routes";
import { viewFromHash } from "./views";
import type { View } from "./views";

// The route table (#373). What is worth pinning here is not that a string is
// built — it is that the URL is now the single statement of what is on screen,
// which only holds if a route survives being written down and read back, if
// every hash that worked before still means the same thing, and if a hash
// someone mangled cannot take the app down.

/** Ids in the shape the server actually issues (ULIDs, ADR-007). */
const M1 = "01JBQZ3K7WX2N4Y6P8R0S1T2V3";
const M2 = "01JBQZ3K7WX2N4Y6P8R0S1T2V4";
const ACC = "01JBQZ3K7WX2N4Y6P8R0S1T2AC";
const LBL = "01JBQZ3K7WX2N4Y6P8R0S1T2LB";
const AREA = "01JBQZ3K7WX2N4Y6P8R0S1T2AR";

/** Every route form, once each. */
const BASE_ROUTES: Route[] = [
  { page: "inbox", tab: null },
  { page: "inbox", tab: "review" },
  { page: "inbox", tab: "rest" },
  { page: "inbox", tab: null, sort: true },
  { page: "tray" },
  { page: "all" },
  { page: "all", area: AREA },
  { page: "all", label: LBL },
  { page: "all", archived: true },
  { page: "all", area: AREA, label: LBL, archived: true },
  { page: "view", view: { kind: "starred" } },
  { page: "view", view: { kind: "priority" } },
  { page: "view", view: { kind: "needsCategory" } },
  { page: "view", view: { kind: "snoozed" } },
  { page: "view", view: { kind: "unread" } },
  { page: "view", view: { kind: "role", role: "archive" } },
  { page: "view", view: { kind: "role", role: "trash" } },
  { page: "view", view: { kind: "label", labelId: LBL } },
  { page: "view", view: { kind: "mailbox", mailboxId: M1 } },
  { page: "view", view: { kind: "account", accountId: ACC } },
  // The Outbox (#448). A view with a URL and no Email/query behind it — the
  // round trip has to hold for it exactly as for the rest, since the rail links
  // to it and Escape peels back through it.
  { page: "view", view: { kind: "outbox" } },
  { page: "view", view: { kind: "tab", tab: "promotions" } },
  { page: "view", view: { kind: "search", query: "from:alice invoice" } },
  { page: "view", view: { kind: "search", query: "a/b?c&d=e" } },
  { page: "settings", section: null },
  // The twelve rows of the settings page, plus the shortcuts sheet (#374
  // increment 2). Enumerated from SETTINGS_ROWS rather than typed out, so a
  // thirteenth row cannot be added to the vocabulary without also being put
  // through the round trip below.
  ...SETTINGS_ROWS.map((section): Route => ({ page: "settings", section })),
  { page: "settings", section: "shortcuts" },
  { page: "settings", section: "accounts", accountId: ACC },
  { page: "settings", section: "areas", areaId: AREA },
  { page: "settings", section: "api-keys", apiKeyId: AREA },
  // Day-one setup (#446): the four steps, and one folder being imported later.
  { page: "setup", step: 1 },
  { page: "setup", step: 2 },
  { page: "setup", step: 3 },
  { page: "setup", step: 4 },
  { page: "setup", step: 2, importMailboxId: M1 },
];

/** The same routes, each also carrying every shared query parameter — the part
 * a formatter is most likely to drop silently. */
const ALL_ROUTES: Route[] = BASE_ROUTES.flatMap((r) => [
  r,
  { ...r, m: M1 },
  { ...r, acct: ACC },
  { ...r, compose: "new" as const },
  { ...r, compose: { reply: M2 } },
  { ...r, compose: { replyAll: M2 } },
  { ...r, compose: { forward: M2 } },
  { ...r, compose: { draft: M2 } },
  { ...r, m: M1, acct: ACC, compose: { reply: M2 } },
]);

describe("a route survives being written down and read back", () => {
  it("round-trips every route form, with every query parameter", () => {
    for (const route of ALL_ROUTES) {
      expect(parseRoute(formatRoute(route)), formatRoute(route)).toEqual(route);
    }
  });

  it("has ONE string per route, so the formatted hash can be a key", () => {
    // Same route, different key order in the literal: one string.
    expect(formatRoute({ page: "inbox", tab: "review", acct: ACC, m: M1 })).toBe(
      formatRoute({ m: M1, acct: ACC, page: "inbox", tab: "review" }),
    );
    expect(formatRoute({ page: "inbox", tab: "review", m: M1, acct: ACC })).toBe(
      `#/inbox/review?m=${M1}&acct=${ACC}`,
    );
    expect(formatRoute({ page: "all", label: LBL, archived: true, m: M1 })).toBe(
      `#/all?label=${LBL}&archived=1&m=${M1}`,
    );
    expect(formatRoute({ page: "inbox", tab: null, compose: { reply: M2 } })).toBe(
      `#/inbox?compose=reply:${M2}`,
    );
    expect(formatRoute({ page: "inbox", tab: null, compose: { replyAll: M2 } })).toBe(
      `#/inbox?compose=replyall:${M2}`,
    );
  });

  // #646. Reply all is a second compose kind alongside reply/forward/draft, and
  // it round-trips the same way — spelled `replyall:<id>` in the query string,
  // parsed back to the camelCase `{replyAll}` shape.
  it("round-trips replyAll like the other compose kinds, and rejects an unknown kind", () => {
    expect(parseRoute(`#/inbox?compose=replyall:${M2}`)).toEqual({
      page: "inbox",
      tab: null,
      compose: { replyAll: M2 },
    });
    expect(parseRoute("#/inbox?compose=replyall:")).toEqual(INBOX_ROUTE);
    // A kind this table has never heard of drops the whole compose param,
    // exactly like the existing `sideways:x` case above.
    expect(parseRoute(`#/inbox?compose=sideways:${M2}`)).toEqual(INBOX_ROUTE);
  });

  // #448. A place with a URL and no query behind it (core/views.ts `outbox`), so
  // the two halves that could disagree — the route table and views.ts's own hash
  // reader — are checked against each other rather than against one expectation
  // written twice.
  it("routes #/outbox to the Outbox view, and back", () => {
    expect(parseRoute("#/outbox")).toEqual({ page: "view", view: { kind: "outbox" } });
    expect(viewForRoute(parseRoute("#/outbox"))).toEqual(viewFromHash("#/outbox"));
    expect(formatRoute(viewRoute({ kind: "outbox" }))).toBe("#/outbox");
  });

  it("does not narrow the Outbox to an account", () => {
    // A submission carries an identityId and no account, so a scoped Outbox
    // would hide rows whenever an identity had not loaded — under-reporting what
    // is waiting, which is the one thing this list must not do.
    expect(viewForRoute(parseRoute(`#/outbox?acct=${ACC}`))).toEqual({ kind: "outbox" });
  });

  it("canonicalises #/inbox to the inbox PAGE, which is the same URL", () => {
    // The one documented exception to the round trip: the role view and the
    // inbox page are the same hash, and viewForRoute gives them the same View,
    // so the page is the canonical reading of it.
    const asView: Route = { page: "view", view: { kind: "role", role: "inbox" } };
    expect(formatRoute(asView)).toBe("#/inbox");
    expect(parseRoute(formatRoute(asView))).toEqual({ page: "inbox", tab: null });
    expect(viewForRoute(parseRoute("#/inbox"))).toEqual(viewFromHash("#/inbox"));
  });
});

// EVERY hash core/views.ts accepts, including the ones it accepts by falling
// back. Compared DIFFERENTIALLY against viewFromHash rather than against a
// second hand-written list of expectations: the claim is "routing changed no
// destination", and only views.ts can be the authority on what a hash meant.
const LEGACY_HASHES = [
  "",
  "#/",
  "#/inbox",
  "#/archive",
  "#/sent",
  "#/drafts",
  "#/trash",
  "#/junk",
  "#/starred",
  "#/priority",
  "#/needs-category",
  "#/snoozed",
  "#/unread",
  `#/label/${LBL}`,
  "#/label/",
  `#/mailbox/${M1}`,
  "#/mailbox/",
  `#/account/${ACC}`,
  "#/account/",
  "#/tab/primary",
  "#/tab/social",
  "#/tab/promotions",
  "#/tab/updates",
  "#/tab/forums",
  "#/tab/nonsense",
  "#/search/invoice",
  "#/search/from%3Aalice",
  "#/nonsense",
];

describe("every hash that worked before still means the same thing", () => {
  it("resolves to the View viewFromHash gives it", () => {
    for (const hash of LEGACY_HASHES) {
      expect(viewForRoute(parseRoute(hash)), hash).toEqual(viewFromHash(hash));
    }
  });

  it("parses as a view route — except the two hashes the new table claims", () => {
    for (const hash of LEGACY_HASHES) {
      // `#/inbox` (and the empty hash it is the default for) is the inbox PAGE
      // now: same URL, same View, different route shape.
      const claimed = hash === "" || hash === "#/" || hash === "#/inbox";
      expect(parseRoute(hash).page, hash).toBe(claimed ? "inbox" : "view");
    }
  });
});

// Day-one setup (#446). The wizard is the second surface with a URL and no query
// behind it, and the first one that must NOT become the remembered list: it is
// entered automatically on a fresh login, so a wrong answer here would follow the
// user around (Escape out of Settings landing back in the wizard) rather than
// showing up as a blank screen.
describe("#/setup — the day-one wizard", () => {
  it("spells step 1 as the bare hash and 2..4 as numbers", () => {
    expect(formatRoute({ page: "setup", step: 1 })).toBe("#/setup");
    expect(formatRoute({ page: "setup", step: 3 })).toBe("#/setup/3");
    expect(parseRoute("#/setup")).toEqual({ page: "setup", step: 1 });
    expect(parseRoute("#/setup/4")).toEqual({ page: "setup", step: 4 });
  });

  it("lands every unrecognised tail on step 1 rather than nowhere", () => {
    // Including `#/setup/1`, which is not a second spelling of step 1 — there is
    // one canonical string per route and it is the bare hash.
    for (const hash of ["#/setup/1", "#/setup/5", "#/setup/0", "#/setup/two", "#/setup/-1"]) {
      expect(parseRoute(hash), hash).toEqual({ page: "setup", step: 1 });
    }
  });

  it("carries the folder of an import, and drops a mangled one", () => {
    expect(parseRoute(`#/setup/import/${M1}`)).toEqual({
      page: "setup",
      step: 2,
      importMailboxId: M1,
    });
    // A folder id that cannot be an id lands on the WIZARD, not on an import of
    // nothing: step 2 with no folder would offer to act on the whole mailbox.
    expect(parseRoute("#/setup/import/has%20a%20space")).toEqual({ page: "setup", step: 1 });
    expect(parseRoute("#/setup/import")).toEqual({ page: "setup", step: 1 });
    // Import mode IS step 2, so the step never appears in the string beside it.
    expect(formatRoute({ page: "setup", step: 2, importMailboxId: M1 })).toBe(
      `#/setup/import/${M1}`,
    );
  });

  it("is not a list route, and keeps the list underneath it", () => {
    const previous: Route = { page: "view", view: { kind: "starred" } };
    expect(isListRoute({ page: "setup", step: 1 })).toBe(false);
    expect(viewForRoute({ page: "setup", step: 2 }, { previousListRoute: previous })).toEqual({
      kind: "starred",
    });
    // With no previous list it is the inbox — the desk "Skip setup" lands on, and
    // never a query of the wizard's own.
    expect(viewForRoute({ page: "setup", step: 1 })).toEqual({ kind: "role", role: "inbox" });
  });

  it("peels one step per press, and the desk out of step 1", () => {
    expect(escapeTarget({ page: "setup", step: 4 })).toEqual({ page: "setup", step: 3 });
    expect(escapeTarget({ page: "setup", step: 2, acct: ACC })).toEqual({
      page: "setup",
      step: 1,
      acct: ACC,
    });
    const previous: Route = { page: "view", view: { kind: "starred" } };
    expect(escapeTarget({ page: "setup", step: 1 }, previous)).toEqual(previous);
    expect(escapeTarget({ page: "setup", step: 1 })).toEqual(INBOX_ROUTE);
  });

  it("leaves an import for the row that opened it", () => {
    // Import mode has no step to go back to, and its own "Done" returns to
    // Settings → Areas and drawers: Escape agrees with the button rather than
    // dropping the user on a list they did not come from.
    expect(escapeTarget({ page: "setup", step: 2, importMailboxId: M1 })).toEqual({
      page: "settings",
      section: "areas",
    });
  });
});

describe("a hash from outside cannot take the app down", () => {
  it("lands an unknown hash on the inbox", () => {
    expect(parseRoute("#/not-a-route")).toEqual({
      page: "view",
      view: { kind: "role", role: "inbox" },
    });
    expect(viewForRoute(parseRoute("#/not-a-route"))).toEqual({ kind: "role", role: "inbox" });
    expect(parseRoute("")).toEqual(INBOX_ROUTE);
    expect(parseRoute("#")).toEqual(INBOX_ROUTE);
    expect(parseRoute("#/")).toEqual(INBOX_ROUTE);
  });

  it("drops a malformed id instead of carrying it into a query", () => {
    expect(parseRoute("#/inbox?m=has%20a%20space")).toEqual(INBOX_ROUTE);
    expect(parseRoute("#/inbox?m=")).toEqual(INBOX_ROUTE);
    expect(parseRoute(`#/inbox?m=${M1}&acct=a/b`)).toEqual({ ...INBOX_ROUTE, m: M1 });
    expect(parseRoute("#/inbox?compose=reply:")).toEqual(INBOX_ROUTE);
    expect(parseRoute("#/inbox?compose=sideways:x")).toEqual(INBOX_ROUTE);
    expect(parseRoute("#/inbox?compose=nonsense")).toEqual(INBOX_ROUTE);
    expect(parseRoute("#/inbox?compose=draft:")).toEqual(INBOX_ROUTE);
    expect(parseRoute("#/settings/not-a-section")).toEqual({ page: "settings", section: null });
    // An account id under a section that takes no id is not carried.
    expect(parseRoute(`#/settings/rules/${ACC}`)).toEqual({ page: "settings", section: "rules" });
  });

  // #374 increment 2. The three section ids v1.65 shipped are gone as ids and
  // alive as links: each parses to the ROW that absorbed its controls, so a
  // bookmark still lands on them. The round-trip test above deliberately does
  // not cover these — they are read-only, and formatRoute must emit the NEW id
  // (asserted below) rather than carrying a retired one forward.
  it("maps the retired section ids onto the rows that absorbed them", () => {
    expect(parseRoute("#/settings/remote-content")).toEqual({
      page: "settings",
      section: "reading",
    });
    expect(parseRoute("#/settings/snippets")).toEqual({
      page: "settings",
      section: "identities",
    });
    expect(parseRoute("#/settings/category-tabs")).toEqual({
      page: "settings",
      section: "areas",
    });
    // Read, never written: the canonical string for each is the new row.
    expect(formatRoute(parseRoute("#/settings/remote-content"))).toBe("#/settings/reading");
    expect(formatRoute(parseRoute("#/settings/snippets"))).toBe("#/settings/identities");
    expect(formatRoute(parseRoute("#/settings/category-tabs"))).toBe("#/settings/areas");
    // The shared parameters survive the rewrite — a legacy link that also named
    // an account scope must not be widened by being redirected.
    expect(parseRoute(`#/settings/snippets?acct=${ACC}`)).toEqual({
      page: "settings",
      section: "identities",
      acct: ACC,
    });
  });

  it("never throws, whatever it is handed", () => {
    const hostile = [
      "#/search/%E0%A4%A",
      "#/inbox?%=%",
      "#/inbox?m",
      "#/////",
      "#/m",
      "#/settings/areas/",
      "#/inbox/review/extra/segments",
      "?m=1",
      "#/all?archived=maybe",
    ];
    for (const hash of hostile) {
      expect(() => formatRoute(parseRoute(hash)), hash).not.toThrow();
    }
    expect(parseRoute("#/all?archived=maybe")).toEqual({ page: "all" });
    expect(parseRoute("#/inbox/review/extra/segments")).toEqual({ page: "inbox", tab: "review" });
  });
});

describe("routeKey — what list-position memory is keyed by", () => {
  it("ignores the open message and the composer, and nothing else", () => {
    const list: Route = { page: "inbox", tab: "review", acct: ACC };
    expect(routeKey({ ...list, m: M1 })).toBe(routeKey(list));
    expect(routeKey({ ...list, compose: { reply: M2 } })).toBe(routeKey(list));
    expect(routeKey({ ...list, m: M1, compose: "new" })).toBe(routeKey(list));
    // Opening a message is not arriving at a different list...
    expect(routeKey({ ...list, m: M1 })).toBe(`#/inbox/review?acct=${ACC}`);
    // ...but a different scope, tab or list IS.
    expect(routeKey({ page: "inbox", tab: "review" })).not.toBe(routeKey(list));
    expect(routeKey({ page: "inbox", tab: "rest", acct: ACC })).not.toBe(routeKey(list));
    expect(routeKey({ page: "tray" })).not.toBe(routeKey({ page: "all" }));
  });
});

describe("viewForRoute — the one table onto the query layer", () => {
  const cases: [Route, View][] = [
    [{ page: "inbox", tab: null }, { kind: "role", role: "inbox" }],
    [{ page: "inbox", tab: "rest" }, { kind: "role", role: "inbox" }],
    [{ page: "inbox", tab: "review" }, { kind: "priority" }],
    [{ page: "inbox", tab: null, sort: true }, { kind: "needsCategory" }],
    [{ page: "tray" }, { kind: "starred" }],
    [{ page: "all" }, { kind: "all" }],
    [{ page: "all", archived: true }, { kind: "all" }],
    [{ page: "all", label: LBL }, { kind: "all", labelId: LBL }],
    [{ page: "all", area: AREA }, { kind: "all", areaId: AREA }],
    [{ page: "all", area: AREA, label: LBL }, { kind: "all", areaId: AREA, labelId: LBL }],
    [{ page: "view", view: { kind: "unread" } }, { kind: "unread" }],
  ];

  it("maps every route form onto the view it shows", () => {
    for (const [route, view] of cases) {
      expect(viewForRoute(route), formatRoute(route)).toEqual(view);
    }
  });

  it("shows an area by its own claim, not by its label (#375)", () => {
    // `?area=` used to be resolved through the area OWN LABEL, via a
    // RouteContext hook, because there was no view that could ask for an area.
    // `inArea` is a strictly better question: it also holds the mail every
    // OTHER feed resolved into the area, which the own label does not carry.
    expect(viewForRoute({ page: "all", area: AREA })).toEqual({ kind: "all", areaId: AREA });
    // And it no longer falls back to the inbox when the area cannot be
    // resolved: there is nothing left to resolve.
    expect(viewForRoute({ page: "all", area: AREA })).not.toEqual({ kind: "role", role: "inbox" });
  });

  it("makes #/all a real all-mail view rather than the archive", () => {
    // The archive is where mail GOES; all mail is where mail IS. `?archived=1`
    // still parses — it was this hash original meaning — and lands on the same
    // view, because archived mail is in all mail.
    expect(viewForRoute(parseRoute("#/all"))).toEqual({ kind: "all" });
    expect(viewForRoute(parseRoute("#/all?archived=1"))).toEqual({ kind: "all" });
    expect(viewForRoute(parseRoute("#/all"))).not.toEqual({ kind: "role", role: "archive" });
  });

  it("keeps the list beneath settings, and falls back to the inbox with none", () => {
    const previous: Route = { page: "view", view: { kind: "starred" } };
    expect(viewForRoute({ page: "settings", section: "rules" }, { previousListRoute: previous }))
      .toEqual({ kind: "starred" });
    expect(viewForRoute({ page: "settings", section: null })).toEqual({
      kind: "role",
      role: "inbox",
    });
    // A settings route can never be its own "previous", so this cannot recur.
    expect(
      viewForRoute(
        { page: "settings", section: null },
        { previousListRoute: { page: "settings", section: "rules" } },
      ),
    ).toEqual({ kind: "role", role: "inbox" });
  });

  it("narrows EVERY list route to the account, not only the plain ones (#375)", () => {
    // This is the behaviour that changed. `acct` used to be honoured only where
    // nothing else narrowed the list, because a per-account view WAS the search
    // `account:<id>` and the server has no filter operators to intersect it
    // with a role, tab or label. `inAccount` is a structured condition on the
    // same flat filter, so the scope now ANDs with whatever else the route
    // asks for — which is what makes the account switcher mean one thing.
    expect(viewForRoute({ page: "inbox", tab: null, acct: ACC })).toEqual({
      kind: "role",
      role: "inbox",
      accountId: ACC,
    });
    expect(viewForRoute({ page: "inbox", tab: "rest", acct: ACC })).toEqual({
      kind: "role",
      role: "inbox",
      accountId: ACC,
    });
    expect(viewForRoute({ page: "inbox", tab: "review", acct: ACC })).toEqual({
      kind: "priority",
      accountId: ACC,
    });
    expect(viewForRoute({ page: "inbox", tab: null, sort: true, acct: ACC })).toEqual({
      kind: "needsCategory",
      accountId: ACC,
    });
    expect(viewForRoute({ page: "tray", acct: ACC })).toEqual({
      kind: "starred",
      accountId: ACC,
    });
    expect(viewForRoute({ page: "all", acct: ACC })).toEqual({ kind: "all", accountId: ACC });
    expect(viewForRoute({ page: "all", label: LBL, acct: ACC })).toEqual({
      kind: "all",
      labelId: LBL,
      accountId: ACC,
    });
    expect(viewForRoute({ page: "view", view: { kind: "starred" }, acct: ACC })).toEqual({
      kind: "starred",
      accountId: ACC,
    });
    expect(viewForRoute({ page: "view", view: { kind: "tab", tab: "social" }, acct: ACC })).toEqual({
      kind: "tab",
      tab: "social",
      accountId: ACC,
    });
  });

  it("leaves a folder and a search unscoped — both already say which account", () => {
    // A mailbox belongs to exactly one account, so `inMailbox` + `inAccount` is
    // redundant at best and empty at worst; a search carries its own
    // `account:` operator, which is the syntax the user typed. On both, `acct`
    // rides along as scope memory without changing the question.
    expect(
      viewForRoute({ page: "view", view: { kind: "mailbox", mailboxId: M1 }, acct: ACC }),
    ).toEqual({ kind: "mailbox", mailboxId: M1 });
    expect(viewForRoute({ page: "view", view: { kind: "search", query: "x" }, acct: ACC })).toEqual({
      kind: "search",
      query: "x",
    });
    // And the legacy per-account view is untouched — a bookmark still works.
    expect(viewForRoute({ page: "view", view: { kind: "account", accountId: ACC } })).toEqual({
      kind: "account",
      accountId: ACC,
    });
  });

  it("round-trips a scoped view back through viewRoute without widening it", () => {
    // The bridge every component that still speaks Views navigates through. A
    // scope it dropped here would silently widen the list the user is on, and
    // an all-mail view formatted through `viewToHash` alone would lose its
    // area and label — both are query parameters of the PAGE.
    const routes: Route[] = [
      { page: "all", area: AREA, acct: ACC },
      { page: "all", label: LBL },
      { page: "view", view: { kind: "starred" }, acct: ACC },
      // `#/inbox` is the documented canonicalisation exception above — the role
      // view and the inbox PAGE are the same URL — so it is checked for the
      // view it resolves to, not for the route shape it comes back as.
      { page: "inbox", tab: null, acct: ACC },
    ];
    for (const route of routes) {
      const view = viewForRoute(route);
      const back = viewRoute(view);
      expect(viewForRoute(back), formatRoute(route)).toEqual(view);
      expect(viewForRoute(parseRoute(formatRoute(back))), formatRoute(back)).toEqual(view);
    }
  });
});

describe("escapeTarget — one layer per press, outermost first", () => {
  const previous: Route = { page: "view", view: { kind: "starred" } };

  it("peels the composer before the message before the section", () => {
    const deep: Route = {
      page: "settings",
      section: "rules",
      m: M1,
      compose: { reply: M2 },
    };
    const noCompose = escapeTarget(deep) as Route;
    expect(noCompose).toEqual({ page: "settings", section: "rules", m: M1 });
    const noMessage = escapeTarget(noCompose) as Route;
    expect(noMessage).toEqual({ page: "settings", section: "rules" });
    const noSection = escapeTarget(noMessage) as Route;
    expect(noSection).toEqual({ page: "settings", section: null });
    expect(escapeTarget(noSection, previous)).toEqual(previous);
  });

  it("returns to the inbox from settings when no list is supplied", () => {
    expect(escapeTarget({ page: "settings", section: null })).toEqual(INBOX_ROUTE);
  });

  it("peels one ACCOUNT before its row, and the row before settings (#374)", () => {
    // A settings detail is a screen inside a row. Escaping from inside one account
    // has to land on the list of accounts — jumping two layers to the bare settings
    // page is what it did while the segment parsed and nothing rendered it.
    const inside: Route = { page: "settings", section: "accounts", accountId: ACC };
    const list = escapeTarget(inside) as Route;
    expect(list).toEqual({ page: "settings", section: "accounts" });
    expect(escapeTarget(list)).toEqual({ page: "settings", section: null });
  });

  it("peels one AREA the same way, keeping the scope", () => {
    expect(
      escapeTarget({ page: "settings", section: "areas", areaId: LBL, acct: ACC }),
    ).toEqual({ page: "settings", section: "areas", acct: ACC });
  });

  it("peels the open message before the account detail", () => {
    // Outermost first: a message opened over a settings detail is the newer layer.
    const deep: Route = { page: "settings", section: "accounts", accountId: ACC, m: M1 };
    expect(escapeTarget(deep)).toEqual({
      page: "settings",
      section: "accounts",
      accountId: ACC,
    });
  });

  it("keeps the scope while backing out", () => {
    expect(escapeTarget({ page: "settings", section: "rules", acct: ACC })).toEqual({
      page: "settings",
      section: null,
      acct: ACC,
    });
    expect(escapeTarget({ page: "inbox", tab: null, sort: true, acct: ACC })).toEqual({
      page: "inbox",
      tab: null,
      acct: ACC,
    });
  });

  it("leaves the sort queue for the inbox", () => {
    expect(escapeTarget({ page: "inbox", tab: null, sort: true })).toEqual(INBOX_ROUTE);
  });

  it("has nothing to back out of on a bare list", () => {
    expect(escapeTarget({ page: "inbox", tab: null })).toBeNull();
    expect(escapeTarget({ page: "inbox", tab: "review" })).toBeNull();
    expect(escapeTarget({ page: "tray" })).toBeNull();
    expect(escapeTarget({ page: "all", label: LBL })).toBeNull();
    expect(escapeTarget({ page: "view", view: { kind: "starred" } })).toBeNull();
  });
});

describe("listRouteOf — what the settings ✕ closes to, in ONE step", () => {
  // Operator report against the v1.80.0 beta: closing Settings from inside a
  // section or a detail took one click PER LAYER, because SettingsRoute's ✕ used
  // to call `leaveTo(listRoute)` (app/routing.ts), which undoes exactly one
  // pushed history entry — right for a single-layer sheet, wrong for settings,
  // where entering the page, opening a section and opening a detail each push
  // their own entry. `listRouteOf` is what the ✕ now calls instead: unlike
  // `escapeTarget`, which peels ONE layer per press and is what Escape/Back still
  // do, this returns the list underneath in a single step for any depth.
  const listRoute: Route = { page: "view", view: { kind: "starred" } };

  it("returns the list directly from the bare settings root", () => {
    expect(listRouteOf({ page: "settings", section: null }, listRoute)).toEqual(listRoute);
  });

  it("returns the list directly from an open section — not the settings root", () => {
    // escapeTarget would answer `{page:"settings",section:null}` here (one
    // layer); the ✕ must not settle for that.
    const deep: Route = { page: "settings", section: "appearance" };
    expect(listRouteOf(deep, listRoute)).toEqual(listRoute);
    expect(listRouteOf(deep, listRoute)).not.toEqual(escapeTarget(deep, listRoute));
  });

  it("returns the list directly from a settings DETAIL (account/area), skipping every layer", () => {
    const deep: Route = { page: "settings", section: "accounts", accountId: ACC };
    expect(listRouteOf(deep, listRoute)).toEqual(listRoute);
  });

  it("returns the list directly even with a message or composer open over settings", () => {
    const deep: Route = {
      page: "settings",
      section: "rules",
      m: M1,
      compose: { reply: M2 },
    };
    expect(listRouteOf(deep, listRoute)).toEqual(listRoute);
  });

  it("falls back to the inbox when no list route is known", () => {
    expect(listRouteOf({ page: "settings", section: "areas" })).toEqual(INBOX_ROUTE);
  });
});

describe("legacyRedirect — the one hash that has to be rewritten", () => {
  const last: Route = { page: "view", view: { kind: "starred" } };

  it("opens #/m/<id> on the list the user was last on", () => {
    expect(legacyRedirect(`#/m/${M1}`, last)).toEqual({ ...last, m: M1 });
    expect(legacyRedirect(`#/m/${M1}`)).toEqual({ ...INBOX_ROUTE, m: M1 });
  });

  it("drops a malformed id rather than putting it in the URL", () => {
    expect(legacyRedirect("#/m/", last)).toEqual(last);
    expect(legacyRedirect("#/m/not a ulid", last)).toEqual(last);
  });

  it("is null for every other hash, including the ones that look close", () => {
    expect(legacyRedirect("#/inbox", last)).toBeNull();
    expect(legacyRedirect(`#/mailbox/${M1}`, last)).toBeNull();
    expect(legacyRedirect("", last)).toBeNull();
    expect(legacyRedirect(`#/inbox?m=${M1}`, last)).toBeNull();
  });

  it("never returns a settings route to open a message on", () => {
    expect(legacyRedirect(`#/m/${M1}`, { page: "settings", section: "rules" })).toEqual({
      ...INBOX_ROUTE,
      m: M1,
    });
  });
});
