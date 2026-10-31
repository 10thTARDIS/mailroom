// SPDX-License-Identifier: AGPL-3.0-only

// BACK UP (#796): the key, its secret half wrapped under a passphrase the user
// chooses now, downloaded as a file.
//
// ⚠ NOTHING OUTLIVES THE DOWNLOAD. The passphrase lives in this dialog's state
// only while the dialog is open and is cleared the moment it is sent; the
// wrapped key comes back in the response, goes straight into a Blob whose URL
// is revoked right after the click (core/userKeys.ts downloadArmoredKey), and
// is never put in state at all. Closing the dialog drops whatever was typed.

import { useId, useState } from "react";
import type { JSX } from "react";

import { MIN_BACKUP_PASSPHRASE, backupPassphraseProblem, downloadArmoredKey } from "../../../../core/userKeys";
import type { KeyActionError } from "../../../../core/userKeys";
import { t } from "../../../i18n";
import { KeyDialog } from "./KeyDialog";
import { keyErrorText } from "./text";

export function BackupKeyDialog({
  address,
  onExport,
  onDone,
  onClose,
  save = downloadArmoredKey,
}: {
  address: string | null;
  onExport: (
    passphrase: string,
  ) => Promise<{ ok: true; value: { armoredKey: string; fileName: string } } | { ok: false; error: KeyActionError }>;
  /** Told the file name once it has downloaded. */
  onDone: (fileName: string) => void;
  onClose: () => void;
  /** Injectable for tests; the default saves through a revoked Blob URL. */
  save?: (armored: string, fileName: string) => void;
}): JSX.Element {
  const [pass, setPass] = useState("");
  const [again, setAgain] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<KeyActionError | null>(null);
  const [tried, setTried] = useState(false);
  const passId = useId();
  const againId = useId();
  const problemId = useId();
  const problem = backupPassphraseProblem(pass, again);

  const submit = async (): Promise<void> => {
    setTried(true);
    if (problem !== null || busy) return;
    const passphrase = pass;
    // Cleared before the request resolves: the only copy left is the argument.
    setPass("");
    setAgain("");
    setBusy(true);
    setError(null);
    const out = await onExport(passphrase);
    setBusy(false);
    if (!out.ok) {
      setError(out.error);
      setTried(false);
      return;
    }
    save(out.value.armoredKey, out.value.fileName);
    onDone(out.value.fileName);
    onClose();
  };

  const problemText =
    tried && problem === "tooShort"
      ? t("keys.backup.tooShort", { min: MIN_BACKUP_PASSPHRASE })
      : tried && problem === "mismatch"
        ? t("keys.backup.mismatch")
        : null;

  return (
    <KeyDialog
      title={address ? t("keys.backup.titleFor", { address }) : t("keys.backup.title")}
      busy={busy}
      onClose={onClose}
    >
      <form
        onSubmit={(ev) => {
          ev.preventDefault();
          void submit();
        }}
      >
        <p>{t("keys.backup.explain")}</p>
        <p className="field-hint">{t("keys.backup.notKept")}</p>
        <label htmlFor={passId} className="field-label">
          {t("keys.backup.passphrase", { min: MIN_BACKUP_PASSPHRASE })}
        </label>
        <input
          id={passId}
          className="text-input"
          type="password"
          autoComplete="new-password"
          value={pass}
          disabled={busy}
          aria-describedby={problemText ? problemId : undefined}
          onChange={(ev) => setPass(ev.target.value)}
        />
        <label htmlFor={againId} className="field-label">
          {t("keys.backup.again")}
        </label>
        <input
          id={againId}
          className="text-input"
          type="password"
          autoComplete="new-password"
          value={again}
          disabled={busy}
          aria-describedby={problemText ? problemId : undefined}
          onChange={(ev) => setAgain(ev.target.value)}
        />
        {problemText ? (
          <p id={problemId} className="field-error" role="alert">
            {problemText}
          </p>
        ) : null}
        {error ? (
          <p className="field-error" role="alert">
            {keyErrorText(error)}
          </p>
        ) : null}
        {busy ? (
          <p className="field-hint" role="status">
            {t("keys.backup.busy")}
          </p>
        ) : null}
        <div className="sheet-actions">
          <button type="button" className="button" disabled={busy} onClick={onClose}>
            {t("action.cancel")}
          </button>
          <button type="submit" className="button button-primary" disabled={busy}>
            {t("keys.backup.submit")}
          </button>
        </div>
      </form>
    </KeyDialog>
  );
}
