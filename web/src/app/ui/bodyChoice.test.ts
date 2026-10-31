// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import type { EmailBodyPart } from "../../core/jmap/types";
import { chooseBody, escapeHtml, hasHtmlPart, htmlToRender } from "./bodyChoice";

function part(partId: string, type: string): EmailBodyPart {
  return { partId, blobId: `B.${partId}`, size: 10, name: null, type, charset: null, disposition: null, cid: null };
}
const val = (value: string) => ({ value });

describe("chooseBody (#871)", () => {
  it("a lone text/plain part (listed in both bodies, RFC 8621 4.1.4) is shown as text with its newlines", () => {
    const p = part("1", "text/plain");
    const c = chooseBody({ textBody: [p], htmlBody: [p], bodyValues: { "1": val("line one\r\nline two") } });
    expect(c).toEqual({ kind: "text", text: "line one\r\nline two" });
  });

  it("an alternative message takes the HTML path", () => {
    const t = part("1", "text/plain");
    const h = part("2", "text/html");
    const c = chooseBody({ textBody: [t], htmlBody: [h], bodyValues: { "1": val("plain"), "2": val("<p>rich</p>") } });
    expect(c).toEqual({ kind: "html", html: "<p>rich</p>" });
  });

  it("a mixed list keeps HTML, escaping the plain part and preserving its newlines", () => {
    const t = part("1", "text/plain");
    const h = part("2", "text/html");
    const c = chooseBody({
      textBody: [t, h],
      htmlBody: [t, h],
      bodyValues: { "1": val(`a <b>"x"</b> & 'y'\nb`), "2": val("<p>rich</p>") },
    });
    expect(c.kind).toBe("html");
    const html = (c as { html: string }).html;
    expect(html).toContain("white-space:pre-wrap");
    expect(html).toContain("a &lt;b&gt;&quot;x&quot;&lt;/b&gt; &amp; &#39;y&#39;\nb");
    expect(html).not.toContain("<b>");
    expect(html).toContain("<p>rich</p>");
  });

  it("markup in a plain body is shown literally, never rendered", () => {
    const p = part("1", "text/plain");
    const c = chooseBody({ textBody: [p], htmlBody: [p], bodyValues: { "1": val("<b>hi</b>") } });
    expect(c).toEqual({ kind: "text", text: "<b>hi</b>" });
  });

  it("an empty or unfetched body is text, empty", () => {
    expect(chooseBody({})).toEqual({ kind: "text", text: "" });
    const h = part("2", "text/html");
    expect(chooseBody({ htmlBody: [h], bodyValues: {} })).toEqual({ kind: "text", text: "" });
  });

  it("a flowed text/plain part is unflowed: its soft wraps come out joined (#890)", () => {
    const p = { ...part("1", "text/plain"), format: "flowed", delSp: false };
    const c = chooseBody({
      textBody: [p],
      htmlBody: [p],
      bodyValues: { "1": val("the quick brown fox \r\njumps over the lazy dog") },
    });
    expect(c).toEqual({ kind: "text", text: "the quick brown fox jumps over the lazy dog" });
  });

  it("a non-flowed text/plain part passes through, wrapped lines and all", () => {
    const p = part("1", "text/plain");
    const c = chooseBody({
      textBody: [p],
      htmlBody: [p],
      bodyValues: { "1": val("kept  \nwrapped") },
    });
    expect(c).toEqual({ kind: "text", text: "kept  \nwrapped" });
  });
});

describe("hasHtmlPart / htmlToRender / escapeHtml", () => {
  it("only a text/html part counts, case-insensitively", () => {
    expect(hasHtmlPart([part("1", "text/plain")])).toBe(false);
    expect(hasHtmlPart([part("1", "image/png")])).toBe(false);
    expect(hasHtmlPart([part("1", "Text/HTML")])).toBe(true);
    expect(hasHtmlPart(undefined)).toBe(false);
  });
  it("joins with the given separator", () => {
    const a = part("1", "text/html");
    const b = part("2", "text/html");
    const v = { "1": val("A"), "2": val("B") };
    expect(htmlToRender({ htmlBody: [a, b], bodyValues: v }, "")).toBe("AB");
    expect(htmlToRender({ htmlBody: [a, b], bodyValues: v })).toBe("A\n\nB");
  });
  it("escapes all five characters", () => {
    expect(escapeHtml(`&<>"'`)).toBe("&amp;&lt;&gt;&quot;&#39;");
  });
});
