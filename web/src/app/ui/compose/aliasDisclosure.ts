// SPDX-License-Identifier: AGPL-3.0-only

// WHEN SENDING FROM AN ALIAS TELLS SOMEONE SOMETHING NEW (#533, spec §5).
//
// The primary domain is a CATCHALL, and the aliases on it are per-correspondent
// by design: `shop@`, `amazon@`, `bofa@`. That is what makes a leak
// attributable and an address disposable — and it also means the address itself
// discloses the SCHEME. Show one alias to someone and they can guess the shape
// of every other address you own.
//
// ⚠ THE HAZARD IS THE NEW READER, NOT THE ALIAS. This is the whole design, and
// it is why this file is not "warn whenever From is an alias":
//
//   * A REPLY goes to the person whose mail arrived at that alias, and the alias
//     is the address THEY USED (internal/alias derive.go prefers the pre-rewrite
//     envelope headers precisely so it is). The composer even says so —
//     `compose.aliasHint`, "the address this contact reached you at". Telling
//     them again discloses nothing, and on a catchall it would fire on nearly
//     every reply. CLAUDE.md's own rule about a report that cannot distinguish
//     "nothing to do" from "always fires" applies to warnings too: #533 says it
//     outright, "a warning shown on every reply is a warning nobody reads".
//
//   * A FORWARD, or a recipient ADDED to a reply, is the opposite: that person
//     has not seen this address, and they are about to. That is the disclosure,
//     and it is rare enough to be worth a line.
//
// So the question this module answers is not "is this an alias" but "which of
// these recipients has not already seen it".
//
// WHAT IT DELIBERATELY DOES NOT DO: guess whether a correspondent is a "service"
// or a "human". #533 sketches a discriminator from #186 (a genuine service alias
// takes 50-83% of its mail from one domain, over two), and it is a fine signal
// for RANKING a rule suggestion — but here it would decide whether to warn, and
// getting it wrong means either a silent disclosure or a warning about a
// correspondent who already knows. The participant test above needs no such
// judgement: it is a fact about this thread.
//
// A FRESH COMPOSE IS SILENT (parentParticipants === null). Choosing an alias for
// a new message means typing its local part: the address is the thing being
// decided, deliberately, and there is no "already knows" to compare against.
// Warning there would annotate the intended workflow — handing a new
// correspondent their own address — with an alarm.

/** An address, lowercased and trimmed; "" for anything unusable. */
function key(address: string): string {
  return address.trim().toLowerCase();
}

function domainOf(address: string): string {
  const at = key(address).lastIndexOf("@");
  return at === -1 ? "" : key(address).slice(at + 1);
}

export interface AliasDisclosureInput {
  /** The address this message will be sent AS, or "" when unsettled. */
  alias: string;
  /**
   * False when the From is the user's primary address, which discloses nothing
   * about the scheme. The caller decides this the same way the "replies come
   * back here" note does (ui/compose/from.ts aliasNote).
   */
  isAlias: boolean;
  /** Every recipient currently in the composer: To, Cc and Bcc alike. */
  recipients: readonly string[];
  /**
   * Everyone already on the message being replied to or forwarded — its sender
   * and, when the client has them, its To/Cc. `null` for a fresh compose or a
   * resumed draft, which is the silent case (see the header).
   *
   * ⚠ ABSENT To/Cc IS NOT THE SAME AS AN EMPTY THREAD, and this is the one place
   * the difference can bite: a reply opens on whatever the LIST loaded, which
   * carries `from` but not `to`/`cc` (ui/routes/ComposeRoute.tsx says so). The
   * effect is conservative — someone who was on the original Cc can look "new"
   * and earn a line that was not needed — never the other way round, which would
   * be a silent disclosure. Pass whatever is known; do not synthesise.
   */
  parentParticipants: readonly string[] | null;
  /**
   * The user's own domains (every identity's, catchall domains included). A
   * recipient at one of these is the user's own mailbox: nothing is disclosed to
   * yourself, and the pgptest@ round-trip address would otherwise warn on every
   * use.
   */
  ownDomains: readonly string[];
}

export interface AliasDisclosure {
  /** The address that would be disclosed. */
  alias: string;
  /** The recipients who have not seen it, in the order they appear. */
  newRecipients: string[];
}

/**
 * The recipients about to see this alias for the first time, or null when there
 * are none — which is the common case, and the point.
 */
export function aliasDisclosure(input: AliasDisclosureInput): AliasDisclosure | null {
  const alias = input.alias.trim();
  if (!input.isAlias || alias === "") return null;
  // A fresh compose has nobody to compare against: see the header.
  if (input.parentParticipants === null) return null;

  const known = new Set(input.parentParticipants.map(key).filter((a) => a !== ""));
  // The alias itself is "known" to its own thread — a reply-all that keeps the
  // user's own address on Cc must not count as a new reader of it.
  known.add(key(alias));
  const mine = new Set(input.ownDomains.map((d) => d.trim().toLowerCase()).filter((d) => d !== ""));

  const seen = new Set<string>();
  const newRecipients: string[] = [];
  for (const raw of input.recipients) {
    const address = raw.trim();
    const k = key(address);
    if (k === "" || known.has(k) || seen.has(k)) continue;
    if (mine.has(domainOf(address))) continue;
    seen.add(k);
    newRecipients.push(address);
  }
  return newRecipients.length === 0 ? null : { alias, newRecipients };
}
