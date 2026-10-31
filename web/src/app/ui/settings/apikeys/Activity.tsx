// SPDX-License-Identifier: AGPL-3.0-only

// A KEY'S ACTIVITY (#49, ADR-042 §5–§6): ApiKey/getEvents as a readable list —
// when, which method, what happened, and why in plain words (model.ts
// eventText). Refusals past the per-minute cap arrive as counts
// (`suppressedRefusals`) and are shown where they happened as
// "+N more refused calls (…)" rows, so the log is bounded without hiding
// anything. Anomalies, suspensions and security-meaningful refusals take the
// warning tone, on top of words that already say so.
//
// The method name is the server's text (a method it does not have is stored as
// "unknown"); it is still shown as isolated text.

import type { JSX } from "react";

import type { ApiKeyEvent, ApiKeysCapability, SuppressedRefusal } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import { keyDateLong } from "../../../../core/userKeys";
import { clockText } from "../../desk/format";
import { eventText, suppressedIsWarn, suppressedText } from "./model";
import { Untrusted } from "./Untrusted";

export type ActivityReading =
  | { kind: "asking" }
  | { kind: "unavailable" }
  | { kind: "loaded"; list: ApiKeyEvent[]; suppressed: SuppressedRefusal[] };

type Row =
  | { at: string; kind: "event"; event: ApiKeyEvent }
  | { at: string; kind: "suppressed"; s: SuppressedRefusal };

/** Events and suppressed counts in one newest-first list. */
export function activityRows(list: readonly ApiKeyEvent[], suppressed: readonly SuppressedRefusal[]): Row[] {
  const rows: Row[] = [
    ...list.map((event): Row => ({ at: event.at, kind: "event", event })),
    ...suppressed.map((s): Row => ({ at: s.minute, kind: "suppressed", s })),
  ];
  return rows.sort((a, b) => b.at.localeCompare(a.at));
}

function when(iso: string): string {
  return `${keyDateLong(iso)} ${clockText(iso)}`;
}

export function Activity({
  reading,
  cap,
  limit,
}: {
  reading: ActivityReading;
  cap: ApiKeysCapability | null;
  /** The page size asked for, so a full page says it may not be everything. */
  limit: number;
}): JSX.Element {
  if (reading.kind === "asking") return <p className="field-hint">{t("apiKeys.activity.asking")}</p>;
  if (reading.kind === "unavailable") return <p className="field-hint">{t("apiKeys.activity.unavailable")}</p>;
  const rows = activityRows(reading.list, reading.suppressed);
  if (rows.length === 0) return <p className="field-hint">{t("apiKeys.activity.none")}</p>;
  return (
    <>
      <ol className="apikey-activity">
        {rows.map((r) => {
          if (r.kind === "suppressed") {
            const warn = suppressedIsWarn(r.s);
            return (
              <li
                key={`s-${r.s.minute}-${r.s.outcome}-${r.s.method}-${r.s.reason}`}
                className={`apikey-event is-suppressed${warn ? " is-warn" : ""}`}
              >
                <span className="apikey-event-time">{when(r.at)}</span>{" "}
                <span className="apikey-event-what">{suppressedText(r.s, cap)}</span>
              </li>
            );
          }
          const e = r.event;
          const words = eventText(e, cap);
          return (
            <li key={e.id} className={`apikey-event${words.tone === "warn" ? " is-warn" : words.tone === "refused" ? " is-refused" : ""}`}>
              <span className="apikey-event-time">{when(e.at)}</span>{" "}
              <code className="apikey-event-method">
                <Untrusted text={e.method} />
              </code>{" "}
              <span className="apikey-event-what">{words.outcome}</span>
              {words.reason ? <span className="apikey-event-why">: {words.reason}</span> : null}
            </li>
          );
        })}
      </ol>
      {reading.list.length >= limit ? <p className="field-hint">{t("apiKeys.activity.bound", { n: limit })}</p> : null}
    </>
  );
}
