// SPDX-License-Identifier: AGPL-3.0-only

// Pure helpers for the Activity row (ADR-053): the words for an entry, the link
// it gets, and the filter arithmetic. No message content is ever involved: an
// entry names a message by id, and the link opens it where the viewer shows it.

import type { ActivityEntry, ActivityPage, ActivitySource } from "../../../../core/jmap/types";
import type { Route } from "../../../../core/routes";
import { keyDateLong } from "../../../../core/userKeys";
import { t, tCount } from "../../../i18n";

/** The five filter groups, in display order. */
export const ACTIVITY_SOURCES: readonly ActivitySource[] = ["ai_tool", "decryption", "purge", "session", "bulk"];

export function sourceLabel(s: ActivitySource): string {
  return t(`activity.source.${s}`);
}

/** What an entry says: a message-key and, where the sentence names something the
 * user (or a tool) typed, that name, to be rendered as isolated text. */
export interface EntryWords {
  /** The sentence with `{name}` where `name` goes. */
  template: string;
  name: string | null;
  tone: "plain" | "warn";
}

const REFUSAL_REASONS = new Set([
  "method",
  "session_only",
  "restriction",
  "arguments",
  "scope",
  "rate",
  "budget",
  "approval",
  "mass_delete",
  "mass_send",
  "pending_cap",
]);

const NAME = "{name}";

function withName(key: Parameters<typeof t>[0], params: Record<string, string | number> = {}): string {
  return t(key, { name: NAME, ...params });
}

/** The sentence for one entry. Unknown outcomes fall back to a plain sentence
 * that still says which kind of thing it was, never to nothing. A key or rule
 * that no longer exists is named by a fixed phrase instead of its old name. */
export function entryWords(e: ActivityEntry): EntryWords {
  const key = e.keyName !== null && e.keyName !== "" ? e.keyName : t("activity.unnamedKey");
  switch (e.kind) {
    case "ai_tool": {
      const sentence = (k: Parameters<typeof t>[0], tone: "plain" | "warn" = "plain", params = {}): EntryWords => ({
        template: withName(k, params),
        name: key,
        tone,
      });
      switch (e.outcome) {
        case "denied": {
          const why =
            e.reason !== null && REFUSAL_REASONS.has(e.reason)
              ? t(`activity.reason.${e.reason}` as Parameters<typeof t>[0])
              : t("activity.reason.other");
          return sentence("activity.ai.denied", "warn", { why });
        }
        case "rate_limited":
          return sentence("activity.ai.rateLimited", "warn");
        case "over_budget":
          return sentence("activity.ai.overBudget", "warn");
        case "pending":
          return sentence("activity.ai.pending");
        case "approved":
          return sentence("activity.ai.approved");
        case "denied_by_user":
          return sentence("activity.ai.deniedByUser");
        case "expired":
          return sentence("activity.ai.expired");
        case "anomaly":
          return sentence("activity.ai.anomaly", "warn");
        default:
          return sentence("activity.ai.changed");
      }
    }
    case "ai_key": {
      const action = (e.method ?? "").replace(/^api_key\./, "");
      const known = ["create", "update", "revoke", "reactivate", "suspend", "pending.approve", "pending.deny", "family_revoke"];
      const k = (known.includes(action) ? `activity.key.${action}` : "activity.key.other") as Parameters<typeof t>[0];
      return { template: withName(k), name: key, tone: action === "suspend" ? "warn" : "plain" };
    }
    case "decryption": {
      const part = t(e.purpose === "attachment" ? "activity.decrypt.attachment" : "activity.decrypt.body");
      if (e.outcome === "failed") return { template: t("activity.decrypt.failed", { part }), name: null, tone: "warn" };
      const tail = (e.fingerprint ?? "").slice(-8);
      return { template: t("activity.decrypt.ok", { part, key: tail }), name: null, tone: "plain" };
    }
    case "purge":
      return { template: t("activity.purge"), name: null, tone: "plain" };
    case "sign_in":
      return { template: withName("activity.session.in"), name: e.device ?? t("activity.session.unknownDevice"), tone: "plain" };
    case "sign_out": {
      const name = e.device ?? t("activity.session.unknownDevice");
      switch (e.outcome) {
        case "signed_out_elsewhere":
          return { template: withName("activity.session.outElsewhere"), name, tone: "plain" };
        case "refused":
          return { template: withName("activity.session.refused"), name, tone: "warn" };
        case "expired":
          return { template: withName("activity.session.expired"), name, tone: "plain" };
        default:
          return { template: withName("activity.session.out"), name, tone: "plain" };
      }
    }
    case "placement": {
      const n = e.count ?? 0;
      const undo = e.outcome === "placement.backfill.undo";
      return { template: tCount(undo ? "activity.placement.undo" : "activity.placement.move", n), name: null, tone: "plain" };
    }
    case "rule_apply": {
      const n = e.count ?? 0;
      const undo = e.outcome === "rule.apply_undo";
      return { template: tCount(undo ? "activity.rule.undo" : "activity.rule.apply", n, { name: NAME }), name: key, tone: "plain" };
    }
    default:
      return { template: t("activity.unknown"), name: null, tone: "plain" };
  }
}

/** Where an entry's link goes, or null when the object is gone (or there was
 * none). A message opens in the mail view; a key opens its page; a rule opens
 * the rules list. */
export function entryLink(e: ActivityEntry): { route: Route; label: string } | null {
  const o = e.object;
  if (!o || !o.exists) return null;
  if (o.type === "Email") return { route: { page: "all", m: o.id }, label: t("activity.link.message") };
  if (o.type === "ApiKey") return { route: { page: "settings", section: "api-keys", apiKeyId: o.id }, label: t("activity.link.key") };
  if (o.type === "Rule") return { route: { page: "settings", section: "rules" }, label: t("activity.link.rule") };
  return null;
}

/** The UTC range a pair of date inputs (yyyy-mm-dd, either may be empty) asks
 * for: since is the start of the first day, until the START of the day after the
 * last (the server's until is exclusive), so the last day is included whole. */
export function dateRange(from: string, to: string): { since?: string; until?: string } {
  const out: { since?: string; until?: string } = {};
  if (/^\d{4}-\d{2}-\d{2}$/.test(from)) out.since = `${from}T00:00:00Z`;
  if (/^\d{4}-\d{2}-\d{2}$/.test(to)) {
    const d = new Date(`${to}T00:00:00Z`);
    d.setUTCDate(d.getUTCDate() + 1);
    out.until = d.toISOString().replace(/\.\d{3}Z$/, "Z");
  }
  return out;
}

/** True when both bounds are set and cross, so the page says so instead of asking. */
export function rangeIsBackwards(from: string, to: string): boolean {
  return from !== "" && to !== "" && from > to;
}

/** The honest sentence about how far back the list can go, for the sources that
 * keep less than the others. Null when nothing needs saying. */
export function coverNote(page: Pick<ActivityPage, "covers">, selected: readonly ActivitySource[]): string | null {
  if (!selected.includes("session")) return null;
  const at = page.covers.session;
  if (!at) return null;
  return t("activity.cover.session", { date: keyDateLong(at) });
}
