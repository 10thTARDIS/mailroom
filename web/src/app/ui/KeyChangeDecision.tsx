// SPDX-License-Identifier: AGPL-3.0-only

// The key-changed decision (#47, #626 section E, ADR-039): a sender's key is not
// the one the user had. Shown in the reading pane under a signed message, and in
// compose where an Encrypt switch is withheld because a recipient's key changed -
// the same component, so the two cannot say different things.
//
// WHAT IT SAYS, IN ORDER: which key it is (both fingerprints, grouped in fours,
// labelled in words), what that can mean (one sentence, with both readings), the
// two decisions, and a way to compare the keys. Nothing here is conveyed by colour
// alone: every state is a sentence, every control is a real button.
//
// THE DECISION IS NOT MADE HERE. `onDecide` is the caller's; it answers whether
// the server took it, and the caller re-renders from the server's new state, so
// this component never claims a decision that was not recorded.

import { useState } from "react";
import type { JSX } from "react";

import { stripBidiControls } from "../../core/hostileText";
import { groupFingerprint } from "../../core/pgpSignature";
import { t } from "../i18n";

export function KeyChangeDecision({
  address,
  fingerprint,
  previousFingerprint,
  refused = false,
  heading,
  onDecide,
}: {
  address: string;
  /** The NEW key, raw. */
  fingerprint: string;
  /** The key it differs from, raw. */
  previousFingerprint: string;
  /** The user already chose to keep the old key. */
  refused?: boolean;
  /** The sentence that opens the block. It is the caller's, because what it is
   * about (this message's signature, a recipient's key) is the caller's. */
  heading: string;
  onDecide: (decision: "trust" | "keep") => Promise<boolean>;
}): JSX.Element {
  const [busy, setBusy] = useState(false);
  // The address is the sender's own text, quoted back inside a warning about them.
  const shown = stripBidiControls(address);

  const decide = async (decision: "trust" | "keep"): Promise<void> => {
    if (busy) return;
    setBusy(true);
    try {
      await onDecide(decision);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="key-change" role="group" aria-label={t("thread.signature.region")}>
      <p className="key-change-heading" role="status">
        <strong>{heading}</strong> {t("thread.signature.keyChangedWhy", { address: shown })}
      </p>
      <dl className="key-change-keys">
        <dt>{t("thread.signature.before")}</dt>
        <dd>
          <code>{groupFingerprint(previousFingerprint)}</code>
        </dd>
        <dt>{t("thread.signature.now")}</dt>
        <dd>
          <code>{groupFingerprint(fingerprint)}</code>
        </dd>
      </dl>
      <div className="key-change-actions">
        <button type="button" disabled={busy} onClick={() => void decide("trust")}>
          {refused ? t("thread.signature.trustAnyway") : t("thread.signature.trust")}
        </button>
        {refused ? null : (
          <button type="button" disabled={busy} onClick={() => void decide("keep")}>
            {t("thread.signature.keep")}
          </button>
        )}
        {busy ? <span role="status">{t("thread.signature.deciding")}</span> : null}
      </div>
      <details className="key-change-compare">
        <summary>{t("thread.signature.compare")}</summary>
        <p>{t("thread.signature.compareHelp", { address: shown })}</p>
      </details>
    </div>
  );
}
