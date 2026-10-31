// SPDX-License-Identifier: AGPL-3.0-only

// THE BROWSER HALF OF THE SIGN-IN RESULT (#52). core/oauth.ts decides what the
// query means; this reads it from `location` once, takes it off the URL with
// history.replaceState — so a reload, a bookmark or a shared link never shows
// the message again — and starts a sign-in by leaving for the provider.

import type { Id } from "../../core/jmap/types";
import type { JmapClient } from "../../core/jmap/client";
import { parseOAuthReturn, searchWithoutOAuth } from "../../core/oauth";
import type { OAuthReturn } from "../../core/oauth";

/** undefined: not read yet. Module-level, because the URL is cleaned on the
 * first read and a second reader would find nothing. */
let taken: OAuthReturn | null | undefined;

/**
 * The result the provider's sign-in came back with, or null. The first call
 * reads `loc` and removes the callback's parameters from the address bar
 * (keeping the path, any other parameters and the hash); later calls return
 * the same answer without touching the URL.
 */
export function takeOAuthReturn(
  loc: Pick<Location, "pathname" | "search" | "hash"> = window.location,
  hist: Pick<History, "replaceState" | "state"> = window.history,
): OAuthReturn | null {
  if (taken !== undefined) return taken;
  taken = parseOAuthReturn(loc.search);
  const q = new URLSearchParams(loc.search);
  // Cleaned whenever any of the three was present, valid or not: a stray
  // `?oauth=` left behind would be read again on the next load.
  if (q.has("oauth") || q.has("provider") || q.has("account")) {
    try {
      hist.replaceState(hist.state, "", loc.pathname + searchWithoutOAuth(loc.search) + loc.hash);
    } catch {
      /* a sandboxed frame without history: the message still shows once */
    }
  }
  return taken;
}

/** Forget what was read (tests). */
export function resetOAuthReturn(): void {
  taken = undefined;
}

/** startOAuth's answer when the server returned no usable https address. */
export const START_BAD_URL = "badUrl";

/**
 * Starts a sign-in at `provider` — a new account, or `id` to reconnect — and
 * leaves for the provider's page. Resolves with an error (the server's words,
 * or START_BAD_URL) when it could not start, and the page stays; never rejects.
 *
 * Only an https address is followed: the server builds it from its own
 * registry, and anything else is not something to send a person to.
 */
export async function startOAuth(
  client: Pick<JmapClient, "accountOAuthStart">,
  provider: string,
  id?: Id,
  go: (url: string) => void = (url) => window.location.assign(url),
): Promise<string | null> {
  try {
    const res = await client.accountOAuthStart(provider, id);
    const url = res?.authorizationUrl;
    if (typeof url !== "string" || !url.startsWith("https://")) return START_BAD_URL;
    go(url);
    return null;
  } catch (err) {
    return err instanceof Error ? err.message : String(err);
  }
}
