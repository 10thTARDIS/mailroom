// SPDX-License-Identifier: AGPL-3.0-only

// THE DESK'S KEY-EXPIRY BANNER (#795): which of your own keys to warn about,
// and which of those warnings you have already put away.
//
// TWO STATES, AND THEY ARE DIFFERENT WARNINGS:
//   expiring  a usable key inside the server's warning window (its own
//             `expiringSoon` flag, which uses the capability's
//             keyExpiryWarningDays). Extending it now costs nothing.
//   expired   a key still BOUND to an address whose packets say it has
//             expired. Mail from that address goes out unsigned and nobody can
//             encrypt to it, so this one is worth saying even after the first
//             warning was dismissed.
// A retired or revoked key is never warned about: it was put away on purpose.
//
// DISMISSAL IS PER KEY PER STATE. Dismissing "expires on 2 Nov" stores
// `<keyId>:expiring`; when the key then expires the warning is `<keyId>:expired`
// and comes back, because it is a new fact. Stored in localStorage (a device
// preference, like the tray's open state), in try/catch: a browser with storage
// disabled loses the memory, never the app.

import { isExpiringSoon } from "./userKeys";
import type { Id, Identity, UserKey } from "./jmap/types";

export interface KeyExpiryWarning {
  keyId: Id;
  state: "expiring" | "expired";
  /** When it expires or expired (UTCDate); null when the server did not say. */
  at: string | null;
  /** The address it is bound to, when it is bound. */
  address: string | null;
  fingerprint: string;
}

export function ownKeyWarnings(
  keys: readonly UserKey[],
  identities: readonly Identity[],
  warnDays: number | null,
  now: Date,
): KeyExpiryWarning[] {
  const out: KeyExpiryWarning[] = [];
  for (const k of keys) {
    if (k.decryptOnly || k.revokedAt !== null) continue;
    const address = k.identityId ? (identities.find((i) => i.id === k.identityId)?.email ?? null) : null;
    if (k.expired === true) {
      // An expired key nobody uses is housekeeping, not a warning: only a
      // BOUND one is still being chosen for an address.
      if (k.identityId === null) continue;
      out.push({ keyId: k.id, state: "expired", at: k.expiresAt ?? null, address, fingerprint: k.fingerprint });
      continue;
    }
    if (k.usable !== false && isExpiringSoon(k, warnDays, now)) {
      out.push({ keyId: k.id, state: "expiring", at: k.expiresAt ?? null, address, fingerprint: k.fingerprint });
    }
  }
  // Expired first (it is already costing something), then soonest first.
  return out.sort((a, b) => {
    if (a.state !== b.state) return a.state === "expired" ? -1 : 1;
    return (a.at ?? "").localeCompare(b.at ?? "");
  });
}

/** The dismissal key for one warning. */
export function warningKey(w: Pick<KeyExpiryWarning, "keyId" | "state">): string {
  return `${w.keyId}:${w.state}`;
}

export const DISMISSED_KEY_WARNINGS = "mailroom.keyWarnings.dismissed";

/** The warnings not yet dismissed. */
export function visibleWarnings(
  warnings: readonly KeyExpiryWarning[],
  dismissed: ReadonlySet<string>,
): KeyExpiryWarning[] {
  return warnings.filter((w) => !dismissed.has(warningKey(w)));
}

type StorageLike = Pick<Storage, "getItem" | "setItem">;

function storageOrNull(storage?: StorageLike): StorageLike | null {
  if (storage) return storage;
  try {
    return typeof localStorage === "undefined" ? null : localStorage;
  } catch {
    return null;
  }
}

export function loadDismissedWarnings(storage?: StorageLike): Set<string> {
  try {
    const raw = storageOrNull(storage)?.getItem(DISMISSED_KEY_WARNINGS);
    if (!raw) return new Set();
    const parsed: unknown = JSON.parse(raw);
    return new Set(Array.isArray(parsed) ? parsed.filter((s): s is string => typeof s === "string") : []);
  } catch {
    return new Set();
  }
}

/**
 * Record one dismissal. Entries for warnings that no longer exist are dropped
 * on the way (`current`), so the list stays as long as the list of keys rather
 * than growing for ever.
 */
export function dismissWarning(
  w: Pick<KeyExpiryWarning, "keyId" | "state">,
  current: readonly KeyExpiryWarning[],
  storage?: StorageLike,
): Set<string> {
  const live = new Set(current.map(warningKey));
  const next = new Set([...loadDismissedWarnings(storage)].filter((k) => live.has(k)));
  next.add(warningKey(w));
  try {
    storageOrNull(storage)?.setItem(DISMISSED_KEY_WARNINGS, JSON.stringify([...next]));
  } catch {
    /* see the header: the memory is lost, the app is not */
  }
  return next;
}
