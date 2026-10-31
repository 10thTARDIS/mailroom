// SPDX-License-Identifier: AGPL-3.0-only

// GENERATE (#796): a new key for one of your FIXED addresses. The name and
// address on the key come from the identity, never from free text (the server
// refuses a wildcard). The expiry defaults to two years (the operator's
// decision on #795), or an exact date (#813); "Sign my mail from this address
// with it" binds it in the same step.

import { useId, useState } from "react";
import type { JSX } from "react";

import { newKeyExpiryArg } from "../../../../core/keyLifecycle";
import type { NewKeyExpiry } from "../../../../core/keyLifecycle";
import type { KeyActionError } from "../../../../core/userKeys";
import type { Id, Identity, KeyExpiryArg } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import { KeyDialog } from "./KeyDialog";
import { NewKeyExpiryField } from "./NewKeyExpiryField";
import { keyErrorText } from "./text";

export function GenerateKeyDialog({
  identities,
  onGenerate,
  onClose,
  now = new Date(),
  warnDays = null,
}: {
  /** Fixed identities only (core/userKeys.ts fixedIdentities). */
  identities: readonly Identity[];
  onGenerate: (
    identityId: Id,
    expiry: KeyExpiryArg,
    bind: boolean,
  ) => Promise<{ ok: true } | { ok: false; error: KeyActionError }>;
  onClose: () => void;
  now?: Date;
  /** The server's warning threshold, for "will show Expires soon". */
  warnDays?: number | null;
}): JSX.Element {
  const [identityId, setIdentityId] = useState<Id>(identities[0]?.id ?? "");
  const [expiry, setExpiry] = useState<NewKeyExpiry>("2y");
  const [date, setDate] = useState("");
  const [bind, setBind] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<KeyActionError | null>(null);
  const identityField = useId();
  const arg = newKeyExpiryArg(expiry, date, now);
  const dateResultId = useId();

  const submit = async (): Promise<void> => {
    if (busy || identityId === "" || arg === null) return;
    setBusy(true);
    setError(null);
    const out = await onGenerate(identityId, arg, bind);
    setBusy(false);
    if (out.ok) onClose();
    else setError(out.error);
  };

  return (
    <KeyDialog title={t("keys.generate.title")} busy={busy} onClose={onClose}>
      {identities.length === 0 ? (
        <>
          <p>{t("keys.generate.noIdentity")}</p>
          <div className="sheet-actions">
            <button type="button" className="button" onClick={onClose}>
              {t("action.close")}
            </button>
          </div>
        </>
      ) : (
        <form
          onSubmit={(ev) => {
            ev.preventDefault();
            void submit();
          }}
        >
          <p>{t("keys.generate.explain")}</p>
          <label htmlFor={identityField} className="field-label">
            {t("keys.generate.identity")}
          </label>
          <select
            id={identityField}
            className="select"
            value={identityId}
            disabled={busy}
            onChange={(ev) => setIdentityId(ev.target.value)}
          >
            {identities.map((i) => (
              <option key={i.id} value={i.id}>
                {i.name ? `${i.name} <${i.email}>` : i.email}
              </option>
            ))}
          </select>
          <NewKeyExpiryField
            label={t("keys.generate.expiry")}
            choice={expiry}
            date={date}
            onChoice={setExpiry}
            onDate={setDate}
            now={now}
            warnDays={warnDays}
            disabled={busy}
            resultId={dateResultId}
          />
          <label className="key-checkbox">
            <input type="checkbox" checked={bind} disabled={busy} onChange={(ev) => setBind(ev.target.checked)} />{" "}
            {t("keys.generate.bind")}
          </label>
          {error ? (
            <p className="field-error" role="alert">
              {keyErrorText(error)}
            </p>
          ) : null}
          {busy ? (
            <p className="field-hint" role="status">
              {t("keys.generate.busy")}
            </p>
          ) : null}
          <div className="sheet-actions">
            <button type="button" className="button" disabled={busy} onClick={onClose}>
              {t("action.cancel")}
            </button>
            <button
              type="submit"
              className="button button-primary key-submit"
              disabled={busy}
              aria-disabled={arg === null}
              aria-describedby={arg === null ? dateResultId : undefined}
            >
              {t("keys.generate.submit")}
            </button>
          </div>
        </form>
      )}
    </KeyDialog>
  );
}
