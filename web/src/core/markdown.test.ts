// SPDX-License-Identifier: AGPL-3.0-only

// Tests for the Markdown compose subset (#56). The first block is the one that matters:
// this renderer emits HTML, so what it refuses to emit is the point. The rest pin the
// subset down so "unsupported" stays a documented shape rather than a surprise.

import { describe, expect, it } from "vitest";

import { markdownToHtml } from "./markdown";

describe("markdownToHtml: what it refuses to emit", () => {
  it("escapes inline HTML instead of passing it through", () => {
    const out = markdownToHtml("<script>alert(1)</script>");
    expect(out).not.toContain("<script>");
    expect(out).toContain("&lt;script&gt;");
  });

  // An EXACT match, not a substring check: the payload's own text legitimately still
  // reads "onerror=", so the only meaningful assertion is that every angle bracket and
  // quote became an entity and no tag was emitted but the paragraph.
  it("escapes an img onerror payload", () => {
    expect(markdownToHtml('hi <img src=x onerror="alert(1)">')).toBe(
      "<p>hi &lt;img src=x onerror=&quot;alert(1)&quot;&gt;</p>",
    );
  });

  it("refuses a javascript: link and shows the text literally", () => {
    const out = markdownToHtml("[click](javascript:alert(1))");
    // No anchor and no href anywhere — the scheme string survives only as visible
    // text, which is the intended degradation, so asserting on it would be wrong.
    expect(out).not.toContain("<a ");
    expect(out).not.toContain("href=");
    expect(out).toContain("[click](javascript:alert(1))");
  });

  it("refuses data:, vbscript: and file: links", () => {
    for (const url of ["data:text/html;base64,PHNjcmlwdD4=", "vbscript:msgbox", "file:///etc/passwd"]) {
      expect(markdownToHtml(`[x](${url})`)).not.toContain("href=");
    }
  });

  it("is not fooled by case or entity obfuscation of the scheme", () => {
    expect(markdownToHtml("[x](JaVaScRiPt:alert(1))")).not.toContain("href=");
    expect(markdownToHtml("[x](java&#115;cript:alert(1))")).not.toContain("href=");
  });

  // NEGATIVE CONTROL for the four cases above: prove the allowlist lets the good ones
  // through, or "no <a> emitted" would pass even if links were broken entirely.
  it("does emit a link for http, https and mailto", () => {
    expect(markdownToHtml("[a](https://example.com/x)")).toContain(
      '<a href="https://example.com/x">a</a>',
    );
    expect(markdownToHtml("[a](http://example.com)")).toContain('<a href="http://example.com">a</a>');
    expect(markdownToHtml("[a](mailto:you@example.com)")).toContain(
      '<a href="mailto:you@example.com">a</a>',
    );
  });

  it("cannot be made to emit an extra attribute by quoting the URL", () => {
    const out = markdownToHtml('[x](https://example.com/"onmouseover="alert(1))');
    expect(out).not.toContain('onmouseover="');
    expect(out).toContain("&quot;");
  });

  it("escapes markup inside a code span and a fenced block", () => {
    expect(markdownToHtml("`<b>x</b>`")).toContain("<code>&lt;b&gt;x&lt;/b&gt;</code>");
    expect(markdownToHtml("```\n<b>x</b>\n```")).toContain("<pre><code>&lt;b&gt;x&lt;/b&gt;</code></pre>");
  });
});

describe("markdownToHtml: the supported subset", () => {
  it("renders paragraphs, keeping a single newline as a line break", () => {
    expect(markdownToHtml("one\ntwo\n\nthree")).toBe("<p>one<br>two</p><p>three</p>");
  });

  it("renders ATX headings at their level", () => {
    expect(markdownToHtml("# a\n### b")).toBe("<h1>a</h1><h3>b</h3>");
  });

  it("does not treat a hash without a space as a heading", () => {
    expect(markdownToHtml("#nothashtag")).toBe("<p>#nothashtag</p>");
  });

  it("renders bold, italic and inline code", () => {
    expect(markdownToHtml("**b** and *i* and _i_ and `c`")).toBe(
      "<p><strong>b</strong> and <em>i</em> and <em>i</em> and <code>c</code></p>",
    );
  });

  it("does not read ** inside a code span as bold", () => {
    expect(markdownToHtml("`a ** b`")).toBe("<p><code>a ** b</code></p>");
  });

  it("leaves snake_case words alone", () => {
    expect(markdownToHtml("call some_long_name now")).toBe("<p>call some_long_name now</p>");
  });

  it("renders unordered and ordered lists, one level", () => {
    expect(markdownToHtml("- a\n- b")).toBe("<ul><li>a</li><li>b</li></ul>");
    expect(markdownToHtml("1. a\n2. b")).toBe("<ol><li>a</li><li>b</li></ol>");
  });

  it("renders a blockquote, and blocks inside it", () => {
    expect(markdownToHtml("> quoted\n> - a")).toBe(
      "<blockquote><p>quoted</p><ul><li>a</li></ul></blockquote>",
    );
  });

  it("renders a thematic break", () => {
    expect(markdownToHtml("a\n\n---\n\nb")).toBe("<p>a</p><hr><p>b</p>");
  });

  it("keeps an unclosed fence's text rather than dropping it", () => {
    expect(markdownToHtml("```\nkept\nstill kept")).toBe("<pre><code>kept\nstill kept</code></pre>");
  });

  it("returns nothing for empty or whitespace-only input", () => {
    expect(markdownToHtml("")).toBe("");
    expect(markdownToHtml("\n\n  \n")).toBe("");
  });

  it("normalises CRLF", () => {
    expect(markdownToHtml("a\r\n\r\nb")).toBe("<p>a</p><p>b</p>");
  });

  // A table is explicitly unsupported. It must come out as readable text, not as a
  // broken half-table — the documented degradation.
  it("renders an unsupported table as literal text", () => {
    const out = markdownToHtml("| a | b |\n| - | - |");
    expect(out).not.toContain("<table");
    expect(out).toContain("| a | b |");
  });
});
