// SPDX-License-Identifier: AGPL-3.0-only

// REVOKE (#796, ADR-040 §3): the one key action that cannot be taken back, so
// it is the one that asks the most of the user.
//
//   * It says plainly what it does (tells the world nobody should use the key),
//     what it does not do (old mail still opens), and that it is permanent.
//   * It asks why (the reason goes into the revocation signature itself).
//   * It asks the user to type the last 8 characters of the fingerprint, the
//     same check the crypto process makes against the key it resolved, so the
//     key confirmed is the key revoked.
//
// THE BUTTON IS aria-disabled, NOT disabled, until the characters match. A
// `disabled` button leaves the tab order, so a keyboard or screen-reader user
// could not find it to learn that it exists and why it is not ready; an
// aria-disabled one stays reachable, is announced as unavailable, and points at
// the sentence saying what it is waiting for (aria-describedby). A click on it
// while it is not ready does nothing.

import { useId, useRef, useState } from "react";
import type { JSX } from "react";

import { fingerprintTail, revokeConfirmMatches } from "../../../../core/userKeys";
import type { KeyActionError } from "../../../../core/userKeys";
import { groupFingerprint } from "../../../../core/pgpSignature";
import type { UserKey } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import type { MessageKey } from "../../../i18n";
import type { RevokeReason } from "./api";
import { KeyDialog } from "./KeyDialog";
import { keyErrorText } from "./text";

const REASONS: { value: RevokeReason; label: MessageKey }[] = [
  { value: "compromised", label: "keys.revoke.reason.compromised" },
  { value: "superseded", label: "keys.revoke.reason.superseded" },
  { value: "retired", label: "keys.revoke.reason.retired" },
];

export function RevokeKeyDialog({
  keyItem,
  address,
  onRevoke,
  onClose,
}: {
  keyItem: UserKey;
  /** The address it is bound to, for the title. */
  address: string | null;
  onRevoke: (reason: RevokeReason, confirm: string) => Promise<{ ok: true } | { ok: false; error: KeyActionError }>;
  onClose: () => void;
}): JSX.Element {
  const [reason, setReason] = useState<RevokeReason | null>(null);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<KeyActionError | null>(null);
  const confirmId = useId();
  const waitingId = useId();
  const reasonName = useId();
  const cancelRef = useRef<HTMLButtonElement | null>(null);

  const tail = fingerprintTail(keyItem.fingerprint);
  const matches = revokeConfirmMatches(keyItem.fingerprint, typed);
  const ready = matches && reason !== null && !busy;

  const submit = async (): Promise<void> => {
    if (!ready || reason === null) return;
    setBusy(true);
    setError(null);
    const out = await onRevoke(reason, typed.replace(/\s+/g, "").toUpperCase());
    setBusy(false);
    if (out.ok) onClose();
    else setError(out.error);
  };

  return (
    <KeyDialog
      title={address ? t("keys.revoke.titleFor", { address }) : t("keys.revoke.title")}
      className="key-revoke"
      busy={busy}
      onClose={onClose}
      initialFocus={cancelRef}
    >
      <p>{t("keys.revoke.what")}</p>
      <p>
        <strong>{t("keys.revoke.permanent")}</strong>
      </p>
      <p className="field-hint">{t("keys.revoke.stillDecrypts")}</p>
      <p className="key-revoke-fingerprint">
        <span className="field-label">{t("keys.fingerprint")}</span>{" "}
        <code className="setting-fingerprint">{groupFingerprint(keyItem.fingerprint)}</code>
      </p>
      <fieldset className="key-fieldset">
        <legend>{t("keys.revoke.reasonLegend")}</legend>
        {REASONS.map((r) => (
          <label key={r.value} className="key-radio">
            <input
              type="radio"
              name={reasonName}
              value={r.value}
              checked={reason === r.value}
              disabled={busy}
              onChange={() => setReason(r.value)}
            />{" "}
            {t(r.label)}
          </label>
        ))}
      </fieldset>
      <label htmlFor={confirmId} className="field-label">
        {t("keys.revoke.confirmLabel", { tail: `${tail.slice(0, 4)} ${tail.slice(4)}` })}
      </label>
      <input
        id={confirmId}
        className="text-input key-revoke-confirm"
        type="text"
        autoComplete="off"
        autoCapitalize="characters"
        spellCheck={false}
        value={typed}
        disabled={busy}
        aria-describedby={waitingId}
        onChange={(ev) => setTyped(ev.target.value)}
        onKeyDown={(ev) => {
          if (ev.key === "Enter") {
            ev.preventDefault();
            void submit();
          }
        }}
      />
      <p id={waitingId} className="field-hint">
        {ready
          ? t("keys.revoke.ready")
          : reason === null
            ? t("keys.revoke.waitingReason")
            : t("keys.revoke.waitingTail")}
      </p>
      {error ? (
        <p className="field-error" role="alert">
          {keyErrorText(error)}
        </p>
      ) : null}
      {busy ? (
        <p className="field-hint" role="status">
          {t("keys.revoke.busy")}
        </p>
      ) : null}
      <div className="sheet-actions">
        <button type="button" className="button" ref={cancelRef} disabled={busy} onClick={onClose}>
          {t("action.cancel")}
        </button>
        <button
          type="button"
          className="button button-danger key-revoke-submit"
          aria-disabled={!ready}
          aria-describedby={waitingId}
          onClick={() => void submit()}
        >
          {t("keys.revoke.submit")}
        </button>
      </div>
    </KeyDialog>
  );
}
