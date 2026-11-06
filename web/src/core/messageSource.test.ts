// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import {
  MAX_HEADER_PARSE_BYTES,
  MAX_HEADER_ROWS,
  MAX_SOURCE_DISPLAY_BYTES,
  decodeAll,
  decodeSource,
  parseHeaderRows,
  parseHeaderRowsCapped,
} from "./messageSource";

const enc = (s: string) => new TextEncoder().encode(s);

describe("decodeSource", () => {
  it("decodes a short message whole, untruncated", () => {
    const out = decodeSource(enc("Subject: hi\r\n\r\nbody"));
    expect(out).toEqual({ text: "Subject: hi\r\n\r\nbody", totalBytes: 19, truncated: false });
  });

  it("replaces invalid UTF-8 rather than throwing", () => {
    const out = decodeSource(new Uint8Array([0x61, 0xff, 0x62]));
    expect(out.text).toBe("a�b");
  });

  it("truncates at the limit, says so, and still reports the full size", () => {
    const big = enc("x".repeat(MAX_SOURCE_DISPLAY_BYTES + 10));
    const out = decodeSource(big);
    expect(out.truncated).toBe(true);
    expect(out.text.length).toBe(MAX_SOURCE_DISPLAY_BYTES);
    expect(out.totalBytes).toBe(MAX_SOURCE_DISPLAY_BYTES + 10);
  });

  it("does not call a message of exactly the limit truncated", () => {
    expect(decodeSource(enc("x".repeat(100)), 100).truncated).toBe(false);
    expect(decodeSource(enc("x".repeat(101)), 100).truncated).toBe(true);
  });

  it("decodeAll ignores the display limit (the copy button)", () => {
    expect(decodeAll(enc("y".repeat(MAX_SOURCE_DISPLAY_BYTES + 5))).length).toBe(MAX_SOURCE_DISPLAY_BYTES + 5);
  });
});

describe("parseHeaderRows", () => {
  it("keeps order and duplicates as received", () => {
    const rows = parseHeaderRows("Received: a\r\nFrom: x\r\nReceived: b\r\n\r\nbody: not a header");
    expect(rows.map((r) => `${r.name}=${r.value}`)).toEqual(["Received=a", "From=x", "Received=b"]);
  });

  it("unfolds a long header by removing only the line break", () => {
    const rows = parseHeaderRows(
      "Subject: a very long\r\n  subject line that\r\n\twraps\r\nTo: you@example.com\r\n\r\n",
    );
    expect(rows).toEqual([
      { name: "Subject", value: "a very long  subject line that\twraps" },
      { name: "To", value: "you@example.com" },
    ]);
  });

  it("accepts bare LF line endings", () => {
    expect(parseHeaderRows("A: 1\n  2\nB: 3\n\nC: 4")).toEqual([
      { name: "A", value: "1  2" },
      { name: "B", value: "3" },
    ]);
  });

  it("leaves RFC 2047 encoded words as written", () => {
    const rows = parseHeaderRows("Subject: =?UTF-8?B?aMOpbGxv?=\r\n\r\n");
    expect(rows[0]!.value).toBe("=?UTF-8?B?aMOpbGxv?=");
  });

  it("keeps a malformed line as a nameless row instead of hiding it", () => {
    expect(parseHeaderRows("not a header\r\nA: 1\r\n")).toEqual([
      { name: "", value: "not a header" },
      { name: "A", value: "1" },
    ]);
    expect(parseHeaderRows("  orphan continuation\r\n")).toEqual([{ name: "", value: "  orphan continuation" }]);
  });

  it("returns nothing for a message with no header block", () => {
    expect(parseHeaderRows("\r\nbody")).toEqual([]);
  });
});

describe("parseHeaderRowsCapped", () => {
  it("caps 100k one-character header lines at the row cap and counts what it left out", () => {
    const src = "a:\r\n".repeat(100_000) + "\r\nbody";
    const out = parseHeaderRowsCapped(src);
    expect(out.rows.length).toBe(MAX_HEADER_ROWS);
    expect(out.omitted).toBe(100_000 - MAX_HEADER_ROWS);
  });

  it("stops parsing at the byte cap, never reading the whole megabyte", () => {
    const src = "a:\r\n".repeat(MAX_SOURCE_DISPLAY_BYTES / 4);
    const out = parseHeaderRowsCapped(src);
    expect(out.rows.length).toBe(MAX_HEADER_ROWS);
    expect(out.omitted).toBeGreaterThan(0);
    expect(MAX_HEADER_PARSE_BYTES).toBeLessThan(MAX_SOURCE_DISPLAY_BYTES);
  });

  it("reports nothing omitted for an ordinary message", () => {
    const out = parseHeaderRowsCapped("A: 1\r\nB: 2\r\n\r\nbody");
    expect(out.rows.length).toBe(2);
    expect(out.omitted).toBe(0);
  });
});
