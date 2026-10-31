// SPDX-License-Identifier: AGPL-3.0-only

// EXTEND (#795, ADR-040 §3): a new expiry, counted from today. One or two
// years, a custom number of days within the server's ten-year cap, or an exact
// date (#813). The fingerprint stays the same, so nobody sees "key changed".

import { useId, useState } from "react";
import type { JSX } from "react";

import { exactDateProblem } from "../../../../core/keyLifecycle";
import { MAX_KEY_DAYS, expiryFact, extendDays, keyDateLong } from "../../../../core/userKeys";
import type { KeyActionError } from "../../../../core/userKeys";
import type { UserKey } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import { ExpiryDateField } from "./ExpiryDateField";
import { KeyDialog } from "./KeyDialog";
import { keyErrorText } from "./text";

type Choice = "1y" | "2y" | "custom" | "date";

export function ExtendKeyDialog({
  keyItem,
  address,
  onExtend,
  onClose,
  now = new Date(),
  warnDays = null,
}: {
  keyItem: UserKey;
  address: string | null;
  /** Days from now, or (#813) the end of a calendar day in UTC. */
  onExtend: (to: number | { expiresAt: string }) => Promise<{ ok: true } | { ok: false; error: KeyActionError }>;
  onClose: () => void;
  now?: Date;
  warnDays?: number | null;
}): JSX.Element {
  const [choice, setChoice] = useState<Choice>("1y");
  const [custom, setCustom] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<KeyActionError | null>(null);
  const group = useId();
  const customId = useId();
  const [date, setDate] = useState("");
  const dateResultId = useId();
  const to: number | { expiresAt: string } | null =
    choice === "date"
      ? exactDateProblem(date, now) === null
        ? { expiresAt: date.trim() }
        : null
      : extendDays(choice, custom);
  const fact = expiryFact(keyItem);

  const submit = async (): Promise<void> => {
    if (to === null || busy) return;
    setBusy(true);
    setError(null);
    const out = await onExtend(to);
    setBusy(false);
    if (out.ok) onClose();
    else setError(out.error);
  };

  const options: { value: Choice; label: string }[] = [
    { value: "1y", label: t("keys.extend.oneYear") },
    { value: "2y", label: t("keys.extend.twoYears") },
    { value: "custom", label: t("keys.extend.custom") },
    { value: "date", label: t("keys.expiry.date") },
  ];

  return (
    <KeyDialog
      title={address ? t("keys.extend.titleFor", { address }) : t("keys.extend.title")}
      busy={busy}
      onClose={onClose}
    >
      <form
        onSubmit={(ev) => {
          ev.preventDefault();
          void submit();
        }}
      >
        <p>{t("keys.extend.explain")}</p>
        {fact.kind === "expires" ? (
          <p className="field-hint">{t("keys.expires", { date: keyDateLong(fact.at) })}</p>
        ) : fact.kind === "expired" && fact.at ? (
          <p className="field-hint">{t("keys.expiredOn", { date: keyDateLong(fact.at) })}</p>
        ) : fact.kind === "never" ? (
          <p className="field-hint">{t("keys.neverExpires")}</p>
        ) : null}
        <fieldset className="key-fieldset">
          <legend>{t("keys.extend.legend")}</legend>
          {options.map((o) => (
            <label key={o.value} className="key-radio">
              <input
                type="radio"
                name={group}
                value={o.value}
                checked={choice === o.value}
                disabled={busy}
                onChange={() => setChoice(o.value)}
              />{" "}
              {o.label}
            </label>
          ))}
        </fieldset>
        {choice === "custom" ? (
          <>
            <label htmlFor={customId} className="field-label">
              {t("keys.extend.customLabel", { max: MAX_KEY_DAYS })}
            </label>
            <input
              id={customId}
              className="text-input key-days"
              type="text"
              inputMode="numeric"
              autoComplete="off"
              value={custom}
              disabled={busy}
              onChange={(ev) => setCustom(ev.target.value)}
            />
            {custom.trim() !== "" && to === null ? (
              <p className="field-error">{t("keys.extend.customInvalid", { max: MAX_KEY_DAYS })}</p>
            ) : null}
          </>
        ) : choice === "date" ? (
          <ExpiryDateField
            value={date}
            onChange={setDate}
            now={now}
            warnDays={warnDays}
            disabled={busy}
            resultId={dateResultId}
          />
        ) : null}
        {error ? (
          <p className="field-error" role="alert">
            {keyErrorText(error)}
          </p>
        ) : null}
        {busy ? (
          <p className="field-hint" role="status">
            {t("keys.extend.busy")}
          </p>
        ) : null}
        <div className="sheet-actions">
          <button type="button" className="button" disabled={busy} onClick={onClose}>
            {t("action.cancel")}
          </button>
          <button
            type="submit"
            className="button button-primary key-submit"
            disabled={busy || (choice !== "date" && to === null)}
            aria-disabled={choice === "date" && to === null ? true : undefined}
            aria-describedby={choice === "date" && to === null ? dateResultId : undefined}
          >
            {t("keys.extend.submit")}
          </button>
        </div>
      </form>
    </KeyDialog>
  );
}
