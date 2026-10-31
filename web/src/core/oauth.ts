// SPDX-License-Identifier: AGPL-3.0-only

// CONNECTING A MAIL ACCOUNT BY SIGNING IN AT ITS PROVIDER (#52, ADR-046) — the
// decisions, with no DOM: which result the server sent the browser back with,
// how to take it off the URL, which accounts need reconnecting, and whether
// the "reconnect" banner was already put away for this exact set of accounts.
//
// The server's callback redirects to `/?oauth=<code>&provider=<p>[&account=<id>]#/settings`
// (internal/oauthmail ResultLocation). The query is read ONCE and removed, so a
// reload does not show the message again (ui/oauthReturn.ts does the browser
// half).

import type { Account, Id } from "./jmap/types";

/** The eighteen result codes, exactly as ADR-046 §10 lists them. Anything
 * else in `?oauth=` did not come from this server's callback and is ignored. */
export const OAUTH_RESULT_CODES = [
  "connected",
  "reconnected",
  "already_connected",
  "denied",
  "provider_error",
  "invalid_request",
  "invalid_state",
  "expired",
  "provider_mismatch",
  "exchange_failed",
  "identity_invalid",
  "missing_scope",
  "no_refresh_token",
  "mailbox_refused",
  "mailbox_unreachable",
  "wrong_account",
  "not_configured",
  "server_error",
] as const;

export type OAuthResultCode = (typeof OAUTH_RESULT_CODES)[number];

/** The providers this client has words for. */
export const OAUTH_PROVIDERS = ["google", "microsoft"] as const;
export type OAuthProvider = (typeof OAUTH_PROVIDERS)[number];

export function isOAuthProvider(p: unknown): p is OAuthProvider {
  return typeof p === "string" && (OAUTH_PROVIDERS as readonly string[]).includes(p);
}

export interface OAuthReturn {
  code: OAuthResultCode;
  /** null when the query named none, or one this client does not know. */
  provider: OAuthProvider | null;
  /** The account the result is about (connected, reconnected,
   * already_connected), when the server named one. */
  account: Id | null;
}

/** The query parameters the callback adds, and nothing else. */
const OAUTH_PARAMS = ["oauth", "provider", "account"] as const;

/** An account id as the server writes one: a ULID. A value that is not one is
 * dropped rather than carried into a JMAP call. */
const ULID_RE = /^[0-9A-HJKMNP-TV-Z]{26}$/;

/**
 * The result in a location's query string, or null when there is none — or
 * when `oauth` holds anything but one of the eighteen codes, because a link
 * someone else crafted must not put words of its choosing in front of the user.
 */
export function parseOAuthReturn(search: string): OAuthReturn | null {
  const q = new URLSearchParams(search);
  const code = q.get("oauth");
  if (code === null || !(OAUTH_RESULT_CODES as readonly string[]).includes(code)) return null;
  const provider = q.get("provider");
  const account = q.get("account");
  return {
    code: code as OAuthResultCode,
    provider: isOAuthProvider(provider) ? provider : null,
    account: account !== null && ULID_RE.test(account) ? account : null,
  };
}

/** `search` without the callback's three parameters (anything else is kept),
 * as a string to put back in the URL: "" or "?…". */
export function searchWithoutOAuth(search: string): string {
  const q = new URLSearchParams(search);
  for (const p of OAUTH_PARAMS) q.delete(p);
  const rest = q.toString();
  return rest === "" ? "" : `?${rest}`;
}

/** How a result reads: done, a decision for the user, or a failure. */
export function oauthResultTone(code: OAuthResultCode): "ok" | "info" | "error" {
  if (code === "connected" || code === "reconnected") return "ok";
  if (code === "already_connected") return "info";
  return "error";
}

/** Whether an account signs in at a provider (the field is absent on a server
 * before v1.96, which had none). */
export function isOAuthAccount(a: Pick<Account, "authKind"> | null | undefined): boolean {
  return a?.authKind === "oauth2";
}

/** Whether the account can be reconnected from here: its grant needs the user
 * again, or it was disconnected. operator_error is NOT one — reconnecting
 * cannot fix a server's broken registration, and saying it could is the lie
 * ADR-046 §5 exists to avoid. */
export function canReconnect(a: Pick<Account, "authKind" | "authState"> | null | undefined): boolean {
  return isOAuthAccount(a) && (a?.authState === "needs_reauth" || a?.authState === "disconnected");
}

/** Whether the account has a grant that Disconnect can end. */
export function canDisconnect(a: Pick<Account, "authKind" | "authState"> | null | undefined): boolean {
  return isOAuthAccount(a) && a?.authState !== "disconnected" && a?.authState != null;
}

/** The accounts the reconnect banner is about, in a stable order (by id). */
export function accountsNeedingReconnect(accounts: readonly Account[]): Account[] {
  return accounts
    .filter((a) => isOAuthAccount(a) && a.authState === "needs_reauth")
    .sort((x, y) => x.id.localeCompare(y.id));
}

/**
 * The key a banner dismissal is remembered under: the exact set of accounts it
 * was about. Dismissing hides it for THESE accounts for the rest of the
 * session; another account needing reconnecting is a new fact and brings it
 * back.
 */
export function reconnectDismissKey(accounts: readonly Pick<Account, "id">[]): string {
  return accounts
    .map((a) => a.id)
    .sort()
    .join(",");
}

/**
 * The provider's own page where a person removes an app's access, named in
 * words rather than linked: the exact URL is the provider's to move, and a
 * stale link is worse than a sentence that says where to look.
 */
export function revocationKind(revocation: string): "revoked" | "manual" {
  return revocation === "revoked" ? "revoked" : "manual";
}
