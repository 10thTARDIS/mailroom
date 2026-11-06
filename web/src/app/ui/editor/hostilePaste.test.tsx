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

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { mountEditor, paste } from "./editorTestkit";
import type { Mounted } from "./editorTestkit";

// Every test mounts a real editor, some several; the default 5 s is too tight when the
// machine is busy (a loaded CI runner timed out on the first, plainest tests).
vi.setConfig({ testTimeout: 60_000 });

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

const OWN_DECLARATIONS: Record<string, RegExp> = {
  color: /^(#[0-9a-f]{6}|rgb\(\d{1,3}, \d{1,3}, \d{1,3}\))$/,
  "background-color": /^(#[0-9a-f]{6}|rgb\(\d{1,3}, \d{1,3}, \d{1,3}\))$/,
  "font-family": /^(Arial, Helvetica, sans-serif|Georgia, ["']Times New Roman["'], serif|["']Courier New["'], Courier, monospace|Verdana, Geneva, sans-serif)$/,
  "font-size": /^(12|18|24)px$/,
  "margin-left": /^(2|4|6|8|10|12)em$/,
};

/** A style attribute on a span or paragraph holds only the editor's own
 * declarations, each with a value from its list or canonical form. */
function assertOwnStyle(el: Element, value: string, where: string): void {
  expect(["SPAN", "P"], `${where}: style on ${el.tagName}`).toContain(el.tagName);
  for (const decl of value.split(";").map((d) => d.trim()).filter(Boolean)) {
    const i = decl.indexOf(":");
    const prop = decl.slice(0, i).trim();
    const val = decl.slice(i + 1).trim();
    const re = OWN_DECLARATIONS[prop];
    expect(re, `${where}: style property ${prop}`).toBeDefined();
    expect(val, `${where}: ${prop} value`).toMatch(re!);
  }
}

/** Everything that must not be in a document the editor produced. */
function assertClean(root: ParentNode, where: string): void {
  expect(root.querySelectorAll(DANGEROUS).length, `${where}: dangerous element`).toBe(0);
  for (const el of Array.from(root.querySelectorAll("*"))) {
    // Links keep only href; pictures only src, alt and the fixed style.
    const allowed = el.tagName === "A" ? ["href"] : el.tagName === "IMG" ? ["src", "alt", "style", "draggable"] : null;
    for (const attr of Array.from(el.attributes)) {
      if (allowed && !el.classList.contains("ProseMirror-separator")) {
        expect(allowed, `${where}: ${el.tagName} carries ${attr.name}`).toContain(attr.name);
      }
      expect(attr.name.startsWith("on"), `${where}: ${el.tagName} has ${attr.name}`).toBe(false);
      if (attr.name === "style") {
        // Images carry the fixed one; spans and paragraphs only the style marks'
        // own declarations, each in a form written out here independently of the
        // production gate (so loosening the gate cannot loosen this check too).
        if (el.tagName === "IMG") expect(attr.value).toMatch(/^max-width:\s*100%;?$/);
        else assertOwnStyle(el, attr.value, where);
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
    expect(m.html()).toBe("<p>before</p><p>after</p>");
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
      html: `<img src="cid:a@inline.mailroom" style="position:fixed;background:url(https://evil.example/x)" onclick="${SET}" width="9999" height="9999" title="hover">`,
    });
    const img = pictures(m)[0] as HTMLImageElement;
    expect(img.getAttribute("style")).toMatch(/^max-width:\s*100%;?$/);
    expect(img.hasAttribute("onclick")).toBe(false);
    // Size and label attributes are not kept either.
    expect(img.hasAttribute("width")).toBe(false);
    expect(img.hasAttribute("height")).toBe(false);
    expect(img.hasAttribute("title")).toBe(false);
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

describe("anchors keep only their href", () => {
  it("class, target, rel, title and data attributes are dropped on paste", async () => {
    m = await mountEditor("");
    await paste(m, {
      html: '<p><a href="https://example.com/x" class="c" target="_blank" rel="opener" title="t" data-x="1" id="i" style="color:red">link</a></p>',
    });
    expect(m.html()).toBe('<p><a href="https://example.com/x">link</a></p>');
    const a = m.surface.querySelector("a") as HTMLAnchorElement;
    expect(a.getAttributeNames()).toEqual(["href"]);
  });

  it("control: the attribute check can fail (a live anchor with a class is rejected)", () => {
    const box = document.createElement("div");
    box.appendChild(live(box, '<a href="https://example.com" class="c" target="_blank">x</a>'));
    expect(() => assertClean(box, "control")).toThrow();
  });
});

describe("pictures are refused on every parse path, not only paste", () => {
  const remote = '<p>t</p><img src="https://evil.example/t.png"><img src="http://evil.example/u.png">';

  it("the seed (a signature or snippet inserted at mount)", async () => {
    m = await mountEditor(remote);
    expect(pictures(m)).toHaveLength(0);
    expect(m.html()).not.toContain("evil.example");
  });

  it("insertContent with a markup string (a template or signature inserted later)", async () => {
    m = await mountEditor("<p>draft</p>");
    await act(async () => {
      m!.editor.commands.insertContent(remote);
    });
    expect(pictures(m)).toHaveLength(0);
    expect(m.html()).not.toContain("evil.example");
  });

  it("setContent", async () => {
    m = await mountEditor("<p>draft</p>");
    await act(async () => {
      m!.editor.commands.setContent(remote);
    });
    expect(pictures(m)).toHaveLength(0);
  });

  it("control: an inline picture is still seeded (the rule is not refusing everything)", async () => {
    m = await mountEditor('<p><img src="cid:a@inline.mailroom"></p>');
    expect(pictures(m)).toHaveLength(1);
  });
});

describe("what counts as an inline picture mirrors the browser's URL parser", () => {
  const sources: Array<[string, string, number]> = [
    ["a leading space, which a browser strips", " cid:a@inline.mailroom", 1],
    ["interior tab/newline, which a browser strips", "ci\td:a@inline.mailroom", 1],
    ["a space inside the scheme (a relative URL to a browser)", "d a t a:image/png;base64,iVBORw0KGgo=", 0],
    ["a space inside cid", "c id:a@inline.mailroom", 0],
    ["a leading no-break space (not stripped by a browser)", "\u00a0cid:a@inline.mailroom", 0],
    ["SVG data (the server drops it on send)", "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=", 0],
    ["SVG data, mixed case", "DATA:IMAGE/SVG+XML,%3Csvg%3E", 0],
    ["PNG data", "data:image/png;base64,iVBORw0KGgo=", 1],
  ];
  for (const [what, src, want] of sources) {
    it(`${what}`, async () => {
      m = await mountEditor("");
      await paste(m, { html: `<p><img src="${src}"></p>` });
      expect(pictures(m)).toHaveLength(want);
      if (want === 0) expect(m.notices).toHaveLength(1);
    });
  }
});

describe("a paste during IME composition", () => {
  it("is refused (the browser would otherwise paste markup natively into the live surface)", async () => {
    m = await mountEditor("<p>draft</p>");
    await act(async () => {
      m!.surface.dispatchEvent(new Event("compositionstart", { bubbles: true }));
    });
    const before = m.html();
    const ev = await paste(m, { html: `<img src=x onerror="${SET}"><p>pasted</p>`, text: "pasted" });
    expect(ev.defaultPrevented).toBe(true);
    expect(m.html()).toBe(before);
    expect(pwned()).toBe(false);
  });

  it("control: outside composition the same paste is handled by the editor", async () => {
    m = await mountEditor("<p>draft</p>");
    const ev = await paste(m, { html: "<p>pasted</p>", text: "pasted" });
    expect(m.html()).toContain("pasted");
    expect(ev.defaultPrevented).toBe(true); // ProseMirror cancelled the native paste itself
  });
});

// ---- pasted styles (ADR-051 beta 2) ---------------------------------------
//
// Colour, highlight, font family, font size and indent are values copied from
// markup the user pasted. Each hostile one must leave the TEXT and lose the VALUE:
// absent from getHTML(), absent from the live DOM's style attributes, and the
// mark not applied at all (no empty span, no stray style). The values are built
// from parts where a literal would trip the source scans.

const EVIL = "https://evil.example/x";
// (Assembled: the token check scans .tsx for var(--name) uses.)
const VAR_X = ["var", "(--x)"].join("");

interface StyleCase {
  name: string;
  html: string;
  /** Text that must survive. */
  text: string;
  /** Substrings that must be in neither getHTML() nor any live style attribute. */
  absent: string[];
}

const STYLE_CASES: StyleCase[] = [
  { name: "url() in background, with a named colour", html: `<p><span style="color:red;background:url(${EVIL})">kept one</span></p>`, text: "kept one", absent: ["evil.example", "url(", "red", "background"] },
  { name: "src: url() beside a font-family not on the list", html: `<p><span style="font-family:'x';src:url(${EVIL})">kept two</span></p>`, text: "kept two", absent: ["evil.example", "url(", "src", "'x'"] },
  { name: "font-size 999px", html: `<p><span style="font-size:999px">kept three</span></p>`, text: "kept three", absent: ["999"] },
  { name: "negative margin-left", html: `<p style="margin-left:-9999px">kept four</p>`, text: "kept four", absent: ["9999", "margin"] },
  { name: "expression()", html: `<p><span style="color: expression(alert(1))">kept five</span></p>`, text: "kept five", absent: ["expression", "alert"] },
  { name: "var()", html: `<p><span style="color:${VAR_X}">kept six</span></p>`, text: "kept six", absent: ["var(", "--x"] },
  { name: "a CSS-escaped url in a background colour", html: `<p><span style="background-color:\\75 rl(${EVIL})">kept seven</span></p>`, text: "kept seven", absent: ["evil.example", "\\75", "rl(", "url"] },
  { name: "a CSS-escaped colour name", html: `<p><span style="color:\\72 ed">kept eight</span></p>`, text: "kept eight", absent: ["\\72", "red", "color"] },
  { name: "a font list that is not ours", html: `<p><span style="font-family:Comic Sans MS, cursive">kept nine</span></p>`, text: "kept nine", absent: ["Comic", "cursive"] },
  { name: "!important", html: `<p><span style="color:#336699 !important">kept ten</span></p>`, text: "kept ten", absent: ["important", "336699", "51, 102, 153"] },
  { name: "a comment inside a value", html: `<p><span style="color:#33/**/6699">kept eleven</span></p>`, text: "kept eleven", absent: ["/*", "6699", "51, 102, 153"] },
  { name: "@import in the declaration list", html: `<p><span style="@import url('${EVIL}');font-size:24px">kept twelve</span></p>`, text: "kept twelve", absent: ["evil.example", "@import", "url("] },
  { name: "odd units (em, rem, calc)", html: `<p><span style="font-size:1.5em">a</span><span style="font-size:calc(10px + 2px)">b</span><span style="font-size:2rem">c</span> kept thirteen</p>`, text: "kept thirteen", absent: ["1.5em", "calc", "2rem"] },
  { name: "rgba and hsl colours (not canonical)", html: `<p><span style="color:rgba(1,2,3,0.5)">a</span><span style="color:hsl(0,0%,0%)">b</span> kept fourteen</p>`, text: "kept fourteen", absent: ["rgba", "hsl"] },
  { name: "properties the editor has no mark for (position, display)", html: `<div style="position:fixed;display:none;margin-left:2em;top:0">kept fifteen</div>`, text: "kept fifteen", absent: ["position", "display", "top"] },
  { name: "a style on a list item and a quote", html: `<ul><li style="background:url(${EVIL})"><p>kept sixteen</p></li></ul><blockquote style="color:red;background:url(${EVIL})">kept seventeen</blockquote>`, text: "kept seventeen", absent: ["evil.example", "url("] },
  { name: "margin-left written as a shorthand with a url", html: `<p style="margin:0 0 0 2em;background:url(${EVIL})">kept eighteen</p>`, text: "kept eighteen", absent: ["evil.example", "url(", "margin"] },
];

describe("hostile styles in pasted HTML: the value goes, the text stays", () => {
  for (const c of STYLE_CASES) {
    it(`${c.name}`, async () => {
      m = await mountEditor("<p>draft</p>");
      await paste(m, { html: c.html, text: c.text });
      fireEverything(m.surface);
      await settle();

      expect(pwned()).toBe(false);
      expect(m.surface.textContent, "the text survived").toContain(c.text);
      assertClean(m.surface, "live editor");
      assertClean(inert(m.html()).body, "getHTML");

      // Absent from the serialized document AND from every live style attribute.
      const html = m.html();
      const liveStyles = Array.from(m.surface.querySelectorAll("[style]")).map((e) => e.getAttribute("style") ?? "");
      for (const bad of c.absent) {
        expect(html, `getHTML holds ${bad}`).not.toContain(bad);
        for (const st of liveStyles) expect(st, `a live style holds ${bad}`).not.toContain(bad);
      }
      // The mark was NOT applied: no style attribute anywhere except where a
      // listed value was also pasted (those cases are marked by their own tests).
      if (!/font-size:24px|margin-left:2em/.test(c.html)) {
        expect(liveStyles, "no style attribute at all").toEqual([]);
        expect(html).not.toContain("style=");
      }
      expect(html).not.toMatch(/<span>/); // and no empty span left behind
    });
  }

  it("control: a listed value pasted beside a hostile one is kept (the suite is not passing because everything is dropped)", async () => {
    m = await mountEditor("");
    await paste(m, {
      html: `<p><span style="color:#336699;background:url(${EVIL});font-size:18px;font-family:Verdana, Geneva, sans-serif">kept</span></p><p style="margin-left:4em">indented</p>`,
    });
    const html = m.html();
    expect(html).toContain("51, 102, 153");
    expect(html).toContain("18px");
    expect(html).toContain("Verdana, Geneva, sans-serif");
    expect(html).toContain("margin-left: 4em");
    expect(html).not.toContain("evil.example");
    expect(html).not.toContain("url(");
    assertClean(m.surface, "live editor");
  });

  it("the style check can fail (negative control): a live span with a url background, a named colour or a foreign property is rejected", () => {
    for (const style of [`background:url(${EVIL})`, "color:red", "position:fixed", "font-size:999px", "margin-left:-9999px", "font-family:x"]) {
      const box = document.createElement("div");
      box.appendChild(live(box, `<span style="${style}">x</span>`));
      expect(() => assertClean(box, "control"), style).toThrow();
    }
    // ...and accepts our own forms.
    const ok = document.createElement("div");
    ok.appendChild(live(ok, `<span style="color: rgb(1, 2, 3); font-size: 18px">x</span><p style="margin-left: 4em">y</p>`));
    expect(() => assertClean(ok, "control")).not.toThrow();
  });

  it("insertContent, setContent and the seed apply the same gate as paste", async () => {
    const hostile = `<p><span style="color:red;background:url(${EVIL});font-size:999px">t</span></p>`;
    m = await mountEditor(hostile);
    expect(m.html()).toBe("<p>t</p>");
    await act(async () => {
      m!.editor.commands.insertContent(hostile);
    });
    expect(m.html()).not.toMatch(/evil|url\(|999|style/);
    await act(async () => {
      m!.editor.commands.setContent(hostile);
    });
    expect(m.html()).toBe("<p>t</p>");
  });
});

// ---- tables (ADR-051 beta 4) ----------------------------------------------------
// A table keeps its structure and text and nothing else: no style, background,
// bgcolor, width, class, align or handler on a table, row or cell survives, and
// the editor's own fixed border and padding are written instead. The oracle below
// is written out here, independently of ./tables.ts, so loosening the extension
// cannot loosen the check with it.

const TABLE_TAGS = new Set(["TABLE", "TBODY", "TR", "TH", "TD"]);

/** What the editor writes on a cell, as the DOM reads it back. */
const OWN_CELL = { border: "1px solid #999999", padding: "4px 8px" };

/** A table document holds only the editor's own forms. */
function assertTableClean(root: ParentNode, where: string): void {
  expect(root.querySelectorAll(DANGEROUS).length, `${where}: dangerous element`).toBe(0);
  expect(root.querySelectorAll("colgroup, col, thead, tfoot, caption").length, `${where}: a table part the editor never writes`).toBe(0);
  for (const el of Array.from(root.querySelectorAll("table, tbody, tr, th, td"))) {
    expect(TABLE_TAGS.has(el.tagName)).toBe(true);
    const names = el.getAttributeNames().filter((n) => !(el.tagName === "TD" || el.tagName === "TH") || !["colspan", "rowspan"].includes(n));
    const want = el.tagName === "TABLE" ? ["style"] : el.tagName === "TH" ? ["scope", "style"] : el.tagName === "TD" ? ["style"] : [];
    expect(names.sort(), `${where}: attributes of ${el.tagName}`).toEqual(want);
    const style = (el as HTMLElement).style;
    if (el.tagName === "TABLE") {
      expect(style.getPropertyValue("border-collapse"), `${where}: table style`).toBe("collapse");
      expect(style.length, `${where}: table style has only one declaration`).toBe(1);
    }
    if (el.tagName === "TD" || el.tagName === "TH") {
      expect(style.getPropertyValue("padding"), `${where}: ${el.tagName} padding`).toBe(OWN_CELL.padding);
      const border = style.getPropertyValue("border").replace(/rgb\(153, 153, 153\)/, "#999999");
      expect(border, `${where}: ${el.tagName} border`).toBe(OWN_CELL.border);
      for (const prop of Array.from(style)) {
        expect(prop, `${where}: ${el.tagName} declares ${prop}`).toMatch(/^(border(-.+)?|padding(-.+)?|text-align|font-weight)$/);
      }
      if (el.tagName === "TD") expect(style.getPropertyValue("font-weight") + style.getPropertyValue("text-align"), `${where}: td is not a header`).toBe("");
      expect(style.cssText, `${where}: ${el.tagName} style`).not.toMatch(/url\(|expression|background|width|evil/i);
    }
    if (el.tagName === "TH") expect(el.getAttribute("scope")).toBe("col");
  }
  for (const el of Array.from(root.querySelectorAll("*"))) {
    for (const attr of Array.from(el.attributes)) {
      expect(attr.name.startsWith("on"), `${where}: ${el.tagName} has ${attr.name}`).toBe(false);
      if (["TABLE", "TBODY", "TR", "TH", "TD"].includes(el.tagName)) {
        expect(["style", "scope", "colspan", "rowspan"], `${where}: ${el.tagName} carries ${attr.name}`).toContain(attr.name);
      }
    }
  }
  // (ProseMirror's own trailing-break hack carries a class in the live DOM only.)
  const all = ((root as Element).innerHTML ?? "").replace(/ class="ProseMirror-trailingBreak"/g, "");
  expect(all, `${where}: markup`).not.toMatch(/evil\.example|expression\(|url\(|javascript:|bgcolor|background=|\bclass=|\bwidth=/i);
}

interface TableCase {
  name: string;
  html: string;
  /** Text that must still be there. */
  text: string[];
  live: boolean;
}

const NEST = (n: number): string => {
  let inner = `level ${n}`;
  for (let i = n - 1; i >= 1; i--) inner = `level ${i}<table><tbody><tr><td>${inner}</td></tr></tbody></table>`;
  return `<table><tbody><tr><td>${inner}</td></tr></tbody></table>`;
};

const TABLE_CASES: TableCase[] = [
  { name: "td with a url() background", html: `<table><tr><td style="background:url(${EVIL})">cell one</td></tr></table>`, text: ["cell one"], live: false },
  { name: "table background attribute", html: `<table background="${EVIL}"><tr><td>cell two</td></tr></table>`, text: ["cell two"], live: false },
  { name: "td onclick", html: `<table><tr><td onclick="${SET}" onmouseover="${SET}">cell three</td></tr></table>`, text: ["cell three"], live: true },
  { name: "th with expression() in its style", html: `<table><tr><th style="width:expression(${SET});color:expression(${SET})">head four</th></tr></table>`, text: ["head four"], live: false },
  { name: "bgcolor, width, class, align, valign, height, cellpadding, id", html: `<table class="x" id="t" width="9999" bgcolor="#ff0000" cellpadding="40" border="9" align="right" style="width:99999px;min-width:5000px;background:url(${EVIL})"><tr class="r" onclick="${SET}" style="height:900px"><td class="c" width="5000" height="900" bgcolor="red" align="right" valign="top" style="background:red;position:fixed">cell five</td></tr></table>`, text: ["cell five"], live: true },
  { name: "a table in a live handler-bearing wrapper", html: `<div onclick="${SET}"><table><tr><td><img src=x onerror="${SET}">cell six</td></tr></table></div>`, text: ["cell six"], live: true },
  { name: "colspan and rowspan with hostile styles beside them", html: `<table><tr><td colspan="2" style="background:url(${EVIL})">wide seven</td></tr><tr><td>a</td><td rowspan="2">b</td></tr></table>`, text: ["wide seven"], live: false },
  { name: "colgroup with widths", html: `<table><colgroup><col width="900" style="width:900px"></colgroup><tr><td>cell eight</td></tr></table>`, text: ["cell eight"], live: false },
  { name: "a table nested five deep", html: NEST(5), text: [1, 2, 3, 4, 5].map((n) => `level ${n}`), live: false },
  { name: "a script and a style element inside cells", html: `<table><tr><td><script>${SET}</script>cell nine<style>td{background:url(${EVIL})}</style></td></tr></table>`, text: ["cell nine"], live: false },
];

describe("hostile tables: the structure and the text stay, everything else goes", () => {
  for (const c of TABLE_CASES) {
    it(`${c.name}`, async () => {
      m = await mountEditor("<p>draft</p>");
      await paste(m, { html: c.html, text: "pasted text" });
      fireEverything(m.surface);
      await settle();

      expect(pwned()).toBe(false);
      for (const t of c.text) {
        expect(m.surface.textContent, `live: ${t}`).toContain(t);
        expect(m.html(), `getHTML: ${t}`).toContain(t);
      }
      expect(m.surface.querySelectorAll("table").length, "the table itself survived").toBeGreaterThan(0);
      assertTableClean(m.surface, "live editor");
      assertTableClean(inert(m.html()).body, "getHTML");
      // The seed and insertContent route apply the same rules as paste.
      for (const route of ["seed", "insertContent", "setContent"] as const) {
        const seeded = route === "seed" ? await mountEditor(c.html) : await mountEditor("<p>draft</p>");
        try {
          if (route === "insertContent") await act(async () => void seeded.editor.commands.insertContent(c.html));
          if (route === "setContent") await act(async () => void seeded.editor.commands.setContent(c.html));
          fireEverything(seeded.surface);
          expect(pwned(), route).toBe(false);
          assertTableClean(inert(seeded.html()).body, route);
          for (const t of c.text) expect(seeded.html(), `${route}: ${t}`).toContain(t);
        } finally {
          await seeded.unmount();
        }
      }
    });
  }

  it("the harness can see a table handler run (negative control): the live-page route trips the flag", async () => {
    for (const c of TABLE_CASES.filter((x) => x.live)) {
      document.title = "calm";
      const box = document.createElement("div");
      document.body.appendChild(box);
      box.appendChild(live(box, c.html));
      fireEverything(box);
      await settle();
      box.remove();
      expect(pwned(), c.name).toBe(true);
    }
  });

  it("nested five deep: all five levels are still tables, none lost", async () => {
    m = await mountEditor(NEST(5));
    expect(inert(m.html()).body.querySelectorAll("table").length).toBe(5);
  });

  it("a pasted table of 500 rows parses in bounded time and keeps every row", async () => {
    const rows = Array.from({ length: 500 }, (_, i) => `<tr><td style="background:url(${EVIL})" onclick="${SET}">row ${i} a</td><td width="999">row ${i} b</td></tr>`).join("");
    m = await mountEditor("<p>draft</p>");
    const t0 = performance.now();
    await paste(m, { html: `<table>${rows}</table>`, text: "x" });
    const took = performance.now() - t0;
    // A ceiling generous enough for a loaded CI runner; the point is "not
    // quadratic or unbounded", which would be minutes, not seconds.
    console.info(`500-row paste: ${Math.round(took)} ms`);
    expect(took, `paste took ${Math.round(took)} ms`).toBeLessThan(30_000);
    expect(grid500(m)).toBe(500);
    expect(m.surface.textContent).toContain("row 499 b");
    assertTableClean(inert(m.html()).body, "getHTML");
    fireEverything(m.surface.querySelector("table")!);
    expect(pwned()).toBe(false);
  }, 120_000);

  it("control: a clean table keeps its structure, its colspan and its own border, so the suite is not passing because everything is dropped", async () => {
    m = await mountEditor("");
    await paste(m, { html: `<table><tr><th>Name</th><th>Qty</th></tr><tr><td colspan="2" style="background:url(${EVIL})">kept</td></tr></table>` });
    const doc = inert(m.html()).body;
    expect(doc.querySelectorAll("th").length).toBe(2);
    expect(doc.querySelector("td")!.getAttribute("colspan")).toBe("2");
    expect(doc.querySelector("td")!.style.getPropertyValue("padding")).toBe("4px 8px");
    expect(doc.querySelector("td")!.style.getPropertyValue("border")).toContain("1px solid");
    assertTableClean(doc, "control");
    expect(doc.textContent).toContain("kept");
  });

  it("the table oracle can fail (negative control): each hostile form, put in a live table, is rejected", () => {
    const bad = [
      `<table><tr><td style="background:url(${EVIL})">x</td></tr></table>`,
      `<table background="${EVIL}"><tr><td>x</td></tr></table>`,
      `<table><tr><td onclick="${SET}">x</td></tr></table>`,
      `<table><tr><th style="width:expression(1)">x</th></tr></table>`,
      `<table class="x"><tr><td>x</td></tr></table>`,
      `<table><tr><td bgcolor="red">x</td></tr></table>`,
      `<table><tr><td width="9">x</td></tr></table>`,
      `<table><colgroup><col></colgroup><tr><td>x</td></tr></table>`,
      // and a table that is merely missing the editor's own border
      `<table style="border-collapse: collapse"><tr><td style="padding: 4px 8px">x</td></tr></table>`,
    ];
    for (const html of bad) {
      const box = document.createElement("div");
      box.appendChild(live(box, html));
      expect(() => assertTableClean(box, "control"), html).toThrow();
    }
    const ok = document.createElement("div");
    ok.appendChild(
      live(ok, `<table style="border-collapse: collapse"><tbody><tr><th scope="col" style="border: 1px solid rgb(153, 153, 153); padding: 4px 8px; text-align: left; font-weight: bold">h</th></tr><tr><td colspan="2" style="border: 1px solid rgb(153, 153, 153); padding: 4px 8px">x</td></tr></tbody></table>`),
    );
    expect(() => assertTableClean(ok, "control")).not.toThrow();
  });
});

const grid500 = (mm: Mounted): number => mm.surface.querySelectorAll("table tr").length;
