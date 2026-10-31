// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { messageCSP, messageDocument } from "./messageDocument";

// The message frame's img-src and the app's own origin (#460).
//
// Since the image proxy, an allowed remote image is a same-origin
// /jmap/proxy/... path. In the srcdoc frame's opaque origin `'self'` matches
// nothing, so the origin must be named: `https:` alone covered it on an https
// deployment and covered NOTHING on an http one — every allowed image was refused
// by the browser, with no server-side trace, until someone saw the broken
// placeholders. These tests pin both halves: the origin is admitted when (and
// only when) remote content is allowed, and a blocked message never admits it.

function imgSrc(csp: string): string {
  const d = csp.split(";").map((s) => s.trim()).find((s) => s.startsWith("img-src"));
  if (!d) throw new Error("no img-src in " + csp);
  return d;
}

describe("messageCSP img-src and the app origin", () => {
  it("admits the cleartext app origin when remote content is allowed", () => {
    expect(imgSrc(messageCSP(true, "http://mail.example.test:8090"))).toBe("img-src data: https: http://mail.example.test:8090");
  });

  it("admits an https app origin too (harmless: https: already covers it)", () => {
    expect(imgSrc(messageCSP(true, "https://mail.example.test"))).toBe("img-src data: https: https://mail.example.test");
  });

  // THE NEGATIVE CONTROL. A blocked message must not admit the proxy path: that
  // path is exactly what "blocked" means the browser must not fetch.
  it("admits only data: for a blocked message, whatever the origin", () => {
    expect(imgSrc(messageCSP(false, "http://mail.example.test:8090"))).toBe("img-src data:");
    expect(imgSrc(messageCSP(false))).toBe("img-src data:");
  });

  it("never writes the bare http: scheme, only the one origin", () => {
    const tokens = imgSrc(messageCSP(true, "http://mail.example.test:8090")).split(/\s+/);
    expect(tokens).not.toContain("http:");
    expect(tokens).not.toContain("*");
  });

  it("drops anything that is not an origin rather than writing it into the policy", () => {
    for (const bad of ["", "not an origin", "http://h/path", "http://h; script-src *", "javascript:alert(1)", "http://h:notaport"]) {
      expect(imgSrc(messageCSP(true, bad))).toBe("img-src data: https:");
    }
  });

  it("threads the origin into the document meta tag", () => {
    const doc = messageDocument("<p>hi</p>", true, "http://mail.example.test:8090");
    expect(doc).toContain("img-src data: https: http://mail.example.test:8090;");
    const blocked = messageDocument("<p>hi</p>", false, "http://mail.example.test:8090");
    expect(blocked).toContain("img-src data:;");
    expect(blocked).not.toContain("mail.example.test");
  });
});
