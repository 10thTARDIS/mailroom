// SPDX-License-Identifier: AGPL-3.0-only

import type { AuthResults } from "./jmap/types";

// Deciding what — if anything — to tell a reader about a message's sender
// authentication (#55 increment 1).
//
// ⚠ THIS FILE EXISTS BECAUSE THE ANSWER IS USUALLY "NOTHING", AND THAT WAS
// MEASURED. On a live corpus, nearly all messages pass all three
// methods cleanly. A green "verified" badge would therefore fire on 99.4% of mail,
// which is not reassurance — it is furniture that a reader stops seeing, and it
// would be at its least visible exactly when it changed. So this returns null for
// the overwhelming majority and speaks only about the ~0.8% with something wrong.
//
// WHAT COUNTS AS "SOMETHING WRONG" IS A JUDGEMENT, AND HERE IS THE LINE:
// the harm being guarded against is *this message may not be from who it claims*.
//
//   - A method that FAILED says exactly that.                       (3 messages)
//   - No authentication at all says nobody checked.                 (5 messages)
//   - An evaluation ERROR says the check could not complete.        (6 messages)
//
// and, deliberately excluded:
//
//   - `none` — the sending domain publishes no such policy. 19 messages carry one.
//     It is a fact about the SENDER'S configuration, not evidence about this
//     message, the reader cannot act on it, and most of the internet's small mail
//     servers would trip it. Reporting it would quadruple the notice rate while
//     adding nothing a person could do.

/** How serious the notice is, worst first. */
export type SenderAuthSeverity = "failed" | "unauthenticated" | "unverifiable";

/** The three methods, as an i18n-key-safe union rather than bare strings. */
export type AuthMethodName = "spf" | "dkim" | "dmarc";

export interface SenderAuthNotice {
  severity: SenderAuthSeverity;
  /** The methods responsible, in the server's canonical order. Empty for
   * "unauthenticated", where no method ran and so none is to blame. */
  methods: AuthMethodName[];
}

/** The methods, in the order a notice should list them. Mirrors ml.AuthMethods. */
const METHODS: readonly AuthMethodName[] = ["spf", "dkim", "dmarc"];

/** Results that mean the check could not be completed, as opposed to completing
 * with a negative answer. RFC 8601 §2.7. */
const ERRORED = new Set(["permerror", "temperror"]);

/**
 * True when a method FAILED: it reported `fail` and never reported `pass`.
 *
 * ⚠ THE "AND NEVER PASSED" HALF IS LOAD-BEARING and must match the server's
 * ml.AuthResults.Failed exactly. A message carrying two DKIM signatures — one
 * valid, one not — holds both keywords, and that is ordinary multi-signature mail
 * rather than a forgery. Treating the bare presence of `fail` as a failure would
 * accuse a whole class of legitimate senders. A real corpus happens to
 * contain no such message today, so this rule is protecting against a case the
 * live numbers cannot currently demonstrate — which is precisely why it is
 * written down and tested rather than left to be noticed later.
 */
function methodFailed(results: readonly string[] | null): boolean {
  if (!results) return false;
  return results.includes("fail") && !results.includes("pass");
}

function methodErrored(results: readonly string[] | null): boolean {
  if (!results) return false;
  return results.some((r) => ERRORED.has(r));
}

/**
 * The notice to show for a message, or null to say nothing at all.
 *
 * Null is returned for the common cases and they are NOT the same case:
 *  - `authResults` itself null — Mailroom wrote this message, so authentication
 *    never applied. Silence here is correctness, not an omission: 7 of the 12
 *    headerless messages on a real corpus are the user's own drafts and
 *    Sent copies, and telling someone their outbox looks unverified is worse than
 *    telling them nothing.
 *  - everything passed — silence, per the 99.4% argument above.
 */
export function senderAuthNotice(
  auth: AuthResults | null | undefined,
): SenderAuthNotice | null {
  if (!auth) return null;

  // Arrived with nothing at all. Checked BEFORE the per-method rules because with
  // no methods present those rules are all vacuously false, and the message would
  // otherwise fall through to silence — the exact shape of an absence assertion
  // that passes for free.
  if (!auth.present) return { severity: "unauthenticated", methods: [] };

  const failed = METHODS.filter((m) => methodFailed(auth[m]));
  if (failed.length > 0) return { severity: "failed", methods: [...failed] };

  const errored = METHODS.filter((m) => methodErrored(auth[m]));
  if (errored.length > 0) return { severity: "unverifiable", methods: [...errored] };

  return null;
}

/**
 * Mailroom's own verdict (#776, ADR-036): the server verified a DKIM signature
 * itself, because no trusted Authentication-Results header was present, and the
 * signing domain is aligned with the From domain. Returns the signing domain, or
 * null for anything else.
 *
 * ⚠ DISTINCT FROM A PROVIDER VERDICT ON PURPOSE, and the one place the pane says
 * something positive. It replaces "unauthenticated" on mail that would otherwise
 * carry that caution, so it is not the 99.4% badge senderAuthNotice avoids: it
 * is rare, it explains a change, and it names who vouched (Mailroom) and for
 * what (DKIM, this domain). Requires `source === "mailroom"`, `present`, a
 * passing `dkim` and a non-empty domain; anything weaker is not shown, so a
 * client talking to an older server (no `source`) renders exactly as before.
 */
export function mailroomVerified(
  auth: AuthResults | null | undefined,
): { domain: string } | null {
  if (!auth || auth.source !== "mailroom" || !auth.present) return null;
  if (!auth.dkim?.includes("pass")) return null;
  const domain = (auth.dkimDomain ?? "").trim();
  return domain ? { domain } : null;
}
