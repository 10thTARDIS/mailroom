// SPDX-License-Identifier: AGPL-3.0-only

// MARKDOWN COMPOSE (#56, spec §5) — a small, deliberately incomplete renderer.
//
// WHY OURS AND NOT A LIBRARY. A Markdown renderer is a parser fed by a human, and every
// general-purpose one is a moving target with a CVE history in its HTML output path.
// This project wraps third-party protocol code behind its own interfaces for exactly
// that reason (spec §3), and here the honest alternative is smaller still: support the
// subset people actually type in mail, and nothing else. What is NOT supported degrades
// to the literal characters the user typed, which is the correct failure for mail — the
// recipient sees what was written, never a mangled guess.
//
// THE SUBSET: ATX headings, fenced code blocks, blockquotes, unordered and ordered
// lists (one level), thematic breaks, paragraphs; inline code, bold, italic and links.
// No tables, no nesting, no reference links, no inline HTML, no footnotes, no
// autolinking of bare URLs.
//
// SAFETY, in order of load-bearing-ness:
//
//   1. EVERY CHARACTER OF USER TEXT IS ESCAPED BEFORE ANY MARKUP IS ADDED. Inline HTML
//      is therefore not "unsupported" in the sense of being ignored — `<script>` comes
//      out as the visible text `<script>`, because escaping happens first and markup is
//      only ever emitted by this file's own template strings.
//   2. LINK HREFS ARE AN ALLOWLIST: http, https, mailto. Anything else is not a link at
//      all — the text and the URL are shown literally. An allowlist, not a blocklist,
//      so no amount of `java&#115;cript:` obfuscation reaches an href.
//   3. The emitted tag set is fixed and closed: p, br, h1-h6, strong, em, code, pre,
//      blockquote, ul, ol, li, hr, a. No attributes are ever emitted except `href`.
//
// This is a CONVENIENCE, not the security boundary. The server sanitizes every outgoing
// HTML body before transmitting it (internal/api/draft.go → SanitizeOutboundHTML), so a
// mistake here cannot put hostile bytes on the wire. The rules above exist so this file
// is not the mistake.
//
// The Markdown SOURCE is what travels as the text/plain part of the message — the point
// of Markdown being that its source reads fine unrendered — so a recipient with no HTML
// sees exactly what the sender typed. See Compose.tsx's send().

/** Schemes a link may use. An allowlist: everything else is shown as literal text. */
const LINK_SCHEMES = ["http://", "https://", "mailto:"];

/** Sentinel wrapping extracted code spans while the other inline rules run, so that
 * `**` inside backticks is never read as bold. NUL cannot appear in the input (it is
 * stripped first), which is what makes it usable as a marker. */
const MARK = String.fromCharCode(0);

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function safeHref(url: string): string | null {
  const lower = url.toLowerCase();
  return LINK_SCHEMES.some((s) => lower.startsWith(s)) ? url : null;
}

/** Inline markup within one already-escaped line of text. */
function inline(escaped: string): string {
  // Code spans come out first and go back last: their contents are literal.
  const codes: string[] = [];
  let out = escaped.replace(/`([^`]+)`/g, (_m, code: string) => {
    codes.push(`<code>${code}</code>`);
    return `${MARK}${codes.length - 1}${MARK}`;
  });

  // Links. A rejected scheme is not silently dropped — the user should be able to see
  // that their link did not become one, rather than lose the address entirely.
  out = out.replace(/\[([^\]]*)\]\(([^)\s]+)\)/g, (m, text: string, url: string) => {
    const href = safeHref(url);
    return href === null ? m : `<a href="${href}">${text}</a>`;
  });

  out = out.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  out = out.replace(/(^|[^*\w])\*([^*\n]+)\*/g, "$1<em>$2</em>");
  out = out.replace(/(^|[^_\w])_([^_\n]+)_/g, "$1<em>$2</em>");

  return out.replace(new RegExp(`${MARK}(\\d+)${MARK}`, "g"), (_m, i: string) => codes[Number(i)] ?? "");
}

const HEADING = /^(#{1,6})[ \t]+(.*)$/;
const RULE = /^[ \t]{0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
const BULLET = /^[ \t]{0,3}[-*+][ \t]+(.*)$/;
const ORDERED = /^[ \t]{0,3}\d{1,9}[.)][ \t]+(.*)$/;
const QUOTE = /^[ \t]{0,3}>[ \t]?(.*)$/;
const FENCE = /^[ \t]{0,3}(`{3,}|~{3,})/;

/** Renders the supported Markdown subset to HTML. Never throws; unsupported syntax is
 * emitted as the literal text the user typed. */
export function markdownToHtml(src: string): string {
  const lines = src.replace(/\0/g, "").replace(/\r\n?/g, "\n").split("\n");
  const out: string[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i] ?? "";

    if (line.trim() === "") {
      i++;
      continue;
    }

    // Fenced code: everything to the closing fence is literal, including Markdown.
    // An UNCLOSED fence runs to the end of the message rather than being abandoned —
    // dropping the text would lose what the user wrote.
    const fence = FENCE.exec(line);
    if (fence) {
      const marker = fence[1] as string;
      const body: string[] = [];
      i++;
      while (i < lines.length && !(lines[i] ?? "").trimStart().startsWith(marker)) {
        body.push(lines[i] ?? "");
        i++;
      }
      if (i < lines.length) i++; // consume the closing fence
      out.push(`<pre><code>${escapeHtml(body.join("\n"))}</code></pre>`);
      continue;
    }

    if (RULE.test(line)) {
      out.push("<hr>");
      i++;
      continue;
    }

    const heading = HEADING.exec(line);
    if (heading) {
      const level = (heading[1] as string).length;
      out.push(`<h${level}>${inline(escapeHtml(heading[2] ?? ""))}</h${level}>`);
      i++;
      continue;
    }

    // Blockquote: collect the marked run, strip the markers, and render the contents
    // through the same renderer so a quote can hold paragraphs and lists.
    if (QUOTE.test(line)) {
      const inner: string[] = [];
      while (i < lines.length && QUOTE.test(lines[i] ?? "")) {
        inner.push(QUOTE.exec(lines[i] ?? "")?.[1] ?? "");
        i++;
      }
      out.push(`<blockquote>${markdownToHtml(inner.join("\n"))}</blockquote>`);
      continue;
    }

    // Lists, one level. A blank line ends the list, which is why "loose" lists render
    // as separate lists — acceptable for mail, and never wrong, only plainer.
    const listMatch = BULLET.exec(line) ?? ORDERED.exec(line);
    if (listMatch) {
      const ordered = BULLET.exec(line) === null;
      const pattern = ordered ? ORDERED : BULLET;
      const items: string[] = [];
      while (i < lines.length) {
        const m = pattern.exec(lines[i] ?? "");
        if (!m) break;
        items.push(`<li>${inline(escapeHtml(m[1] ?? ""))}</li>`);
        i++;
      }
      const tag = ordered ? "ol" : "ul";
      out.push(`<${tag}>${items.join("")}</${tag}>`);
      continue;
    }

    // Paragraph: to the next blank line or the start of any other block. A single
    // newline inside it becomes <br>, which is what someone writing mail means by it —
    // strict Markdown would join the lines and surprise them.
    const para: string[] = [];
    while (i < lines.length) {
      const l = lines[i] ?? "";
      if (
        l.trim() === "" ||
        RULE.test(l) ||
        HEADING.test(l) ||
        QUOTE.test(l) ||
        BULLET.test(l) ||
        ORDERED.test(l) ||
        FENCE.test(l)
      ) {
        break;
      }
      para.push(inline(escapeHtml(l)));
      i++;
    }
    out.push(`<p>${para.join("<br>")}</p>`);
  }

  return out.join("");
}
