// SPDX-License-Identifier: AGPL-3.0-only

// OUT OF OFFICE (#50): the pure half of the Settings row — the date handling,
// the state line, and the form's own checks — so it is tested without a DOM.
//
// THE WINDOW IS WHOLE DAYS IN THE USER'S OWN TIME ZONE. The form asks for a
// first and a last day away; the server stores instants. The first day starts
// at local midnight, and the LAST day runs to the following local midnight,
// so "last day: Friday" replies all of Friday. An empty first day means
// "from now", an empty last day means "until I turn it off".

import type { OutOfOffice, OutOfOfficeUpdate } from "../../../core/jmap/types";
import { t } from "../../i18n";

/** The editable half of the object, as the form holds it. */
export interface OutOfOfficeForm {
  isEnabled: boolean;
  /** "YYYY-MM-DD" in local time, or "" for open-ended. */
  firstDay: string;
  lastDay: string;
  subject: string;
  textBody: string;
  replyDays: number;
  fromAddress: string;
  contactsOnly: boolean;
}

const pad = (n: number): string => String(n).padStart(2, "0");

/** A Date's local calendar day as "YYYY-MM-DD". */
export function localDay(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** Local midnight at the start of "YYYY-MM-DD", or null for anything else. */
export function startOfLocalDay(day: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day);
  if (!m) return null;
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
  return Number.isNaN(d.getTime()) ? null : d;
}

/** The UTCDate the server takes: seconds precision, "Z". */
export function utcDate(d: Date): string {
  return d.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** The form for a stored object. toDate is the midnight AFTER the last day. */
export function formFrom(o: OutOfOffice): OutOfOfficeForm {
  const last = o.toDate ? new Date(new Date(o.toDate).getTime() - 1) : null;
  return {
    isEnabled: o.isEnabled,
    firstDay: o.fromDate ? localDay(new Date(o.fromDate)) : "",
    lastDay: last ? localDay(last) : "",
    subject: o.subject,
    textBody: o.textBody,
    replyDays: o.replyDays,
    fromAddress: o.fromAddress || (o.addresses[0] ?? ""),
    contactsOnly: o.contactsOnly,
  };
}

/** The update for a form, or the reason the form cannot be saved. */
export function updateFrom(form: OutOfOfficeForm, now: Date = new Date()): OutOfOfficeUpdate | { error: string } {
  const first = form.firstDay ? startOfLocalDay(form.firstDay) : null;
  const lastStart = form.lastDay ? startOfLocalDay(form.lastDay) : null;
  if (form.firstDay && !first) return { error: t("oof.err.firstDay") };
  if (form.lastDay && !lastStart) return { error: t("oof.err.lastDay") };
  let end: Date | null = null;
  if (lastStart) {
    end = new Date(lastStart);
    end.setDate(end.getDate() + 1);
  }
  if (first && end && end.getTime() <= first.getTime()) return { error: t("oof.err.order") };
  if (form.isEnabled && end && end.getTime() <= now.getTime()) return { error: t("oof.err.past") };
  if (form.isEnabled && form.textBody.trim() === "") return { error: t("oof.err.body") };
  if (!Number.isInteger(form.replyDays) || form.replyDays < 1 || form.replyDays > 30) {
    return { error: t("oof.err.days") };
  }
  return {
    isEnabled: form.isEnabled,
    fromDate: first ? utcDate(first) : null,
    toDate: end ? utcDate(end) : null,
    subject: form.subject,
    textBody: form.textBody,
    replyDays: form.replyDays,
    fromAddress: form.fromAddress,
    contactsOnly: form.contactsOnly,
  };
}

/** Whether what the server last found disagrees with what Mailroom set. */
export function isDrift(o: OutOfOffice): boolean {
  return o.serverState === "displaced" || o.serverState === "missing" || o.serverState === "modified";
}

/** The status sentence for one account, and whether it is a warning. */
export function statusLine(o: OutOfOffice): { text: string; warn: boolean } {
  if (!o.isSupported) return { text: t("oof.unsupported", { reason: o.unsupportedReason ?? "" }), warn: false };
  if (isDrift(o) || o.serverState === "error") {
    return { text: o.serverDetail || t(`oof.state.${o.serverState}`), warn: true };
  }
  if (!o.isEnabled) return { text: o.serverDetail || t("oof.state.off"), warn: false };
  const until = o.toDate ? new Date(new Date(o.toDate).getTime() - 1) : null;
  const from = o.fromDate ? new Date(o.fromDate) : null;
  const day = (d: Date): string => d.toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "short" });
  if (from && from.getTime() > Date.now()) {
    return {
      text: until
        ? t("oof.state.scheduledUntil", { from: day(from), until: day(until) })
        : t("oof.state.scheduled", { from: day(from) }),
      warn: false,
    };
  }
  return { text: until ? t("oof.state.onUntil", { until: day(until) }) : t("oof.state.on"), warn: false };
}

/** The contacts-only bound, stated: "Replies to 1,412 of 2,650 contacts". */
export function contactsLine(o: OutOfOffice): string | null {
  if (!o.contactsOnly || o.contactsIncluded === null || o.contactsTotal === null) return null;
  const n = (v: number): string => v.toLocaleString("en-US");
  if (o.contactsIncluded < o.contactsTotal) {
    return t("oof.contacts.capped", { included: n(o.contactsIncluded), total: n(o.contactsTotal) });
  }
  return t("oof.contacts.all", { total: n(o.contactsTotal) });
}

/** The row's state line: which accounts are replying. Null until read. */
export function outOfOfficeFacts(list: OutOfOffice[] | null): string[] {
  if (list === null) return [];
  const on = list.filter((o) => o.isEnabled);
  if (on.length === 0) return [t("oof.fact.off")];
  const out = on.map((o) => t("oof.fact.on", { name: o.accountName }));
  if (list.some(isDrift)) out.push(t("oof.fact.drift"));
  return out;
}
