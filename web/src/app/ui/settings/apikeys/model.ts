// SPDX-License-Identifier: AGPL-3.0-only

// THE WORDS AND DECISIONS BEHIND SETTINGS → API KEYS (#49, ADR-042, ADR-044).
//
// Pure functions, no DOM, so every rule the screens rely on is unit-tested
// (model.test.ts) rather than inferred from a rendering:
//
//   * PRESETS are named only when a key's scopes are EXACTLY one of them; a key
//     with more than a preset is "Assistant + Send", never "Assistant".
//   * The SEND-AND-DELETE confirmation is enforced here, client-side, before
//     the server's own check: `buildCreate` sends `acknowledgeSendAndDelete`
//     only when the typed phrase matches, and refuses to build a create
//     holding both scopes without it.
//   * An APPROVAL REQUEST is rendered from PendingAction/get's structured
//     fields — recipients, to/cc/bcc, subject, attachments, archive, sendAt —
//     and NEVER from `summary`, which carries text the key wrote (ADR-042 §7).
//     `pendingView` does not read `summary` at all; the test proves it.
//   * EVENTS are worded from the server's fixed outcome and reason lists, with
//     an honest fallback for a value this client does not know yet.
//   * The SETUP SNIPPETS are docs/mcp.md's, character for character with the
//     placeholders substituted (setupDocs.test.ts compares them).

import type {
  ApiKey,
  ApiKeyEvent,
  ApiKeySettings,
  ApiKeysCapability,
  Id,
  PendingAction,
  SuppressedRefusal,
} from "../../../../core/jmap/types";
import { t, tCount } from "../../../i18n";
import type { MessageKey } from "../../../i18n";
import { bytesText, durationText } from "../../desk/format";

// ---------------------------------------------------------------------------
// Scopes and presets
// ---------------------------------------------------------------------------

/** The closed scope set (ADR-042 §1), used only when the session object does
 * not list its own (an older or partial capability). */
export const FALLBACK_SCOPES: readonly string[] = [
  "mail:read",
  "search:read",
  "labels:read",
  "contacts:read",
  "mail:move",
  "labels:write",
  "mail:draft",
  "contacts:write",
  "mail:send",
  "mail:delete",
];

/** The scopes that can wait for approval when the capability does not say. */
const FALLBACK_CONFIRMABLE: readonly string[] = ["mail:send", "mail:delete"];

export type PresetId = "read" | "organise" | "assistant";

/** The three presets (docs/mcp.md's safety model): Read only; Organise adds
 * moving and labels; Assistant adds drafting. Sending and deleting are never
 * part of a preset: they are separate, deliberate choices. */
export const PRESETS: readonly { id: PresetId; scopes: readonly string[] }[] = [
  { id: "read", scopes: ["mail:read", "search:read", "labels:read", "contacts:read"] },
  {
    id: "organise",
    scopes: ["mail:read", "search:read", "labels:read", "contacts:read", "mail:move", "labels:write"],
  },
  {
    id: "assistant",
    scopes: ["mail:read", "search:read", "labels:read", "contacts:read", "mail:move", "labels:write", "mail:draft"],
  },
];

/** The scope list to offer, from the session object when it has one. */
export function offeredScopes(cap: ApiKeysCapability | null): string[] {
  const listed = cap?.scopes;
  if (!listed || listed.length === 0) return [...FALLBACK_SCOPES];
  // The fallback's order where it knows the scope (reads, then writes, then the
  // destructive two), then anything new the server lists, in its order.
  const known = FALLBACK_SCOPES.filter((s) => listed.includes(s));
  const extra = listed.filter((s) => !FALLBACK_SCOPES.includes(s));
  return [...known, ...extra];
}

/** The scopes "ask me first" applies to. */
export function confirmableScopes(cap: ApiKeysCapability | null): string[] {
  const listed = cap?.confirmableScopes;
  return listed && listed.length > 0 ? [...listed] : [...FALLBACK_CONFIRMABLE];
}

function sameSet(a: readonly string[], b: readonly string[]): boolean {
  const sa = new Set(a);
  const sb = new Set(b);
  if (sa.size !== sb.size) return false;
  for (const x of sa) if (!sb.has(x)) return false;
  return true;
}

/** The preset whose scopes are EXACTLY these, or null (custom). */
export function matchPreset(scopes: readonly string[]): PresetId | null {
  for (const p of PRESETS) if (sameSet(p.scopes, scopes)) return p.id;
  return null;
}

/** Scopes in reading order — reads, then writes, then send and delete — with
 * any scope this client does not know after them, as the server listed them. */
export function orderScopes(scopes: readonly string[]): string[] {
  const rank = (s: string): number => {
    const i = FALLBACK_SCOPES.indexOf(s);
    return i < 0 ? FALLBACK_SCOPES.length : i;
  };
  return scopes.map((s, i) => ({ s, i })).sort((a, b) => rank(a.s) - rank(b.s) || a.i - b.i).map((x) => x.s);
}

export function presetName(id: PresetId): string {
  return t(`apiKeys.preset.${id}` as MessageKey);
}

/** A scope in words ("Send mail"); the raw id for one this client has no
 * words for, so a new server scope is shown rather than hidden. */
export function scopeLabel(scope: string): string {
  const key = `apiKeys.scope.${scope}` as MessageKey;
  const text = t(key);
  return text === key ? scope : text;
}

/**
 * A key's scopes in one line, for the list.
 *
 *   exactly a preset          → "Assistant"
 *   a preset plus more        → "Assistant + Send mail, Delete mail"
 *   anything else             → each scope in words
 *
 * "Plus more" uses the LARGEST preset the scopes contain, so the extras listed
 * are the fewest that are true. A preset is never named for a key that lacks
 * one of its scopes.
 */
export function scopeSummary(scopes: readonly string[]): string {
  const exact = matchPreset(scopes);
  if (exact) return presetName(exact);
  const have = new Set(scopes);
  for (let i = PRESETS.length - 1; i >= 0; i--) {
    const p = PRESETS[i];
    if (!p) continue;
    if (p.scopes.every((s) => have.has(s))) {
      const extra = orderScopes(scopes.filter((s) => !p.scopes.includes(s)));
      return t("apiKeys.summary.plus", { preset: presetName(p.id), extra: extra.map(scopeLabel).join(", ") });
    }
  }
  return orderScopes(scopes).map(scopeLabel).join(", ");
}

// ---------------------------------------------------------------------------
// Key state
// ---------------------------------------------------------------------------

export type KeyState =
  | { kind: "active" }
  | { kind: "suspended"; at: string | null; reason: string | null }
  | { kind: "revoked"; at: string };

export function keyState(k: Pick<ApiKey, "revokedAt" | "suspendedAt" | "suspendedReason">): KeyState {
  if (k.revokedAt) return { kind: "revoked", at: k.revokedAt };
  if (k.suspendedAt) return { kind: "suspended", at: k.suspendedAt, reason: k.suspendedReason };
  return { kind: "active" };
}

/** "Every account", or the restricted accounts' names (an id this page cannot
 * name is shown as "an account no longer here"). */
export function restrictionText(accountIds: readonly Id[] | null, names: ReadonlyMap<Id, string>): string {
  if (accountIds === null) return t("apiKeys.restrict.all");
  const shown = accountIds.map((id) => names.get(id) ?? t("apiKeys.restrict.unknown"));
  return t("apiKeys.restrict.only", { accounts: shown.join(", ") });
}

/** "123 of 500 used in the last 24 hours" — omitted (null) when the server did
 * not report usage, never printed as zero. */
export function usageText(k: Pick<ApiKey, "usedLast24h" | "dailyBudget">): string | null {
  if (typeof k.usedLast24h !== "number") return null;
  return t("apiKeys.usage", { used: k.usedLast24h, budget: k.dailyBudget });
}

// ---------------------------------------------------------------------------
// Creating a key
// ---------------------------------------------------------------------------

/** The phrase the person types to grant send AND delete together. The server
 * wants `acknowledgeSendAndDelete: true`; the UI makes it deliberate. */
export const SEND_AND_DELETE_PHRASE = "send and delete";

export function grantsSendAndDelete(scopes: readonly string[]): boolean {
  return scopes.includes("mail:send") && scopes.includes("mail:delete");
}

/** Case and surrounding/inner whitespace forgiven; nothing else. */
export function sendAndDeleteConfirmed(typed: string): boolean {
  return typed.trim().replace(/\s+/g, " ").toLowerCase() === SEND_AND_DELETE_PHRASE;
}

export interface CreateForm {
  name: string;
  scopes: readonly string[];
  /** null: every account. */
  accountIds: readonly Id[] | null;
  /** The destructive scopes that wait for approval. */
  confirmScopes: readonly string[];
  /** As typed; "" leaves the server's default. */
  ratePerMinute: string;
  dailyBudget: string;
  /** null: not touched, so the server's default applies. */
  suspendOnAnomaly: boolean | null;
  /** The typed confirmation, consulted only when send and delete are both ticked. */
  sendAndDeleteTyped: string;
}

export type CreateProblem =
  | "name"
  | "scopes"
  | "accounts"
  | "rate"
  | "budget"
  | "sendAndDelete";

function parseBound(raw: string, max: number): number | null | "bad" {
  const s = raw.trim();
  if (s === "") return null;
  if (!/^\d+$/.test(s)) return "bad";
  const n = Number(s);
  return n >= 1 && n <= max ? n : "bad";
}

/**
 * The ApiKey/set create for a form, or the first thing wrong with it. The
 * server checks all of this again; checking here is what lets the dialog say
 * so before anything is sent, and what makes the typed confirmation a
 * client-side control rather than a decoration.
 */
export function buildCreate(
  form: CreateForm,
  cap: ApiKeysCapability | null,
): { ok: true; settings: ApiKeySettings } | { ok: false; problem: CreateProblem } {
  const name = form.name.trim();
  if (name === "" || [...name].length > 100) return { ok: false, problem: "name" };
  const offered = offeredScopes(cap);
  const scopes = offered.filter((s) => form.scopes.includes(s));
  if (scopes.length === 0) return { ok: false, problem: "scopes" };
  if (form.accountIds !== null && form.accountIds.length === 0) return { ok: false, problem: "accounts" };
  const rate = parseBound(form.ratePerMinute, cap?.maxRatePerMinute ?? 600);
  if (rate === "bad") return { ok: false, problem: "rate" };
  const budget = parseBound(form.dailyBudget, cap?.maxDailyBudget ?? 20000);
  if (budget === "bad") return { ok: false, problem: "budget" };
  const both = grantsSendAndDelete(scopes);
  if (both && !sendAndDeleteConfirmed(form.sendAndDeleteTyped)) return { ok: false, problem: "sendAndDelete" };

  const confirmable = confirmableScopes(cap);
  const settings: ApiKeySettings = {
    name,
    scopes,
    accountIds: form.accountIds === null ? null : [...form.accountIds],
    // Always sent, so "ask me first" is exactly what the form shows rather than
    // the server's default for scopes the person unticked.
    confirmScopes: scopes.filter((s) => confirmable.includes(s) && form.confirmScopes.includes(s)),
  };
  if (rate !== null) settings.ratePerMinute = rate;
  if (budget !== null) settings.dailyBudget = budget;
  if (form.suspendOnAnomaly !== null) settings.suspendOnAnomaly = form.suspendOnAnomaly;
  if (both) settings.acknowledgeSendAndDelete = true;
  return { ok: true, settings };
}

/** The scopes ticked for a preset choice. */
export function presetScopes(id: PresetId, offered: readonly string[]): string[] {
  const p = PRESETS.find((x) => x.id === id);
  return p ? offered.filter((s) => p.scopes.includes(s)) : [];
}

/** "Ask me first" starts ON for every destructive scope (the operator's
 * default for send and delete). */
export function defaultConfirmScopes(cap: ApiKeysCapability | null): string[] {
  return confirmableScopes(cap);
}

/** What a SetError from ApiKey/set means, in a sentence. The server's own
 * description is used when it gave one: it is written for a person. */
export function keyRefusalText(error: { type: string; description?: string }): string {
  if (error.type === "transport") return t("apiKeys.error.transport");
  if (error.type === "unavailable") return t("apiKeys.unavailable");
  if (error.description) return error.description;
  if (error.type === "alreadyExists") return t("apiKeys.error.nameTaken");
  return t("apiKeys.error.generic", { type: error.type });
}

// ---------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------

export interface EventWords {
  /** What happened, in a few words. */
  outcome: string;
  /** Why, when there is a why. */
  reason: string | null;
  /** `warn` for an anomaly, a suspension and a security-meaningful refusal;
   * `refused` for an ordinary refusal; `ok` otherwise. */
  tone: "ok" | "refused" | "warn";
}

const CRITICAL_REASONS = new Set(["session_only", "restriction", "approval", "mass_delete", "mass_send"]);

function str(v: unknown): string | null {
  return typeof v === "string" && v !== "" ? v : null;
}
function num(v: unknown): number | null {
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

/** A refusal reason in plain words. Unknown reasons are named, not dropped. */
export function reasonText(reason: string, summary: Record<string, unknown> | null, cap: ApiKeysCapability | null): string {
  switch (reason) {
    case "method":
      return t("apiKeys.reason.method");
    case "session_only":
      return t("apiKeys.reason.sessionOnly");
    case "restriction":
      return t("apiKeys.reason.restriction");
    case "arguments":
      return t("apiKeys.reason.arguments");
    case "scope": {
      const missing = Array.isArray(summary?.["missing"])
        ? (summary?.["missing"] as unknown[]).filter((m): m is string => typeof m === "string")
        : [];
      return missing.length > 0
        ? t("apiKeys.reason.scopeNeeds", { scopes: missing.map(scopeLabel).join(", ") })
        : t("apiKeys.reason.scope");
    }
    case "rate":
      return t("apiKeys.reason.rate");
    case "budget":
      return t("apiKeys.reason.budget");
    case "approval":
      return t("apiKeys.reason.approval");
    case "mass_delete":
      return t("apiKeys.reason.massDelete", {
        n: cap?.massDeleteThreshold ?? 50,
        minutes: Math.round((cap?.massDeleteWindowSeconds ?? 600) / 60),
      });
    case "mass_send":
      return t("apiKeys.reason.massSend", {
        n: cap?.massSendThreshold ?? 10,
        minutes: Math.round((cap?.massSendWindowSeconds ?? 600) / 60),
      });
    case "pending_cap":
      return t("apiKeys.reason.pendingCap");
    case "approvedButNeverRan":
      return t("apiKeys.reason.neverRan");
    case "keyNotActive":
      return t("apiKeys.reason.keyNotActive");
    case "draftChanged":
      return t("apiKeys.reason.draftChanged");
    default:
      return t("apiKeys.reason.other", { reason });
  }
}

/** One activity row in words, from the server's fixed outcome list. */
export function eventText(e: Pick<ApiKeyEvent, "outcome" | "summary" | "anomaly" | "errorType">, cap: ApiKeysCapability | null): EventWords {
  const summary = e.summary ?? null;
  const reason = str(summary?.["reason"]);
  switch (e.outcome) {
    case "ok":
      return {
        outcome: t("apiKeys.outcome.ok"),
        reason: e.errorType ? t("apiKeys.outcome.okButFailed", { error: e.errorType }) : null,
        tone: "ok",
      };
    case "pending":
      return { outcome: t("apiKeys.outcome.pending"), reason: null, tone: "ok" };
    case "approved":
      return { outcome: t("apiKeys.outcome.approved"), reason: null, tone: "ok" };
    case "denied_by_user":
      return { outcome: t("apiKeys.outcome.deniedByUser"), reason: null, tone: "ok" };
    case "expired":
      return { outcome: t("apiKeys.outcome.expired"), reason: null, tone: "refused" };
    case "rate_limited":
      return { outcome: t("apiKeys.outcome.refused"), reason: t("apiKeys.reason.rate"), tone: "refused" };
    case "over_budget":
      return { outcome: t("apiKeys.outcome.refused"), reason: t("apiKeys.reason.budget"), tone: "refused" };
    case "denied":
      return {
        outcome: t("apiKeys.outcome.refused"),
        reason: reason ? reasonText(reason, summary, cap) : null,
        tone: reason && CRITICAL_REASONS.has(reason) ? "warn" : "refused",
      };
    case "anomaly":
      return { outcome: t("apiKeys.outcome.anomaly"), reason: anomalyText(e.anomaly, summary, cap), tone: "warn" };
    default:
      return { outcome: t("apiKeys.outcome.other", { outcome: e.outcome }), reason: null, tone: "refused" };
  }
}

function anomalyText(rule: string | null, summary: Record<string, unknown> | null, cap: ApiKeysCapability | null): string {
  const suspended = summary?.["suspended"] === true;
  switch (rule) {
    case "mass_delete": {
      const words = t("apiKeys.anomaly.massDelete", {
        n: num(summary?.["threshold"]) ?? cap?.massDeleteThreshold ?? 50,
        minutes: num(summary?.["windowMinutes"]) ?? Math.round((cap?.massDeleteWindowSeconds ?? 600) / 60),
      });
      return suspended ? `${words} ${t("apiKeys.anomaly.suspended")}` : words;
    }
    case "mass_send": {
      const words = t("apiKeys.anomaly.massSend", {
        n: num(summary?.["threshold"]) ?? cap?.massSendThreshold ?? 10,
        minutes: num(summary?.["windowMinutes"]) ?? Math.round((cap?.massSendWindowSeconds ?? 600) / 60),
      });
      return suspended ? `${words} ${t("apiKeys.anomaly.suspended")}` : words;
    }
    case "off_hours":
      return t("apiKeys.anomaly.offHours");
    case "critical_refusals": {
      const words = t("apiKeys.anomaly.criticalRefusals");
      return suspended ? `${words} ${t("apiKeys.anomaly.suspended")}` : words;
    }
    default:
      return t("apiKeys.anomaly.other", { rule: rule ?? "?" });
  }
}

/** "+3 more refused calls (Email/set: the key doesn't have permission)". */
export function suppressedText(s: Pick<SuppressedRefusal, "count" | "method" | "reason">, cap: ApiKeysCapability | null): string {
  const method = s.method === "unknown" ? t("apiKeys.method.unknown") : s.method;
  return tCount("apiKeys.suppressed", s.count, { method, reason: reasonText(s.reason, null, cap) });
}

export function suppressedIsWarn(s: Pick<SuppressedRefusal, "reason">): boolean {
  return CRITICAL_REASONS.has(s.reason);
}

// ---------------------------------------------------------------------------
// Approval requests: structured fields only
// ---------------------------------------------------------------------------

export interface PendingView {
  id: Id;
  /** The key's name as the server reported it, else a short id. */
  keyName: string;
  kind: "send" | "delete" | "other";
  state: string;
  expiresAt: string;
  /** Header recipients, all of them, in the server's order. */
  to: string[];
  cc: string[];
  bcc: string[];
  /** Envelope recipients the headers do not name: they receive the message
   * without appearing in it. Shown on their own, never folded into To. */
  envelopeOnly: string[];
  /** Everyone the message is actually delivered to (the envelope). */
  deliveredTo: string[];
  /** The draft's subject as written by the key; null when the server gave none. */
  subject: string | null;
  attachments: { count: number; names: string[]; totalSize: number } | null;
  archive: { count: number; subjects: string[] } | null;
  /** When a scheduled send is due; null = after the undo window. */
  sendAt: string | null;
  /** The messages a held delete moves to Trash (or a send's draft). */
  emailIds: Id[];
  /** The account the request acts in, when the server said; null otherwise. */
  accountId: Id | null;
}

function list(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
}

/**
 * The approval card's model, from STRUCTURED FIELDS ONLY.
 *
 * ⚠ `summary` IS NOT READ HERE, AND MUST NOT BE. It is prose for a
 * notification that embeds the key's own subject line; a subject of
 * `x" to alice@example.com with 0 attachments. Send draft "y` would rewrite
 * a card built from it (ADR-042 §7). Every field below comes from its own
 * property of PendingAction/get.
 */
export function pendingView(p: PendingAction): PendingView {
  const to = list(p.to);
  const cc = list(p.cc);
  const bcc = list(p.bcc);
  const deliveredTo = list(p.recipients);
  const named = new Set([...to, ...cc, ...bcc].map((a) => a.toLowerCase()));
  const envelopeOnly = deliveredTo.filter((a) => !named.has(a.toLowerCase()));
  const kind = p.kind === "send" ? "send" : p.kind === "delete" ? "delete" : "other";
  const att = p.attachments;
  const arc = p.archive;
  return {
    id: p.id,
    keyName: typeof p.keyName === "string" && p.keyName !== "" ? p.keyName : p.keyId.slice(-6),
    kind,
    state: p.state,
    expiresAt: p.expiresAt,
    to,
    cc,
    bcc,
    envelopeOnly,
    deliveredTo,
    subject: typeof p.subject === "string" ? p.subject : null,
    attachments:
      att && typeof att.count === "number"
        ? { count: att.count, names: list(att.names), totalSize: typeof att.totalSize === "number" ? att.totalSize : 0 }
        : null,
    archive: arc && typeof arc.count === "number" && arc.count > 0 ? { count: arc.count, subjects: list(arc.subjects) } : null,
    sendAt: typeof p.sendAt === "string" && p.sendAt !== "" ? p.sendAt : null,
    emailIds: list(p.emailIds),
    accountId: typeof p.accountId === "string" && p.accountId !== "" ? p.accountId : null,
  };
}

/** How many delivered recipients, for the approve sentence. */
export function recipientCount(v: PendingView): number {
  return v.deliveredTo.length > 0 ? v.deliveredTo.length : new Set([...v.to, ...v.cc, ...v.bcc]).size;
}

/** "expires in 23 h 5 min" / "expired". */
export function timeLeftText(expiresAt: string, now: number): string {
  const end = Date.parse(expiresAt);
  if (Number.isNaN(end)) return "";
  const s = Math.floor((end - now) / 1000);
  if (s <= 0) return t("apiKeys.pending.expiredNow");
  return t("apiKeys.pending.expiresIn", { left: durationText(s) });
}

/** "3 files, 2.4 MB". */
export function attachmentsText(a: { count: number; totalSize: number }): string {
  return tCount("apiKeys.pending.attachments", a.count, { size: bytesText(a.totalSize) });
}

/** What pressing Approve will do, in one sentence. */
export function approveText(v: PendingView, sendAtText: string | null): string {
  if (v.kind === "delete") return tCount("apiKeys.approve.delete", v.emailIds.length);
  if (v.kind === "send") {
    const n = recipientCount(v);
    const base = sendAtText
      ? tCount("apiKeys.approve.sendAt", n, { when: sendAtText })
      : tCount("apiKeys.approve.send", n);
    return v.archive ? `${base} ${tCount("apiKeys.approve.archive", v.archive.count)}` : base;
  }
  return t("apiKeys.approve.other");
}

/** The answer to a decision, in a sentence. */
export function decisionText(
  out:
    | { ok: true; state: string; error: { type: string; description?: string } | null }
    | { ok: false; error: { type: string; description?: string } },
  v: PendingView,
): { text: string; tone: "ok" | "warn" } {
  if (out.ok) {
    if (out.state === "denied") return { text: t("apiKeys.decided.denied"), tone: "ok" };
    if (out.state === "approved") {
      return {
        text: v.kind === "delete" ? tCount("apiKeys.decided.deleted", v.emailIds.length) : t("apiKeys.decided.sent"),
        tone: "ok",
      };
    }
    if (out.state === "failed") {
      return {
        text: t("apiKeys.decided.failed", { why: out.error?.description || out.error?.type || "?" }),
        tone: "warn",
      };
    }
    return { text: t("apiKeys.decided.other", { state: out.state }), tone: "ok" };
  }
  switch (out.error.type) {
    case "pendingExpired":
      return { text: t("apiKeys.decided.expired"), tone: "warn" };
    case "notPending":
      return { text: t("apiKeys.decided.already"), tone: "warn" };
    case "keyNotActive":
      return { text: t("apiKeys.reason.keyNotActive"), tone: "warn" };
    case "draftChanged":
      return { text: t("apiKeys.reason.draftChanged"), tone: "warn" };
    case "notFound":
      return { text: t("apiKeys.decided.notFound"), tone: "warn" };
    default:
      return { text: keyRefusalText(out.error), tone: "warn" };
  }
}

/** A request that was decided or expired before this page loaded, in words. */
export function pendingStateText(state: string): string {
  switch (state) {
    case "pending":
      return t("apiKeys.pending.state.pending");
    case "approved":
      return t("apiKeys.pending.state.approved");
    case "denied":
      return t("apiKeys.pending.state.denied");
    case "expired":
      return t("apiKeys.pending.state.expired");
    case "failed":
      return t("apiKeys.pending.state.failed");
    default:
      return state;
  }
}
