// SPDX-License-Identifier: AGPL-3.0-only

// HOW LONG AN AREA'S DRAWER KEEPS MAIL (#374 increment 3).
//
// One select on the card, seven options, and TWO stored fields behind them
// (`Area.autoArchive` and `Area.autoArchiveDays`). Three of the seven options map
// onto states a reader would not guess, which is why this mapping is a tested
// pure function rather than a switch inside a component:
//
//   never          autoArchive: false                  — "off" is ONLY ever this.
//                                                         A null period has never
//                                                         meant off (types.ts).
//   default (30)   autoArchive: true, days: null        — FOLLOWS the login default,
//                                                         so changing the login
//                                                         default moves this area.
//   7 / 14 / 30 /  autoArchive: true, days: N           — PINNED. Choosing "30"
//   90 / custom                                           when the login default
//                                                         is also 30 is not the
//                                                         same state as choosing
//                                                         "default": one tracks the
//                                                         default and one does not.
//
// ⚠ `never` DOES NOT SEND `autoArchiveDays`. Sending null alongside it would
// succeed and throw away the period the user had chosen, so re-enabling the sweep
// would silently land on the login default instead of where they left it. The
// patch type makes that omission explicit rather than leaving it to a caller to
// remember.
//
// ⚠ NONE OF THIS IS A PROMISE THAT THE SWEEP RUNS. `DeskPreferences.drawerSweepEnabled`
// is a login-level master switch that defaults to FALSE, and while it is off
// nothing sweeps whatever any area says (core/desk.ts drawerPeriodDays says the
// same from the other side). The row prints `sweep off` on every card in that
// case; this module answers only "what is this area's period".

import type { Area, DeskPreferences } from "../../../core/jmap/types";

/** The four choices the select offers, as states rather than as strings. `days`
 * covers 7 / 14 / 30 / 90 and the custom number alike — a custom 21 is not a
 * different KIND of setting from a preset 14, it is the same field with a number
 * the select happens not to list. */
export type DrawerPeriod =
  | { kind: "never" }
  | { kind: "default" }
  | { kind: "days"; days: number };

/** The days the select offers as presets. `custom` is anything else. */
export const DRAWER_PERIOD_PRESETS: readonly number[] = [7, 14, 30, 90];

/** The login default the server ships with, from `db/schema.sql`
 * (`drawer_default_days int NOT NULL DEFAULT 30`) — the number the Areas row's
 * `reset to 30` puts back. Hard-coded because the client cannot ask a server for
 * the value a FRESH login would have had; read from the actual preferences
 * whenever they are loaded, and used only as the reset target. */
export const DRAWER_DEFAULT_DAYS = 30;

/** The patch an `Area/set update` carries for a period change.
 *
 * `autoArchiveDays` is OPTIONAL and its absence is meaningful: "do not touch the
 * stored period" — see the header on `never`. */
export interface DrawerPeriodPatch {
  autoArchive: boolean;
  autoArchiveDays?: number | null;
}

/**
 * Which option an area is currently on.
 *
 * ⚠ IT DOES NOT TAKE THE PREFERENCES, and that is a deliberate narrowing of what
 * the design asked for. Whether an area FOLLOWS the login default is a property of
 * the area alone (`autoArchiveDays === null`); reading the preferences to decide
 * it would make the answer depend on a number that may not have loaded, and would
 * collapse "follows the default, which is 30" into "pinned at 30" — two different
 * states that behave differently the moment the login default moves. The
 * preferences are needed only to LABEL the option, which is
 * `drawerDefaultLabelDays` below and the caller's job.
 */
export function drawerPeriodChoice(area: Area): DrawerPeriod {
  if (!area.autoArchive) return { kind: "never" };
  if (area.autoArchiveDays === null) return { kind: "default" };
  return { kind: "days", days: area.autoArchiveDays };
}

/** The patch that puts an area on one option. */
export function drawerPeriodPatch(period: DrawerPeriod): DrawerPeriodPatch {
  switch (period.kind) {
    case "never":
      // No `autoArchiveDays` — see the header.
      return { autoArchive: false };
    case "default":
      return { autoArchive: true, autoArchiveDays: null };
    case "days":
      return { autoArchive: true, autoArchiveDays: period.days };
  }
}

/**
 * What the label of the login-default option says, given the preferences.
 *
 * Null when the preferences have not loaded: the option is still offered (it is a
 * state an area can be in whether or not this client has read the number), but it
 * is described without a number rather than with an invented one.
 */
export function drawerDefaultLabelDays(prefs: DeskPreferences | null): number | null {
  return prefs === null ? null : prefs.drawerDefaultDays;
}
