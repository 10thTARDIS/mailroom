// SPDX-License-Identifier: AGPL-3.0-only

// WHETHER TO OPEN THE DAY-ONE WIZARD ON ITS OWN (#446).
//
// This is the one decision in the feature that happens WITHOUT the user asking
// for anything, which is why it is a pure function with a test rather than four
// conditions inside an effect. Getting it wrong is not a cosmetic bug in either
// direction:
//
//   too eager  — a wizard over somebody's mail on a login that is already set
//                up, or before the mailboxes have loaded (which would offer to
//                act on a folder list that is simply not there yet, and say
//                "Found 0 messages in 0 folders" while doing it, #176);
//   too shy    — a fresh login lands on an empty desk with no folders feeding
//                it and no explanation, which is the whole problem #446 exists
//                to solve.
//
// It answers only "would the wizard be right HERE, NOW". Doing it ONCE per
// session is the caller's half (ui/Shell.tsx + setupStorage.ts): a person who
// skipped by navigating away must not be dragged back on the next render, and
// that is a memory, not a predicate.

import type { DeskPreferences } from "../../../core/jmap/types";
import type { Route } from "../../../core/routes";

export interface SetupGate {
  /**
   * The login's desk record, or null before it has loaded.
   *
   * ⚠ `null` HERE IS "NOT READ YET", NOT "NOT SET UP". The wizard opens on
   * `setupCompletedAt === null`, which is a fact this client has to have READ —
   * a record that has not arrived says nothing at all, and opening on its
   * absence would put a wizard in front of every user on every cold start for
   * as long as the first round trip takes.
   */
  prefs: DeskPreferences | null;
  /**
   * How many mail accounts this login has.
   *
   * Zero means there is nothing to census: the folders belong to accounts, and
   * a wizard whose first sentence counts folders has nothing to count. On a
   * brand-new login this is the state right before the add-account wizard is
   * the thing that should be on screen.
   */
  accounts: number;
  /**
   * Whether the mailbox list has landed. Not "is it non-empty" — see the
   * caller — but "has the reference-data load answered", because the census
   * sentence is made of counts and an empty folder map before the load is a
   * load in progress rather than a mailbox with no folders.
   */
  mailboxesLoaded: boolean;
  /** Where the app currently is. */
  route: Route;
}

/**
 * The route the app boots on, in the only sense that matters here: the inbox,
 * with nothing on top of it.
 *
 * The TAB and the ACCOUNT SCOPE are deliberately not part of it — both are
 * applied from this device's memory before the first paint (app/routing.ts
 * applyRemembered), so requiring the bare `#/inbox` would mean the wizard never
 * opened for anyone who had ever used the review tab. What DOES disqualify the
 * route is anything the user has opened on top: a message they are reading, a
 * composer they are typing in. Neither is a moment to take the screen away.
 */
function isBootInboxRoute(route: Route): boolean {
  return route.page === "inbox" && !route.sort && !route.m && !route.compose;
}

/**
 * Whether the day-one wizard should open itself.
 *
 * All four conditions, and every one of them is a "we know" rather than a "we
 * assume": the preferences have been read and say the login has never been set
 * up, at least one account exists, the folder list has landed, and the user is
 * on the surface the app boots to rather than somewhere they navigated.
 */
export function shouldOpenSetup(gate: SetupGate): boolean {
  const { prefs, accounts, mailboxesLoaded, route } = gate;
  if (prefs === null) return false;
  // Strict `=== null`: a server that predates #446 sends no such property at
  // all, and `undefined` must NOT open a wizard against a server with no
  // `Setup/*` methods behind it. Fail closed — the user keeps their desk.
  if (prefs.setupCompletedAt !== null) return false;
  if (accounts < 1) return false;
  if (!mailboxesLoaded) return false;
  return isBootInboxRoute(route);
}
