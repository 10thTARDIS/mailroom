// SPDX-License-Identifier: AGPL-3.0-only

// A new key's expiry (#796, #813): the presets, "Never", and "On a date…".
// Generate and replace both use it, so "the same expiry choices as generate"
// (#817) is one component rather than a promise.

import { useId } from "react";
import type { JSX } from "react";

import type { NewKeyExpiry } from "../../../../core/keyLifecycle";
import { t } from "../../../i18n";
import { ExpiryDateField } from "./ExpiryDateField";

export function NewKeyExpiryField({
  label,
  choice,
  date,
  onChoice,
  onDate,
  now,
  warnDays,
  disabled,
  resultId,
}: {
  resultId?: string;
  label: string;
  choice: NewKeyExpiry;
  date: string;
  onChoice: (c: NewKeyExpiry) => void;
  onDate: (d: string) => void;
  now: Date;
  warnDays: number | null;
  disabled: boolean;
}): JSX.Element {
  const id = useId();
  return (
    <>
      <label htmlFor={id} className="field-label">
        {label}
      </label>
      <select
        id={id}
        className="select"
        value={choice}
        disabled={disabled}
        onChange={(ev) => onChoice(ev.target.value as NewKeyExpiry)}
      >
        <option value="1y">{t("keys.generate.expiry.1y")}</option>
        <option value="2y">{t("keys.generate.expiry.2y")}</option>
        <option value="5y">{t("keys.generate.expiry.5y")}</option>
        <option value="never">{t("keys.generate.expiry.never")}</option>
        <option value="date">{t("keys.expiry.date")}</option>
      </select>
      {choice === "date" ? (
        <ExpiryDateField value={date} onChange={onDate} now={now} warnDays={warnDays} disabled={disabled} resultId={resultId} />
      ) : null}
    </>
  );
}
