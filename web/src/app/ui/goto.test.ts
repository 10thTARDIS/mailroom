// SPDX-License-Identifier: AGPL-3.0-only

// The `g` sequences (#375 increment 4).
//
// What is worth testing is the two cases that are not "a letter maps to a
// view": a DIGIT names an account by position, and asking for an account that
// does not exist must do nothing rather than something. `g 3` with two accounts
// is the one a user will actually hit — the keys sheet says `g 1`–`g 9` — and
// the wrong answers available to it are all bad ones: switching to the last
// account, switching to the first, or leaving the scope half-applied.

import { describe, expect, it } from "vitest";

import { gotoTarget } from "./goto";

const A1 = "01JBQZ3K7WX2N4Y6P8R0S1T2A1";
const A2 = "01JBQZ3K7WX2N4Y6P8R0S1T2A2";
const TWO = [A1, A2];

describe("gotoTarget", () => {
  it("makes `g 0` all accounts — a choice, not an absence", () => {
    // `acct: null` IS the widening, and the caller writes it to the per-device
    // memory outright, because "all accounts" cannot be read back off a route
    // that simply carries no account.
    expect(gotoTarget("0", TWO)).toEqual({ kind: "scope", acct: null });
    // ...and it works with no accounts at all, which is what a fresh login is.
    expect(gotoTarget("0", [])).toEqual({ kind: "scope", acct: null });
  });

  it("makes `g 1`/`g 2` the Nth account, in the switcher's own order", () => {
    expect(gotoTarget("1", TWO)).toEqual({ kind: "scope", acct: A1 });
    expect(gotoTarget("2", TWO)).toEqual({ kind: "scope", acct: A2 });
  });

  it("does NOTHING for a digit past the end", () => {
    // The wrong answers here are all worse than nothing: clamping to the last
    // account would switch scope to an account the user did not name.
    expect(gotoTarget("3", TWO)).toBeNull();
    expect(gotoTarget("9", TWO)).toBeNull();
    expect(gotoTarget("1", [])).toBeNull();
  });

  it("opens the switcher on `g g`", () => {
    expect(gotoTarget("g", TWO)).toEqual({ kind: "switcher" });
    expect(gotoTarget("G", TWO)).toEqual({ kind: "switcher" });
  });

  it("reaches the undo toast on `g u` (#447)", () => {
    // Not a view and not a scope: the third kind of answer this table has, for
    // the one control in the app that is unreachable by Tab in the time it
    // exists.
    expect(gotoTarget("u", TWO)).toEqual({ kind: "undo" });
    expect(gotoTarget("U", TWO)).toEqual({ kind: "undo" });
  });

  it("still resolves every letter destination the sheet promises", () => {
    expect(gotoTarget("i", TWO)).toEqual({ kind: "view", view: { kind: "role", role: "inbox" } });
    expect(gotoTarget("s", TWO)).toEqual({ kind: "view", view: { kind: "starred" } });
    expect(gotoTarget("p", TWO)).toEqual({ kind: "view", view: { kind: "priority" } });
    expect(gotoTarget("n", TWO)).toEqual({ kind: "view", view: { kind: "needsCategory" } });
    expect(gotoTarget("b", TWO)).toEqual({ kind: "view", view: { kind: "snoozed" } });
    expect(gotoTarget("e", TWO)).toEqual({ kind: "view", view: { kind: "role", role: "sent" } });
    expect(gotoTarget("d", TWO)).toEqual({ kind: "view", view: { kind: "role", role: "drafts" } });
    expect(gotoTarget("t", TWO)).toEqual({ kind: "view", view: { kind: "role", role: "trash" } });
    expect(gotoTarget("a", TWO)).toEqual({ kind: "view", view: { kind: "role", role: "archive" } });
    // Case-insensitive, because a held Shift must not silently do nothing.
    expect(gotoTarget("I", TWO)).toEqual({ kind: "view", view: { kind: "role", role: "inbox" } });
  });

  it("is null for anything else — a mistyped sequence does not also do something", () => {
    expect(gotoTarget("z", TWO)).toBeNull();
    expect(gotoTarget("Escape", TWO)).toBeNull();
    expect(gotoTarget("", TWO)).toBeNull();
    expect(gotoTarget(" ", TWO)).toBeNull();
  });
});
