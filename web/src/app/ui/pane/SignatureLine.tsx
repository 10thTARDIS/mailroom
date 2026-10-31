// SPDX-License-Identifier: AGPL-3.0-only

// What a PGP signature on the message means (#47, #626 section E, ADR-039), on a
// plain message or inside a decrypted one. Silent for unsigned mail. Every state
// is a sentence (never a colour alone); a changed key adds the decision. The
// words and the judgement are core/pgpSignature.ts, under test.

import { useState } from "react";
import type { JSX } from "react";

import { stripBidiControls } from "../../../core/hostileText";
import { signatureNotice } from "../../../core/pgpSignature";
import { keyDateLong } from "../../../core/userKeys";
import type { PgpSignature } from "../../../core/jmap/types";
import { t } from "../../i18n";
import { useApp } from "../../state";
import { ImportKeyDialog } from "../ImportKeyDialog";
import { KeyChangeDecision } from "../KeyChangeDecision";

export function SignatureLine({
  email,
}: {
  email: { threadId?: string; pgpSignature?: PgpSignature | null };
}): JSX.Element | null {
  const { actions } = useApp();
  const [importing, setImporting] = useState(false);
  const notice = signatureNotice(email.pgpSignature);
  if (!notice) return null;

  // "Import a key for this sender" (#47): offered where a key is the missing or
  // the disputed thing. The server's own `address` for the signature is the
  // address the key must name; without one there is nothing to import for.
  const importAddress =
    notice.state === "unknownKey" || notice.state === "keyChanged" ? (notice.address ?? null) : null;
  const importButton =
    importAddress !== null ? (
      <p className="key-import-action">
        <button type="button" onClick={() => setImporting(true)}>
          {t("thread.signature.importKey")}
        </button>
      </p>
    ) : null;
  const importDialog =
    importing && importAddress !== null ? (
      <ImportKeyDialog
        address={importAddress}
        check={(key) => actions.importContactKey(importAddress, key, { dryRun: true })}
        onImport={(key) => actions.importContactKey(importAddress, key, { threadId: email.threadId })}
        onImported={() => {}}
        onClose={() => setImporting(false)}
      />
    ) : null;

  switch (notice.state) {
    case "verified":
      return (
        <p className="signature-line signature-line-verified" role="status">
          {t("thread.signature.verified", { fingerprint: notice.fingerprint })}
        </p>
      );
    case "bad":
      return (
        <p className="signature-line signature-line-bad" role="status">
          {t("thread.signature.bad")}
        </p>
      );
    case "keyExpired":
      // NEUTRAL (#795): not the warn palette a forgery gets, and not the plain
      // note a verified signature gets. A stale key is neither.
      return (
        <p className="signature-line signature-line-expired" role="status">
          {notice.keyExpiredAt
            ? t(
                notice.validWhenSigned
                  ? "thread.signature.keyExpiredValidWhenSigned"
                  : "thread.signature.keyExpiredOn",
                { date: keyDateLong(notice.keyExpiredAt) },
              )
            : t("thread.signature.keyExpired")}
        </p>
      );
    case "unknownKey":
      return (
        <div className="signature-line signature-line-unknown">
          <p role="status">
            {notice.fingerprint
              ? t("thread.signature.unknownKeyWith", { fingerprint: notice.fingerprint })
              : t("thread.signature.unknownKey")}
          </p>
          {importButton}
          {importDialog}
        </div>
      );
    case "keyChanged":
      return (
        <div className="signature-line signature-line-changed">
          <KeyChangeDecision
            address={notice.address}
            fingerprint={notice.fingerprint}
            previousFingerprint={notice.previousFingerprint}
            refused={notice.refused}
            heading={t(
              notice.refused ? "thread.signature.keyChangedRefused" : "thread.signature.keyChanged",
              { address: stripBidiControls(notice.address) },
            )}
            onDecide={(decision) =>
              actions.decideContactKey(notice.address, notice.fingerprint, decision, email.threadId)
            }
          />
          {importButton}
          {importDialog}
        </div>
      );
  }
}
