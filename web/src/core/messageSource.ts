// SPDX-License-Identifier: AGPL-3.0-only

// VIEW SOURCE (#43): turning a message's raw bytes into text a person can read,
// and its header block into rows.
//
// EVERY BYTE HERE IS THE SENDER'S. Nothing in this module produces markup; it
// produces strings, and the one component that shows them (ui/ViewSourceDialog)
// puts them in the DOM as React text children, never as HTML. Keeping the
// parsing here, free of the DOM, is what lets a test feed it a hostile message
// and see exactly what comes out.

/** How much of a message the dialog will render. A message can be tens of
 * megabytes; a `<pre>` that size freezes a tab. The download always has all of it. */
export const MAX_SOURCE_DISPLAY_BYTES = 1024 * 1024;

export type SourceText = {
  /** The text to display: at most `MAX_SOURCE_DISPLAY_BYTES` of the message. */
  text: string;
  /** The whole message's size in bytes. */
  totalBytes: number;
  /** True when `text` stops short of the end of the message. */
  truncated: boolean;
};

/** Decodes the first `limit` bytes as UTF-8, replacing what is not valid with
 * U+FFFD (the decoder's default; it never throws). A message is 7-bit or
 * arbitrary 8-bit and often not UTF-8 at all, so this is a view, not a parse. */
export function decodeSource(bytes: Uint8Array, limit: number = MAX_SOURCE_DISPLAY_BYTES): SourceText {
  const truncated = bytes.byteLength > limit;
  const head = truncated ? bytes.subarray(0, limit) : bytes;
  return {
    text: new TextDecoder("utf-8", { fatal: false }).decode(head),
    totalBytes: bytes.byteLength,
    truncated,
  };
}

/** The whole message as text, for the copy button. */
export function decodeAll(bytes: Uint8Array): string {
  return new TextDecoder("utf-8", { fatal: false }).decode(bytes);
}

export type HeaderRow = {
  /** The field name as received (case kept). Empty for a line with no colon. */
  name: string;
  /** The value with folding undone (RFC 5322 §2.2.3: only the line break is
   * removed, the whitespace that follows it stays). Not decoded: an RFC 2047
   * encoded word is left as written. */
  value: string;
};

/** How much of the start of the message the Headers view will parse. */
export const MAX_HEADER_PARSE_BYTES = 64 * 1024;
/** How many header rows the Headers view will render. */
export const MAX_HEADER_ROWS = 1000;

export type HeaderRows = {
  rows: HeaderRow[];
  /** Header lines that were not turned into rows (past the row or byte cap). */
  omitted: number;
};

/** parseHeaderRows bounded for display: only the first MAX_HEADER_PARSE_BYTES
 * characters are looked at, and at most MAX_HEADER_ROWS rows come back, so a
 * hostile message of a million one-character header lines cannot become a
 * million DOM nodes. `omitted` counts the header lines left out (a lower bound
 * when the byte cap cut the block short). */
export function parseHeaderRowsCapped(
  source: string,
  maxChars: number = MAX_HEADER_PARSE_BYTES,
  maxRows: number = MAX_HEADER_ROWS,
): HeaderRows {
  const window = source.length > maxChars ? source.slice(0, maxChars) : source;
  const all = parseHeaderRows(window);
  const cutByBytes = source.length > maxChars && !/\r?\n\r?\n|\r\r/.test(window);
  const rows = all.length > maxRows ? all.slice(0, maxRows) : all;
  let omitted = all.length - rows.length;
  if (cutByBytes) {
    // Count the lines after the window up to the end of the header block, bounded
    // by the display limit the caller already applied.
    const rest = source.slice(maxChars).split(/\r\n|\n|\r/);
    for (const line of rest) {
      if (line === "") break;
      omitted++;
    }
  }
  return { rows, omitted };
}

/** The header block of a message, in order, as received. The block ends at the
 * first empty line. A continuation line (it starts with a space or a tab)
 * belongs to the field above it; a continuation with nothing above it, or a
 * line with no colon, is kept as a row with an empty name rather than dropped,
 * so the view never hides a malformed line. */
export function parseHeaderRows(source: string): HeaderRow[] {
  const lines = source.split(/\r\n|\n|\r/);
  const rows: HeaderRow[] = [];
  for (const line of lines) {
    if (line === "") break;
    if ((line.startsWith(" ") || line.startsWith("\t")) && rows.length > 0) {
      rows[rows.length - 1]!.value += line;
      continue;
    }
    const colon = line.indexOf(":");
    if (colon <= 0 || /[\s\u0000-\u001f]/.test(line.slice(0, colon))) {
      rows.push({ name: "", value: line });
      continue;
    }
    rows.push({ name: line.slice(0, colon), value: line.slice(colon + 1).replace(/^[ \t]/, "") });
  }
  return rows;
}
