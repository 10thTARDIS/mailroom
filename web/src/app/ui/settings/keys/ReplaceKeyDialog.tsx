// SPDX-License-Identifier: AGPL-3.0-only

// REPLACE (#817, ADR-040 §14): a new key for this key's address, put in its
// place in one step, with the old key retired. The dialog says what happens
// before anything does:
//
//   * a new key for <address>, signing from now on;
//   * published in the key directory if this one is (and not otherwise);
//   * the old key stops signing and is taken out of the directory, but still
//     opens mail already encrypted to it;
//   * and, plainly, that a key which may have been stolen is revoked instead:
//     replacing tells nobody the old key is unsafe.
//
// The expiry choices are generate's (NewKeyExpiryField). The confirm button is
// aria-disabled (not disabled) while an "On a date…" date cannot be sent, and
// points at the sentence that says why, as revoke's does.
//
// A FAILURE AFTER THE NEW KEY WAS MADE is its own message: the server leaves
// the old key exactly as it was and the new key unbound, and says so (core/
// keyLifecycle.ts replaceLeftNewKey). The page reloads so that key is listed.

import { useId, useRef, useState } from "react";
import type { JSX } from "react";

import { newKeyExpiryArg } from "../../../../core/keyLifecycle";
import type { NewKeyExpiry } from "../../../../core/keyLifecycle";
import type { KeyActionError } from "../../../../core/userKeys";
import type { KeyExpiryArg, UserKey } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import { KeyDialog } from "./KeyDialog";
import { NewKeyExpiryField } from "./NewKeyExpiryField";
import { keyErrorText } from "./text";

export type ReplaceDialogOutcome = { ok: true } | { ok: false; error: KeyActionError; leftNewKey: boolean };

export function ReplaceKeyDialog({
  keyItem,
  address,
  onReplace,
  onClose,
  now = new Date(),
  warnDays = null,
}: {
  keyItem: UserKey;
  /** The address it is bound to. */
  address: string;
  onReplace: (expiry: KeyExpiryArg) => Promise<ReplaceDialogOutcome>;
  onClose: () => void;
  now?: Date;
  warnDays?: number | null;
}): JSX.Element {
  const [expiry, setExpiry] = useState<NewKeyExpiry>("2y");
  const [date, setDate] = useState("");
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<{ error: KeyActionError; leftNewKey: boolean } | null>(null);
  const cancelRef = useRef<HTMLButtonElement | null>(null);
  const dateResultId = useId();
  const arg = newKeyExpiryArg(expiry, date, now);
  const ready = arg !== null && !busy;

  const submit = async (): Promise<void> => {
    if (!ready || arg === null) return;
    setBusy(true);
    setFailure(null);
    const out = await onReplace(arg);
    setBusy(false);
    if (out.ok) onClose();
    else setFailure({ error: out.error, leftNewKey: out.leftNewKey });
  };

  return (
    <KeyDialog
      title={t("keys.replace.titleFor", { address })}
      className="key-replace"
      busy={busy}
      onClose={onClose}
      initialFocus={cancelRef}
    >
      <form
        onSubmit={(ev) => {
          ev.preventDefault();
          void submit();
        }}
      >
        <p>{t("keys.replace.what", { address })}</p>
        <p>{keyItem.wkdPublished ? t("keys.replace.published") : t("keys.replace.notPublished")}</p>
        <p>{t("keys.replace.old")}</p>
        <p className="key-replace-stolen">
          <strong>{t("keys.replace.stolen")}</strong>
        </p>
        <NewKeyExpiryField
          label={t("keys.replace.expiry")}
          choice={expiry}
          date={date}
          onChoice={setExpiry}
          onDate={setDate}
          now={now}
          warnDays={warnDays}
          disabled={busy}
          resultId={dateResultId}
        />
        {failure ? (
          <div className="field-error" role="alert">
            <p>{keyErrorText(failure.error)}</p>
            {failure.leftNewKey ? <p>{t("keys.replace.leftNewKey")}</p> : null}
          </div>
        ) : null}
        {busy ? (
          <p className="field-hint" role="status">
            {t("keys.replace.busy")}
          </p>
        ) : null}
        <div className="sheet-actions">
          <button type="button" className="button" ref={cancelRef} disabled={busy} onClick={onClose}>
            {t("action.cancel")}
          </button>
          <button
            type="submit"
            className="button button-primary key-replace-submit"
            aria-disabled={!ready}
            aria-describedby={arg === null ? dateResultId : undefined}
          >
            {t("keys.replace.submit")}
          </button>
        </div>
      </form>
    </KeyDialog>
  );
}
