// Mailroom service worker (Forgejo #42, ADR-016) — plain JS, NO imports.
//
// This file is served verbatim from web/public/ at same-origin "/sw.js", so its
// registration scope is the whole app ("/"). It is registered from an EXTERNAL
// app module (src/app/main.tsx via navigator.serviceWorker.register), NEVER from
// an inline <script>, so the site's `script-src 'self'` CSP and the
// no-inline-script invariant hold (ADR-016 adds `worker-src 'self'`).
//
// It does two jobs, both conservative:
//
//   1. Web Push display (RFC 8030 client half). The server sends an encrypted
//      JSON payload {title, body, url}; we render a notification and route a
//      click to the app. Subscription/keys are managed by src/app/push.ts — the
//      SW only DISPLAYS.
//
//   2. Offline app-shell cache (Cache API). The build's HTML + hashed assets are
//      the only things cached: the navigation shell (index.html at "/") and the
//      immutable /assets/* bundles. API/auth traffic (/jmap, /auth,
//      /.well-known) is NEVER cached — it is always live, so a stale cache can
//      never mask auth state or serve a user another user's data.

/* global self, caches, clients, fetch, Response, URL */

// Bump this when the shell-caching logic changes; `activate` deletes any cache
// whose name is not the current one, so old shells are reclaimed on upgrade.
const CACHE_VERSION = "v1";
const CACHE_NAME = `mailroom-shell-${CACHE_VERSION}`;

// The navigation fallback: the app is a single-page app (hash-routed), so every
// in-app navigation resolves to the same server-rendered index.html at "/".
const SHELL_URL = "/";

// Path prefixes that must always go to the network, never the cache: the JMAP
// API, the auth/session endpoints, and the RFC 8620 well-known alias. Caching
// any of these would be a correctness AND a security bug.
const NEVER_CACHE_PREFIXES = ["/jmap", "/auth", "/.well-known"];

// --- Lifecycle --------------------------------------------------------------

self.addEventListener("install", (event) => {
  // Precache the shell so a cold, offline start still boots the app.
  event.waitUntil(
    caches
      .open(CACHE_NAME)
      .then((cache) => cache.add(SHELL_URL))
      .then(() => self.skipWaiting())
      .catch(() => {
        // A failed precache must not wedge the install: the fetch handler
        // degrades to network-only, which is the pre-SW behaviour.
      }),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((names) =>
        Promise.all(names.filter((n) => n !== CACHE_NAME).map((n) => caches.delete(n))),
      )
      .then(() => self.clients.claim()),
  );
});

// --- Fetch: offline app-shell cache -----------------------------------------

function isNeverCached(url) {
  return NEVER_CACHE_PREFIXES.some(
    (p) => url.pathname === p || url.pathname.startsWith(`${p}/`),
  );
}

self.addEventListener("fetch", (event) => {
  const { request } = event;

  // Only ever touch same-origin GETs; everything else is passed straight
  // through to the network (no respondWith = default browser handling).
  if (request.method !== "GET") return;
  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;
  if (isNeverCached(url)) return;

  // Navigations: network-first with the cached shell as the offline fallback.
  // Network-first keeps auth live (an unauthenticated navigation still gets the
  // server's real response/redirect); the cache only steps in when offline.
  if (request.mode === "navigate") {
    event.respondWith(
      fetch(request).catch(() =>
        caches
          .open(CACHE_NAME)
          .then((cache) => cache.match(SHELL_URL))
          .then(
            (cached) =>
              cached ??
              new Response("Offline", {
                status: 503,
                headers: { "Content-Type": "text/plain; charset=utf-8" },
              }),
          ),
      ),
    );
    return;
  }

  // Immutable hashed build assets (Vite emits /assets/<name>.<hash>.<ext>):
  // cache-first, since the hash in the name makes them safe to serve forever.
  if (url.pathname.startsWith("/assets/")) {
    event.respondWith(
      caches.open(CACHE_NAME).then(async (cache) => {
        const hit = await cache.match(request);
        if (hit) return hit;
        const res = await fetch(request);
        // Only cache a clean, same-origin 200 — never an opaque or error body.
        if (res.ok && res.type === "basic") cache.put(request, res.clone());
        return res;
      }),
    );
    return;
  }

  // Anything else (favicon, manifest, icons): network with a cache fallback so
  // it still resolves offline once seen, but we do not aggressively pre-cache.
  event.respondWith(
    fetch(request).catch(() =>
      caches.open(CACHE_NAME).then((cache) => cache.match(request)),
    ),
  );
});

// --- Web Push: display ------------------------------------------------------

self.addEventListener("push", (event) => {
  // The server sends an RFC 8291-encrypted JSON body {title, body, url}. Be
  // defensive: a malformed or bodyless push still shows something sane.
  let payload = {};
  if (event.data) {
    try {
      payload = event.data.json();
    } catch {
      payload = { body: event.data.text() };
    }
  }

  const title = typeof payload.title === "string" && payload.title ? payload.title : "Mailroom";
  const body = typeof payload.body === "string" ? payload.body : "";
  const url = typeof payload.url === "string" && payload.url ? payload.url : "/";
  const tag = typeof payload.tag === "string" && payload.tag ? payload.tag : "mailroom";

  event.waitUntil(
    self.registration.showNotification(title, {
      body,
      tag,
      data: { url },
      icon: "/icons/icon-192.png",
      badge: "/icons/icon-192.png",
    }),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();

  const raw = event.notification.data && event.notification.data.url;
  let target = new URL(typeof raw === "string" && raw ? raw : "/", self.location.origin);
  // Same origin only: a push payload naming another origin (an absolute URL)
  // opens the app root instead, so a notification can never send a tap off
  // site (#856 review F2).
  if (target.origin !== self.location.origin) target = new URL("/", self.location.origin);
  // The app routes in the HASH (#/settings/api-keys); the server serves files,
  // not those paths, so a push that names a route as a PATH (the API-key alerts
  // send "/settings/api-keys", #49) would open a 404. Fold such a path into the
  // hash it means.
  if (!target.hash && target.pathname.startsWith("/settings/")) {
    target.hash = `#${target.pathname}`;
    target.pathname = "/";
  }

  // Focus an existing Mailroom tab if one is open, else open a new one. Matching
  // by origin (not exact URL) means an already-open app just gets focused and
  // navigated, rather than spawning a duplicate tab.
  event.waitUntil(
    self.clients
      .matchAll({ type: "window", includeUncontrolled: true })
      .then((windowClients) => {
        for (const client of windowClients) {
          const clientUrl = new URL(client.url);
          if (clientUrl.origin === target.origin && "focus" in client) {
            if ("navigate" in client && client.url !== target.href) {
              return client.focus().then(() => client.navigate(target.href));
            }
            return client.focus();
          }
        }
        if (self.clients.openWindow) return self.clients.openWindow(target.href);
        return undefined;
      }),
  );
});
