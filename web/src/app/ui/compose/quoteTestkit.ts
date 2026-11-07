// SPDX-License-Identifier: AGPL-3.0-only

// Shared by the formatted-quote suites (ADR-051 §7): the fixture the server's
// reader sanitiser wrote (internal/api/sanitize_quote_received_test.go), the
// check that it is not stale, and the helpers that build a quote the way the
// composer does.

import sources from "./testdata/quote-sources.json";
import received from "./testdata/quote-received.json";
import type { BlobId } from "../../../core/jmap/types";
import { formatQuote } from "./quote";
import type { Quote, QuotePart } from "./quote";

export interface ReceivedCase {
  name: string;
  group: "hostile" | "layout" | "format";
  live: boolean;
  /** What the sender's message held. */
  source: string;
  /** What the reader's sanitiser returned for it, the proxy on: the value a reply quotes. */
  html: string;
}

export const RECEIVED = received as ReceivedCase[];
export const SOURCES = sources as Array<{ name: string; group: string; live: boolean; html: string }>;

/** The fixture is what the Go side wrote from today's sources: every hand-written
 * source is in it, unchanged. (That the sanitiser's output is current is the Go
 * test's check; this is the half it cannot make.) */
export function staleness(): string[] {
  const problems: string[] = [];
  for (const s of SOURCES) {
    const r = RECEIVED.find((c) => c.name === s.name);
    if (!r) problems.push(`quote-received.json has no case "${s.name}"`);
    else if (r.source !== s.html) problems.push(`case "${s.name}": its source changed since quote-received.json was written`);
  }
  return problems;
}

export const NOTES = { picture: "[picture not included]", rest: "[The rest of the original message is not included]" };

/** A part per `cid:` the HTML names, as an original that lists its pictures would. */
export function partsFor(html: string): QuotePart[] {
  const doc = new DOMParser().parseFromString(html, "text/html");
  const seen = new Set<string>();
  const out: QuotePart[] = [];
  doc.querySelectorAll("img").forEach((img) => {
    const src = img.getAttribute("src") ?? "";
    if (!/^cid:/i.test(src)) return;
    const cid = src.slice(4);
    if (seen.has(cid)) return;
    seen.add(cid);
    out.push({ partId: `p${out.length}`, blobId: `B-${cid}` as BlobId, type: "image/png", name: `${cid}.png`, size: 100, cid });
  });
  return out;
}

export function quoteOf(html: string, parts: QuotePart[] = [], over: { truncated?: boolean } = {}): Quote {
  return formatQuote({
    html,
    truncated: over.truncated ?? false,
    parts,
    withheld: () => false,
    canCarry: true,
    notes: NOTES,
  });
}

/** The editor's seed for a reply: what the composer builds from a quote. */
export function seedOf(q: Quote, header = "On 3/2/2026, Alice wrote:"): string {
  return `<p><br></p><p>${header}</p><blockquote>${q.html}</blockquote>`;
}

/** A document as the DOM reads it back, so quoting and entity forms cannot differ. */
export function norm(h: string): string {
  const ser = new XMLSerializer();
  return Array.from(new DOMParser().parseFromString(h, "text/html").body.childNodes)
    .map((n) => ser.serializeToString(n))
    .join("");
}
