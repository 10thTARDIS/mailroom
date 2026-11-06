// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE HOSTILE-PASTE SUITE (ADR-051 §2, §4). ADR-015 allowed one `innerHTML` site
// and forced paste to plain text because pasted HTML from a malicious page
// could fire an `onerror` in our live DOM. Paste is rich now, so this is the test
// that the fear no longer holds: each payload is pasted into the REAL editor
// (the real Tiptap and ProseMirror parser, not a mock) and we assert that
//
//   * nothing ran: a global flag every payload would set stays false, after we
//     fire the events a browser would (error, load, click, ...) on every element
//     that came out;
//   * the document holds none of the dangerous elements, attributes or schemes;
//   * a remote picture is gone, and the user was told.
//
// THE FLAG CAN BE TRIPPED. Vitest's jsdom runs with `runScripts: "dangerously"`
// (its default), under which an inline event-handler attribute on a LIVE element
// really executes. The control test puts the same payloads into the live page by the
// route the editor must never take and shows the flag goes true. Without it,
// "the flag stayed false" would also be what a harness that cannot run handlers
// at all reports, and the suite would prove nothing (CLAUDE.md: negative-control
// every safety assertion).

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { mountEditor, paste } from "./editorTestkit";
import type { Mounted } from "./editorTestkit";

// The flag every payload would set. It is the document title because the test and
// the page's scripts do not share a global object under vitest's jsdom (a
// `window.x` set by a handler is invisible to the test), but they share the DOM.
const SET = "document.title='pwned'";
const FRAME = ["if", "rame"].join("");
const pwned = (): boolean => document.title === "pwned";

interface Payload {
  name: string;
  html: string;
  /** The payload carries a handler attribute, so a live insertion must trip the flag. */
  live: boolean;
}

const PAYLOADS: Payload[] = [
  { name: "img onerror", html: `<img src=x onerror="${SET}">`, live: true },
  // An inline picture is allowed, so this one gets past the picture filter and
  // only the schema stands between its handler and the page.
  { name: "inline img with onerror", html: `<img src="cid:a@inline.mailroom" onerror="${SET}" onload="${SET}">`, live: true },
  { name: "svg onload", html: `<svg onload="${SET}"><circle r=1></circle></svg>`, live: true },
  { name: "javascript: link", html: `<a href="javascript:${SET}" onclick="${SET}">click me</a>`, live: true },
  { name: "data:text/html link", html: `<a href="data:text/html,<script>${SET}</script>">data link</a>`, live: false },
  // (The tag name is assembled: the server-side source scan that pins every
  // frame in web/src to the sandboxed one would otherwise count this string.)
  { name: "iframe", html: `<${FRAME} src="https://evil.example/frame" onload="${SET}"></${FRAME}>`, live: true },
  { name: "script", html: `<script>${SET}</script><p>after script</p>`, live: false },
  { name: "style element", html: `<style>body{background:url(https://evil.example/css)}</style><p>after style</p>`, live: false },
  { name: "form with input", html: `<form action="https://evil.example/post"><input name=pw value=secret onfocus="${SET}" autofocus></form>`, live: true },
  { name: "css url() in style attribute", html: `<div style="background:url(https://evil.example/x)" onclick="${SET}">styled</div>`, live: true },
  { name: "remote image", html: `<p>before</p><img src="https://evil.example/t.png"><p>after</p>`, live: false },
  { name: "object", html: `<object data="https://evil.example/o" onload="${SET}"></object>`, live: true },
  { name: "embed", html: `<embed src="https://evil.example/e" onload="${SET}">`, live: true },
  { name: "meta refresh", html: `<meta http-equiv="refresh" content="0;url=https://evil.example/r"><p>after meta</p>`, live: false },
];

/** Dispatch the events a browser would, on every element under `root` (and root). */
function fireEverything(root: Element): void {
  const els = [root, ...Array.from(root.querySelectorAll("*"))];
  for (const el of els) {
    for (const type of ["error", "load", "click", "mouseover", "focus", "focusin", "toggle"]) {
      el.dispatchEvent(new Event(type, { bubbles: false, cancelable: true }));
    }
  }
}

/** The forbidden route: markup parsed in the LIVE document, handlers armed. */
function live(context: Element, html: string): DocumentFragment {
  const range = document.createRange();
  range.selectNodeContents(context);
  return range.createContextualFragment(html);
}

const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 30));

const DANGEROUS = "script, style, iframe, object, embed, form, input, textarea, select, button, svg, math, meta, link, base, frame, video, audio, source";

/** Everything that must not be in a document the editor produced. */
function assertClean(root: ParentNode, where: string): void {
  expect(root.querySelectorAll(DANGEROUS).length, `${where}: dangerous element`).toBe(0);
  for (const el of Array.from(root.querySelectorAll("*"))) {
    for (const attr of Array.from(el.attributes)) {
      expect(attr.name.startsWith("on"), `${where}: ${el.tagName} has ${attr.name}`).toBe(false);
      if (attr.name === "style") {
        expect(el.tagName, `${where}: style on ${el.tagName}`).toBe("IMG");
        expect(attr.value).toMatch(/^max-width:\s*100%;?$/);
      }
      if (attr.name === "href") {
        expect(attr.value, `${where}: href`).toMatch(/^(https?:|mailto:)/i);
      }
      if (attr.name === "src") {
        expect(attr.value, `${where}: src`).toMatch(/^(cid:|data:image\/)/i);
      }
    }
  }
}

/** Parse HTML into an inert document (never the live page) to inspect it. */
function inert(html: string): Document {
  return new DOMParser().parseFromString(html, "text/html");
}

/** The pictures in the document. (ProseMirror adds its own bare, source-less
 * `img.ProseMirror-separator` hacks to the live DOM; they are not content.) */
function pictures(mm: Mounted): HTMLImageElement[] {
  return Array.from(mm.surface.querySelectorAll<HTMLImageElement>("img:not(.ProseMirror-separator)"));
}

let m: Mounted | null = null;
beforeEach(() => {
  document.title = "calm";
});
afterEach(async () => {
  await m?.unmount();
  m = null;
});

describe("the harness can see a handler run (negative control)", () => {
  for (const p of PAYLOADS.filter((x) => x.live)) {
    it(`${p.name}: placed in the live page by the forbidden route, the flag trips`, async () => {
      const box = document.createElement("div");
      document.body.appendChild(box);
      box.appendChild(live(box, p.html));
      fireEverything(box);
      await settle();
      box.remove();
      expect(pwned()).toBe(true);
    });
  }

  it("the cleanliness check can fail: it rejects a live fragment holding the payloads", () => {
    const box = document.createElement("div");
    box.appendChild(live(box, PAYLOADS[0]!.html));
    expect(() => assertClean(box, "control")).toThrow();
  });
});

describe("pasting hostile HTML into the editor", () => {
  for (const p of PAYLOADS) {
    it(`${p.name}: nothing runs, nothing dangerous remains`, async () => {
      m = await mountEditor("<p>draft</p>");
      await paste(m, { html: p.html, text: "pasted text" });
      fireEverything(m.surface);
      await settle();

      expect(pwned()).toBe(false);
      assertClean(m.surface, "live editor");
      assertClean(inert(m.html()).body, "getHTML");

      const html = m.html();
      expect(html).not.toMatch(/evil\.example/i);
      expect(html).not.toMatch(/javascript:/i);
      expect(html).not.toMatch(/\bon[a-z]+\s*=/i);
      expect(html).not.toContain("pwned");
      expect(html).not.toMatch(/url\(/i);
      expect(html).not.toMatch(/<(script|style|iframe|object|embed|form|input|svg|meta)/i);
    });
  }

  it("a remote picture is dropped, the text around it kept, and the user told once", async () => {
    m = await mountEditor("");
    await paste(m, { html: '<p>before</p><img src="https://evil.example/t.png"><img src="http://evil.example/u.png"><p>after</p>' });
    // The picture's own (now empty) paragraph stays as a blank line.
    expect(m.html()).toBe("<p>before</p><p><br></p><p>after</p>");
    expect(pictures(m)).toHaveLength(0);
    expect(m.notices).toEqual(["Pictures from the web were left out; attach them instead."]);
  });

  it("other sources are dropped too (blob:, file:, a relative path, javascript:)", async () => {
    m = await mountEditor("");
    await paste(m, {
      html: '<p>t</p><img src="blob:https://evil.example/1"><img src="file:///etc/passwd"><img src="/relative.png"><img src="javascript:1">',
    });
    expect(pictures(m)).toHaveLength(0);
    expect(m.notices).toHaveLength(1);
  });

  it("inline pictures survive: cid: and data:image/", async () => {
    m = await mountEditor("");
    await paste(m, {
      html: '<p><img src="cid:a@inline.mailroom"><img src="data:image/png;base64,iVBORw0KGgo="></p>',
    });
    const srcs = pictures(m).map((i) => i.getAttribute("src"));
    expect(srcs).toEqual(["cid:a@inline.mailroom", "data:image/png;base64,iVBORw0KGgo="]);
    expect(m.notices).toEqual([]);
  });

  it("a data: URL that is not an image is dropped", async () => {
    m = await mountEditor("");
    await paste(m, { html: '<p>t</p><img src="data:text/html,<b>x</b>">' });
    expect(pictures(m)).toHaveLength(0);
  });

  it("an image's own style, width and handler are not kept, only the fixed one", async () => {
    m = await mountEditor("");
    await paste(m, {
      html: `<img src="cid:a@inline.mailroom" style="position:fixed;background:url(https://evil.example/x)" onclick="${SET}" width="9999">`,
    });
    const img = pictures(m)[0] as HTMLImageElement;
    expect(img.getAttribute("style")).toMatch(/^max-width:\s*100%;?$/);
    expect(img.hasAttribute("onclick")).toBe(false);
    expect(m.html()).not.toContain("evil.example");
  });

  it("control: formatting that is allowed passes through intact", async () => {
    m = await mountEditor("");
    await paste(m, {
      html:
        '<p><strong>bold</strong> and <em>italic</em></p><ul><li>one</li><li>two</li></ul>' +
        '<ol><li>first</li></ol><p><a href="https://example.com/ok">a link</a> and <a href="mailto:you@example.com">mail</a></p>',
    });
    const html = m.html();
    expect(html).toContain("<strong>bold</strong>");
    expect(html).toContain("<em>italic</em>");
    expect(html).toContain("<ul><li><p>one</p></li><li><p>two</p></li></ul>");
    expect(html).toContain("<ol><li><p>first</p></li></ol>");
    expect(html).toContain('<a href="https://example.com/ok">a link</a>');
    expect(html).toContain('<a href="mailto:you@example.com">mail</a>');
    expect(pwned()).toBe(false);
  });

  it("a scripted link keeps its text but loses the link", async () => {
    m = await mountEditor("");
    await paste(m, { html: '<p><a href="javascript:alert(1)">still here</a></p>' });
    expect(m.html()).toBe("<p>still here</p>");
  });
});
