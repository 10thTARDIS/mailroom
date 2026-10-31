// SPDX-License-Identifier: AGPL-3.0-only

/// <reference types="vitest/config" />
// A TYPE-ONLY reference, deliberately: it teaches vite's `defineConfig` about
// the `test` block below without importing anything from vitest at config-load
// time, so `vite build` (and the image build, which runs it) never depends on a
// test runner being installed.

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Vite config for the Mailroom web app (spec docs/mailroom-spec.md §12).
//
// Same-origin deployment: the Go server serves this build's dist/ output at "/"
// (cmd/mailroom webHandler → http.FileServer over config.WebRoot, /srv/web in
// the image), and JMAP/auth live on the same origin — so there is no CORS, no
// bearer token, and no base-path rewriting.
//
// The dev server proxies the API to a locally running `mailroom` so
// `npm run dev` works against a real backend (MAILROOM_API overrides the target).
const target = process.env["MAILROOM_API"] ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react()],
  // ⚠ `css: true` IS LOAD-BEARING FOR ONE TEST (#514). Vitest stubs CSS imports
  // to the empty string by default — including `import "./tokens.css?raw"` —
  // and prefs.readingComfort.test.ts reads that stylesheet to prove the five
  // `html[data-text-size]` font sizes are the same five numbers prefs.ts's
  // TEXT_SIZE_PX names. With the stub the regex matched nothing, the test
  // compared two empty lists and PASSED, which is the exact shape of a check
  // that cannot fail. Nothing else in the suite imports a stylesheet, so this
  // costs the run nothing.
  test: { css: true },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: true,
  },
  server: {
    proxy: {
      "/jmap": { target, changeOrigin: false },
      "/.well-known/jmap": { target, changeOrigin: false },
      "/auth": { target, changeOrigin: false },
    },
  },
});
