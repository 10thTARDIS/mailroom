// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import type { Route } from "../core/routes";
import { INBOX_ROUTE } from "../core/routes";
import {
  POSITION_LIMIT,
  applyRemembered,
  decideEscape,
  recallPosition,
  rememberPosition,
  rememberedFrom,
} from "./routing";
import type { PositionMap } from "./routing";

// The decisions routing makes, tested where they are decisions rather than
// where they are side effects (#373). A history stack cannot be inspected from
// script and sessionStorage is not the interesting part — what matters is that
// Escape and the back button agree, that returning to a list returns to the
// same row, and that a remembered scope never overrides a URL that names one.

const M1 = "01JBQZ3K7WX2N4Y6P8R0S1T2V3";
const ACC = "01JBQZ3K7WX2N4Y6P8R0S1T2AC";

describe("decideEscape — Escape and the back button do the same thing", () => {
  const withMessage: Route = { page: "inbox", tab: null, m: M1 };

  it("goes back when the previous entry is one we pushed", () => {
    expect(decideEscape(withMessage, 1)).toBe("back");
    expect(decideEscape(withMessage, 7)).toBe("back");
  });

  it("replaces when it is not — going back would leave the app", () => {
    // The deep-link case: a pasted `#/inbox?m=…` in a fresh tab has our route
    // as the FIRST entry, so history.back() would go to whatever the user was
    // looking at before Mailroom, or nowhere.
    expect(decideEscape(withMessage, 0)).toEqual({ replace: { page: "inbox", tab: null } });
  });

  it("says none when there is nothing to back out of, whatever the depth", () => {
    expect(decideEscape({ page: "inbox", tab: null }, 0)).toBe("none");
    expect(decideEscape({ page: "inbox", tab: null }, 5)).toBe("none");
    expect(decideEscape({ page: "view", view: { kind: "starred" } }, 3)).toBe("none");
  });

  it("peels one layer at a time, and settings returns to the list supplied", () => {
    const previous: Route = { page: "view", view: { kind: "starred" } };
    expect(decideEscape({ page: "settings", section: "rules" }, 0)).toEqual({
      replace: { page: "settings", section: null },
    });
    expect(decideEscape({ page: "settings", section: null }, 0, previous)).toEqual({
      replace: previous,
    });
    expect(decideEscape({ page: "settings", section: null }, 0)).toEqual({
      replace: INBOX_ROUTE,
    });
  });

  it("treats a nonsense depth as zero rather than backing out blind", () => {
    expect(decideEscape(withMessage, -3)).toEqual({ replace: { page: "inbox", tab: null } });
  });
});

describe("list position — coming back lands on the row you left", () => {
  it("remembers and recalls by key", () => {
    const map = rememberPosition({}, "#/inbox", { cursor: M1, scrollTop: 240 });
    expect(recallPosition(map, "#/inbox")).toEqual({ cursor: M1, scrollTop: 240 });
    expect(recallPosition(map, "#/tray")).toBeNull();
    expect(recallPosition({}, "#/inbox")).toBeNull();
  });

  it("does not mutate the map it was given", () => {
    const before: PositionMap = { "#/inbox": { cursor: null, scrollTop: 0 } };
    const after = rememberPosition(before, "#/tray", { cursor: M1, scrollTop: 10 });
    expect(before).toEqual({ "#/inbox": { cursor: null, scrollTop: 0 } });
    expect(Object.keys(after)).toEqual(["#/inbox", "#/tray"]);
  });

  it("overwrites a list rather than keeping two answers for it", () => {
    let map = rememberPosition({}, "#/inbox", { cursor: M1, scrollTop: 10 });
    map = rememberPosition(map, "#/tray", { cursor: null, scrollTop: 0 });
    map = rememberPosition(map, "#/inbox", { cursor: null, scrollTop: 90 });
    expect(recallPosition(map, "#/inbox")).toEqual({ cursor: null, scrollTop: 90 });
    // ...and re-visiting makes it the most recent, so the eviction below cannot
    // drop the list you are actually using.
    expect(Object.keys(map)).toEqual(["#/tray", "#/inbox"]);
  });

  it("evicts the least recently used rather than growing without bound", () => {
    let map: PositionMap = {};
    for (let i = 0; i < POSITION_LIMIT + 5; i += 1) {
      map = rememberPosition(map, `#/search/q${i}`, { cursor: null, scrollTop: i });
    }
    expect(Object.keys(map)).toHaveLength(POSITION_LIMIT);
    expect(recallPosition(map, "#/search/q0")).toBeNull();
    expect(recallPosition(map, "#/search/q4")).toBeNull();
    expect(recallPosition(map, `#/search/q${POSITION_LIMIT + 4}`)).toEqual({
      cursor: null,
      scrollTop: POSITION_LIMIT + 4,
    });
  });
});

describe("the last-used scope and tab, per device", () => {
  it("fills in what the URL does not say", () => {
    expect(applyRemembered({ page: "inbox", tab: null }, { tab: "review" })).toEqual({
      page: "inbox",
      tab: "review",
    });
    expect(applyRemembered({ page: "inbox", tab: null }, { acct: ACC })).toEqual({
      page: "inbox",
      tab: null,
      acct: ACC,
    });
    expect(applyRemembered({ page: "inbox", tab: null }, { tab: "rest", acct: ACC })).toEqual({
      page: "inbox",
      tab: "rest",
      acct: ACC,
    });
    expect(applyRemembered({ page: "all" }, { acct: ACC })).toEqual({ page: "all", acct: ACC });
    // The tray is one of the desk's three pages since #375, so it adopts the
    // remembered scope like the other two.
    expect(applyRemembered({ page: "tray" }, { acct: ACC })).toEqual({ page: "tray", acct: ACC });
  });

  it("never overrides a URL that names its own — a pasted link means what it says", () => {
    expect(applyRemembered({ page: "inbox", tab: "review" }, { tab: "rest" })).toBeNull();
    expect(applyRemembered({ page: "inbox", tab: null, acct: ACC }, { acct: "other" })).toBeNull();
    expect(applyRemembered({ page: "inbox", tab: "rest", acct: ACC }, { tab: "review" })).toBeNull();
  });

  it("applies once and then leaves the route alone", () => {
    const first = applyRemembered({ page: "inbox", tab: null }, { tab: "review", acct: ACC });
    expect(first).not.toBeNull();
    // The replace this causes re-runs the same check against the new route; a
    // second answer here would be an infinite redirect.
    expect(applyRemembered(first as Route, { tab: "review", acct: ACC })).toBeNull();
  });

  it("never gives the sort queue a TAB — it is not one of the inbox's halves", () => {
    expect(applyRemembered({ page: "inbox", tab: null, sort: true }, { tab: "review" })).toBeNull();
    // ...though it does adopt the scope, like every other inbox route: the
    // rail counts this queue from the SCOPED summary, so an unscoped queue
    // would sit under a badge counting something else.
    expect(applyRemembered({ page: "inbox", tab: null, sort: true }, { acct: ACC })).toEqual({
      page: "inbox",
      tab: null,
      sort: true,
      acct: ACC,
    });
  });

  it("leaves a legacy view hash and every non-list route exactly as written", () => {
    // `acct` narrows these now (core/routes.ts), but they are links people
    // paste, and the rail and the `g` keys carry the scope onto them
    // explicitly. Silently rewriting someone's URL to add a filter they did not
    // ask for is the one thing scope memory must not do.
    expect(
      applyRemembered({ page: "view", view: { kind: "starred" } }, { tab: "review", acct: ACC }),
    ).toBeNull();
    expect(applyRemembered({ page: "settings", section: null }, { acct: ACC })).toBeNull();
  });

  it("does nothing when nothing is remembered", () => {
    expect(applyRemembered({ page: "inbox", tab: null }, {})).toBeNull();
    expect(applyRemembered({ page: "all" }, {})).toBeNull();
  });

  it("records only what a route SAYS", () => {
    expect(rememberedFrom({ page: "inbox", tab: "review" }, {})).toEqual({ tab: "review" });
    expect(rememberedFrom({ page: "inbox", tab: null, acct: ACC }, {})).toEqual({ acct: ACC });
    // A route with no scope is "wherever I was", not "all accounts" — it must
    // not silently clear a memory the user never asked to clear.
    expect(rememberedFrom({ page: "tray" }, { tab: "rest", acct: ACC })).toEqual({
      tab: "rest",
      acct: ACC,
    });
    expect(rememberedFrom({ page: "inbox", tab: null }, { tab: "review" })).toEqual({
      tab: "review",
    });
  });
});
