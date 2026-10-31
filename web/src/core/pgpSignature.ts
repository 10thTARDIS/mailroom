// SPDX-License-Identifier: AGPL-3.0-only

// What the reading pane says about a PGP signature (#47, #626 section E,
// ADR-039). Kept out of the JSX and under test, the same split as
// senderAuthNotice and encryptionNotice.
//
// ⚠ HONEST WORDING IS THE REQUIREMENT. "Verified" here means: the content matches
// a key Mailroom holds for this sender address, that key names the address, and it
// is a key the user may rely on. It is NOT a statement that the person is who they
// say - and a changed key is exactly the case where that matters, so it is said in
// a sentence rather than left to a colour.

import type { PgpSignature } from "./jmap/types";

export type SignatureNotice =
  | { state: "verified"; fingerprint: string }
  | { state: "bad" }
  | {
      /** A `bad` with reason keyExpired (#795/#796): the signature checked out
       * and was refused only because the key that made it has expired. A stale
       * key, not a forgery, and said as such; never verified either. */
      state: "keyExpired";
      /** When the key expired (UTCDate), null when the server did not say. */
      keyExpiredAt: string | null;
      /** True when the signature says it was made before that. The date is
       * the signer's own claim, so this only changes the wording. */
      validWhenSigned: boolean;
    }
  | {
      state: "unknownKey";
      fingerprint: string | null;
      /** The From address it was judged against, when the server said (it is
       * what "import a key for this sender" imports for). */
      address?: string;
    }
  | {
      state: "keyChanged";
      address: string;
      /** The full fingerprint of the key that signed THIS message, raw. */
      fingerprint: string;
      /** The one it differs from, raw. */
      previousFingerprint: string;
      /** The user already chose to keep the old key. */
      refused: boolean;
    };

/** The notice for one message, or null when it is not signed. Anything this
 * client does not understand is shown as a signature that could not be checked,
 * never as good news. */
export function signatureNotice(sig: PgpSignature | null | undefined): SignatureNotice | null {
  if (!sig) return null;
  switch (sig.state) {
    case "verified":
      // A "verified" that names no key names nobody; say the weaker thing.
      return sig.fingerprint
        ? { state: "verified", fingerprint: shortFingerprint(sig.fingerprint) }
        : { state: "unknownKey", fingerprint: null };
    case "bad":
      // Only the one reason this client knows softens the wording; any other
      // (or none) is the plain "does not check out".
      if (sig.reason === "keyExpired") {
        return {
          state: "keyExpired",
          keyExpiredAt: sig.keyExpiredAt ?? null,
          validWhenSigned: sig.validWhenSigned === true,
        };
      }
      return { state: "bad" };
    case "unknownKey":
      return {
        state: "unknownKey",
        fingerprint: sig.fingerprint ? shortFingerprint(sig.fingerprint) : null,
        ...(sig.address ? { address: sig.address } : {}),
      };
    case "keyChanged":
      // The decision needs both keys and the address; without them it cannot be
      // offered, and a changed-key warning with no way to act is the unknown case.
      if (!sig.fingerprint || !sig.previousFingerprint || !sig.address) {
        return {
          state: "unknownKey",
          fingerprint: sig.fingerprint ? shortFingerprint(sig.fingerprint) : null,
          ...(sig.address ? { address: sig.address } : {}),
        };
      }
      return {
        state: "keyChanged",
        address: sig.address,
        fingerprint: sig.fingerprint,
        previousFingerprint: sig.previousFingerprint,
        refused: sig.refused === true,
      };
    default:
      return { state: "bad" };
  }
}

/** The last 16 hex digits, grouped in fours - the long key id people compare. */
export function shortFingerprint(fpr: string): string {
  const hex = fpr.replace(/[^0-9A-Fa-f]/g, "").toUpperCase();
  const tail = hex.slice(-16);
  return tail.match(/.{1,4}/g)?.join(" ") ?? tail;
}

/** The whole fingerprint, grouped in fours, for comparing two keys by eye. */
export function groupFingerprint(fpr: string): string {
  const hex = fpr.replace(/[^0-9A-Fa-f]/g, "").toUpperCase();
  return hex.match(/.{1,4}/g)?.join(" ") ?? hex;
}
