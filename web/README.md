# Mailroom web

The web app + shared TS client-core for Mailroom (spec `docs/mailroom-spec.md`
§12), and the daily-driver UI of Forgejo issue #23. It is a separate toolchain
from the Go core — no Go code is touched from here — but it is a **shipped
artifact**: the `web` stage of the repo-root `Dockerfile` builds it and the
runtime image serves it from `/srv/web` (`config.DefaultWebRoot`), so a type
error fails the image build.

**Accessibility** — the grid-with-an-active-descendant model, the one live
region, the roles per surface and the keyboard maps are written down in
[`ACCESSIBILITY.md`](ACCESSIBILITY.md). Most of that answer is invisible in a
diff and easy to undo by accident, which is why it is a document and not a
comment.

## Layout

```
web/
├── src/
│   ├── core/            # the shared client-core — framework-agnostic, DOM-FREE.
│   │   │                # A future native app (React Native / Capacitor / Tauri,
│   │   │                # spec §12) imports this unmodified.
│   │   ├── jmap/
│   │   │   ├── types.ts   # the wire shapes internal/api ACTUALLY emits
│   │   │   └── client.ts  # JmapClient: real transport, batched calls, result refs
│   │   ├── session.ts   # the RFC 8620 Session object + the per-type /changes cursors
│   │   ├── storage.ts   # KeyValueStorage — the one host primitive core needs
│   │   ├── store.ts     # normalized cache + the OPTIMISTIC OVERLAY (see below)
│   │   ├── queue.ts     # the durable offline action queue (intent deltas)
│   │   ├── sync.ts      # initial load, view queries, the Foo/changes loop
│   │   ├── actions.ts   # every mutation: instant local apply → queue → reconcile
│   │   ├── views.ts     # the view model → JMAP filters
│   │   └── routes.ts    # every surface as a value: parse/format the hash, and the
│   │                    # one table from a route to a View (#373)
│   └── app/             # the web app — React, and the ONLY place DOM code lives
│       ├── main.tsx     # entry point
│       ├── auth.ts      # login/logout redirects (the session cookie does the rest)
│       ├── routing.ts   # the browser half of routes: location/history, Escape vs
│       │                # the back button, list position, per-device scope
│       ├── state.tsx    # the React binding (useSyncExternalStore over the core store);
│       │                # `view` is DERIVED from the route
│       ├── prefs.ts     # theme (system/light/dark) + density
│       ├── i18n.ts      # t() + the `en` catalog — no hardcoded user-facing strings
│       ├── styles.css   # the design system (solid surfaces, no transparencies)
│       ├── desk.css     # the desk (#375)
│       ├── settings.css # the settings page (#374) — one searchable page, twelve rows
│       └── ui/          # App (the routing table) + Shell (header, rail, toasts, the
│                        # one key map) + routes/{List,Settings,Compose}Route, and the
│                        # components they render: Sidebar, MessageList, ThreadPane,
│                        # Compose, CommandPalette, LabelMenu, ShortcutsHelp, Toasts
├── index.html
├── vite.config.ts
├── tsconfig.json
└── package.json
```

## Architecture

**Framework: React 18** (spec §3 names React), with **no query library**.
TanStack Query is deliberately not used: JMAP already has its own
cache-coherence protocol (opaque per-type state strings + exactly-correct
`Foo/changes` deltas, spec §13), and the offline/optimistic layer has to live in
the DOM-free client-core so a native shell can reuse it — a request cache in
`src/app` would duplicate that and could not be shared. The core exposes a plain
`subscribe`/`getState` store, which React 18 consumes directly with
`useSyncExternalStore`. **This is a deviation from spec §3's "React + TanStack
Query" and should be ratified (or reversed) by an ADR.**

**Sync loop.** `SyncEngine` fetches the session, loads mailboxes/labels/
identities, then polls `Email|Mailbox|Label|Identity|EmailSubmission /changes`
with the per-type cursor. Only when the *Email* state actually moves does it
re-run the current view's `Email/query` — the RFC 8620 §5.6 fallback the server
requires (`Email/queryChanges` answers `cannotCalculateChanges` by design). It
polls because the session's `eventSourceUrl` is advertised but not routed.

**Optimistic apply.** Star/read/archive/trash/label are applied to an *overlay*
in the store (instant), appended to a durable action queue, and replayed against
`Email/set`. A rejection (`SetError`) rolls the overlay delta back and surfaces
the server's own description; a transport failure keeps the action queued and
retries with backoff. An overlay entry retires only when the server's own
`Email` object agrees with it — which matters, because an archive/trash is not
reflected in `Email/query`'s `inMailbox` results until the IMAP writeback lands
and the folder is re-synced.

## Install / build / dev-run

No local Node is assumed.

```sh
cd web
npm ci
npm run dev        # Vite dev server (proxies /jmap + /auth to MAILROOM_API,
                   # default http://127.0.0.1:8080)
npm run build      # tsc --noEmit && vite build  ->  web/dist
```

### Building in Docker (no local Node required)

```sh
docker run --rm -v "$(pwd)/web:/w" -w /w node:22-alpine sh -c "npm ci && npm run build"
```

On Windows/Git Bash prefix with `MSYS_NO_PATHCONV=1`.

## Relationship to the Go server

- **Same-origin.** The Go server serves `web/dist` at `/` (`cmd/mailroom`'s
  `webHandler` → `http.FileServer`), behind the auth middleware. JMAP is
  `GET /jmap/session` (and the `/.well-known/jmap` alias) + `POST /jmap`. No
  CORS, no bearer token: the HttpOnly session cookie authenticates every request.
- **Hash routing, deliberately.** The server serves the SPA with a plain
  `http.FileServer`, so a deep path (`/thread/abc`) would 404 on reload. Every
  surface lives in the URL *hash* instead (`#/inbox`, `#/search/from:alice`,
  `#/inbox?m=<id>`, `#/settings/rules`, `?compose=reply:<id>`) — parsed and
  formatted by `core/routes.ts`, driven by `app/routing.ts` (#373).
- **Auth.** An unauthenticated browser never loads this bundle — nor any asset of
  it: `/` and every other path redirect to `/auth/login`, which is a
  *server-rendered* sign-in page (`internal/auth/page.go`), deliberately NOT part
  of this bundle, because it must be servable to someone with no session and
  every byte here is behind the gate. JMAP answers 401 instead of redirecting,
  which the client turns into a login redirect if the session expires mid-use.
  Under dev-auth there is no login at all, and the sign-in page says so. The
  whole route table is asserted in `cmd/mailroom/gate_test.go`.

## What's real vs stub

| Area | Status |
|---|---|
| `core/jmap/client.ts` | **Real** — batched `POST /jmap`, result references, typed errors |
| `core/sync.ts` | **Real** — session, initial load, view queries, `Foo/changes` cursors |
| `core/store.ts` | **Real** — normalized cache + optimistic overlay + reconciliation |
| `core/queue.ts` | **Real** — durable (localStorage) action queue, retry/backoff/rollback |
| `core/actions.ts` | **Real** — read/star/archive/trash/label, compose, send, Undo Send |
| Unified + per-account views, per-mailbox views, starred, unread, label views | **Real** |
| Multi-select, shift-range, select-all-matching-search, bulk actions | **Real** |
| Search box → `filter.query` + `SearchSnippet/get` highlighting | **Real** |
| Keyboard shortcuts + command palette | **Real** |
| Light/dark (system + manual), density, responsive, high-contrast | **Real** |
| **Message bodies, attachments, reply-all, PWA/Web Push, account health** | **Real.** See the note below. |

### ⚠ This table was stale for the whole 1.x line (#351)

Until 2026-08-30 the rows above listed message bodies as **"NOT POSSIBLE YET"**,
attachments as not downloadable, reply-all as impossible, PWA/Web Push as
unimplemented, and the account-health panel as having no JMAP surface. Every one
of those had shipped:

| claimed missing | actually |
|---|---|
| `bodyValues` / `textBody` / `htmlBody` / `attachments` | `internal/api/body.go` |
| `to` / `cc` on a fetched Email | `internal/api/email.go` |
| Web Push | `web/src/app/push.ts` |
| account health (spec §12) | `internal/api/account_health.go` + `web/src/app/ui/AccountHealthPanel.tsx` |

The "server-side blockers" section that used to follow was stale in the same
way. Its headline item — the search index corrupting itself because Mailroom's
cursor file collided with Tantivy's own `meta.json` — **was fixed**; the cursor
is now `mailroom-index.json`, and `internal/search/index.go` records why the name
matters.

This file was last touched 2026-07-12 and then described the client for roughly
fifty releases without being read again. **The lesson is the format, not the
content:** a hand-maintained "what's real vs stub" table in a README is a claim
with no test behind it, and it fails silently. What the client actually supports
is the code, and what is missing is in the project's issues.

## CI

`.forgejo/workflows/ci.yml` has a `web` job (`npm ci && npm run build`), and the
Dockerfile's `web` stage runs the same command — a type error fails the image.
