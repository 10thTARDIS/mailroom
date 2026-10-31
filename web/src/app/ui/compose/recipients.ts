// SPDX-License-Identifier: AGPL-3.0-only

// WHO A REPLY GOES TO (#646).
//
// A reply used to be addressed to the parent's From, always. That is wrong
// whenever a message carries a Reply-To — a mailing list, a ticketing system,
// most automated mail — because RFC 5322 §3.6.2 says the Reply-To is where a
// reply belongs, and From is only the fallback. And there was no Reply all, so
// answering a conversation meant retyping its other recipients.
//
// These are pure functions, like the rest of compose's decisions (./from.ts),
// because there is no DOM test harness here and "who does this go to" is the
// part worth pinning.
//
// ⚠ "ONE OF YOUR ADDRESSES" INCLUDES EVERY LOCAL PART OF A CATCH-ALL DOMAIN. A
// wildcard identity (`*@example.com`) makes every address at that domain yours;
// the alias scheme hands out a different one per correspondent. Reply all that
// only removed concrete identities would mail the user's own aliases back to
// them — and, worse, keep an alias the user was deliberately not using on the
// thread.

import type { EmailAddress, Identity } from "../../../core/jmap/types";
import type { Chip } from "./chips";

/** The two kinds of reply. */
export type ReplyMode = "reply" | "replyAll";

/** The parent's addressing, as a shape rather than an Email so each test can
 * state one fact. `undefined` means "not fetched", which callers must not pass:
 * see ComposeRoute's readiness check, which waits for the full headers. */
export interface ReplyParent {
  from?: EmailAddress[] | null;
  replyTo?: EmailAddress[] | null;
  to?: EmailAddress[] | null;
  cc?: EmailAddress[] | null;
}

export interface ReplyRecipients {
  to: Chip[];
  cc: Chip[];
}

function norm(address: string): string {
  return address.trim().toLowerCase();
}

function domainOf(address: string): string {
  const at = address.lastIndexOf("@");
  return at < 0 ? "" : norm(address.slice(at + 1));
}

/**
 * Whether an address belongs to the user: a concrete identity's address, or any
 * address at a domain one of their catch-all identities owns. Disabled
 * identities count — being unable to SEND as an address does not make mail to it
 * someone else's.
 */
export function isOwnAddress(address: string, identities: readonly Identity[]): boolean {
  const a = norm(address);
  if (a === "") return false;
  const domain = domainOf(a);
  return identities.some((i) =>
    i.isWildcard ? norm(i.email.replace(/^\*@/, "")) === domain : norm(i.email) === a,
  );
}

function chips(addrs: EmailAddress[] | null | undefined): Chip[] {
  return (addrs ?? [])
    .filter((a) => a.email.trim() !== "")
    .map((a) => ({ address: a.email, name: a.name ?? null }));
}

/** Drops chips already seen (case-insensitively), keeping the FIRST, which is
 * the one with the display name the most specific header gave. */
function dedupe(list: Chip[], seen: Set<string>): Chip[] {
  const out: Chip[] = [];
  for (const c of list) {
    const k = norm(c.address);
    if (seen.has(k)) continue;
    seen.add(k);
    out.push(c);
  }
  return out;
}

/**
 * To and Cc for a reply or a reply-all.
 *
 * - The TARGET is the parent's Reply-To when it has one, else its From.
 * - A message the user SENT (its From is theirs) is answered by continuing it:
 *   the target is the parent's To, the way mainstream clients do it, rather than
 *   a message to themselves.
 * - Reply all adds the parent's To (to To) and Cc (to Cc).
 * - The user's own addresses are removed and duplicates collapsed, EXCEPT that
 *   To is never left empty: if filtering removed everything (mail to
 *   themselves), the unfiltered target is used so the composer does not open
 *   addressed to nobody.
 */
export function replyRecipients(
  parent: ReplyParent,
  identities: readonly Identity[],
  mode: ReplyMode,
): ReplyRecipients {
  const own = (c: Chip): boolean => isOwnAddress(c.address, identities);
  const from = chips(parent.from);
  const sentByMe = from.length > 0 && from.every(own);
  const replyTo = chips(parent.replyTo);

  // Reply-To wins even on the user's own message: they set it deliberately.
  const target = replyTo.length > 0 ? replyTo : sentByMe ? chips(parent.to) : from;

  const seen = new Set<string>();
  let to = dedupe(target.filter((c) => !own(c)), seen);
  let cc: Chip[] = [];
  if (mode === "replyAll") {
    to = to.concat(dedupe(chips(parent.to).filter((c) => !own(c)), seen));
    cc = dedupe(chips(parent.cc).filter((c) => !own(c)), seen);
  }
  if (to.length === 0) {
    // Everything was the user's own. Keep the target as-is (deduplicated) rather
    // than open a composer addressed to nobody; and anything Cc'd moves up, so a
    // reply-all to a note-to-self that Cc'd someone still reaches them.
    to = cc.length > 0 ? cc : dedupe(target, new Set<string>());
    if (to === cc) cc = [];
    // A message of their own with no To at all (sent Bcc-only) has no target
    // either; From is the last address the parent names.
    if (to.length === 0) to = dedupe(from, new Set<string>());
  }
  return { to, cc };
}
