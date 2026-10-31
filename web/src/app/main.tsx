// SPDX-License-Identifier: AGPL-3.0-only

// Mailroom web app entry point (Forgejo issue #23).
//
// The Go server only serves this bundle to an AUTHENTICATED browser (the "/"
// route is wrapped in the auth middleware with an unauthenticated redirect —
// cmd/mailroom/main.go), so there is no pre-flight auth probe here: if this code
// is running, the session cookie was already accepted. A session that expires
// mid-use surfaces as a 401 from JMAP, and the store's `unauthenticated` state
// takes us back to the login page (state.tsx useAuthGuard).

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

// tokens.css FIRST (#376): it defines every custom property styles.css reads, and
// import order is load order — styles.css after it would otherwise be one Vite
// dev-server reorder away from resolving tokens against nothing.
import "./tokens.css";
import "./styles.css";
// The route surfaces, after styles.css so each can lean on the component classes
// there rather than restating them. `routes.css` used to sit here and is gone
// (#374): it held nothing but the settings INDEX's three rules, and there is no
// index any more — settings.css is its replacement, not an addition beside it.
//
// The desk (#375), then the settings page (#374), then the composer (#448): the settings page borrows
// the desk's idiom (a heading that is a button, muted fact lines, words rather
// than glyphs) and overrides the shared `.sheet` geometry, so it has to be the
// last word on both.
import "./desk.css";
import "./settings.css";
// The contacts page (#816): its own list/page geometry, after settings.css so
// it can reuse the key cards' rules it shows on a contact.
import "./contacts.css";
// The composer (#448) last of the five: it takes the reading-pane column now,
// which means overriding the docked-panel geometry styles.css still declares for
// `.compose`, and placing itself in the workspace grid — both of which have to be
// the last word.
import "./compose.css";
// The attachment viewer (#449) after it, and last: it takes the same reading-pane
// column the composer does — from the same placement function — so its rules have to
// be able to override both the docked-panel geometry in styles.css and anything
// compose.css says about that track.
import "./viewer.css";
import { AppProvider } from "./state";
import { App } from "./ui/App";
import { installPopoverFit } from "./ui/popoverFit";
import {
  applyDensity,
  applyRowLayout,
  applyTextSize,
  applyTheme,
  loadDensity,
  loadRowLayout,
  loadTextSize,
  loadTheme,
  watchSystemTheme,
} from "./prefs";

// BEFORE THE FIRST RENDER, all four of them. Theme and density have always been
// applied here; the text size and the row layout (#514) join them for a
// stronger version of the same reason — the root font size decides the size of
// every box on the page, so one applied after the first paint is a whole app
// visibly resizing itself on every load. The server's own values arrive later
// and win (ui/Shell.tsx applyServerPrefs, "SERVER WINS AT LOAD"); these are what
// this device last saw, which is the right thing to paint in the meantime.
const theme = loadTheme();
applyTheme(theme);
applyDensity(loadDensity());
applyTextSize(loadTextSize());
applyRowLayout(loadRowLayout());
watchSystemTheme(loadTheme);

const rootEl = document.getElementById("app");
if (!rootEl) throw new Error("mailroom: #app root element missing from index.html");

createRoot(rootEl).render(
  <StrictMode>
    <AppProvider>
      <App />
    </AppProvider>
  </StrictMode>,
);

// Every anchored pop-up (menus, suggestion lists) is kept on the screen by one
// shared rule (#880), not by a rule per menu.
installPopoverFit();

// Register the service worker (Forgejo #42, ADR-016) for offline app-shell
// caching and Web Push display. This runs from THIS external module — never an
// inline <script> — so `script-src 'self'` and the no-inline-script invariant
// hold. Feature-detected and best-effort: a browser without service workers, or
// a failed registration, just runs the app online-only. Registered after `load`
// so it never contends with the initial render/boot for bandwidth.
if ("serviceWorker" in navigator) {
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch(() => {
      // Non-fatal: the app works without the SW; it only loses offline boot and
      // Web Push. Nothing to surface to the user here.
    });
  });
}
