// SPDX-License-Identifier: AGPL-3.0-only

import type { SenderWarningRaw } from "./jmap/types";

// Deciding what to tell a reader about a message's SENDING DOMAIN (#397),
// alongside senderAuth.ts's decision about the message's AUTHENTICATION.
//
// ⚠ A DIFFERENT QUESTION FROM senderAuth.ts, ON PURPOSE. That file asks "did the
// servers that handled this message vouch for it?". This asks "is the domain it
// came from one the user knows, or one that looks like one?". A lookalike domain
// PASSES AUTHENTICATION PERFECTLY — it belongs to the attacker, who configured
// SPF and DKIM for it like anyone else. The two notices are adjacent and
// independent, and a reader may see either, both, or neither.
//
// The server already did the judging (internal/senderwarn) and already returns
// worst-first; this module exists anyway for the same reason senderAuth.ts's
// logic is not inline in the JSX: the ordering and the "what counts" decision
// belong under test, not trusted by construction — and, here specifically, a
// server that adds a fourth kind someday must not put an untranslated raw
// string into a security notice while the client waits for a release.

/** The three warning kinds, worst first. Mirrors internal/senderwarn's constants. */
export type SenderWarningKind =
  "selfSpoof" | "multipleFrom" | "lookalikeDomain" | "punycodeDomain";

/** One warning about a message's sending domain, normalised from the wire shape. */
export interface SenderWarning {
  kind: SenderWarningKind;
  /** The sending domain, exactly as the message presented it. */
  domain: string;
  /** lookalikeDomain only: the known domain it collides with. What makes the
   * warning actionable rather than ominous — "this is not walmart.com" is
   * something a reader can check in a second. */
  resembles?: string;
  /** punycodeDomain only: the decoded form, when it decodes. */
  unicode?: string;
}

/** Worst first. selfSpoof is a stranger claiming the user's OWN domain; a
 * lookalike is a domain the user actually corresponds with, imitated;
 * punycode is informational — an ordinary and legitimate mechanism that
 * merely happens to be what a homograph attack would also need. */
const SEVERITY: Readonly<Record<SenderWarningKind, number>> = {
  selfSpoof: 0,
  // #754: more than one From field — the receiving server's DMARC verdict may
  // be about a different author from the one shown.
  multipleFrom: 1,
  lookalikeDomain: 2,
  punycodeDomain: 3,
};

function isKnownKind(kind: string): kind is SenderWarningKind {
  return (
    kind === "selfSpoof" ||
    kind === "multipleFrom" ||
    kind === "lookalikeDomain" ||
    kind === "punycodeDomain"
  );
}

/**
 * The warnings to show for a message, severity-ordered, or an empty list for
 * "nothing to say" — which `null`/`undefined` and the common case both mean.
 *
 * ⚠ AN UNRECOGNISED `kind` IS DROPPED, NOT RENDERED. The wire type leaves `kind`
 * as a bare `string` for exactly this: a server that ships a fourth kind before
 * this client has translations for it must fail SILENT here, because the
 * alternative is a raw, untranslated string interpolated into a warning about a
 * message the reader is being told to distrust — the worst possible surface for
 * text nobody wrote copy for.
 */
export function senderWarningsList(
  raw: readonly SenderWarningRaw[] | null | undefined,
): SenderWarning[] {
  if (!raw) return [];
  const out: SenderWarning[] = [];
  for (const w of raw) {
    if (!isKnownKind(w.kind)) continue;
    const warning: SenderWarning = { kind: w.kind, domain: w.domain };
    if (w.resembles) warning.resembles = w.resembles;
    if (w.unicode) warning.unicode = w.unicode;
    out.push(warning);
  }
  // Sorted here rather than trusted from the server: the ordering is part of
  // what this module promises, and Array#sort is stable (ES2019), so warnings
  // of equal severity keep the server's relative order.
  out.sort((a, b) => SEVERITY[a.kind] - SEVERITY[b.kind]);
  return out;
}
