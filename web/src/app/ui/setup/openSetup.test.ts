// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { shouldOpenSetup } from "./openSetup";
import type { SetupGate } from "./openSetup";
import type { DeskPreferences } from "../../../core/jmap/types";

// The one decision in day-one that happens without the user asking (#446). Both
// directions are tested, because both are real damage: a wizard over a mailbox
// that is already set up, or a fresh login left on an empty desk with no
// explanation.

const M1 = "01JBQZ3K7WX2N4Y6P8R0S1T2V3";

function prefs(over: Partial<DeskPreferences> = {}): DeskPreferences {
  return {
    id: "01JBQZ3K7WX2N4Y6P8R0S1T2U0",
    lineThreshold: 0.5,
    drawerDefaultDays: 30,
    trayNagDays: 7,
    drawerSweepEnabled: false,
    setupCompletedAt: null,
    ...over,
  };
}

/** A login that has never been set up, on the route the app boots to. */
function gate(over: Partial<SetupGate> = {}): SetupGate {
  return {
    prefs: prefs(),
    accounts: 1,
    mailboxesLoaded: true,
    route: { page: "inbox", tab: null },
    ...over,
  };
}

describe("shouldOpenSetup", () => {
  it("opens for a login that has never been set up", () => {
    expect(shouldOpenSetup(gate())).toBe(true);
  });

  it("does not open once setup has been completed", () => {
    expect(shouldOpenSetup(gate({ prefs: prefs({ setupCompletedAt: "2026-09-01T10:00:00Z" }) }))).toBe(
      false,
    );
  });

  it("does not open before the preferences have been read", () => {
    // null prefs is "not read yet", NOT "not set up": opening on the absence of
    // the record would put a wizard in front of every user on every cold start.
    expect(shouldOpenSetup(gate({ prefs: null }))).toBe(false);
  });

  it("does not open on a server that does not have the property at all", () => {
    // An older server sends no `setupCompletedAt`. `undefined` is not `null`, and
    // the gate fails closed rather than opening a wizard whose Setup/* methods
    // would every one of them answer unknownMethod.
    const older = { ...prefs(), setupCompletedAt: undefined } as unknown as DeskPreferences;
    expect(shouldOpenSetup(gate({ prefs: older }))).toBe(false);
  });

  it("does not open with no accounts, or before the folders have landed", () => {
    expect(shouldOpenSetup(gate({ accounts: 0 }))).toBe(false);
    expect(shouldOpenSetup(gate({ mailboxesLoaded: false }))).toBe(false);
  });

  it("opens on the remembered tab and scope, which are still the boot route", () => {
    // Both are applied from this device's memory before the first paint, so
    // requiring the bare `#/inbox` would mean the wizard never opened for
    // anybody who had ever used the review tab.
    expect(shouldOpenSetup(gate({ route: { page: "inbox", tab: "review" } }))).toBe(true);
    expect(shouldOpenSetup(gate({ route: { page: "inbox", tab: null, acct: M1 } }))).toBe(true);
  });

  it("does not take the screen away from something the user opened", () => {
    expect(shouldOpenSetup(gate({ route: { page: "inbox", tab: null, m: M1 } }))).toBe(false);
    expect(shouldOpenSetup(gate({ route: { page: "inbox", tab: null, compose: "new" } }))).toBe(
      false,
    );
    expect(shouldOpenSetup(gate({ route: { page: "inbox", tab: null, sort: true } }))).toBe(false);
    expect(shouldOpenSetup(gate({ route: { page: "tray" } }))).toBe(false);
    expect(shouldOpenSetup(gate({ route: { page: "settings", section: "areas" } }))).toBe(false);
    expect(shouldOpenSetup(gate({ route: { page: "all" } }))).toBe(false);
    // And never over itself: the wizard is already open.
    expect(shouldOpenSetup(gate({ route: { page: "setup", step: 2 } }))).toBe(false);
  });
});
