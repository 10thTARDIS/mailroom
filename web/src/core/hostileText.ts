// SPDX-License-Identifier: AGPL-3.0-only

// TEXT FROM A STRANGER, RENDERED IN OUR OWN CHROME (Forgejo #307, spec §11).
//
// docs/hostile-content.md is about the message BODY, which is defended by being
// put somewhere it cannot reach us: sanitized on the server, then rendered in a
// sandboxed, opaque-origin iframe. None of that applies to the sender name, the
// subject or the preview, because those are NOT in the frame — they are React
// text nodes in the app's own DOM, in the list row and the reading pane's header,
// which is exactly where they have to be.
//
// React escapes them, so they cannot inject markup. What they can still do is
// LIE, and two of the ways are worth code rather than a comment:
//
//  1. A display name that is itself an address. `From: "security@bank.example"
//     <attacker@evil.example>` renders, in a list that shows the display name
//     alone, as `security@bank.example` — a true-looking statement of an identity
//     that is not this message's. Nothing is escaped wrongly; the row is simply
//     showing an attacker-chosen string in the place a user reads as "who".
//
//  2. A bidirectional control character. U+202E RIGHT-TO-LEFT OVERRIDE and its
//     family do not reorder themselves — they reorder EVERYTHING AFTER THEM in
//     the same bidi paragraph, and the override runs until a matching pop or the
//     end of the paragraph. In the reading pane the display name and the address
//     that follows it are two spans in ONE line box, i.e. one paragraph: a U+202E
//     in the name therefore rewrites the address beside it on screen. The user is
//     reading the right bytes in the wrong order.
//
// BOTH HALVES ARE NEEDED AND THEY DO DIFFERENT JOBS. `unicode-bidi: isolate` in
// styles.css CONFINES a control's effect to the element that contains it, so a
// name can never reorder its neighbours; stripping the controls here PREVENTS the
// reordering at all, including WITHIN the element, and including in the places
// where the text is not a styled element at all (an aria-label, a `title`, a
// window title). CSS alone leaves a scrambled name that a screen reader still
// reads scrambled; stripping alone leaves a future unisolated layout free to leak
// again. See docs/hostile-content.md §2.3b.
//
// This module is deliberately pure, DOM-free and catalog-free — the same reason
// core/remoteAllows.ts is: web/ has no jsdom, so the logic worth reasoning about
// has to be reasonable about without a browser. Callers pass their own fallback
// string, so no message key is baked in here.

/** The bidi FORMATTING characters — the ones that scope a direction change over
 * the text that FOLLOWS them:
 *
 *   U+202A LRE, U+202B RLE, U+202C PDF, U+202D LRO, U+202E RLO  (the legacy
 *                                                                embedding set)
 *   U+2066 LRI, U+2067 RLI, U+2068 FSI, U+2069 PDI              (the isolates)
 *
 * U+200E LRM and U+200F RLM are deliberately NOT here. They are single-character
 * direction HINTS with no scope — neither can reorder a following run — and both
 * appear legitimately in real Arabic and Hebrew names and subjects. Removing them
 * would corrupt correct text to defend against nothing.
 *
 * ⚠ WRITTEN AS CODE POINTS, NOT AS A REGEX WITH `\u` ESCAPES, ON PURPOSE. This is
 * a module about characters that cannot be seen. If its own source spelled them
 * as literals — which is what happens the first time somebody pastes one in — the
 * range would be invisible in every diff, every review and every grep that ever
 * touched it. Hex numbers are legible; the characters are not. */
const BIDI_RANGES: ReadonlyArray<readonly [number, number]> = [
  [0x202a, 0x202e],
  [0x2066, 0x2069],
];

function isBidiControl(codePoint: number): boolean {
  for (const [lo, hi] of BIDI_RANGES) {
    if (codePoint >= lo && codePoint <= hi) return true;
  }
  return false;
}

/** Whether s carries any scoping bidi control (see BIDI_RANGES). */
export function hasBidiControls(s: string | null | undefined): boolean {
  if (!s) return false;
  // for..of iterates CODE POINTS, not UTF-16 units, so a surrogate pair is one
  // step and cannot be half-matched.
  for (const ch of s) {
    if (isBidiControl(ch.codePointAt(0) ?? 0)) return true;
  }
  return false;
}

/** s with every scoping bidi control removed.
 *
 * Removal, not replacement: substituting a visible marker would put an
 * attacker-influenced glyph in the middle of a name for no gain. That a control
 * was present is surfaced as a DECISION instead — see senderIdentity, which shows
 * the real address when it had to strip one. */
export function stripBidiControls(s: string | null | undefined): string {
  if (!s) return "";
  if (!hasBidiControls(s)) return s; // the overwhelmingly common case: no copy
  let out = "";
  for (const ch of s) {
    if (!isBidiControl(ch.codePointAt(0) ?? 0)) out += ch;
  }
  return out;
}

/** One From address, as a list row or a header should present it. */
export interface SenderIdentity {
  /** The display name, bidi-stripped. "" when the From carried none. */
  name: string;
  /** The address, bidi-stripped and trimmed. "" when the From carried none. */
  address: string;
  /** Show the address IN ADDITION to the name.
   *
   * False when there is no name (the address is then the display itself, and
   * repeating it would be noise), and false for the ordinary `"Ada Lovelace"
   * <ada@example.com>`, whose name claims nothing it is not. */
  showAddress: boolean;
}

/** What a row should show for a From (#307).
 *
 * THE NARROW RULE, AND WHY IT IS THE NARROW ONE. The list row's sender cell is a
 * single ellipsised grid column (`.sender` in styles.css: overflow hidden,
 * nowrap). Appending `<addr>` to every row roughly doubles the text in a cell
 * whose entire design is "one short thing", and in the narrow layout — the one
 * with the reading pane open, i.e. the one people triage in — the address is
 * usually the part that gets clipped. So the always-show version degrades every
 * ordinary row while frequently not even showing the address it was added for.
 * The reading pane shows both, always; that is where a user goes to ask "who".
 *
 * What is a LIE, as opposed to merely incomplete, is a display name that IS an
 * address, or one that has been reordered. Those two trigger the address here:
 *
 *   - the name contains "@", so it reads as an address, and it is not this
 *     message's address;
 *   - the name contained a bidi control, so somebody crafted it.
 *
 * WHAT THIS DOES NOT CATCH, said plainly: a plausible lookalike with no "@".
 * `From: "Bank Security" <attacker@evil.example>` still shows "Bank Security"
 * alone in the list. No display-name heuristic fixes that one — only an
 * AUTHENTICATED From does, which is DMARC/ARC verification (#55) and is not this
 * change. The reading pane shows that message's address like any other. */
export function senderIdentity(
  from: { name?: string | null; email?: string | null } | null | undefined,
): SenderIdentity {
  const rawName = from?.name ?? "";
  const name = stripBidiControls(rawName).trim();
  const address = stripBidiControls(from?.email ?? "").trim();
  const confusable = name.includes("@") || hasBidiControls(rawName);
  return { name, address, showAddress: name !== "" && address !== "" && confusable };
}

/** The one-line form of a SenderIdentity, for an aria-label or a title where
 * there is no room for two elements. `fallback` is used when the From carried
 * neither a name nor an address — the caller supplies it so this module needs no
 * message catalog. */
export function senderText(id: SenderIdentity, fallback: string): string {
  if (id.showAddress) return `${id.name} <${id.address}>`;
  return id.name || id.address || fallback;
}
