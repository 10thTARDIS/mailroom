// SPDX-License-Identifier: AGPL-3.0-only

// THE DESK'S KEY-EXPIRY BANNER (#795). core/keyWarnings.ts decides which keys
// to warn about and which warnings were put away; this reads the keys, renders
// one banner per warning, and remembers a dismissal.
//
// "Your key for you@example.com expires on 2 Nov. Extend it" — the link opens
// Settings → Keys, where Extend is. An expired BOUND key says so instead, and
// comes back even if the "expires on" warning was dismissed: it is a new fact.
//
// READ ONCE PER DESK VISIT. The keys are read when the desk mounts and again
// when the user comes back from Settings (where they may just have extended
// the key), never on a timer: the dates move on the scale of days.

import { useEffect, useState } from "react";
import type { JSX } from "react";

import { dismissWarning, loadDismissedWarnings, ownKeyWarnings, visibleWarnings } from "../../../core/keyWarnings";
import type { KeyExpiryWarning } from "../../../core/keyWarnings";
import { keyDateShort } from "../../../core/userKeys";
import { groupFingerprint } from "../../../core/pgpSignature";
import type { UserKey } from "../../../core/jmap/types";
import { t } from "../../i18n";
import { navigate, useRoute } from "../../routing";
import { useApp, useMailState } from "../../state";

/** The words for one warning. */
export function keyWarningText(w: KeyExpiryWarning, now: Date): string {
  const who = w.address ?? groupFingerprint(w.fingerprint).slice(-9);
  if (w.state === "expired") {
    return w.at
      ? t(w.address ? "keyWarning.expiredOn" : "keyWarning.expiredOnKey", { who, date: keyDateShort(w.at, now) })
      : t(w.address ? "keyWarning.expired" : "keyWarning.expiredKey", { who });
  }
  return w.at
    ? t(w.address ? "keyWarning.expiresOn" : "keyWarning.expiresOnKey", { who, date: keyDateShort(w.at, now) })
    : t(w.address ? "keyWarning.expiresSoon" : "keyWarning.expiresSoonKey", { who });
}

export function KeyExpiryBanners({
  keys,
  now = new Date(),
}: {
  /** null: not read (or cannot be): nothing is shown. */
  keys: readonly UserKey[] | null;
  now?: Date;
}): JSX.Element | null {
  const { client } = useApp();
  const state = useMailState();
  const [dismissed, setDismissed] = useState<Set<string>>(() => loadDismissedWarnings());
  if (keys === null) return null;
  const warnings = ownKeyWarnings(keys, Object.values(state.identities), client.pgpKeyExpiryWarningDays(), now);
  const shown = visibleWarnings(warnings, dismissed);
  if (shown.length === 0) return null;
  return (
    <>
      {shown.map((w) => (
        <div key={`${w.keyId}:${w.state}`} className="banner banner-warn key-expiry-banner">
          <p role="status">{keyWarningText(w, now)}</p>
          <div className="key-expiry-banner-actions">
            <button
              type="button"
              className="link-button"
              onClick={() => navigate({ page: "settings", section: "keys" })}
            >
              {w.state === "expired" ? t("keyWarning.openExpired") : t("keyWarning.open")}
            </button>
            <button
              type="button"
              className="link-button"
              aria-label={t("keyWarning.dismissFor", { who: w.address ?? groupFingerprint(w.fingerprint).slice(-9) })}
              onClick={() => setDismissed(dismissWarning(w, warnings))}
            >
              {t("keyWarning.dismiss")}
            </button>
          </div>
        </div>
      ))}
    </>
  );
}

/**
 * The user's keys for the banner: read when the desk mounts and each time the
 * route comes back from Settings. null until read, and null for good on a
 * server without the keys API (the banner then says nothing, which is true).
 */
export function useOwnKeysForBanner(): readonly UserKey[] | null {
  const { client } = useApp();
  const state = useMailState();
  const route = useRoute();
  const inSettings = route.page === "settings";
  const [keys, setKeys] = useState<readonly UserKey[] | null>(null);
  const accountId = state.accountId;
  useEffect(() => {
    if (!accountId || inSettings) return;
    let cancelled = false;
    // Through a promise chain with a catch: a banner is never a reason for the
    // desk to fail, whatever the client in hand can or cannot do.
    void Promise.resolve()
      .then(() => client.userKeysRead())
      .catch(() => null)
      .then((r) => {
        if (!cancelled) setKeys(r === null ? null : r.keys);
      });
    return () => {
      cancelled = true;
    };
  }, [client, accountId, inSettings]);
  return keys;
}
