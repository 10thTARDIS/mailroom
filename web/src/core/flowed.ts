// SPDX-License-Identifier: AGPL-3.0-only

// RFC 3676 "format=flowed" UNFLOWING (#890).
//
// A sender that hard-wraps a plain body to a fixed column and marks the part
// `Content-Type: text/plain; format=flowed` says the wrapping is a transport
// artifact, not a line break: the reader is expected to re-flow the text,
// where the receiver's screen is narrow. The server exposes the part's two
// RFC 3676 parameters — `format` and `delSp` — on every EmailBodyPart
// (internal/api/body.go); this is the client's one decode of them, applied
// where the plain body is assembled (app/ui/bodyChoice.ts).
//
// The decode, per line (RFC 3676 §4):
//   - the run of leading '>' characters is the quoting depth;
//   - a single space after them is either space-stuffing (the content really
//     begins with a space) or the space a quote mark is written with; one is
//     removed;
//   - a line that then still ENDS in a space is a soft break: it joins the
//     following line, keeping that trailing space unless DelSp=yes says the
//     sender folded an actual space away, and only while the following line
//     sits at the same quoting depth;
//   - the signature delimiter "-- " is a hard break and is never joined in
//     either direction ("shall NOT be folded or unfolded", RFC 3676 §4.2);
//   - every other line is a hard break and ends the paragraph.
//
// Delivered text is normalized to LF on the way out; a message with no soft
// breaks comes back byte-identical except for CRLF.
//
// Pure and DOM-free, like the other core modules: web/ has no jsdom, so the
// logic has to hold up reasoned about without a browser.

/** The RFC 3676 §4.2 signature delimiter, exactly. */
const SIG_DELIM = "-- ";

/** How many leading '>' a line carries (its quoting depth). */
function quoteDepth(line: string): number {
  let depth = 0;
  while (depth < line.length && line[depth] === ">") depth++;
  return depth;
}

/** The line's content: quote prefix removed, then ONE leading space removed
 * (the stuffed space, or the space after the last quote mark). */
function lineContent(line: string): string {
  let content = line.slice(quoteDepth(line));
  if (content.startsWith(" ")) content = content.slice(1);
  return content;
}

/** A line is a soft break when, once quoted and unstuffed, it ends in a space —
 * except the signature delimiter, which is never flowed. */
function isSoftBreak(content: string): boolean {
  return content.endsWith(" ") && content !== SIG_DELIM;
}

/** The output prefix for a quoting depth: "> " per level (">> " at depth 2). */
function quotePrefix(depth: number): string {
  return depth > 0 ? ">".repeat(depth) + " " : "";
}

/** Decode a flowed text body. `delSp` is the part's DelSp parameter (true for
 * "yes"): when set, the trailing space of each soft break is dropped; when
 * not, it is kept — it is the space the sender wrapped on. */
export function unflow(text: string, delSp: boolean): string {
  const lines = text.split(/\r\n|\n/);
  const out: string[] = [];
  let i = 0;
  // `?? ""` only satisfies noUncheckedIndexedAccess: every index read here is
  // bounded by `i < lines.length`.
  while (i < lines.length) {
    const line = lines[i] ?? "";
    const depth = quoteDepth(line);
    let piece = lineContent(line);
    let flowing = isSoftBreak(piece);
    i++;
    // A paragraph is gathered as pieces and joined ONCE. Appending to a growing
    // string and slicing its trailing space off each time flattens the whole
    // string per line, which is quadratic in a hostile message's length (a
    // megabyte of soft-wrapped lines would stall the reading pane).
    const pieces: string[] = [];
    // A soft break joins the next line only at the same quoting depth, and
    // never across the signature delimiter.
    while (flowing && i < lines.length && quoteDepth(lines[i] ?? "") === depth) {
      const next = lineContent(lines[i] ?? "");
      if (next === SIG_DELIM) break;
      pieces.push(delSp ? piece.slice(0, -1) : piece);
      piece = next;
      flowing = isSoftBreak(next);
      i++;
    }
    pieces.push(piece);
    out.push(quotePrefix(depth) + pieces.join(""));
  }
  return out.join("\n");
}
