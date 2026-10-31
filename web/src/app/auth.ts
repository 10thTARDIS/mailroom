// SPDX-License-Identifier: AGPL-3.0-only

// Auth for the web app. The one place src/app does browser-specific auth work
// (redirects); core/ stays DOM-free.
//
// Same-origin deployment (VERIFIED against cmd/mailroom and asserted route by
// route in cmd/mailroom/gate_test.go — not an assumption):
//
//   * the browser surface "/" is wrapped in the auth middleware with
//     WithUnauthenticatedRedirect(LOGIN_PATH) — so an unauthenticated browser
//     never even loads this bundle. Nor can it load a single asset of it: every
//     path that is not the sign-in page or a health probe is behind the gate;
//   * LOGIN_PATH is a PAGE (server-rendered, internal/auth/page.go), not a bounce
//     to the identity provider. It has to be: a subject the operator allowlist
//     refuses would otherwise loop forever between here and a provider that is
//     perfectly happy to re-issue the same rejected identity. The page's own
//     "Sign in" action (/auth/start) is what begins the OIDC flow;
//   * the JMAP routes (/jmap, /jmap/session, /.well-known/jmap, and the blob and
//     eventsource endpoints) answer 401 rather than redirecting, which is why the
//     client treats a 401 from JMAP as "the session expired mid-session" and
//     redirects itself;
//   * under the dev-auth escape hatch (MAILROOM_DEV_AUTH_SUBJECT) there is no
//     login at all and every request is already the operator's.
//
// The session cookie is HttpOnly, so there is nothing for JS to read: the only
// authentication signal available to the client is "did /jmap/session answer".

/** The sign-in page. A real page — see the note above. */
export const LOGIN_PATH = "/auth/login";
export const LOGOUT_PATH = "/auth/logout";

/** The JMAP session resource (internal/api/service.go mounts this and the
 * RFC 8620 well-known alias). */
export const SESSION_URL = "/jmap/session";

/** Redirects the browser to the login flow, preserving where we were.
 * `return_to` is sanitized server-side (internal/auth sanitizeReturnTo). */
export function redirectToLogin(): void {
  const returnTo = window.location.pathname + window.location.search + window.location.hash;
  const url = new URL(LOGIN_PATH, window.location.origin);
  if (returnTo && returnTo !== "/") url.searchParams.set("return_to", returnTo);
  window.location.assign(url.toString());
}

export function logout(): void {
  window.location.assign(LOGOUT_PATH);
}
