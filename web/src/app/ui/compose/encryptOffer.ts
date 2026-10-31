// SPDX-License-Identifier: AGPL-3.0-only

// THE ENCRYPT SWITCH (#47, ADR-038): whether the composer may OFFER "Encrypt", and
// if not, why — in terms the UI can print.
//
// ⚠ AN OFFER, NEVER A PROMISE. The server is the authority: it re-resolves every
// key when the message is submitted and refuses (`cannotEncrypt`) rather than
// send in the clear. This function only decides whether to show the switch, and
// it errs toward NOT showing it. It never turns an unknown into a "yes": a
// recipient the lookup has not answered for is "pending", and the switch is off
// until it has.
//
// THE REASONS ARE A CLOSED SET, checked in order, because a user fixes them in
// order: the server cannot encrypt at all > you have no key of your own > a Bcc
// recipient > somebody's key changed > somebody has no key > still asking.

import type { RecipientKeyInfo } from "../../../core/jmap/types";

export type EncryptWhy =
  | "ok"
  | "unsupported" // the server does not advertise urn:mailroom:pgp {encrypt: true}
  | "noOwnKey" // the sending identity has no signing key bound
  | "bcc" // a Bcc recipient: one ciphertext would reveal their key id
  | "noRecipients"
  | "changed" // a recipient's key differs from the one last encrypted to
  | "missing" // a recipient has no usable key
  | "pending"; // the lookup has not answered for everyone yet

export interface EncryptOffer {
  /** True only when EVERY To/Cc recipient has a usable key and nothing else forbids it. */
  offered: boolean;
  why: EncryptWhy;
  /** Recipients without a usable key (why === "missing"). */
  missing: string[];
  /** Recipients whose key changed (why === "changed"). */
  changed: string[];
}

export interface EncryptOfferInput {
  /** `client.pgpEncrypt()`: null = capability absent (cannot ask). */
  serverCanEncrypt: boolean | null;
  /** `willSign(identity, signing)`: the identity has a bound key AND the server signs. */
  ownKey: boolean | null;
  /** The sending address, lower-cased. A message to yourself needs no lookup:
   * the server encrypts to your own key anyway. */
  ownAddress: string;
  to: readonly string[];
  cc: readonly string[];
  bcc: readonly string[];
  /** Lookup answers keyed by lower-cased address; absent = not answered yet. */
  keys: Readonly<Record<string, RecipientKeyInfo | undefined>>;
}

const norm = (a: string): string => a.trim().toLowerCase();

export function encryptOffer(input: EncryptOfferInput): EncryptOffer {
  const none = (why: EncryptWhy, extra?: Partial<EncryptOffer>): EncryptOffer => ({
    offered: false,
    why,
    missing: [],
    changed: [],
    ...extra,
  });
  if (input.serverCanEncrypt !== true) return none("unsupported");
  if (input.ownKey !== true) return none("noOwnKey");
  if (input.bcc.some((a) => norm(a) !== "")) return none("bcc");

  const own = norm(input.ownAddress);
  const seen = new Set<string>();
  const recipients: string[] = [];
  for (const a of [...input.to, ...input.cc]) {
    const n = norm(a);
    if (n === "" || seen.has(n)) continue;
    seen.add(n);
    recipients.push(n);
  }
  if (recipients.length === 0) return none("noRecipients");

  const missing: string[] = [];
  const changed: string[] = [];
  let pending = false;
  for (const n of recipients) {
    if (n === own) continue;
    const info = input.keys[n];
    if (info === undefined) {
      pending = true;
    } else if (info.keyChanged) {
      changed.push(n);
    } else if (!info.usable) {
      missing.push(n);
    }
  }
  if (changed.length > 0) return none("changed", { changed, missing });
  if (missing.length > 0) return none("missing", { missing });
  if (pending) return none("pending");
  return { offered: true, why: "ok", missing: [], changed: [] };
}

/** The addresses the composer must ask the server about: To and Cc, minus the
 * sender and duplicates. Bcc is never asked about: encryption with Bcc is refused
 * outright, and asking would send a hidden recipient's address to a key lookup
 * that may go to the network. */
export function addressesToLookUp(
  to: readonly string[],
  cc: readonly string[],
  ownAddress: string,
): string[] {
  const own = norm(ownAddress);
  const out: string[] = [];
  const seen = new Set<string>();
  for (const a of [...to, ...cc]) {
    const n = norm(a);
    if (n === "" || n === own || seen.has(n)) continue;
    seen.add(n);
    out.push(n);
  }
  return out;
}
