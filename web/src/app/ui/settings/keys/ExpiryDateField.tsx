// SPDX-License-Identifier: AGPL-3.0-only

// "ON A DATE…" (#813): the native date picker the generate, extend and replace
// dialogs share, so the three cannot disagree about what a date means.
//
//   * The value is the user's calendar day, sent verbatim as `expiresAt`; the
//     server reads it as the end of that day in UTC, and the small print says
//     so (core/keyLifecycle.ts says why no Date ever touches it).
//   * min and max are the server's own bounds (at least a day, at most ten
//     years), so the picker does not offer a date the server would refuse; a
//     typed date outside them is said in words, and the dialog cannot send it.
//   * The result is said in words before anything is confirmed ("Expires on
//     4 Nov 2026, in 14 days"), and a date inside the warning window says the
//     key will show "Expires soon" at once, so the chip is not a surprise.
//
// The input is described by the sentences under it (aria-describedby), so a
// screen reader hears the result and the UTC note with the field.

import { useId } from "react";
import type { JSX } from "react";

import {
  calendarDateLong,
  calendarDaysFromToday,
  exactDateBounds,
  exactDateExpiresSoon,
  exactDateProblem,
} from "../../../../core/keyLifecycle";
import { t } from "../../../i18n";

export function ExpiryDateField({
  value,
  onChange,
  now,
  warnDays,
  disabled,
  resultId: givenResultId,
}: {
  /** The id of the sentence saying the result (or what is wrong), so the
   * dialog's confirm button can point at it while it is unavailable. */
  resultId?: string;
  value: string;
  onChange: (date: string) => void;
  now: Date;
  warnDays: number | null;
  disabled: boolean;
}): JSX.Element {
  const inputId = useId();
  const ownResultId = useId();
  const resultId = givenResultId ?? ownResultId;
  const utcId = useId();
  const { min, max } = exactDateBounds(now);
  const problem = value.trim() === "" ? "invalid" : exactDateProblem(value, now);
  const days = calendarDaysFromToday(value, now);

  let result: { text: string; error: boolean } | null = null;
  if (value.trim() !== "") {
    if (problem === "tooSoon") result = { text: t("keys.expiry.dateTooSoon", { min: calendarDateLong(min) }), error: true };
    else if (problem === "tooFar") result = { text: t("keys.expiry.dateTooFar", { max: calendarDateLong(max) }), error: true };
    else if (problem === "invalid") result = { text: t("keys.expiry.dateInvalid"), error: true };
    else if (days === 1) result = { text: t("keys.expiry.datePreviewOne", { date: calendarDateLong(value) }), error: false };
    else result = { text: t("keys.expiry.datePreview", { date: calendarDateLong(value), days: days ?? 0 }), error: false };
  }
  const soon = problem === null && exactDateExpiresSoon(value, now, warnDays);

  return (
    <div className="key-date">
      <label htmlFor={inputId} className="field-label">
        {t("keys.expiry.dateLabel")}
      </label>
      <input
        id={inputId}
        className="text-input key-date-input"
        type="date"
        min={min}
        max={max}
        value={value}
        disabled={disabled}
        required
        aria-invalid={result?.error ? true : undefined}
        aria-describedby={`${resultId} ${utcId}`}
        onChange={(ev) => onChange(ev.target.value)}
      />
      <p id={resultId} className={result?.error ? "field-error key-date-result" : "field-hint key-date-result"}>
        {result ? result.text : t("keys.expiry.dateInvalid")}
        {soon && warnDays !== null ? <> {t("keys.expiry.dateSoon", { days: warnDays })}</> : null}
      </p>
      <p id={utcId} className="field-hint key-date-utc">
        <small>{t("keys.expiry.dateUtc")}</small>
      </p>
    </div>
  );
}
