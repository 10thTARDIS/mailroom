// SPDX-License-Identifier: AGPL-3.0-only

// IMPORT YOUR OWN KEY (#796): a private key exported from another mail app
// (armored text, or a binary file sent as base64), and its passphrase if it
// has one. The server hands both to the crypto process and keeps neither.
//
// ⚠ THE KEY TEXT AND THE PASSPHRASE ARE THIS DIALOG'S STATE ONLY, cleared as
// soon as they are sent and dropped with the dialog. They are never logged
// and never put anywhere that outlives it.

import { useId, useState } from "react";
import type { JSX } from "react";

import { MAX_KEY_BYTES, keyFileToText } from "../../../../core/keyImport";
import type { KeyActionError } from "../../../../core/userKeys";
import { t } from "../../../i18n";
import { KeyDialog } from "./KeyDialog";
import { keyErrorText } from "./text";

export function ImportOwnKeyDialog({
  onImport,
  onDone,
  onClose,
}: {
  onImport: (
    privateKey: string,
    passphrase: string,
  ) => Promise<{ ok: true; value: { alreadyStored: boolean } } | { ok: false; error: KeyActionError }>;
  /** Told whether the key was already held, once imported. */
  onDone: (alreadyStored: boolean) => void;
  onClose: () => void;
}): JSX.Element {
  const [text, setText] = useState("");
  const [pass, setPass] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<KeyActionError | "empty" | "fileUnreadable" | null>(null);
  const textId = useId();
  const fileId = useId();
  const passId = useId();

  const readFile = async (file: File | undefined): Promise<void> => {
    if (!file) return;
    setError(null);
    if (file.size > 2 * MAX_KEY_BYTES) {
      setError("tooLarge");
      return;
    }
    try {
      setText(keyFileToText(new Uint8Array(await file.arrayBuffer())));
    } catch {
      setError("fileUnreadable");
    }
  };

  const submit = async (): Promise<void> => {
    if (busy) return;
    if (text.trim() === "") {
      setError("empty");
      return;
    }
    const key = text;
    const passphrase = pass;
    // Cleared at once: a refusal asks for them again rather than keeping them.
    setText("");
    setPass("");
    setBusy(true);
    setError(null);
    const out = await onImport(key, passphrase);
    setBusy(false);
    if (out.ok) {
      onDone(out.value.alreadyStored);
      onClose();
    } else {
      setError(out.error);
    }
  };

  const errorText =
    error === "empty"
      ? t("keys.importOwn.empty")
      : error === "fileUnreadable"
        ? t("keyImport.error.fileUnreadable")
        : error
          ? keyErrorText(error)
          : null;

  return (
    <KeyDialog title={t("keys.importOwn.title")} busy={busy} onClose={onClose}>
      <form
        onSubmit={(ev) => {
          ev.preventDefault();
          void submit();
        }}
      >
        <p>{t("keys.importOwn.explain")}</p>
        <label htmlFor={textId} className="field-label">
          {t("keys.importOwn.keyLabel")}
        </label>
        <textarea
          id={textId}
          className="import-key-text"
          rows={6}
          spellCheck={false}
          autoComplete="off"
          value={text}
          disabled={busy}
          onChange={(ev) => setText(ev.target.value)}
        />
        <label htmlFor={fileId} className="field-label">
          {t("keys.importOwn.fileLabel")}
        </label>
        <input
          id={fileId}
          type="file"
          accept=".asc,.pgp,.gpg,.key,application/pgp-keys"
          disabled={busy}
          onChange={(ev) => {
            const input = ev.currentTarget;
            void readFile(input.files?.[0]).finally(() => {
              input.value = "";
            });
          }}
        />
        <label htmlFor={passId} className="field-label">
          {t("keys.importOwn.passphrase")}
        </label>
        <input
          id={passId}
          className="text-input"
          type="password"
          autoComplete="off"
          value={pass}
          disabled={busy}
          onChange={(ev) => setPass(ev.target.value)}
        />
        {errorText ? (
          <p className="field-error" role="alert">
            {errorText}
          </p>
        ) : null}
        {busy ? (
          <p className="field-hint" role="status">
            {t("keys.importOwn.busy")}
          </p>
        ) : null}
        <div className="sheet-actions">
          <button type="button" className="button" disabled={busy} onClick={onClose}>
            {t("action.cancel")}
          </button>
          <button type="submit" className="button button-primary" disabled={busy}>
            {t("keys.importOwn.submit")}
          </button>
        </div>
      </form>
    </KeyDialog>
  );
}
