// SPDX-License-Identifier: AGPL-3.0-only

// What the compose editor will let into a message, decided in one place (ADR-051
// §2). These are conveniences for the person typing: the server's
// SanitizeOutboundHTML is still the boundary and re-decides all of it on send.

/** The URL without the characters a browser ignores inside a scheme: controls,
 * spaces and the Unicode space separators ("java\tscript:" is "javascript:"). */
function squeeze(url: string): string {
  let out = "";
  for (const ch of url) {
    const c = ch.codePointAt(0) as number;
    const ignored =
      c <= 0x20 ||
      c === 0xa0 ||
      c === 0x1680 ||
      c === 0x180e ||
      (c >= 0x2000 && c <= 0x2029) ||
      c === 0x205f ||
      c === 0x3000;
    if (!ignored) out += ch;
  }
  return out;
}

/** The scheme of a URL, lower-cased, or "" when it has none. */
function schemeOf(url: string): string {
  const m = /^([a-z][a-z0-9+.-]*):/i.exec(squeeze(url));
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
 * (cid:) or inline image bytes (data:image/*). Anything else (https:, http:,
 * blob:, file:, a relative path) would be a remote fetch or a hot-link, and a
 * picture is attached, never linked (ADR-051 §2). */
export function isInlineImageSrc(src: string | null | undefined): boolean {
  if (!src) return false;
  const compact = squeeze(src).toLowerCase();
  return compact.startsWith("cid:") || compact.startsWith("data:image/");
}
