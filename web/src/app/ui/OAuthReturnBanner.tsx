// SPDX-License-Identifier: AGPL-3.0-only

// WHAT HAPPENED AT THE PROVIDER (#52). The connect flow's callback sends the
// browser back to `/?oauth=<code>&provider=<p>[&account=<id>]#/settings`; this
// says, in plain words, which of the eighteen outcomes it was, once.
//
// The query is taken off the URL on the first read (oauthReturn.ts), so a
// reload does not repeat the message. The text is set AFTER mount into a
// region that already exists: a live region inserted together with its words
// is not reliably announced (web/ACCESSIBILITY.md). Polite even for a
// failure: the person has just come back from the provider and is looking at
// the page, and an assertive interruption would add nothing.
//
// `already_connected` offers Reconnect for the account the server named.

import { useEffect, useState } from "react";
import type { JSX } from "react";

import { oauthResultTone } from "../../core/oauth";
import type { OAuthReturn } from "../../core/oauth";
import { t } from "../i18n";
import { useApp } from "../state";
import { startOAuth, takeOAuthReturn } from "./oauthReturn";
import { oauthResultText, startErrorText } from "./oauthWords";

export function OAuthReturnBanner(): JSX.Element {
  const { client } = useApp();
  const [result, setResult] = useState<OAuthReturn | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setResult(takeOAuthReturn());
  }, []);

  const tone = result === null ? null : oauthResultTone(result.code);
  const reconnect = (): void => {
    if (result === null || result.provider === null || result.account === null) return;
    setBusy(true);
    setError(null);
    void startOAuth(client, result.provider, result.account).then((err) => {
      setBusy(false);
      if (err !== null) setError(startErrorText(err, result.provider));
    });
  };

  return (
    <div className="oauth-result-region" role="status">
      {result === null ? null : (
        <div
          className={`banner ${tone === "error" ? "banner-error" : tone === "info" ? "banner-warn" : "is-ok"} oauth-result`}
        >
          <p>{oauthResultText(result)}</p>
          {error === null ? null : <p>{error}</p>}
          <div className="oauth-result-actions">
            {result.code === "already_connected" && result.provider !== null && result.account !== null ? (
              <button type="button" className="link-button" disabled={busy} onClick={reconnect}>
                {t("oauth.result.reconnect")}
              </button>
            ) : null}
            <button
              type="button"
              className="link-button"
              aria-label={t("oauth.result.dismissLabel")}
              onClick={() => setResult(null)}
            >
              {t("oauth.result.dismiss")}
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
