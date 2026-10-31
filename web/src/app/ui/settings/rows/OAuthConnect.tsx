// SPDX-License-Identifier: AGPL-3.0-only

// "Connect Gmail" / "Connect Outlook" (#52, ADR-046 §3 and §8) — offered only
// for the providers the server's `oauthProviders` capability lists, so an
// instance whose operator configured neither shows nothing at all.
//
// THE HONESTY INTERSTITIAL comes before the redirect, and it is not a formality
// (ADR-046 §8 "consent honesty"): the scope Google grants for IMAP is full
// control of the mailbox, and whoever runs the server can use it. So the dialog
// says that in plain words, says why Google will call the app unverified, and,
// for Microsoft, says the two things that most often stop a connect — IMAP
// switched off on Outlook.com, and a work tenant that needs its administrator's
// approval. Only then does Continue call Account/oauthStart and leave for the
// provider.
//
// A real modal, in the shell every settings dialog uses (keys/KeyDialog.tsx):
// named by its title, focus trapped (a Tab must not reach the page behind it),
// Escape and the backdrop cancel, Cancel takes focus first, focus returns to
// the button that opened it, and it is portalled above the settings sheet so
// it stays on screen when the page has scrolled — on a phone too.

import { useRef, useState } from "react";
import type { JSX } from "react";

import { isOAuthProvider } from "../../../../core/oauth";
import type { OAuthProvider } from "../../../../core/oauth";
import { t } from "../../../i18n";
import type { MessageKey } from "../../../i18n";
import { useApp } from "../../../state";
import { startOAuth } from "../../oauthReturn";
import { providerName, startErrorText } from "../../oauthWords";
import { KeyDialog } from "../keys/KeyDialog";

export function OAuthConnectDialog({
  provider,
  onClose,
}: {
  provider: OAuthProvider;
  onClose: () => void;
}): JSX.Element {
  const { client } = useApp();
  const cancelRef = useRef<HTMLButtonElement | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const name = providerName(provider);

  const go = (): void => {
    setBusy(true);
    setError(null);
    void startOAuth(client, provider).then((err) => {
      // On success the page is leaving; staying busy keeps a second click
      // from starting a second flow.
      if (err === null) return;
      setBusy(false);
      setError(startErrorText(err, provider));
    });
  };

  return (
    <KeyDialog
      title={t(`oauth.dialog.title.${provider}` as MessageKey)}
      className="oauth-connect"
      busy={busy}
      onClose={onClose}
      initialFocus={cancelRef}
    >
      <p>{t("oauth.dialog.access")}</p>
      {provider === "google" ? <p>{t("oauth.dialog.google.unverified")}</p> : null}
      {provider === "microsoft" ? (
        <>
          <p>{t("oauth.dialog.microsoft.imap")}</p>
          <p>{t("oauth.dialog.microsoft.work")}</p>
        </>
      ) : null}
      <p>{t("oauth.dialog.revoke", { provider: name })}</p>
      {error === null ? null : (
        <p className="banner banner-error" role="alert">
          {error}
        </p>
      )}
      <div className="sheet-actions">
        <button type="button" className="button" onClick={onClose} disabled={busy} ref={cancelRef}>
          {t("action.cancel")}
        </button>
        <button type="button" className="button button-primary" disabled={busy} onClick={go}>
          {busy ? t("oauth.dialog.working", { provider: name }) : t("oauth.dialog.continue", { provider: name })}
        </button>
      </div>
    </KeyDialog>
  );
}

/** The Connect buttons: one per provider the server offers, nothing when it
 * offers none. */
export function OAuthConnectButtons(): JSX.Element | null {
  const { client } = useApp();
  const [open, setOpen] = useState<OAuthProvider | null>(null);
  const providers = client.oauthProviders().filter(isOAuthProvider);
  if (providers.length === 0) return null;
  // Focus goes back to the opener when the dialog unmounts (KeyDialog).
  const close = (): void => setOpen(null);

  return (
    <div className="setting-oauth-connect" role="group" aria-label={t("oauth.connect.group")}>
      <p className="field-hint">{t("oauth.connect.explain")}</p>
      <div className="setting-oauth-buttons">
        {providers.map((p) => (
          <button key={p} type="button" className="button" onClick={() => setOpen(p)}>
            {t(`oauth.connect.${p}` as MessageKey)}
          </button>
        ))}
      </div>
      {open === null ? null : <OAuthConnectDialog provider={open} onClose={close} />}
    </div>
  );
}
