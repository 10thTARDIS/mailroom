// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE FORMATTED QUOTE'S RULES (ADR-051 §7, #92), one at a time. The composer-level
// and hostile-original suites are elsewhere (Compose.quote.test.tsx,
// quote.hostile.test.tsx); this is the module on its own, over HTML shaped like
// what the server's reader returns.

import { describe, expect, it } from "vitest";
import type { BlobId } from "../../../core/jmap/types";
import { QUOTE_PICTURE_BYTE_CAP, QUOTE_TEXT_BUDGET, formatQuote, isDataTable } from "./quote";
import type { QuoteInput, QuotePart } from "./quote";

const PIC = "[picture not included]";
const REST = "[The rest of the original message is not included]";

const part = (over: Partial<QuotePart> = {}): QuotePart => ({
  partId: "2",
  blobId: "B-logo" as BlobId,
  type: "image/png",
  name: "logo.png",
  size: 1000,
  cid: "logo@example.com",
  ...over,
});

function quote(html: string, over: Partial<QuoteInput> = {}) {
  return formatQuote({
    html,
    truncated: false,
    parts: [],
    withheld: () => false,
    canCarry: true,
    notes: { picture: PIC, rest: REST },
    ...over,
  });
}

/** The quote as a DOM, to ask about structure rather than string shape. */
function dom(html: string): Document {
  return new DOMParser().parseFromString(html, "text/html");
}

/** The text of each top-level block, joined by a space. */
const text = (html: string): string =>
  Array.from(dom(html).body.childNodes)
    .map((n) => n.textContent ?? "")
    .join(" ")
    .replace(/\s+/g, " ")
    .trim();

describe("pictures", () => {
  it("a parked remote picture is dropped, never revived, and the quote says so", () => {
    const q = quote(
      '<p>before</p><img data-mailroom-remote-src="/jmap/proxy/A/M/tok" data-mailroom-blocked="" alt="x"><p>after</p>',
    );
    expect(dom(q.html).querySelectorAll("img")).toHaveLength(0);
    expect(q.html).not.toContain("/jmap/proxy/");
    expect(q.html).not.toContain("data-mailroom");
    expect(text(q.html)).toBe(`before ${PIC} after`);
  });

  it("a picture with a srcset is dropped", () => {
    const q = quote('<p>a</p><img src="cid:logo@example.com" srcset="cid:logo@example.com 2x">', {
      parts: [part()],
    });
    expect(dom(q.html).querySelectorAll("img")).toHaveLength(0);
    expect(q.carried).toEqual([]);
  });

  it("a picture whose src is a Mailroom proxy path is dropped", () => {
    const q = quote('<p>a</p><img src="/jmap/proxy/A/M/tok"><p>b</p>');
    expect(q.html).not.toContain("proxy");
    expect(text(q.html)).toBe(`a ${PIC} b`);
  });

  it("an absolute address that merely carries the proxy path is dropped too", () => {
    const q = quote('<p>a</p><img src="https://host.example/jmap/proxy/A/M/tok">');
    expect(q.html).not.toContain("proxy");
  });

  it("a remote, relative, blob: or javascript: source is dropped with the note", () => {
    const q = quote(
      '<p>t</p><img src="https://evil.example/a.png"><p>u</p><img src="/rel.png"><p>v</p><img src="blob:x"><p>w</p><img src="javascript:1">',
    );
    expect(dom(q.html).querySelectorAll("img")).toHaveLength(0);
    expect(q.html.match(/\[picture not included\]/g)).toHaveLength(4);
  });

  it("a hidden beacon is dropped without a note", () => {
    const q = quote('<p>hello</p><img data-mailroom-pixel-src="https://tracker.example/p.gif" width="1" height="1" data-mailroom-blocked="">');
    expect(text(q.html)).toBe("hello");
  });

  it("pictures removed side by side are noted once", () => {
    const q = quote(
      '<p><img data-mailroom-remote-src="/jmap/proxy/A/M/1" data-mailroom-blocked=""> <img data-mailroom-remote-src="/jmap/proxy/A/M/2" data-mailroom-blocked=""></p><p>text</p>',
    );
    expect(q.html.match(/\[picture not included\]/g)).toHaveLength(1);
  });

  it("an inline raster picture (data:image) stays, counted against the byte cap", () => {
    const q = quote('<p><img src="data:image/png;base64,iVBORw0KGgo=" onerror="x()" width="9"></p>');
    const img = dom(q.html).querySelector("img");
    expect(img?.getAttribute("src")).toBe("data:image/png;base64,iVBORw0KGgo=");
    expect(img?.getAttributeNames()).toEqual(["src"]);
    const huge = quote(`<p>t</p><img src="data:image/png;base64,${"A".repeat(QUOTE_PICTURE_BYTE_CAP)}">`);
    expect(dom(huge.html).querySelectorAll("img")).toHaveLength(0);
  });

  describe("a cid: picture", () => {
    const html = '<p>hi</p><p><img src="cid:logo@example.com" alt="Logo" onerror="x()" style="width:9px"></p>';

    it("is kept, as the reply's own inline picture, when the original lists the part", () => {
      const q = quote(html, { parts: [part()] });
      const img = dom(q.html).querySelector("img");
      expect(img?.getAttribute("src")).toBe("cid:logo@example.com");
      expect(img?.getAttributeNames().sort()).toEqual(["alt", "src"]);
      expect(q.carried).toEqual([
        { blobId: "B-logo", cid: "logo@example.com", name: "logo.png", type: "image/png", partId: "2" },
      ]);
    });

    it("matches a content-id written with angle brackets or percent-encoding", () => {
      const q = quote('<p><img src="cid:a%40example.com"></p>', { parts: [part({ cid: "<a@example.com>" })] });
      expect(q.carried.map((c) => c.cid)).toEqual(["a@example.com"]);
    });

    it("is carried once however often it is used", () => {
      const q = quote(`${html}${html}`, { parts: [part()] });
      expect(q.carried).toHaveLength(1);
      expect(dom(q.html).querySelectorAll("img")).toHaveLength(2);
    });

    const dropped: Array<[string, Partial<QuoteInput>]> = [
      ["the original lists no such part", { parts: [] }],
      ["the part has another content-id", { parts: [part({ cid: "other@example.com" })] }],
      ["the part has no blob", { parts: [part({ blobId: null })] }],
      ["the part is not an image", { parts: [part({ type: "application/pdf" })] }],
      ["the part is SVG", { parts: [part({ type: "image/svg+xml" })] }],
      ["the part is withheld by the attachment scan", { parts: [part()], withheld: () => true }],
      ["the reply cannot carry parts (an encrypted original)", { parts: [part()], canCarry: false }],
      ["the part is over the byte cap", { parts: [part({ size: QUOTE_PICTURE_BYTE_CAP + 1 })] }],
    ];
    for (const [why, over] of dropped) {
      it(`is dropped, with the note, when ${why}`, () => {
        const q = quote(html, over);
        expect(dom(q.html).querySelectorAll("img")).toHaveLength(0);
        expect(q.carried).toEqual([]);
        expect(text(q.html)).toBe(`hi ${PIC}`);
      });
    }

    it("stops carrying at the cap: the first fit, the rest dropped with the note", () => {
      const half = Math.floor(QUOTE_PICTURE_BYTE_CAP / 2) + 1;
      const q = quote('<p><img src="cid:a@x"></p><p><img src="cid:b@x"></p>', {
        parts: [part({ cid: "a@x", blobId: "A" as BlobId, size: half }), part({ cid: "b@x", blobId: "B" as BlobId, size: half })],
      });
      expect(q.carried.map((c) => c.cid)).toEqual(["a@x"]);
      expect(dom(q.html).querySelectorAll("img")).toHaveLength(1);
      expect(q.html).toContain(PIC);
    });

    it("is not carried when the size budget cut its block", () => {
      const long = `<p>${"x".repeat(QUOTE_TEXT_BUDGET - 5)}</p><p>0123456789</p>`;
      const q = quote(`${long}<p><img src="cid:logo@example.com"></p>`, { parts: [part()] });
      expect(q.carried).toEqual([]);
      expect(dom(q.html).querySelectorAll("img")).toHaveLength(0);
    });
  });
});

describe("layout tables are flattened, data tables kept", () => {
  const cells = (html: string): number => dom(html).querySelectorAll("td, th").length;
  const tables = (html: string): number => dom(html).querySelectorAll("table").length;

  it("a plain data table stays a table", () => {
    const html =
      '<p>Prices:</p><table><thead><tr><th>Item</th><th>Price</th></tr></thead><tbody><tr><td>Tea</td><td>3</td></tr></tbody></table>';
    const q = quote(html);
    expect(tables(q.html)).toBe(1);
    expect(cells(q.html)).toBe(4);
  });

  it("a table with a nested table is flattened, its cells' content kept in order", () => {
    const q = quote(
      '<table><tr><td><p>one</p></td><td><table><tr><td>inner a</td></tr></table></td></tr><tr><td>three</td></tr></table>',
    );
    expect(tables(q.html)).toBe(0);
    expect(dom(q.html).querySelectorAll("td, th, tr")).toHaveLength(0);
    expect(Array.from(dom(q.html).body.children).map((e) => e.textContent)).toEqual(["one", "inner a", "three"]);
  });

  it("role=presentation and role=none (any case) are layout, whatever the shape", () => {
    for (const role of ["presentation", "none", " Presentation "]) {
      const q = quote(`<table role="${role}"><tr><td>a</td><td>b</td></tr><tr><td>c</td><td>d</td></tr></table>`);
      expect(tables(q.html), role).toBe(0);
      expect(text(q.html)).toBe("a b c d");
    }
  });

  it("a table with one cell in every row is layout (a single column)", () => {
    const q = quote("<table><tr><td>a</td></tr><tr><td>b</td></tr></table>");
    expect(tables(q.html)).toBe(0);
  });

  it("a table over 20 columns is flattened; exactly 20 stays (colspan counts)", () => {
    const row = (n: number): string => `<tr>${Array.from({ length: n }, (_, i) => `<td>${i}</td>`).join("")}</tr>`;
    expect(tables(quote(`<table>${row(21)}</table>`).html)).toBe(0);
    expect(tables(quote(`<table>${row(20)}</table>`).html)).toBe(1);
    expect(tables(quote(`<table><tr><td colspan="19">a</td><td>b</td><td>c</td></tr></table>`).html)).toBe(0);
  });

  it("isDataTable reads the rule directly", () => {
    const t = (h: string): Element => dom(h).querySelector("table") as Element;
    expect(isDataTable(t("<table><tr><td>a</td><td>b</td></tr></table>"))).toBe(true);
    expect(isDataTable(t("<table><tr><td>a</td></tr></table>"))).toBe(false);
    expect(isDataTable(t("<table><tr><td><table><tr><td>a</td><td>b</td></tr></table></td></tr></table>"))).toBe(false);
  });

  it("loose text in a layout cell becomes a paragraph; whitespace-only cells vanish", () => {
    const q = quote("<table><tr><td>Hello <b>there</b></td></tr><tr><td> </td></tr></table>");
    expect(Array.from(dom(q.html).body.children).map((e) => e.outerHTML.replace(/ xmlns="[^"]*"/g, ""))).toEqual([
      "<p>Hello <b>there</b></p>",
    ]);
  });

  describe("newsletter-shaped mail", () => {
    const newsletter = (role: string): string =>
      `<table ${role} width="600"><tr><td>` +
      `<table ${role} width="100%"><tr><td><h1>Example News</h1></td></tr></table>` +
      `<table ${role} width="100%"><tr><td><h2>Main story</h2><p>Story text.</p></td><td><h3>Sidebar</h3><ul><li>One</li></ul></td></tr></table>` +
      `<table ${role} width="100%"><tr><td>Footer <a href="https://example.com/unsub">Unsubscribe</a></td></tr></table>` +
      "</td></tr></table>";

    it("without role=presentation on the inner tables, the two-column one reads as data and stays (the rule, not a guess)", () => {
      const q = quote(newsletter(""));
      expect(dom(q.html).querySelectorAll("table")).toHaveLength(1);
    });

    it("comes out as blocks in reading order, with no table left", () => {
      const q = quote(newsletter('role="presentation"'));
      const d = dom(q.html);
      expect(d.querySelectorAll("table")).toHaveLength(0);
      expect(Array.from(d.body.children).map((e) => e.localName)).toEqual(["h1", "h2", "p", "h3", "ul", "p"]);
      expect(text(q.html)).toBe("Example News Main story Story text. Sidebar One Footer Unsubscribe");
    });

    it("a data table inside a layout table is kept, and the outer layout is flattened", () => {
      const q = quote(
        '<table role="presentation"><tr><td><h2>Report</h2><table><tr><th>Region</th><th>Total</th></tr><tr><td>North</td><td>10</td></tr></table><p>End</p></td></tr></table>',
      );
      const d = dom(q.html);
      expect(d.querySelectorAll("table")).toHaveLength(1);
      expect(d.querySelectorAll("table th")).toHaveLength(2);
      expect(Array.from(d.body.children).map((e) => e.localName)).toEqual(["h2", "table", "p"]);
    });
  });
});

describe("the size budget", () => {
  const block = (n: number, c = "x"): string => `<p>${c.repeat(n)}</p>`;

  it("keeps everything within the budget, and adds no note", () => {
    const q = quote(`${block(QUOTE_TEXT_BUDGET / 2)}${block(QUOTE_TEXT_BUDGET / 2)}`);
    expect(dom(q.html).querySelectorAll("p")).toHaveLength(2);
    expect(q.html).not.toContain(REST);
  });

  it("cuts at a whole block boundary, never inside one, and says so", () => {
    const q = quote(`${block(30_000, "a")}${block(30_000, "b")}${block(10, "c")}`);
    const ps = Array.from(dom(q.html).querySelectorAll("p")).map((p) => p.textContent as string);
    expect(ps).toHaveLength(2);
    expect(ps[0]).toBe("a".repeat(30_000)); // whole
    expect(ps[1]).toBe(REST);
    expect(q.html).not.toContain("b");
    expect(q.html).not.toContain("cccc");
  });

  it("counts code points, not UTF-16 units", () => {
    // 25 000 astral characters are 50 000 UTF-16 units but 25 000 code points.
    const astral = "\u{1F600}".repeat(25_000);
    const q = quote(`${block(25_000, "\u{1F600}")}<p>${astral}</p>`);
    expect(q.html).not.toContain(REST);
    const over = quote(`${block(25_000, "\u{1F600}")}<p>${astral}</p><p>y</p>`);
    expect(over.html).toContain(REST);
  });

  it("enters a wrapper that does not fit whole, so one big div still gives its leading blocks", () => {
    const q = quote(`<div dir="ltr">${block(30_000, "a")}${block(30_000, "b")}</div>`);
    const d = dom(q.html);
    expect(d.querySelectorAll("p")).toHaveLength(2); // the first block and the note
    expect((d.querySelector("p") as Element).textContent).toBe("a".repeat(30_000));
    expect(q.html).toContain(REST);
  });

  it("a leaf block that does not fit is left out whole, not cut", () => {
    const q = quote(`${block(10, "a")}<pre>${"z".repeat(QUOTE_TEXT_BUDGET + 1)}</pre>`);
    expect(q.html).not.toContain("z");
    expect(q.html).toContain(REST);
    expect(q.html).toContain("aaaaaaaaaa");
  });

  it("the server having truncated the body value adds the note, with nothing else cut", () => {
    const q = quote("<p>short</p>", { truncated: true });
    expect(dom(q.html).querySelectorAll("p")).toHaveLength(2);
    expect(text(q.html)).toBe(`short ${REST}`);
  });

  it("an original with nothing to quote gives an empty quote (the caller falls back)", () => {
    expect(quote("").html).toBe("");
    expect(quote("<p> </p><br>").html).toBe("");
    expect(quote('<img data-mailroom-remote-src="/jmap/proxy/A/M/t" data-mailroom-blocked="">').html).toBe("");
  });
});

describe("what is read is only ever parsed, never run", () => {
  it("a handler on a kept picture is gone from the output", () => {
    const q = quote('<p><img src="cid:logo@example.com" onerror="document.title=1"></p>', { parts: [part()] });
    expect(q.html).not.toContain("onerror");
  });
});
