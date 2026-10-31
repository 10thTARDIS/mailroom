// SPDX-License-Identifier: AGPL-3.0-only

// THE TWO-KEY "GO TO" SEQUENCES (Gmail: `g` then a second key).
//
// This was a `Record<string, View>` inside Shell.tsx, which was exactly right
// while every destination was a view. #375 adds two that are not: `g g` opens
// the account switcher, and `g 0`–`g 9` change the account SCOPE — which is not
// a place, it is a narrowing of wherever you already are.
//
// A view table cannot express either, and the tempting shortcut — inventing a
// `{kind:"account"}` view for the digits — would be wrong twice over: it would
// navigate AWAY from the list the user is on, and it would use the legacy
// search-backed per-account view rather than the scope the switcher writes. So
// the second key resolves to a TARGET, of which a view is one kind, and Shell
// does the one thing each kind means.
//
// Pure, and here rather than in Shell, so the claim the keys sheet makes can be
// tested: "`g 3` with two accounts does nothing" is a sentence about behaviour,
// and behaviour that lives inside a keydown closure cannot be asserted without
// a DOM.

import type { Id } from "../../core/jmap/types";
import type { View } from "../../core/views";

/** The letter destinations. A table rather than a switch so the help sheet's
 * claim — "g then i/p/n/s/b/e/d/t/a" — is one list that can be read at a
 * glance, and so this module can be diffed against that sheet. */
export const GOTO_VIEWS: Record<string, View> = {
  i: { kind: "role", role: "inbox" },
  s: { kind: "starred" },
  b: { kind: "snoozed" },
  p: { kind: "priority" },
  n: { kind: "needsCategory" },
  t: { kind: "role", role: "trash" },
  d: { kind: "role", role: "drafts" },
  a: { kind: "role", role: "archive" },
  e: { kind: "role", role: "sent" },
};

/** What the second key of a `g` sequence asks for. */
export type GotoTarget =
  /** Go to a view — the letters. The caller carries the current account scope
   * onto it, because `g s` from one account's desk means that account's
   * starred mail, not everybody's. */
  | { kind: "view"; view: View }
  /** Narrow (or widen) the account scope without leaving the list: `g 0` is all
   * accounts, `g 1`–`g 9` the Nth. `acct: null` IS the widening — it is a
   * choice, not an absence. */
  | { kind: "scope"; acct: Id | null }
  /** `g g` — open the switcher and let the user look. The mnemonic is the one
   * doubled key people already type by accident when they mean "the go-to
   * thing", and the switcher is the menu of everywhere that goes. */
  | { kind: "switcher" }
  /**
   * `g u` — put focus on the undo offer that is on screen (#447).
   *
   * NOT A PLACE, and it is here anyway for the same reason `g g` is: the `g`
   * prefix is this app's convention for "take me to", and a toast pinned to the
   * bottom of the document is the hardest thing in the app to take yourself to.
   * Tabbing to it is a hunt through every control on the page, and the offer
   * expires in eight seconds.
   *
   * ⚠ IT IS NOT A SECOND UNDO. Ctrl/⌘+Z runs the latest offer from wherever focus
   * is and is the fast path; this one moves FOCUS to the button, which is what
   * lets a reader hear the sentence, decide, and press it — and which pauses the
   * countdown while they do (ui/Toasts.tsx). A key that ran the undo would be a
   * duplicate; a key that only reads it out would leave the button unreachable.
   *
   * `u` alone is taken (it closes the reading pane, ui/Shell.tsx), which is what
   * makes this a `g` sequence rather than a letter.
   */
  | { kind: "undo" };

/**
 * The target for a second key, or null when there is none.
 *
 * NULL IS NOT "DO NOTHING QUIETLY BY MISTAKE" — the caller consumes the key
 * either way (a mistyped sequence must not also archive something), and this
 * saying null is what makes `g 7` with two accounts a no-op rather than a jump
 * to whatever the seventh row of some other list is. An out-of-range digit is
 * the case worth naming: a user with two accounts pressing `g 3` has asked for
 * an account that does not exist, and the only honest answers are "nothing" or
 * an error nobody wants for a keystroke.
 *
 * `accounts` is the switcher's own order (ui/useAccounts.ts sorts by id), so the
 * Nth key and the Nth row of the list are the same account by construction.
 */
export function gotoTarget(secondKey: string, accounts: readonly Id[]): GotoTarget | null {
  const key = secondKey.toLowerCase();
  if (key === "g") return { kind: "switcher" };
  if (key === "u") return { kind: "undo" };
  if (/^[0-9]$/.test(key)) {
    const n = Number(key);
    if (n === 0) return { kind: "scope", acct: null };
    const acct = accounts[n - 1];
    return acct ? { kind: "scope", acct } : null;
  }
  const view = GOTO_VIEWS[key];
  return view ? { kind: "view", view } : null;
}
