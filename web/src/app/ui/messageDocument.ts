// SPDX-License-Identifier: AGPL-3.0-only

// THE MESSAGE RENDER SANDBOX — the client half of Mailroom's hostile-content
// defence (spec §11, Forgejo #39). The whole threat model, and what is and is
// not defended, is written down once in docs/hostile-content.md; read that
// before you change anything in this file.
//
// The one-paragraph version: every HTML message body is attacker-controlled
// input, authored by a stranger, and we render it inside the operator's own
// origin next to their mail. One mistake here turns a phishing email into stored
// XSS with full access to the session. So the body is NEVER injected into our
// DOM — not sanitized, not "carefully", not ever. It goes into a SANDBOXED
// IFRAME whose document has an OPAQUE ORIGIN and its own restrictive CSP, and it
// has ALREADY been sanitized on the server (internal/api/sanitize.go) before it
// reached us.
//
// Two independent layers, on purpose. The server sanitizer can be wrong about
// one element; the browser's origin isolation is the thing we are actually
// betting on.

/** The iframe sandbox token list, and the entire reason this is safe.
 *
 * The load-bearing absence — read this first:
 *
 *   allow-same-origin   — ABSENT, and it must NEVER be added. The document has an
 *                         OPAQUE origin: it cannot read our cookies, our
 *                         localStorage, our DOM, or our session. `allow-scripts` +
 *                         `allow-same-origin` TOGETHER would additionally let the
 *                         frame delete its own sandbox attribute — the classic
 *                         escape. We take `allow-scripts` (below) precisely
 *                         BECAUSE we never take `allow-same-origin`: a script in
 *                         here has no origin to abuse.
 *   allow-forms         — absent. A <form> in a message body is a credential
 *                         phishing surface and nothing else. (The server
 *                         sanitizer also unwraps every form; this is the second
 *                         lock.)
 *   allow-top-navigation, allow-modals, allow-downloads — absent.
 *
 * What IS here, and why none of it is a hole:
 *
 *   allow-scripts                    — present, ADDED for content-fit auto-height
 *                                      (#100). It is safe ONLY because of a triad
 *                                      that all three hold at once:
 *                                        (a) NO allow-same-origin — the frame is
 *                                            opaque; a script has zero same-origin
 *                                            access (no cookies, no parent DOM).
 *                                        (b) CSP HASH-PINS one script — `script-src
 *                                            'sha256-<reporter>'` in BOTH this
 *                                            document's policy AND the inherited
 *                                            app-shell policy (they intersect), so
 *                                            the ONLY inline script that can run is
 *                                            our exact height reporter (below).
 *                                            The message's own scripts, inline
 *                                            event handlers and javascript: URLs
 *                                            are all hash-mismatched and blocked.
 *                                        (c) the SERVER SANITIZER already strips
 *                                            every <script>, on*= and javascript:
 *                                            from the body — belt and suspenders.
 *                                      Remove any one of the three and this token
 *                                      becomes a hole; see docs/hostile-content.md.
 *   allow-popups                     — without it, NO LINK IN ANY HTML EMAIL
 *                                      WORKS. The only script that can run is our
 *                                      reporter, which never calls window.open();
 *                                      a message script cannot run (hash pin), so
 *                                      the only thing that opens a popup is still a
 *                                      genuine user click on a genuine <a>.
 *   allow-popups-to-escape-sandbox   — the tab a user opens must be a NORMAL tab,
 *                                      not an opaque-origin one in which the
 *                                      destination site is broken. Its cost is
 *                                      that the opened page would get a
 *                                      `window.opener` handle on the message
 *                                      frame — so the server forces
 *                                      rel="noopener noreferrer nofollow" onto
 *                                      every surviving <a>, which removes it.
 */
export const MESSAGE_SANDBOX = "allow-popups allow-popups-to-escape-sandbox allow-scripts";

/** The height reporter — the ONE script trusted to run inside a message frame.
 *
 * It runs in the opaque-origin sandbox and can do exactly one thing: read its own
 * document's content height and natural width (#801: the parent scales a wide
 * newsletter to fit a phone) and `postMessage` them to the parent. It has no
 * same-origin access, no eval, no network (message CSP is `connect-src`-less under
 * `default-src 'none'`), and no other capability. The parent (ThreadPane's
 * HtmlBody) validates `event.source === iframe.contentWindow`, ignores everything
 * else, and clamps the value — so even this one channel carries only two bounded ints.
 *
 * ⚠ The bytes of this string are pinned by sha256 in TWO policies (this document's
 * `messageCSP` and the app-shell CSP in cmd/mailroom/main.go). CHANGING A SINGLE
 * BYTE breaks the pin and the reporter silently stops running. If you edit it,
 * recompute the hash and update REPORTER_SHA256 here AND `appCSP` in main.go. The
 * value is asserted in scripts/hostile-content-e2e.sh and the Go security tests. */
const REPORTER_JS =
  "(function(){function s(){parent.postMessage({type:'mailroom-height',height:document.body?document.body.scrollHeight:0,width:document.documentElement.scrollWidth},'*')}window.addEventListener('load',s);if(window.ResizeObserver&&document.body){new ResizeObserver(s).observe(document.body)}s()})();";

/** sha256-base64 of REPORTER_JS, as a CSP source expression. Stable constant: it
 * is duplicated by hand into `appCSP` (cmd/mailroom/main.go) because the message
 * frame inherits that policy and the two must intersect on this exact hash. */
export const REPORTER_SHA256 = "sha256-1XkLCQZRSIgsRrQKKNvaJTcjTinrA6u/CFBnA4fTy+4=";

/** The type field of the height message the reporter posts. */
export const HEIGHT_MESSAGE_TYPE = "mailroom-height";

/** The CSP of the rendered message document.
 *
 * `default-src 'none'` denies every fetch type that falls back to it — connect,
 * frame, child, worker, manifest, object, media, prefetch. The directives that do
 * NOT fall back to default-src, or that we deliberately narrow, are stated
 * explicitly, because a policy that relies on a fallback that does not exist is a
 * policy with a hole:
 *
 *   script-src 'sha256-<reporter>' — the ONLY script permitted is our exact height
 *                       reporter (REPORTER_JS), pinned by hash. `default-src
 *                       'none'` would otherwise ban all script; we open a
 *                       single-byte-exact keyhole and nothing else fits it. A
 *                       message's own <script>, inline on*= handler or javascript:
 *                       URL is a hash mismatch and stays blocked — which is what
 *                       makes `allow-scripts` on the sandbox safe (see
 *                       MESSAGE_SANDBOX and docs/hostile-content.md).
 *   form-action 'none'  — form submission is NOT covered by default-src.
 *   base-uri 'none'     — <base href> is NOT covered by default-src.
 *   (frame-ancestors is ignored in a <meta> policy by spec; the app shell's
 *    header policy and the sandbox cover it.)
 *
 * `img-src data:` is the remote-content block: no tracking pixel, no CSS
 * background image, no srcset, no <picture>. `font-src data:` closes @font-face.
 * `style-src 'unsafe-inline'` permits inline CSS — which cannot script in any
 * modern browser — but permits NO url() source, so CSS cannot fetch either.
 *
 * NOTE that this document ALSO inherits the app shell's CSP (a `srcdoc` iframe
 * inherits its embedder's policy container — CSP3 §7.4), and the two INTERSECT.
 * The shell's `script-src` therefore ALSO lists `'sha256-<reporter>'` (main.go's
 * appCSP) — without that, the intersection with the shell would ban our reporter
 * too and auto-height would silently break. In the opaque origin, the shell's
 * `'self'` still matches nothing, so it opens no other script path.
 *
 * @param allowRemote the user has explicitly asked, for THIS message, to load
 *   remote content (a one-off click, or a remembered sender/domain allow). It is
 *   off by default; see the note on remote content in docs/hostile-content.md.
 * @param origin the app's own origin (`window.location.origin`), admitted for
 *   images ONLY when remote content is allowed. Since the image proxy
 *   (ADR-026) an allowed image is a same-origin `/jmap/proxy/...` path, and in
 *   this frame's opaque origin `'self'` matches nothing, so the origin has to be
 *   spelled out — `https:` alone covers it on an https deployment and covers
 *   NOTHING on an http one, which is how every allowed image was silently
 *   refused for a month on a cleartext instance (#460). A blocked message admits
 *   only `data:` regardless of origin: the proxy path is exactly what "blocked"
 *   must not load. A malformed origin is ignored rather than written into a
 *   policy, since a policy is not the place to discover a typo.
 */
export function messageCSP(allowRemote: boolean, origin = ""): string {
  // What counts as an origin: scheme, host, optional port, nothing else — no
  // path, no credentials, no whitespace, no `;` that could end the directive.
  // `window.location.origin` always fits; anything else is not an origin and is
  // dropped (#460). Inline rather than a module constant ON PURPOSE:
  // scripts/e2e-policy.cjs evaluates this function body by itself, so the
  // browser e2e suites render under exactly the policy the client ships only if
  // the function is self-contained.
  const isOrigin = /^https?:\/\/[A-Za-z0-9.-]+(?::\d{1,5})?$/.test(origin);
  const own = allowRemote && isOrigin ? ` ${origin}` : "";
  const img = allowRemote ? `data: https:${own}` : "data:";
  return [
    "default-src 'none'",
    `script-src '${REPORTER_SHA256}'`,
    `img-src ${img}`,
    "style-src 'unsafe-inline'",
    "font-src data:",
    "form-action 'none'",
    "base-uri 'none'",
  ].join("; ");
}

/** The marker the SERVER puts on an element whose remote src it stripped
 * (internal/api/sanitize.go). Its presence in a body is the ONLY thing that
 * makes the "Show remote content" banner appear — so the banner is a fact about
 * the message, not a guess. */
const BLOCKED_MARKER = "data-mailroom-blocked";

/** Reports whether the server blocked remote content in this body. */
export function hasBlockedRemoteContent(html: string): boolean {
  return html.includes(BLOCKED_MARKER);
}

/** Restores the remote image URLs the server parked in data- attributes.
 *
 * This is a plain string replacement, and that is SOUND rather than lazy: it
 * operates only on the server's own sanitized output, in which every `"` inside
 * text and inside an attribute VALUE has been escaped to `&#34;`
 * (Go's html.EscapeString). The byte sequence `data-mailroom-remote-src="` can
 * therefore occur in exactly one place — an attribute NAME the server itself
 * wrote — and nowhere a message author could put it.
 *
 * It restores images ONLY. CSS `url()`, `@import`, `<link>`, `background=`,
 * `poster=` and the media elements are removed by the server and are NOT
 * restorable: their only use in a mail body is tracking, and none of them is
 * worth a channel.
 */
export function restoreRemoteContent(html: string): string {
  return html
    .replaceAll('data-mailroom-remote-src="', 'src="')
    .replaceAll('data-mailroom-remote-srcset="', 'srcset="');
}

/** The names the server parks a TRACKING PIXEL's URL under (#55 increment 2).
 *
 * ⚠ THE POINT IS THAT restoreRemoteContent ABOVE DOES NOT MENTION THEM. A beacon
 * is parked under a name this module never restores, so suppressing it is not a
 * decision made here and cannot be got wrong here — there is no branch to
 * mis-write and no state to be in. The server decides what IS a beacon; this
 * file only counts them, to say so.
 *
 * They must never be prefixed by a restorable name, or the replacement above
 * would match and resurrect them; internal/api's TestRemoteContentContract
 * asserts that in the same commit these strings live in. */
const PIXEL_MARKERS = [
  'data-mailroom-pixel-src="',
  'data-mailroom-pixel-srcset="',
] as const;

/** How many tracking pixels the server withheld from this body.
 *
 * Counted so the reading pane can SAY it happened. #202 added a line explaining
 * why images loaded from a remembered decision, on the grounds that "a pane that
 * silently behaves differently for one sender than another erodes trust in the
 * default" — withholding part of what the user allowed is the same situation,
 * and on a real mailbox it applies to every message from an allowed
 * sender rather than to a rare one.
 *
 * It also explains an otherwise puzzling outcome: a "show remote content" click
 * on a message whose ONLY remote content was a beacon correctly loads nothing. */
export function blockedPixelCount(html: string): number {
  let n = 0;
  for (const marker of PIXEL_MARKERS) {
    let from = 0;
    for (;;) {
      const i = html.indexOf(marker, from);
      if (i < 0) break;
      n += 1;
      from = i + marker.length;
    }
  }
  return n;
}

/** Wraps a (server-sanitized) message body in the document that will be handed
 * to the sandboxed iframe's srcdoc.
 *
 * There is no <base href> and no <base target>: the server forces
 * target="_blank" rel="noopener noreferrer nofollow" onto every link it keeps,
 * which is both stronger (a <base> cannot set rel) and portable to a client that
 * is not this one.
 *
 * The trusted height reporter (REPORTER_JS) is the LAST thing in <body>, so
 * `document.body` and the sanitized content already exist when it runs. It is the
 * only script the message CSP permits (hash-pinned); the body's own scripts were
 * stripped by the server and would be hash-mismatched anyway.
 */
export function messageDocument(html: string, allowRemote: boolean, origin = ""): string {
  const body = allowRemote ? restoreRemoteContent(html) : html;
  return (
    `<!DOCTYPE html><html><head><meta charset="utf-8">` +
    `<meta http-equiv="Content-Security-Policy" content="${messageCSP(allowRemote, origin)}">` +
    `<meta name="referrer" content="no-referrer">` +
    `<style>body{font:14px/1.5 system-ui,sans-serif;color:#111;margin:0;padding:4px;` +
    `background:#fff;word-break:break-word}` +
    `img{max-width:100%}` +
    `table{max-width:100%}` +
    `</style></head><body>${body}<script>${REPORTER_JS}</script></body></html>`
  );
}
