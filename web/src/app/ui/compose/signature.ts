// SPDX-License-Identifier: AGPL-3.0-only

// PICKING A SAVED SIGNATURE BY HAND (#649) — the replace-or-insert decision,
// kept pure and DOM-free (this project has no jsdom/RTL harness) so it can be
// pinned with vitest rather than trusted on sight.
//
// Compose.tsx already tracks the signature it put in the body ITSELF: the
// `opening` ref (what compose seeded the body with) and the From-change effect
// that re-seeds it while the body is `untouched`. This module answers the same
// question a manual pick has to ask — "is the signature I put there still what
// I put there?" — but generalised: not "is the WHOLE body untouched" but "is
// THIS BLOCK, byte for byte, still somewhere in it", because typing elsewhere
// in the body (above the signature, in the quote's blank line) must not turn a
// legitimate replace into a destructive insert.
//
// REPLACE vs INSERT:
//   * REPLACE when the block compose (or an earlier manual pick) put there is
//     still present, unmodified, as a substring of the current body. This is
//     always true right after compose opens (the untouched case), and stays
//     true if the user typed anywhere else without touching the signature
//     itself.
//   * INSERT otherwise — because either there was never a signature block to
//     find (no default configured), or the user edited it (a name changed, a
//     line removed: no longer an exact substring) — and compose has no undo,
//     so nothing may be deleted to make room. The new signature is appended
//     after everything already in the body, INCLUDING a quote if the parent
//     added one: the end is the one place guaranteed not to overlap anything
//     the user, or an earlier automatic insert, put there. (A signature
//     conventionally sits above the quote, but only compose's OWN insert can
//     assume that geometry — see openingBodies in Compose.tsx; a hand-edited
//     body has no geometry this module may assume without risking exactly the
//     deletion it exists to avoid.)
//
// ONE CALL PER MODE. `body` here is whichever field is live — Compose.tsx runs
// this once against the plain-text body in plain/Markdown mode and once
// against the HTML body in rich mode, each with that mode's own tracked block
// and the newly picked signature's matching form (its own `text` or `html`,
// falling back html<->text exactly as Compose.tsx's `openingBodies` does
// before calling in here — that fallback needs the DOM's `<template>` trick
// for HTML→text and so cannot live in this module).

export type SignatureAction =
  | { kind: "replace"; body: string }
  | { kind: "insert"; body: string };

/**
 * Decide what picking a signature does to ONE body string.
 *
 * `current`: the block last placed in `body` (automatically, or by an earlier
 * manual pick) in THIS mode's form — null/empty when none ever was.
 * `next`: the newly chosen signature's own block, in the same form.
 */
export function pickSignatureFor(body: string, current: string | null, next: string): SignatureAction {
  if (current && body.includes(current)) {
    return { kind: "replace", body: replaceOnce(body, current, next) };
  }
  return { kind: "insert", body: `${body}${next}` };
}

/** Replaces the FIRST occurrence only — there is exactly one signature block to
 * find, and a global replace would also rewrite an accidental repeat of the
 * same text elsewhere in the body (a quoted reply that happens to end with the
 * same sign-off, say). */
function replaceOnce(haystack: string, needle: string, replacement: string): string {
  const i = haystack.indexOf(needle);
  if (i === -1) return haystack;
  return haystack.slice(0, i) + replacement + haystack.slice(i + needle.length);
}
