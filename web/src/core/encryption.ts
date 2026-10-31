// SPDX-License-Identifier: AGPL-3.0-only

// What the reading pane says about an encrypted message (#47 increment 6b,
// ADR-037). Kept out of the JSX and under test, the same split as
// senderAuthNotice and senderWarnings.
//
// ⚠ HONEST WORDING IS THE REQUIREMENT, not decoration. The server holds the
// key and decrypts the message to show it (ADR-024: Mailroom is NOT end-to-end
// encrypted), so nothing here may say or imply "only you can read this". What
// IS true, and worth saying: the message travelled encrypted, this server
// decrypted it to show it, and the decrypted text is not stored.
//
// The signature, inside or outside the encryption, is core/pgpSignature.ts.

import { isExpiringSoon } from "./userKeys";
import type { RecipientKeyInfo } from "./jmap/types";

export type EncryptionNotice = { kind: "decrypted" } | { kind: "failed" };

/** The notice for one message, or null when there is nothing to say (not
 * encrypted, or the body was not asked for so nothing was attempted). */
export function encryptionNotice(email: {
  encrypted?: boolean;
  decrypted?: boolean | null;
}): EncryptionNotice | null {
  if (email.encrypted !== true) return null;
  if (email.decrypted === false) return { kind: "failed" };
  if (email.decrypted !== true) return null;
  return { kind: "decrypted" };
}

// ---------------------------------------------------------------------------
// Recipients' keys and their dates (#795)
// ---------------------------------------------------------------------------
//
// Compose asks RecipientKey/lookup about every To/Cc address. Two things about
// those answers are worth a sentence of their own rather than the generic
// "no key for ...":
//
//   * Encrypt IS offered, and a recipient's key is inside the server's warning
//     window: "Alice's key expires on 2 Nov". This send works; one after that
//     date will not, unless Alice renews it.
//   * Encrypt is NOT offered because the recipient's key has lapsed: "Alice's
//     key expired on 18 Oct" / "Alice's key was revoked". That tells the user
//     what to ask Alice for, where "no key" sends them looking for a key they
//     already have.
//
// The flags are the server's (`expiringSoon`, `reason`), which use the
// capability's keyExpiryWarningDays. A server that sent a date and no flag is
// judged against that same threshold (core/userKeys.ts isExpiringSoon), and a
// server that sent neither gets no date sentence at all.

export type RecipientKeyNotice =
  | { kind: "expiresSoon"; address: string; at: string }
  | { kind: "expired"; address: string; at: string | null }
  | { kind: "revoked"; address: string };

export interface RecipientKeyNotices {
  /** Said beside the Encrypt switch when it is offered. */
  expiring: RecipientKeyNotice[];
  /** Said INSTEAD of the generic "no key" for these addresses. */
  lapsed: RecipientKeyNotice[];
  /** The missing addresses the generic sentence still covers. */
  otherMissing: string[];
}

export function recipientKeyNotices(
  offer: { offered: boolean; missing: readonly string[] },
  recipients: readonly string[],
  keys: Readonly<Record<string, RecipientKeyInfo | undefined>>,
  warnDays: number | null,
  now: Date,
): RecipientKeyNotices {
  const out: RecipientKeyNotices = { expiring: [], lapsed: [], otherMissing: [] };
  if (offer.offered) {
    const seen = new Set<string>();
    for (const raw of recipients) {
      const a = raw.trim().toLowerCase();
      if (a === "" || seen.has(a)) continue;
      seen.add(a);
      const info = keys[a];
      if (!info || !info.usable || !info.expiresAt) continue;
      if (isExpiringSoon(info, warnDays, now)) {
        out.expiring.push({ kind: "expiresSoon", address: a, at: info.expiresAt });
      }
    }
    return out;
  }
  for (const a of offer.missing) {
    const info = keys[a];
    if (info?.reason === "expired") out.lapsed.push({ kind: "expired", address: a, at: info.expiredAt ?? null });
    else if (info?.reason === "revoked") out.lapsed.push({ kind: "revoked", address: a });
    else out.otherMissing.push(a);
  }
  return out;
}
