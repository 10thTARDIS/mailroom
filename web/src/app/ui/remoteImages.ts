// SPDX-License-Identifier: AGPL-3.0-only

// PROXIED REMOTE IMAGES — fetched by the app, handed to the frame as `data:` (#667).
//
// With the image proxy on (ADR-026) the server parks a same-origin
// `/jmap/proxy/{account}/{message}/{token}` path in place of every remote image
// URL. That path is gated by the session cookie, and the message frame cannot
// send it: the frame is sandboxed without `allow-same-origin`, so its origin is
// opaque, a request from it is cross-site, and a `SameSite=Lax` cookie stays
// home. Left for the frame to fetch, every proxied image was a 401 (or, behind a
// fronting auth layer, a redirect into a login page). That is confirmed in
// Chromium and in Firefox; other browsers were not tested. The dev-auth harnesses
// have no credential to withhold, which is how it went unseen.
//
// So the PARENT fetches them, exactly as ./inlineImages.ts does for `cid:`: a
// same-origin request by the app itself, with the cookie, and the bytes go into
// the frame as a `data:` URL. The frame fetches nothing, so the document it is
// given needs no image source but `data:` — the BLOCKED policy — even though the
// user allowed remote content. The server still does the only outbound fetch.
//
// This module is the string half and the bookkeeping. It never touches the
// network itself: the caller passes the fetch in, so everything here is testable
// without a browser.

import { messageDocument } from "./messageDocument";

/** The shape of a proxy path the server writes (internal/api/proxy.go): two
 * ULIDs and a 32-hex token. Anything else in a parked attribute is not a proxy
 * reference and is never fetched. */
const PROXY_PATH = /^\/jmap\/proxy\/[0-9A-Za-z]{26}\/[0-9A-Za-z]{26}\/[0-9a-f]{32}$/;

/** Reports whether `path` is a proxy path this module is prepared to fetch. */
export function isProxyPath(path: string): boolean {
  return PROXY_PATH.test(path);
}

// The same soundness argument as messageDocument.ts's restoreRemoteContent: in
// the server's sanitized output every `"` inside text or an attribute value is
// escaped to `&#34;`, so ` data-mailroom-remote-src="` can only be an attribute
// NAME the server wrote. The leading space is the attribute boundary.
const PARKED_SRC = / data-mailroom-remote-src="([^"]*)"/g;
const PARKED_SRCSET = / data-mailroom-remote-srcset="[^"]*"/g;

/** The most one image may weigh: the proxy's own ceiling (ProxyConfig.MaxBytes). */
export const PROXIED_IMAGE_MAX_BYTES = 8 * 1024 * 1024;

/** The most ALL of one message's images may weigh together.
 *
 * A `data:` URL is the image again at four thirds the size, inside one srcdoc
 * string. Without a ceiling a mail with a dozen large images hands a phone a
 * hundred-megabyte document. Images past the budget are reported as not loaded,
 * which is true, rather than silently dropped. */
export const PROXIED_IMAGES_BUDGET_BYTES = 16 * 1024 * 1024;

/** How many proxy requests are in flight at once. The server limits fetches per
 * user per minute and each uncached one is an outbound request. */
const CONCURRENCY = 6;

/** The distinct proxy paths a sanitized body has parked, in document order. */
export function proxiedImageRefs(html: string): string[] {
  const seen = new Set<string>();
  for (const m of html.matchAll(PARKED_SRC)) {
    const path = m[1] ?? "";
    if (isProxyPath(path)) seen.add(path);
  }
  return [...seen];
}

/** How many `<img>` elements carry a proxy reference (a path used twice counts
 * twice: this is what the reader sees, not what is fetched). */
export function proxiedImageCount(html: string): number {
  let n = 0;
  for (const m of html.matchAll(PARKED_SRC)) if (isProxyPath(m[1] ?? "")) n += 1;
  return n;
}

/** How many of those elements have no entry in `resolved`. */
export function failedImageCount(html: string, resolved: ReadonlyMap<string, string>): number {
  let n = 0;
  for (const m of html.matchAll(PARKED_SRC)) {
    const path = m[1] ?? "";
    if (isProxyPath(path) && !resolved.has(path)) n += 1;
  }
  return n;
}

/** Rewrites every parked remote image to its `data:` URL.
 *
 * ⚠ EVERY PARKED ATTRIBUTE IS CONSUMED, resolved or not. One that did not
 * resolve (a refusal, a wrong type, over budget, not a proxy path at all) is
 * REMOVED, so the image shows its alt text. Nothing is left under a name
 * restoreRemoteContent would turn back into a `src`, and so nothing is left for
 * the frame to fetch: the output is safe to hand to messageDocument with remote
 * content OFF. A tracking pixel is parked under a different name
 * (`data-mailroom-pixel-src`) and is not touched here at all. */
export function inlineProxiedImages(html: string, resolved: ReadonlyMap<string, string>): string {
  return html
    .replace(PARKED_SRC, (_whole, path: string) => {
      const url = isProxyPath(path) ? resolved.get(path) : undefined;
      // A data: URL never contains a `"`, so it is safe to inline unescaped.
      return url !== undefined && url.startsWith("data:image/") ? ` src="${url}"` : "";
    })
    .replace(PARKED_SRCSET, "");
}

/** One fetched image: its `data:` URL and what it weighed on the wire. */
export interface FetchedImage {
  dataUrl: string;
  bytes: number;
}

/** Fetches every path through `fetchOne`, a few at a time, and returns the ones
 * that came back. A rejected or null fetch is simply absent from the result, and
 * so is anything that would take the message past `budgetBytes`. */
export async function loadProxiedImages(
  paths: readonly string[],
  fetchOne: (path: string) => Promise<FetchedImage | null>,
  budgetBytes = PROXIED_IMAGES_BUDGET_BYTES,
): Promise<Map<string, string>> {
  const resolved = new Map<string, string>();
  let spent = 0;
  let next = 0;
  const worker = async (): Promise<void> => {
    for (;;) {
      const path = paths[next];
      next += 1;
      if (path === undefined) return;
      // Once the budget is gone there is nothing a further fetch could add.
      if (spent >= budgetBytes) continue;
      let image: FetchedImage | null = null;
      try {
        image = await fetchOne(path);
      } catch {
        image = null;
      }
      if (!image || spent + image.bytes > budgetBytes) continue;
      spent += image.bytes;
      resolved.set(path, image.dataUrl);
    }
  };
  await Promise.all(Array.from({ length: Math.min(CONCURRENCY, paths.length) }, worker));
  return resolved;
}

/** Everything one allowed message needs from the server, in the order it needs
 * it: the one-off request first when the user clicked on THIS message, and only
 * then the images.
 *
 * ⚠ THE ORDER IS THE POINT. The proxy answers 404 for a message nobody asked for
 * (ADR-026 §3c), so an image requested before `ask` has landed is a broken
 * image that stays broken. That was a real race: the request used to be fired
 * and not awaited. A remembered grant passes `askFirst: false` and no request is
 * made on its behalf — the proxy applies its own, stricter match to a grant. */
export async function fetchAllowedImages(
  html: string,
  opts: {
    askFirst: boolean;
    ask: () => Promise<unknown>;
    fetchOne: (path: string) => Promise<FetchedImage | null>;
  },
): Promise<Map<string, string>> {
  if (opts.askFirst) await opts.ask();
  return loadProxiedImages(proxiedImageRefs(html), opts.fetchOne);
}

/** The document handed to the message frame.
 *
 * ⚠ WITH THE PROXY ON IT IS ALWAYS BUILT WITH REMOTE CONTENT OFF, even for an
 * allowed message. Every image is a `data:` URL by then, so the frame needs —
 * and gets — the blocked policy (`img-src data:`), and nothing is restored for
 * it to fetch. `resolved` is null until the images have arrived; until then the
 * body is shown as it was, images still parked, rather than nothing.
 *
 * With the proxy off nothing changes: the parked URLs are the sender's own and
 * the browser fetches them, which is what "allow" has always meant there. */
export function messageFrameDocument(
  html: string,
  opts: {
    proxied: boolean;
    allowRemote: boolean;
    resolved: ReadonlyMap<string, string> | null;
    origin: string;
  },
): string {
  if (!opts.proxied) return messageDocument(html, opts.allowRemote, opts.origin);
  const body =
    opts.allowRemote && opts.resolved !== null ? inlineProxiedImages(html, opts.resolved) : html;
  return messageDocument(body, false, opts.origin);
}
