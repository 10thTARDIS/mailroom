// SPDX-License-Identifier: AGPL-3.0-only

// "Work mail needs you to sign in again" — across the app, whenever an account's
// OAuth grant needs its user to reconnect (#52, ADR-046 §5; operator decision
// 2026-10-27: a banner, plus one Web Push per transition). The account stops
// syncing, and its changes and outgoing mail wait, until the person signs in
// at the provider again, so it has to be said where they are, not only in
// Settings.
//
// DISMISSIBLE FOR THE SESSION, and for THESE accounts: the dismissal is kept in
// sessionStorage under the set of account ids it was about. Another account
// joining the set is a new fact and brings the banner back; a new session
// shows it again, because the mail is still not arriving.
//
// operator_error is NOT here. That is the server's broken registration, which
// no reconnect can fix; the account's settings say so instead.
//
// The region is always rendered (role="status"), empty while there is nothing
// to say: a live region inserted together with its text is not reliably
// announced (web/ACCESSIBILITY.md).

import { useState } from "react";
import type { JSX } from "react";

import type { Account } from "../../core/jmap/types";
import { accountsNeedingReconnect, reconnectDismissKey } from "../../core/oauth";
import { t } from "../i18n";
import { navigate } from "../routing";
import { useApp } from "../state";
import { startOAuth } from "./oauthReturn";
import { startErrorText } from "./oauthWords";
import { useAccounts } from "./useAccounts";

/** sessionStorage key for the dismissed set. */
export const RECONNECT_DISMISSED_KEY = "mailroom.reconnectBanner.dismissed";

function readDismissed(): string | null {
  try {
    return window.sessionStorage.getItem(RECONNECT_DISMISSED_KEY);
  } catch {
    return null;
  }
}

function writeDismissed(key: string): void {
  try {
    window.sessionStorage.setItem(RECONNECT_DISMISSED_KEY, key);
  } catch {
    /* storage unavailable: the dismissal lasts until the next render tree */
  }
}

export function ReconnectBanner(): JSX.Element {
  const { client } = useApp();
  const { accounts } = useAccounts();
  const [dismissed, setDismissed] = useState<string | null>(() => readDismissed());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const records = accounts.map((e) => e.record).filter((r): r is Account => r !== null);
  const waiting = accountsNeedingReconnect(records);
  const key = reconnectDismissKey(waiting);
  const show = waiting.length > 0 && dismissed !== key;
  const only = waiting.length === 1 ? waiting[0] : undefined;

  const reconnect = (): void => {
    if (!only) {
      navigate({ page: "settings", section: "accounts" });
      return;
    }
    setBusy(true);
    setError(null);
    void startOAuth(client, only.authProvider ?? "", only.id).then((err) => {
      setBusy(false);
      if (err !== null) setError(startErrorText(err, only.authProvider));
    });
  };

  return (
    <div className="reconnect-banner-region" role="status">
      {show ? (
        <div className="banner banner-warn reconnect-banner">
          <p>
            {only
              ? t("oauth.banner.one", { name: only.name })
              : t("oauth.banner.many", { count: waiting.length })}
          </p>
          {error === null ? null : <p>{error}</p>}
          <div className="reconnect-banner-actions">
            <button type="button" className="link-button" disabled={busy} onClick={reconnect}>
              {only ? t("oauth.banner.reconnect") : t("oauth.banner.review")}
            </button>
            <button
              type="button"
              className="link-button"
              aria-label={t("oauth.banner.dismissLabel")}
              onClick={() => {
                writeDismissed(key);
                setDismissed(key);
              }}
            >
              {t("oauth.banner.dismiss")}
            </button>
          </div>
        </div>
      ) : null}
    </div>
  );
}
