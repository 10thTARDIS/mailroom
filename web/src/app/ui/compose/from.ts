// SPDX-License-Identifier: AGPL-3.0-only

// WHO THE MESSAGE IS FROM, AS A SENTENCE (#448, pass-2 §7).
//
// The old composer put a <select> of every identity beside an editable address
// box and a hint underneath. That is three controls to answer a question that
// has one obvious answer almost every time — and the one time it does not
// (a catch-all, where the local part is yours to invent) the picker was the same
// size as it was for the trivial case.
//
// So the From is a SENTENCE with a reason on it — `house@example.com · because
// you are replying to mail sent there` — and a **Change** button that opens the
// full list. This module is both halves of that, as pure functions, because the
// interesting part is the ORDER and the REASON rather than the markup:
//
//   * fromSentence  — the address compose sends as, and why.
//   * identityChoices — every identity a user may switch to, ordered, each with
//     the reason word its row prints.
//   * defaultIdentity — the identity compose OPENS on, which is the subject of
//     fromSentence. Kept here rather than in the component so "which address
//     does a reply go out as" is a testable claim.
//
// TWO SERVER FACTS SHAPE ALL OF IT (see core/jmap/types.ts Identity):
//
//   1. `Identity` CARRIES NO DEFAULT/PRIMARY FLAG. RFC 8621 §6 has none and
//      Mailroom's extension does not add one, so "your primary address" cannot
//      be read off an object — it is DERIVED: the first enabled, concrete
//      (non-wildcard) identity in the scope being composed in. The spec asked
//      for the flag "if the Identity type carries one; check" — it does not, and
//      inventing a field here would read as `undefined` forever (#155).
//   2. A CATCH-ALL IDENTITY IS `*@domain` (`isWildcard`), i.e. "any local part
//      at this domain". Choosing one keeps the old behaviour — the user types
//      the local part — which is why it is its own group rather than a row that
//      looks like an address you can send as.
//
// PER-IDENTITY MESSAGE COUNTS ARE NOT AVAILABLE TO THIS CLIENT (no Identity
// statistic exists in the API), so no row prints one. The design asked for none
// rather than a fabricated one.

import type { Identity } from "../../../core/jmap/types";
import type { MessageKey } from "../../i18n";

/** What a From row says about itself, as an i18n key. One word each: the rows
 * are read in a column and a sentence per row would be a paragraph. */
export type FromReason = "replyingHere" | "primary" | "catchAll" | "disabled";

export const FROM_REASON_KEY: Readonly<Record<FromReason, MessageKey>> = {
  replyingHere: "compose.from.reason.replyingHere",
  primary: "compose.from.reason.primary",
  catchAll: "compose.from.reason.catchAll",
  disabled: "compose.from.reason.disabled",
};

/** The long form, for the sentence under the From line (not the row's word). */
export const FROM_SENTENCE_KEY: Readonly<Record<FromReason, MessageKey>> = {
  replyingHere: "compose.from.becauseReplying",
  primary: "compose.from.primary",
  catchAll: "compose.from.catchAll",
  disabled: "compose.from.disabled",
};

/** Everything this module needs to know about the message being replied to or
 * forwarded. A shape rather than `EffectiveEmail` so the tests can state one
 * fact at a time, and so nothing here depends on the store. */
export interface ParentFacts {
  /** The identity the parent was delivered to, when the server resolved one. */
  identityId?: string | null;
  /** The address it was delivered to (Mailroom's compose extension), used when
   * `identityId` is absent — a catch-all delivery has an address and no
   * concrete identity. */
  deliveredTo?: string | null;
}

export interface FromSentence {
  /** The address to print. For a wildcard identity with nothing typed yet this
   * is `@domain`, which is exactly what the old control showed. */
  address: string;
  reason: FromReason;
}

/** The domain of a wildcard identity (`*@example.com` → `example.com`). */
export function wildcardDomain(id: Identity): string {
  return id.email.replace(/^\*@/, "");
}

function sameAddress(a: string, b: string): boolean {
  return a.trim().toLowerCase() === b.trim().toLowerCase();
}

/**
 * The identity a reply/forward should go out as, or undefined when the parent
 * does not name one.
 *
 * Two ways, in order: the identity the server RESOLVED for the delivery
 * (`parent.identityId`), then the concrete identity whose address matches
 * `deliveredTo`. Both are "reply from the address they wrote to", which is the
 * whole point of the alias model (spec §5); the second exists because a
 * delivery through a catch-all has an address and no identity of its own.
 */
export function replyIdentity(
  identities: readonly Identity[],
  parent: ParentFacts | null,
): Identity | undefined {
  if (!parent) return undefined;
  if (parent.identityId) {
    const byId = identities.find((i) => i.id === parent.identityId);
    if (byId) return byId;
  }
  const to = parent.deliveredTo ?? "";
  if (to.trim() === "") return undefined;
  return identities.find((i) => !i.isWildcard && sameAddress(i.email, to));
}

/**
 * The address a fresh message goes out as: the first ENABLED, CONCRETE identity,
 * in the order the server listed them.
 *
 * ⚠ ORDER-DEPENDENT AND DELIBERATELY SO. There is no primary flag (see the
 * header), so "primary" is a position, and the position that matters is the
 * server's own — `Identity/get`'s order — rather than something re-sorted here.
 * Sorting by address would make "primary" mean "alphabetically first", which is
 * a different and much worse promise.
 */
export function primaryIdentity(identities: readonly Identity[]): Identity | undefined {
  return identities.find((i) => i.enabled && !i.isWildcard);
}

/**
 * The identity compose OPENS on: the reply's, else the primary, else the first
 * enabled catch-all, else the first identity there is.
 *
 * The last two fallbacks are not decoration: a catch-all-only account has no
 * concrete identity at all, and an account whose only identity is disabled must
 * still open a composer that says so rather than one with an empty From.
 */
export function defaultIdentity(
  identities: readonly Identity[],
  parent: ParentFacts | null,
): Identity | undefined {
  return (
    replyIdentity(identities, parent) ??
    primaryIdentity(identities) ??
    identities.find((i) => i.enabled) ??
    identities[0]
  );
}

/**
 * The identity a RESUMED draft goes back to, from the address it was saved with.
 *
 * ⚠ A DERIVED ALIAS IS NOT AN IDENTITY (#599), and assuming it was is what made
 * this wrong. Compose matched the saved From against the CONCRETE identities
 * only; on a catch-all the alias `mortgage@example.com` matches none of them,
 * so resuming fell through to the primary identity and the draft silently
 * changed who it was from. MEASURED on delivered mail: two drafts composed from
 * two different aliases, both refused by the provider, both re-sent 96 minutes
 * later — and both went out as the canonical mailbox. Two drafts, two aliases,
 * one destination address is a default being re-applied, not a person making the
 * same edit twice.
 *
 * That breaks the alias model's whole point: replies to `mortgage@` are supposed
 * to come back to `mortgage@`, and the canonical mailbox is the one address the
 * scheme exists to keep out of circulation. It is the inverse of #533's hazard —
 * that one discloses the SCHEME, this one discloses the MAILBOX — and it does it
 * while the composer says otherwise.
 *
 * So: a concrete identity first (unchanged), then the wildcard identity that
 * OWNS that address's domain, whose local part the caller seeds from the same
 * saved address.
 *
 * ⚠ The domain check is what stops this over-matching. Without it a wildcard
 * identity would claim every resumed draft whose From is not a concrete
 * identity, including addresses this account cannot send as at all — which is
 * worse than reverting to a valid one, because it fails at the provider instead
 * of on screen.
 */
export function resumedIdentity(
  identities: readonly Identity[],
  savedFrom: string | null | undefined,
): Identity | undefined {
  const from = (savedFrom ?? "").trim();
  if (from === "") return undefined;

  const concrete = identities.find((i) => !i.isWildcard && sameAddress(i.email, from));
  if (concrete) return concrete;

  const at = from.lastIndexOf("@");
  if (at < 0) return undefined;
  const domain = from.slice(at + 1).toLowerCase();
  return identities.find(
    (i) => i.isWildcard && wildcardDomain(i).toLowerCase() === domain,
  );
}

/**
 * The From line: the address, and the reason it is that address.
 *
 * `typedAddress` is what the user has put in the local-part box of a catch-all
 * (empty otherwise). It is printed rather than the identity's `*@domain`,
 * because for a catch-all the address IS the thing being typed — and the
 * sentence must never show `*@example.com`, which is not an address anyone can
 * send from.
 */
export function fromSentence(
  identity: Identity | undefined,
  parent: ParentFacts | null,
  identities: readonly Identity[],
  typedAddress = "",
): FromSentence | null {
  if (!identity) return null;
  const typed = typedAddress.trim();
  const address = identity.isWildcard
    ? typed !== ""
      ? typed
      : `@${wildcardDomain(identity)}`
    : identity.email;
  return { address, reason: reasonFor(identity, parent, identities) };
}

/**
 * Why this identity.
 *
 * ORDER MATTERS: `disabled` is checked FIRST. A disabled identity that happened
 * to be the reply target would otherwise print `replying here` on a row that
 * cannot send at all, which is the one reason word that must never be wrong.
 */
function reasonFor(
  identity: Identity,
  parent: ParentFacts | null,
  identities: readonly Identity[],
): FromReason {
  if (!identity.enabled) return "disabled";
  if (identity.isWildcard) return "catchAll";
  const reply = replyIdentity(identities, parent);
  if (reply && reply.id === identity.id) return "replyingHere";
  return "primary";
}

/** One row of the **Change** list. */
export interface IdentityChoice {
  id: string;
  /** What the row prints: the address, or `*@domain` for a catch-all — which is
   * honest there, because the row is offering the DOMAIN and the local part is
   * typed afterwards. */
  address: string;
  reason: FromReason;
  /** True for a wildcard identity: choosing it starts the "type the local part"
   * flow rather than settling the address. */
  isWildcard: boolean;
  enabled: boolean;
  /** The identity's display name, matched by the filter alongside the address. */
  name: string;
}

/**
 * Every identity the user may switch to, in the order the list draws them.
 *
 * THE ORDER IS THE DESIGN: enabled concrete identities first, then enabled
 * catch-alls, then everything disabled — because that is descending order of
 * "can I just click this and send". Within the first group the reply target
 * leads, since it is the one the user is most likely to be confirming.
 *
 * ⚠ THE THREE GROUPS ARE NOT A SORT KEY ON `enabled` ALONE. A DISABLED CONCRETE
 * address must rank below an ENABLED catch-all: the catch-all can send and it
 * cannot. Sorting `[!isWildcard, enabled]` would put it above, which is the
 * mistake identityPrecedence's own test guards in settings (#374).
 */
export function identityChoices(
  identities: readonly Identity[],
  parent: ParentFacts | null,
  filter = "",
): IdentityChoice[] {
  const needle = filter.trim().toLowerCase();
  const reply = replyIdentity(identities, parent);
  const rank = (i: Identity): number => {
    if (!i.enabled) return 2;
    return i.isWildcard ? 1 : 0;
  };
  const within = (i: Identity): number => (reply && reply.id === i.id ? 0 : 1);
  return identities
    .filter(
      (i) =>
        needle === "" ||
        i.email.toLowerCase().includes(needle) ||
        i.name.toLowerCase().includes(needle),
    )
    .map((i, index) => ({ i, index }))
    .sort(
      (a, b) =>
        rank(a.i) - rank(b.i) ||
        within(a.i) - within(b.i) ||
        // The server's order for everything else: it is the only order that
        // makes "primary" mean the same thing here as it does in fromSentence.
        a.index - b.index,
    )
    .map(({ i }) => ({
      id: i.id,
      address: i.email,
      reason: reasonFor(i, parent, identities),
      isWildcard: i.isWildcard,
      enabled: i.enabled,
      name: i.name,
    }));
}

/**
 * The muted note under the body when the message is going out as a NON-PRIMARY
 * alias (design pass-2 §7): the reader's cue that the reply will come back to
 * the same alias rather than to the main address.
 *
 * Null — no note — in the three cases where it would be noise or a lie: there
 * is no identity, this IS the primary one, or there is no primary to compare it
 * with (a catch-all-only account, where every address is an alias and saying so
 * on every message says nothing).
 */
export function aliasNote(
  identity: Identity | undefined,
  primary: Identity | undefined,
): { alias: string } | null {
  if (!identity || !primary) return null;
  if (identity.id === primary.id) return null;
  const alias = identity.isWildcard ? wildcardDomain(identity) : identity.email;
  if (alias.trim() === "") return null;
  return { alias };
}
