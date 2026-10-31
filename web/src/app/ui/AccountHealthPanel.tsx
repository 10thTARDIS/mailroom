// SPDX-License-Identifier: AGPL-3.0-only

// The account-health panel (mailroom#43, spec §12): a read-only, on-demand view
// of how each mail account's sync is doing. It surfaces five operational signals
// the server folds into Account/getHealth (urn:mailroom:accounts):
//
//   * sync status — "synced N ago", "never synced", or the last sync error, plus
//     a consecutive-failure count when an account is stuck;
//   * writeback — whether Mailroom is actually writing to this account right
//     now (ADR-028's ingest-only mode, #521) and how many changes have piled up
//     recorded-but-unsent because it is not;
//   * delivery failures — recent bounces the account's sends produced;
//   * stuck changes — writeback intents that dead-lettered, with their reasons;
//   * send quota — today's outbound spend against the provider's daily cap.
//
// A sixth, envelope-level (not per-account) signal is shown when the server has
// search installed: search staleness (spec §7's "search may be N minutes stale",
// mailroom#41) — how far behind the change log the user's ONE search index is.
// It is a single line under the account cards, since the index is per-user, not
// per-account, and is omitted entirely when the response has no
// searchStalenessSeconds (search not installed on this server).
//
// Account health is OPERATIONAL telemetry, not a synced object, so it is
// fetched rather than read from the mail store: a snapshot, refreshed when this
// is opened. Every string goes through t() (i18n), colors are theme tokens
// (light/dark), and no status is conveyed by color alone.
//
// IT IS CONTENT, NOT A DIALOG, SINCE #375. It used to be a ♥ button in the rail
// that opened a modal sheet; the rail's account switcher is now the one place
// the app talks about accounts, so this renders INSIDE it — under the list of
// accounts, where "which account, and how is it doing" is one question asked
// once instead of a list in one place and its health behind a button beside it.
// The fetch itself moved to ui/useAccounts.ts, which the switcher and the desk
// row share; this component is now only the rendering of it.

import { useEffect } from "react";
import type { JSX } from "react";

import { t, tCount } from "../i18n";
import type { AccountHealth } from "../../core/jmap/types";
import { useAccounts } from "./useAccounts";
import "./accountHealth.css";

type Severity = "ok" | "warn" | "error";

/** Worst-first severity for an account: a failing/erroring sync is an error; a
 * healthy sync with stuck changes or recent bounces is a warning; otherwise ok. */
function severityOf(h: AccountHealth): Severity {
  if (h.syncStatus.consecutiveFailures > 0 || h.syncStatus.lastError) return "error";
  if (h.deadLetters.count > 0 || h.deliveryFailures.count > 0) return "warn";
  return "ok";
}

/** A localized "N units ago" for a non-negative age in seconds, via the platform
 * Intl formatter (a Web API, not a UI dependency). Clamps negatives to now. */
function formatAgo(seconds: number): string {
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
  const abs = Math.max(0, Math.floor(seconds));
  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ["day", 86400],
    ["hour", 3600],
    ["minute", 60],
  ];
  for (const [unit, secs] of units) {
    if (abs >= secs) return rtf.format(-Math.floor(abs / secs), unit);
  }
  return rtf.format(-abs, "second");
}

function SyncStatus({ h }: { h: AccountHealth }): JSX.Element {
  const s = h.syncStatus;
  return (
    <div className="health-section">
      <p className="health-section-title">{t("health.sync")}</p>
      {s.lastError ? (
        <p className="banner banner-error" role="status">
          {t("health.syncError", { error: s.lastError })}
        </p>
      ) : s.lagSeconds != null ? (
        <p className="health-line">{t("health.syncedAgo", { ago: formatAgo(s.lagSeconds) })}</p>
      ) : (
        <p className="health-line health-muted">{t("health.neverSynced")}</p>
      )}
      {s.consecutiveFailures > 0 ? (
        <p className="health-line health-muted">
          {t("health.consecutiveFailures", { count: s.consecutiveFailures })}
        </p>
      ) : null}
    </div>
  );
}

function DeliveryFailures({ h }: { h: AccountHealth }): JSX.Element {
  const df = h.deliveryFailures;
  return (
    <div className="health-section">
      <p className="health-section-title">{t("health.delivery")}</p>
      {df.count === 0 ? (
        <p className="health-line health-muted">{t("health.deliveryNone")}</p>
      ) : (
        <>
          <p className="health-line">{t("health.deliveryCount", { count: df.count })}</p>
          <ul className="health-reasons">
            {df.recent.map((f, i) => (
              <li key={`${f.recipient}-${i}`}>
                {f.recipient}
                {f.statusCode ? ` · ${f.statusCode}` : ""}
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

function DeadLetters({ h }: { h: AccountHealth }): JSX.Element {
  const dl = h.deadLetters;
  return (
    <div className="health-section">
      <p className="health-section-title">{t("health.deadLetters")}</p>
      {dl.count === 0 ? (
        <p className="health-line health-muted">{t("health.deadLettersNone")}</p>
      ) : (
        <>
          <p className="health-line">{t("health.deadLettersCount", { count: dl.count })}</p>
          <ul className="health-reasons">
            {dl.reasons.map((r, i) => (
              <li key={`${r}-${i}`}>{r}</li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

/**
 * Ingest-only mode (ADR-028, #521): whether Mailroom is actually writing to
 * this account right now, read from `h.writeback.enabled` — the EFFECTIVE
 * value, already ANDed with any instance-level switch (ADR-028 §2), rather
 * than the account's own column.
 *
 * Always rendered, on or off, like every other section here: "writing
 * normally" is itself the fact a settings toggle nobody opened would leave
 * unstated, and the whole reason this mode ships with a persistent banner is
 * that a state left to speak for itself by its absence is how ADR-028's
 * silent divergence gets manufactured again.
 */
function WritebackStatus({ h }: { h: AccountHealth }): JSX.Element {
  const w = h.writeback;
  return (
    <div className="health-section">
      <p className="health-section-title">{t("health.writeback")}</p>
      {w.enabled ? (
        <p className="health-line health-muted">{t("health.writeback.on")}</p>
      ) : (
        <>
          <p className="health-line">{t("health.writeback.off")}</p>
          <p className="health-line health-muted">{t("health.writeback.flagsProvisional")}</p>
          <p className="health-line health-muted">{t("health.writeback.localDurable")}</p>
          <p className="health-line">{tCount("health.writeback.suppressed", w.suppressedIntents)}</p>
        </>
      )}
    </div>
  );
}

function SendQuota({ h }: { h: AccountHealth }): JSX.Element {
  const q = h.sendQuota;
  const label = t("health.sendQuotaMeter", { sent: q.sent, limit: q.limit });
  return (
    <div className="health-section">
      <p className="health-section-title">{t("health.sendQuota")}</p>
      <p className="health-line">{label}</p>
      {q.limit > 0 ? (
        <progress
          className={`health-meter${q.sent >= q.limit ? " health-meter-full" : ""}`}
          max={q.limit}
          value={Math.min(q.sent, q.limit)}
          aria-label={label}
        />
      ) : null}
    </div>
  );
}

/** The one per-user "search may be N minutes stale" line (spec §7, mailroom#41).
 * `staleness` is undefined when the server has no search backend installed
 * (Account/getHealth omits the field), in which case this renders nothing —
 * there is no index to report on. Below a minute reads as "up to date" rather
 * than "stale for 0 minutes", which would read as a problem that isn't one. */
function SearchStaleness({ staleness }: { staleness: number | undefined }): JSX.Element | null {
  if (staleness == null) return null;
  return staleness < 60 ? (
    <p className="health-line health-muted">{t("health.searchUpToDate")}</p>
  ) : (
    <p className="health-line health-muted">
      {t("health.searchStale", { minutes: Math.ceil(staleness / 60) })}
    </p>
  );
}

function HealthCard({ h }: { h: AccountHealth }): JSX.Element {
  const sev = severityOf(h);
  const sevLabel =
    sev === "error"
      ? t("health.statusError")
      : sev === "warn"
        ? t("health.statusWarn")
        : t("health.statusOk");
  return (
    <li className="health-card">
      <div className="health-card-head">
        <span className={`health-dot health-dot-${sev}`} aria-hidden="true" />
        <span className="health-name">{h.name}</span>
        <span className="health-muted">· {sevLabel}</span>
      </div>
      <SyncStatus h={h} />
      <WritebackStatus h={h} />
      <DeliveryFailures h={h} />
      <DeadLetters h={h} />
      <SendQuota h={h} />
    </li>
  );
}

/**
 * The health of every account, as the switcher shows it.
 *
 * Refreshed each time it is MOUNTED — i.e. each time the switcher is opened —
 * because it is a snapshot of a moment and a stale one is exactly the sort of
 * reassurance that is worse than none. It never renders "nothing": the loading,
 * failed and empty cases each say which of the three they are, since "no
 * accounts have problems" and "we could not ask" must not look alike.
 */
export function AccountHealthContent(): JSX.Element {
  const { healthCards, loading, error, loaded, searchStalenessSeconds, reload } = useAccounts();

  // Mount-only: opening the switcher is the gesture that asks for a fresh
  // snapshot, and re-running on every change of the snapshot itself would be a
  // request per response.
  useEffect(() => {
    void reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // EVERY card the server returned, not one per switcher entry: the switcher
  // derives its list from Identities, and an account with no identity yet is
  // exactly the one whose health is worth reading.
  const cards = healthCards;

  if (loading && !loaded) {
    return (
      <p className="health-line health-muted" role="status">
        {t("health.loading")}
      </p>
    );
  }
  if (error) {
    return (
      <p className="banner banner-error" role="alert">
        {t("health.error", { error })}
      </p>
    );
  }
  if (cards.length === 0) {
    return <p className="health-line health-muted">{t("health.empty")}</p>;
  }
  return (
    <>
      <ul className="health-list">
        {cards.map((h) => (
          <HealthCard key={h.id} h={h} />
        ))}
      </ul>
      <SearchStaleness staleness={searchStalenessSeconds} />
    </>
  );
}
