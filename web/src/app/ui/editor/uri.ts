// SPDX-License-Identifier: AGPL-3.0-only

// What the compose editor will let into a message, decided in one place (ADR-051
// §2). These are conveniences for the person typing: the server's
// SanitizeOutboundHTML is still the boundary and re-decides all of it on send.

/** The URL as a browser's URL parser sees it before it looks for a scheme: leading
 * and trailing C0 controls and spaces are removed, and tab, LF and CR are removed
 * everywhere. NOTHING ELSE is: a space or no-break space inside "d a t a:" or
 * "java script:" means the text has no scheme and resolves as a relative URL, so
 * it must not be classified as if it had one. */
function browserView(url: string): string {
  let out = url.replace(/[\t\n\r]/g, "");
  let start = 0;
  let end = out.length;
  while (start < end && out.charCodeAt(start) <= 0x20) start += 1;
  while (end > start && out.charCodeAt(end - 1) <= 0x20) end -= 1;
  out = out.slice(start, end);
  return out;
}

/** The scheme of a URL, lower-cased, or "" when it has none. */
function schemeOf(url: string): string {
  const m = /^([a-z][a-z0-9+.-]*):/i.exec(browserView(url));
  return m ? (m[1] as string).toLowerCase() : "";
}

/** A link target the editor accepts: http, https or mailto, and nothing else.
 * A target with no scheme is refused too (the prompt adds https:// to a bare
 * address before it gets here), so a relative or scheme-less value never becomes
 * a link by being pasted. Handed to the Link extension as `isAllowedUri`. */
export function isAllowedLinkUri(url: string | undefined): boolean {
  if (!url) return false;
  const scheme = schemeOf(url);
  return scheme === "http" || scheme === "https" || scheme === "mailto";
}

/** What the link prompt's answer becomes: trimmed, and a bare address such as
 * `www.example.com` gets `https://` in front so it is a link rather than a
 * refusal. An answer with a scheme is returned as typed (and may then be refused
 * by isAllowedLinkUri). "" for no answer. */
export function linkFromPrompt(answer: string | null): string {
  const url = (answer ?? "").trim();
  if (url === "") return "";
  return schemeOf(url) === "" ? `https://${url}` : url;
}

/** An image source the editor keeps on paste: an inline attachment reference
 * (cid:) or inline raster image bytes (data:image/*). Anything else (https:, http:,
 * blob:, file:, a relative path) would be a remote fetch or a hot-link, and a
 * picture is attached, never linked (ADR-051 §2). SVG is refused too: it is a
 * document, and the server drops it on send, so keeping it would make the picture
 * vanish silently later. */
export function isInlineImageSrc(src: string | null | undefined): boolean {
  if (!src) return false;
  const v = browserView(src).toLowerCase();
  if (v.startsWith("cid:")) return true;
  if (!v.startsWith("data:image/")) return false;
  return !/^data:image\/svg/.test(v);
}
