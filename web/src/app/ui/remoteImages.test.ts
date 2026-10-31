// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { messageCSP } from "./messageDocument";
import {
  failedImageCount,
  fetchAllowedImages,
  inlineProxiedImages,
  loadProxiedImages,
  messageFrameDocument,
  proxiedImageCount,
  proxiedImageRefs,
} from "./remoteImages";

// Proxied remote images (#667).
//
// The defect: an allowed message's frame was handed `src="/jmap/proxy/…"`, and a
// sandboxed, opaque-origin frame cannot send the cookie that route is gated by.
// Every such image failed in Chromium and in Firefox on a cookie-gated
// deployment — and no test could see it, because the harnesses that render in a
// browser run without a credential. What CAN be pinned without a browser is the
// property the fix rests on: the frame is never given a proxy path to fetch.

const ACCOUNT = "01KXA000000000000000000AAA";
const MESSAGE = "01KXD0000000000000000000ZZ";
const path = (n: number): string =>
  `/jmap/proxy/${ACCOUNT}/${MESSAGE}/${n.toString(16).padStart(32, "0")}`;

// What internal/api/sanitize.go emits with the proxy on, written out as literal
// server output (the convention messageDocument.pixels.test.ts explains).
const LOGO = `<img alt="logo" data-mailroom-remote-src="${path(1)}" data-mailroom-blocked="">`;
const HERO = `<a href="https://shop.example/"><img alt="hero" data-mailroom-remote-src="${path(2)}" data-mailroom-blocked=""></a>`;
const BEACON =
  '<img data-mailroom-pixel-src="https://tracker.example/o.gif" width="1" height="1" data-mailroom-blocked="">';
const INLINE = '<img alt="sig" src="data:image/png;base64,AAAA">';
const BODY = `<p>Hello</p>${LOGO}${HERO}${BEACON}${INLINE}`;

const PNG = "data:image/png;base64,iVBORw0KGgo=";
const ORIGIN = "https://mail.example.com";

/** Every `src` attribute value in a document, by the server's attribute boundary
 * (a leading space — see the note in messageDocument.pixels.test.ts). */
function srcs(doc: string): string[] {
  return [...doc.matchAll(/ src="([^"]*)"/g)].map((m) => m[1] ?? "");
}

describe("proxiedImageRefs", () => {
  it("finds each proxy path once, in order, and nothing else", () => {
    expect(proxiedImageRefs(BODY + LOGO)).toEqual([path(1), path(2)]);
  });

  it("ignores a parked value that is not a proxy path", () => {
    const odd =
      '<img data-mailroom-remote-src="https://cdn.example/x.png">' +
      '<img data-mailroom-remote-src="/jmap/proxy/../../auth/logout">' +
      '<img data-mailroom-remote-src="">';
    expect(proxiedImageRefs(odd)).toEqual([]);
  });

  it("counts elements, not distinct paths", () => {
    expect(proxiedImageCount(BODY + LOGO)).toBe(3);
  });
});

describe("messageFrameDocument with the image proxy on", () => {
  const allowed = (resolved: Map<string, string> | null): string =>
    messageFrameDocument(BODY, { proxied: true, allowRemote: true, resolved, origin: ORIGIN });

  // ⚠ THE ASSERTION THE FIX RESTS ON. Red on the code before #667, where the
  // allowed document carried both proxy paths as live `src` attributes.
  it("never hands the frame a proxy path: every surviving src is data:", () => {
    const doc = allowed(new Map([[path(1), PNG], [path(2), PNG]]));
    expect(doc).not.toContain(' src="/jmap/proxy/');
    expect(doc).not.toContain("data-mailroom-remote-src");
    const found = srcs(doc);
    // logo, hero and the inline signature. Asserted so that "every src is data:"
    // cannot pass by there being no src at all.
    expect(found).toHaveLength(3);
    for (const s of found) expect(s.startsWith("data:")).toBe(true);
  });

  it("builds the frame with the BLOCKED policy even though remote content is allowed", () => {
    const doc = allowed(new Map([[path(1), PNG]]));
    expect(doc).toContain(`content="${messageCSP(false)}"`);
    expect(doc).not.toContain(ORIGIN);
  });

  it("drops an image that did not arrive instead of leaving it to the frame", () => {
    const doc = allowed(new Map([[path(1), PNG]]));
    expect(doc).not.toContain("/jmap/proxy/");
    expect(doc).toContain('<img alt="hero" data-mailroom-blocked="">');
    expect(srcs(doc)).toHaveLength(2);
  });

  it("shows the body with images still parked while they are on their way", () => {
    const doc = allowed(null);
    expect(srcs(doc)).toEqual(["data:image/png;base64,AAAA"]);
    expect(doc).toContain(`data-mailroom-remote-src="${path(1)}"`);
  });

  it("never restores a tracking pixel", () => {
    const doc = allowed(new Map([[path(1), PNG], [path(2), PNG]]));
    expect(doc).toContain('data-mailroom-pixel-src="https://tracker.example/o.gif"');
    expect(doc).not.toContain(' src="https://tracker.example');
  });

  it("refuses to inline anything that is not an image data: URL", () => {
    const doc = allowed(new Map([[path(1), "https://evil.example/x.png"], [path(2), "data:text/html,x"]]));
    expect(srcs(doc)).toEqual(["data:image/png;base64,AAAA"]);
  });

  it("leaves a blocked message exactly as it was", () => {
    const doc = messageFrameDocument(BODY, {
      proxied: true,
      allowRemote: false,
      resolved: null,
      origin: ORIGIN,
    });
    expect(srcs(doc)).toEqual(["data:image/png;base64,AAAA"]);
  });
});

describe("messageFrameDocument with the image proxy off", () => {
  // The other deployment: the browser fetches the sender's URL itself, and that
  // path is deliberately untouched.
  it("still restores the sender's own URLs for the browser to fetch", () => {
    const body = '<img data-mailroom-remote-src="https://cdn.example/hero.png" data-mailroom-blocked="">';
    const doc = messageFrameDocument(body, {
      proxied: false,
      allowRemote: true,
      resolved: null,
      origin: ORIGIN,
    });
    expect(srcs(doc)).toEqual(["https://cdn.example/hero.png"]);
    expect(doc).toContain(`content="${messageCSP(true, ORIGIN)}"`);
  });
});

describe("failedImageCount", () => {
  it("counts the elements with nothing to show", () => {
    expect(failedImageCount(BODY, new Map([[path(1), PNG], [path(2), PNG]]))).toBe(0);
    expect(failedImageCount(BODY, new Map([[path(1), PNG]]))).toBe(1);
    expect(failedImageCount(BODY, new Map())).toBe(2);
  });
});

describe("loadProxiedImages", () => {
  it("keeps what arrived and drops a null, a rejection and anything over budget", async () => {
    const got = await loadProxiedImages(
      [path(1), path(2), path(3), path(4)],
      async (p) => {
        if (p === path(2)) return null;
        if (p === path(3)) throw new Error("network");
        return { dataUrl: PNG, bytes: p === path(4) ? 90 : 20 };
      },
      100,
    );
    expect([...got.keys()]).toEqual([path(1)]);
  });

  it("stops fetching once the budget is spent", async () => {
    const asked: string[] = [];
    const paths = Array.from({ length: 40 }, (_v, i) => path(i + 1));
    await loadProxiedImages(
      paths,
      async (p) => {
        asked.push(p);
        return { dataUrl: PNG, bytes: 100 };
      },
      100,
    );
    // At most the first wave of concurrent fetches; never all forty.
    expect(asked.length).toBeLessThan(paths.length);
  });
});

describe("fetchAllowedImages", () => {
  // The always-allow / one-off race: the proxy answers 404 until the request for
  // this message has landed, so no image may be asked for before it has.
  it("waits for the one-off request before asking for the first image", async () => {
    const order: string[] = [];
    let landed = false;
    await fetchAllowedImages(BODY, {
      askFirst: true,
      ask: async () => {
        await new Promise((r) => setTimeout(r, 5));
        landed = true;
        order.push("ask");
      },
      fetchOne: async (p) => {
        order.push(landed ? "fetch-after" : "fetch-before");
        return { dataUrl: PNG, bytes: p.length };
      },
    });
    expect(order).toEqual(["ask", "fetch-after", "fetch-after"]);
  });

  it("makes no request on behalf of a remembered grant", async () => {
    let asks = 0;
    const got = await fetchAllowedImages(BODY, {
      askFirst: false,
      ask: async () => {
        asks += 1;
      },
      fetchOne: async () => ({ dataUrl: PNG, bytes: 1 }),
    });
    expect(asks).toBe(0);
    expect(got.size).toBe(2);
  });
});
