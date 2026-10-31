// SPDX-License-Identifier: AGPL-3.0-only

// THE SEND LINE (#448, design pass-2 §7): one sentence under the body saying
// what will happen to this message cryptographically.
//
// ⚠ IT NEVER PROMISES TO ENCRYPT. Encryption lands in v1.69.0; nothing in this
// client can encrypt today. So the seal-tone line names who COULD be encrypted
// to — "keys held for A and B" — and the warn-tone line says outright that the
// message goes as plain text. A line that read "will encrypt" would be the worst
// possible bug in this file: a user acting on it would put something in the clear
// believing it was sealed.
//
// ⚠ AND IT NEVER DOWNGRADES SILENTLY. A recipient with no key is NAMED. The
// design's own words: "never a silent downgrade".
//
// FOUR BRANCHES, and the fourth is the one this project's discipline exists for:
//
//   1. own key + every recipient's key held  → seal   "Will sign · keys held for …"
//   2. own key + a recipient without a key   → warn   "Will sign · no key for … ·
//                                                      would send as plain text to all N"
//   3. no own key                            → plain  "Will not sign: <reason>"
//   4. keys UNASKABLE (no urn:mailroom:pgp)  → plain  "Signing status unknown on
//                                                      this server"
//
// Branch 4 is why `ownKey` is a TRI-STATE (`true | false | null`) rather than the
// boolean the spec's signature sketched. `null` means the session does not
// advertise the keys capability, so the question cannot be asked — and reporting
// that as `false` ("you have no key") would be exactly the #176 mistake of
// collapsing "asked, and the answer is none" into "could not ask".
//
// `ownKey` ITSELF IS THE SERVER'S OWN VERDICT (#492), not a client guess.
// `willSign` below combines the two facts that make up "will this be signed":
// `Identity.signingKeyId` (a key bound to THIS identity, or unbound) and
// `urn:mailroom:pgp`'s `signing` (whether the crypto process is even wired to
// THIS SERVER). Either fact alone is a claim that will be wrong for somebody —
// see willSign's own comment.
//
// "WITHOUT A KEY" MEANS "NOT IN OUR HANDS", which is why both `none` and
// `unknown` chips land in branch 2. For an unknown correspondent we hold no key,
// which is the only fact that decides whether this message could be encrypted —
// so `no key for X` is a true statement about our key ring, and the third clause
// ("would send as plain text to all N") is the actionable one either way.

import type { MessageKey } from "../../i18n";
import type { Identity } from "../../../core/jmap/types";
import type { ChipKeyFact } from "./chips";

/**
 * Whether this send will be signed — the server's own verdict, not a client
 * guess (#492).
 *
 * `null` when `signing` is `null`: the session does not advertise
 * `urn:mailroom:pgp` at all, so the question is UNASKABLE on this server (a
 * different fact from "asked, and no" — #176).
 *
 * `false` when EITHER half is missing: `signing` is `false` (this server has
 * no crypto process wired at all, so no identity's key can ever be used), OR
 * `identity.signingKeyId` is `null`/`undefined` (no key is bound to THIS
 * identity, even on a server that could sign).
 *
 * `true` only when BOTH hold — the one case where a signature will actually
 * be produced.
 */
export function willSign(
  identity: Pick<Identity, "signingKeyId"> | undefined,
  signing: boolean | null,
): boolean | null {
  if (signing === null) return null;
  if (!signing) return false;
  return identity?.signingKeyId != null;
}

/** Why `willSign` came back `false` — so branch 3 can name the actual cause
 * instead of a single sentence that guesses. Meaningless (and unread) when
 * `willSign` is not `false`. */
export type WillNotSignReason = "noKeyBound" | "cannotSign";

/**
 * `"cannotSign"` when `signing` is `false`: THIS SERVER has no crypto process
 * wired, so no key binding would matter — checked first, because it is the
 * more fundamental of the two reasons.
 *
 * `"noKeyBound"` otherwise: the server could sign, but no key is bound to
 * this identity.
 */
export function willNotSignReason(
  identity: Pick<Identity, "signingKeyId"> | undefined,
  signing: boolean | null,
): WillNotSignReason {
  if (signing === false) return "cannotSign";
  return "noKeyBound";
}

export type SendTone = "seal" | "warn" | "plain";

/** One clause of the sentence, as an i18n key plus its parameters. The component
 * joins them with " · " — the separator is presentation, and a function that
 * returned a finished string could not be rendered with per-clause emphasis. */
export interface SendLinePart {
  key: MessageKey;
  params?: Record<string, string | number>;
}

export interface SendLineResult {
  tone: SendTone;
  parts: SendLinePart[];
}

/** A recipient chip as the send line sees it: a name to print and whether a key
 * is held for it. */
export interface SendLineChip {
  /** The display name, or the address when there is no name — whichever the chip
   * itself shows, so the line names people the same way the chips do. */
  name: string;
  fact: ChipKeyFact;
}

/**
 * Joins names the way English does: "A", "A and B", "A, B and C".
 *
 * Exported because the send line is not the only sentence that will need it and
 * because a list of one must not come out as "A and" — which is what a plain
 * `join(" and ")` over a one-element array does not do but a slice-based one
 * easily can.
 */
export function joinNames(names: readonly string[]): string {
  if (names.length === 0) return "";
  if (names.length === 1) return names[0] as string;
  const head = names.slice(0, -1).join(", ");
  return `${head} and ${names[names.length - 1] as string}`;
}

/**
 * The send line for the current state of the composer.
 *
 * `ownKey`: true = a usable signing key for the sending identity; false = none;
 * null = this server cannot be asked (see the header). The caller computes it
 * with `willSign(identity, signing)`.
 * `ownAddress` is the address the line quotes in branch 3's `noKeyBound`
 * reason — the sentence names the address that has no key, so it is an input
 * rather than something the caller pastes in afterwards.
 * `reason` is `willNotSignReason(identity, signing)`'s answer, consulted only
 * in branch 3 (`ownKey === false`); the default is the harmless one for every
 * other branch, which never reads it.
 */
export function sendLine(
  ownKey: boolean | null,
  chips: readonly SendLineChip[],
  ownAddress: string,
  reason: WillNotSignReason = "noKeyBound",
): SendLineResult {
  // 4. Unaskable. Checked FIRST: with no keys API there is nothing true to say
  // about signing at all, and every other branch would be a guess dressed up as
  // a fact.
  if (ownKey === null) {
    return { tone: "plain", parts: [{ key: "compose.send.unknownServer" }] };
  }

  // 3. No key of our own. Nothing about the recipients can change this: without
  // a signing key there is no signature, whoever it is going to. WHICH sentence
  // depends on WHY: a server with no crypto process wired says so outright
  // rather than pointing at a key binding that would not have mattered anyway.
  if (!ownKey) {
    return {
      tone: "plain",
      parts:
        reason === "cannotSign"
          ? [{ key: "compose.send.willNotSign.cannotSign" }]
          : [{ key: "compose.send.willNotSign.noKey", params: { address: ownAddress } }],
    };
  }

  const signClause: SendLinePart = { key: "compose.send.willSign" };

  // No recipients yet. "Will sign" is true; "keys held for" would have nobody to
  // name, and claiming the seal tone over an empty recipient list would make the
  // line go green before the message has anywhere to go.
  if (chips.length === 0) return { tone: "plain", parts: [signClause] };

  const without = chips.filter((c) => c.fact !== "held");

  // 1. Everyone's key is in hand.
  if (without.length === 0) {
    return {
      tone: "seal",
      parts: [
        signClause,
        { key: "compose.send.keysHeldFor", params: { names: joinNames(chips.map((c) => c.name)) } },
      ],
    };
  }

  // 2. Somebody's is not — named, and the consequence stated.
  return {
    tone: "warn",
    parts: [
      signClause,
      { key: "compose.send.noKeyFor", params: { names: joinNames(without.map((c) => c.name)) } },
      {
        key:
          chips.length === 1
            ? "compose.send.plainToAll.one"
            : "compose.send.plainToAll.other",
        params: { count: chips.length },
      },
    ],
  };
}
