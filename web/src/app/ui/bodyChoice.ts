// SPDX-License-Identifier: AGPL-3.0-only

// WHICH BODY A MESSAGE IS SHOWN (OR QUOTED) AS (#871).
//
// RFC 8621 §4.1.4 puts a lone text/plain part in BOTH `textBody` and `htmlBody`
// (the server is right to; a client that only reads htmlBody still gets
// something). A client must therefore not read "htmlBody is non-empty" as "this
// message has HTML": handing plain text to the HTML frame collapses every line
// break. The HTML path is for a message with at least one text/html part.
//
// A MIXED htmlBody (multipart/mixed: text/plain, text/html, ...) keeps the HTML
// path, so its text/plain parts are escaped and made newline-preserving here
// rather than being interpreted as markup.

import { unflow } from "../../core/flowed";
import type { EmailBodyPart } from "../../core/jmap/types";

interface BodySource {
  textBody?: EmailBodyPart[];
  htmlBody?: EmailBodyPart[];
  bodyValues?: Record<string, { value: string }>;
}

const typeOf = (p: EmailBodyPart): string => p.type.toLowerCase();

/** Whether the list has a text/html part. Only then is the HTML path right. */
export function hasHtmlPart(parts: EmailBodyPart[] | undefined): boolean {
  return (parts ?? []).some((p) => typeOf(p) === "text/html");
}

/** Complete escaping for text placed in element content or an attribute. */
export function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

/** Plain text as a block that keeps its line breaks inside the HTML frame. The
 * frame CSP allows inline styles (`style-src 'unsafe-inline'`). */
export function plainAsHtml(text: string): string {
  return `<div style="white-space:pre-wrap">${escapeHtml(text)}</div>`;
}

/**
 * The HTML to render for a message, or "" when it has no text/html part (the
 * caller then shows the plain body). Text/plain parts inside a mixed list are
 * escaped and wrapped to keep their newlines; other parts' values pass through
 * as the server delivered them.
 */
export function htmlToRender(src: BodySource, sep = "\n\n"): string {
  if (!hasHtmlPart(src.htmlBody)) return "";
  const values = src.bodyValues;
  return (src.htmlBody ?? [])
    .map((p) => {
      const v = p.partId ? values?.[p.partId]?.value ?? "" : "";
      if (!v) return "";
      return typeOf(p) === "text/plain" ? plainAsHtml(v) : v;
    })
    .filter(Boolean)
    .join(sep);
}

/** A text/plain value, unflowed when the part is marked format=flowed (RFC
 * 3676, #890) so its soft wraps reflow on a narrow screen. The parameter is
 * case-insensitive here even though the server already lower-cases it: a part
 * fetched before the server understood the parameters simply has none. */
function plainValue(p: EmailBodyPart, value: string): string {
  return typeOf(p) === "text/plain" && (p.format ?? "").toLowerCase() === "flowed"
    ? unflow(value, p.delSp === true)
    : value;
}

/** The plain-text body (textBody values joined). */
export function plainBody(src: BodySource): string {
  const values = src.bodyValues;
  return (src.textBody ?? [])
    .map((p) => plainValue(p, p.partId ? values?.[p.partId]?.value ?? "" : ""))
    .filter(Boolean)
    .join("\n\n");
}

export type BodyChoice = { kind: "html"; html: string } | { kind: "text"; text: string };

/** The one decision: HTML only when a text/html part exists and has content. */
export function chooseBody(src: BodySource): BodyChoice {
  const html = htmlToRender(src);
  if (html) return { kind: "html", html };
  return { kind: "text", text: plainBody(src) };
}
