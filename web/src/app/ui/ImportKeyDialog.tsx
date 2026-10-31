// SPDX-License-Identifier: AGPL-3.0-only

// Import a correspondent's public key (#47, ADR-038): the dialog behind "Import
// their key..." in compose and "Import a key for this sender..." on the signature
// line. One component for both, so they cannot say different things.
//
// TWO STEPS, because the browser has no OpenPGP parser and the one thing worth
// seeing before saying yes is WHICH KEY it is. Step 1 sends the pasted key with
// `dryRun` and the server reads it, applies every rule (public only, names the
// address, can encrypt, not expired, one key) and answers with the fingerprint and
// the user ID. Step 2 shows both, in fours, next to the sentence "compare this with
// what they gave you another way", and only its button stores anything. The text
// that was checked is the text that is imported (kept in state), so the key
// confirmed is the key stored.
//
// THE DIALOG DOES NOT TOUCH THE SERVER ITSELF. `check` and `onImport` are the
// caller's and answer with an outcome, so a refusal is shown here, where the user
// is looking, and the caller decides what to refresh after a success.
//
// Modal, so focus is trapped (the live list behind it has single-key verbs);
// Escape and the backdrop close; focus starts in the text box.

import { useEffect, useId, useRef, useState } from "react";
import type { JSX } from "react";

import { stripBidiControls } from "../../core/hostileText";
import { MAX_KEY_BYTES, keyFileToText } from "../../core/keyImport";
import type { ImportKeyError, ImportKeyOutcome } from "../../core/keyImport";
import { groupFingerprint } from "../../core/pgpSignature";
import { t } from "../i18n";
import type { MessageKey } from "../i18n";
import { trapTab } from "./focusTrap";

const ERROR_TEXT: Record<ImportKeyError | "fileUnreadable", MessageKey> = {
  empty: "keyImport.error.empty",
  secretKeyRefused: "keyImport.error.secretKeyRefused",
  keyDoesNotNameAddress: "keyImport.error.keyDoesNotNameAddress",
  noEncryptionSubkey: "keyImport.error.noEncryptionSubkey",
  keyExpiredOrRevoked: "keyImport.error.keyExpiredOrRevoked",
  invalidKey: "keyImport.error.invalidKey",
  tooLarge: "keyImport.error.tooLarge",
  ownAddress: "keyImport.error.ownAddress",
  unavailable: "keyImport.error.unavailable",
  failed: "keyImport.error.failed",
  fileUnreadable: "keyImport.error.fileUnreadable",
};

export function ImportKeyDialog({
  address,
  check,
  onImport,
  onImported,
  onClose,
}: {
  /** The address the key is for. */
  address: string;
  /** Validate without storing (ContactKey/import with dryRun). */
  check: (publicKey: string) => Promise<ImportKeyOutcome>;
  /** Store (ContactKey/import). */
  onImport: (publicKey: string) => Promise<ImportKeyOutcome>;
  /** Called once the key is stored, before the dialog closes. */
  onImported: () => void;
  onClose: () => void;
}): JSX.Element {
  const [text, setText] = useState("");
  const [checked, setChecked] = useState<{ publicKey: string; fingerprint: string; userId: string } | null>(null);
  const [error, setError] = useState<ImportKeyError | "fileUnreadable" | null>(null);
  const [busy, setBusy] = useState<"checking" | "importing" | null>(null);
  const dialogRef = useRef<HTMLDivElement | null>(null);
  const textRef = useRef<HTMLTextAreaElement | null>(null);
  const confirmRef = useRef<HTMLButtonElement | null>(null);
  const textId = useId();
  const fileId = useId();
  const shown = stripBidiControls(address);

  useEffect(() => {
    textRef.current?.focus();
  }, []);
  // Step 2 opens with focus on its own heading's button so the keyboard does not
  // land on a control that has just been removed.
  useEffect(() => {
    if (checked) confirmRef.current?.focus();
    else textRef.current?.focus();
  }, [checked]);

  const runCheck = async (): Promise<void> => {
    if (busy) return;
    setError(null);
    setBusy("checking");
    try {
      const out = await check(text);
      if (out.ok) setChecked({ publicKey: text, fingerprint: out.fingerprint, userId: out.userId });
      else setError(out.error);
    } finally {
      setBusy(null);
    }
  };

  const runImport = async (): Promise<void> => {
    if (busy || !checked) return;
    setError(null);
    setBusy("importing");
    try {
      const out = await onImport(checked.publicKey);
      if (out.ok) {
        onImported();
        onClose();
      } else {
        setError(out.error);
      }
    } finally {
      setBusy(null);
    }
  };

  const readFile = async (file: File | undefined): Promise<void> => {
    if (!file) return;
    setError(null);
    // An armored key is at most about 4/3 of the cap; a file far over that is not a key.
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

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(ev) => {
        if (ev.target === ev.currentTarget) onClose();
      }}
      onKeyDown={(ev) => {
        if (ev.key === "Escape") {
          ev.stopPropagation();
          onClose();
          return;
        }
        trapTab(dialogRef.current, ev);
      }}
    >
      <div
        className="sheet import-key"
        role="dialog"
        aria-modal="true"
        aria-label={t("keyImport.title", { address: shown })}
        ref={dialogRef}
      >
        <header className="sheet-header">
          <h2>{t("keyImport.title", { address: shown })}</h2>
          <button type="button" className="icon-button" aria-label={t("action.close")} onClick={onClose}>
            ✕
          </button>
        </header>
        <div className="sheet-body">
          {checked ? (
            <>
              <h3 className="import-key-heading">{t("keyImport.confirmHeading")}</h3>
              <dl className="key-change-keys">
                <dt>{t("keyImport.fingerprint")}</dt>
                <dd>
                  <code>{groupFingerprint(checked.fingerprint)}</code>
                </dd>
                <dt>{t("keyImport.userId")}</dt>
                <dd>{stripBidiControls(checked.userId)}</dd>
              </dl>
              <p className="field-hint">{t("keyImport.confirmHelp", { address: shown })}</p>
            </>
          ) : (
            <>
              <p>{t("keyImport.intro", { address: shown })}</p>
              <label htmlFor={textId}>{t("keyImport.pasteLabel")}</label>
              <textarea
                id={textId}
                ref={textRef}
                className="import-key-text"
                rows={8}
                spellCheck={false}
                autoComplete="off"
                value={text}
                onChange={(ev) => setText(ev.target.value)}
              />
              <label htmlFor={fileId}>{t("keyImport.fileLabel")}</label>
              <input
                id={fileId}
                type="file"
                accept=".asc,.pgp,.gpg,.key,application/pgp-keys"
                onChange={(ev) => {
                  const input = ev.currentTarget;
                  const file = input.files?.[0];
                  void readFile(file).finally(() => {
                    input.value = "";
                  });
                }}
              />
            </>
          )}
          {error ? (
            <p className="field-error" role="alert">
              {t(ERROR_TEXT[error], { address: shown })}
            </p>
          ) : null}
          {busy ? (
            <p className="field-hint" role="status">
              {busy === "checking" ? t("keyImport.checking") : t("keyImport.importing")}
            </p>
          ) : null}
          <div className="sheet-actions">
            {checked ? (
              <>
                <button
                  type="button"
                  className="button"
                  disabled={busy !== null}
                  onClick={() => {
                    setChecked(null);
                    setError(null);
                  }}
                >
                  {t("keyImport.back")}
                </button>
                <button
                  type="button"
                  className="button button-primary"
                  disabled={busy !== null}
                  ref={confirmRef}
                  onClick={() => void runImport()}
                >
                  {t("keyImport.import")}
                </button>
              </>
            ) : (
              <>
                <button type="button" className="button" onClick={onClose}>
                  {t("action.cancel")}
                </button>
                <button
                  type="button"
                  className="button button-primary"
                  disabled={busy !== null}
                  onClick={() => void runCheck()}
                >
                  {t("keyImport.check")}
                </button>
              </>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
