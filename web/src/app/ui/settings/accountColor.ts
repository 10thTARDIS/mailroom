// SPDX-License-Identifier: AGPL-3.0-only

// WHAT COLOUR AN ACCOUNT'S DOT IS (#374 increment 3).
//
// Until now the dot was an ORDINAL: the first account got `--acct-1`, the second
// `--acct-2`, and a third got the first one again (ui/useAccounts.ts TONES).
// That is fine as a cue and useless as an identity — the colour moved when an
// account was added, so a user could not learn "the orange one is work". The
// server has always had somewhere to put the answer (`Account.color`, a
// `#RRGGBB` string, internal/api/account.go) and nothing wrote it. This module
// is the decision that reads it.
//
// TWO RULES, AND THE SECOND IS THE ONE THAT MATTERS.
//
//   1. A CHOSEN COLOUR WINS OVER THE ORDINAL. That is the whole point.
//   2. ⚠ PAUSED WINS OVER A CHOSEN COLOUR. `--acct-off` is not a decoration, it
//      is how the rail says "this account is not syncing" (useAccounts.ts "PAUSED
//      WINS OVER FAILING" draws the same line one step earlier). An account the
//      user tinted orange and then paused must not go on looking live because the
//      tint outranked the state. So a paused account reports `color: null` and
//      keeps the `off` token, whatever it holds.
//
// THE TOKEN IS ALWAYS REPORTED, even when a colour is. The caller renders the
// class AND the inline style, so the class is the fallback if the style is ever
// dropped and there is never a dot with no colour at all.
//
// A MALFORMED COLOUR IS REFUSED HERE, not sent to the DOM. The server only
// accepts `#RRGGBB` (validHexColor in account.go) so anything else cannot have
// come from a write this client made — but a value read back from a server is
// input, and a string headed for a `style` attribute is the wrong place to find
// that out. Refusing it falls back to the ordinal, which is what the dot looked
// like before this feature existed.

import type { AccountTone } from "../desk/DeskRow";

/** `#RRGGBB`, case-insensitive on the hex digits — the server's own rule
 * (internal/api/account.go validHexColor). Three-digit shorthand is NOT accepted,
 * because the server would refuse it on the way back. */
export function isHexColor(value: string): boolean {
  return /^#[0-9a-fA-F]{6}$/.test(value);
}

/**
 * The swatches the picker offers.
 *
 * ⚠ HARD-CODED HEXES, TAKEN FROM tokens.css's LIGHT THEME — `--acct-1`,
 * `--acct-2`, `--label-1`, `--label-2`. They cannot be `var(--acct-1)`: a token
 * resolves per theme and `Account.color` is ONE stored string, so whatever is
 * picked here is the same colour in both themes and a token reference would only
 * hide that.
 *
 * ⚠ FOUR, NOT SIX, AND THAT IS THE FILE'S FAULT RATHER THAN A CHOICE. The six
 * tokens the design named hold four distinct colours: tokens.css sets
 * `--label-3` to `#B5652A` (exactly `--acct-2`) and `--label-4` to `#3B6FB6`
 * (exactly `--acct-1`) — it says so itself at the `@contrast-known-low` line for
 * `label-3`. Offering six swatches would mean two pairs of identical ones, which
 * reads as a broken picker; inventing two more hexes would mean a palette
 * defined outside tokens.css, which is the one thing #376 forbids. So: four
 * named swatches and a native colour input for everything else.
 *
 * CONTRAST, MEASURED against `--bg` in both themes (WCAG 1.4.11 wants 3:1 for a
 * non-text graphic):
 *
 *   #3B6FB6  light 5.08   dark 3.47
 *   #B5652A  light 4.32   dark 4.08
 *   #2E6F62  light 5.89   dark 2.99
 *   #7A4E9C  light 6.18   dark 2.85
 *
 * The last two sit just under 3:1 on the dark background, and they are offered
 * anyway for the reason tokens.css already accepts `acct-2` at 4.32 on light: the
 * dot is NEVER the only carrier of the fact. The account's name is beside it and
 * the dot keeps its `title`/`aria-label`, so a dot nobody can pick out costs a
 * cue and not a fact. Every swatch carries a NAME too, so choosing one is not a
 * colour-only decision either.
 */
export const ACCOUNT_COLOR_PRESETS: readonly { hex: string; name: string }[] = [
  { hex: "#3B6FB6", name: "blue" },
  { hex: "#B5652A", name: "orange" },
  { hex: "#2E6F62", name: "teal" },
  { hex: "#7A4E9C", name: "purple" },
];

/** How many ordinal tones tokens.css declares (`--acct-1`, `--acct-2`). The
 * build refuses a token that is not declared, so this must not grow past what
 * the file has — it is the same list ui/useAccounts.ts used to own. */
const TONES: readonly AccountTone[] = ["1", "2"];

/** What the dot needs: the class to carry, and the colour to override it with. */
export interface AccountDot {
  tone: AccountTone;
  /** The chosen colour, or null — meaning "the token is the answer". */
  color: string | null;
}

/**
 * The dot for one account.
 *
 * `ordinal` is the account's position in the login's stable account order (by
 * id, oldest first — useAccounts.ts says why it must be stable), used only for
 * the fallback tone.
 */
export function accountColorOrToken(
  account: { color: string | null; paused: boolean },
  ordinal: number,
): AccountDot {
  if (account.paused) return { tone: "off", color: null };
  // A negative or fractional ordinal would index off the end; the modulo of a
  // non-negative integer cannot, so it is floored and clamped rather than
  // trusted. `TONES[…] as AccountTone` is safe only because of that.
  const i = Math.abs(Math.floor(ordinal)) % TONES.length;
  const tone = TONES[i] as AccountTone;
  const color = account.color !== null && isHexColor(account.color) ? account.color : null;
  return { tone, color };
}
