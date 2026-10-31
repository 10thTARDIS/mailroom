// SPDX-License-Identifier: AGPL-3.0-only

// Tests for the withheld-attachment chip's decisions (#396/#412).
//
// Tested at the pure-function boundary rather than through a rendered
// AttachmentList, for the same reason attachmentPreviewPolicy.test.ts gives:
// this repo's web tests are all pure-function tests (no React Testing
// Library / jsdom is installed). isWithheld is exactly the "is this a
// download/preview link or a withheld chip" branch AttachmentList takes, and
// withheldReasonKey is exactly the text that branch prints — so pinning these
// pins what the component does without a renderer.

import { describe, expect, it } from "vitest";

import { isWithheld, scanStateFor, withheldReasonKey } from "./attachmentScanPolicy";
import type { AttachmentScan } from "../../core/jmap/types";

const scan = (parts: AttachmentScan["parts"], quarantined: string[]): AttachmentScan => ({
  parts,
  quarantined,
});

describe("isWithheld", () => {
  it("is true for a partId the server listed in quarantined", () => {
    const s = scan({ "2": { state: "flagged", released: false } }, ["2"]);
    expect(isWithheld(s, "2")).toBe(true);
  });

  // ---- THE CONTROL. Without this, "withheld" could be what every chip
  // renders, and the assertion above would still pass.
  it("is false for a part NOT in quarantined, even though the message has a scan record", () => {
    const s = scan(
      {
        "2": { state: "flagged", released: false },
        "3": { state: "clean", released: false },
      },
      ["2"],
    );
    expect(isWithheld(s, "3")).toBe(false);
  });

  it("is false when the message has no scan record at all (attachmentScan null)", () => {
    expect(isWithheld(null, "2")).toBe(false);
    expect(isWithheld(undefined, "2")).toBe(false);
  });

  it("is false for a null partId — a multipart node, never a leaf a chip renders", () => {
    const s = scan({ "2": { state: "flagged", released: false } }, ["2"]);
    expect(isWithheld(s, null)).toBe(false);
  });

  it("is false once the part has been released, matching the server's own list", () => {
    // The server only ever puts a partId in `quarantined` when
    // attachscan.Blocked says so, and released content is never blocked — so a
    // released part simply will not appear here. `parts` can still say
    // `released: true` for the record; `quarantined` is what a client must
    // trust for "will the download route 403 this".
    const s = scan({ "2": { state: "flagged", released: true } }, []);
    expect(isWithheld(s, "2")).toBe(false);
  });
});

describe("scanStateFor", () => {
  it("returns the recorded state for a known partId", () => {
    const s = scan({ "2": { state: "unknown", released: false } }, ["2"]);
    expect(scanStateFor(s, "2")).toEqual({ state: "unknown", released: false });
  });

  it("returns null for a partId the scan does not mention", () => {
    const s = scan({ "2": { state: "flagged", released: false } }, ["2"]);
    expect(scanStateFor(s, "9")).toBeNull();
  });

  it("returns null for no scan record and for a null partId", () => {
    expect(scanStateFor(null, "2")).toBeNull();
    expect(scanStateFor(undefined, "2")).toBeNull();
    const s = scan({ "2": { state: "flagged", released: false } }, ["2"]);
    expect(scanStateFor(s, null)).toBeNull();
  });
});

describe("withheldReasonKey", () => {
  // ⚠ THE LOAD-BEARING ASSERTION: these must be DIFFERENT keys. "flagged" is a
  // verdict ("we checked, and it is bad") and "unknown" is the scanner failing
  // to reach one ("we could not check") — collapsing them to one sentence
  // would tell a user the scanner caught something it never actually looked
  // at, or the reverse.
  it("gives flagged and unknown genuinely different keys", () => {
    const flaggedKey = withheldReasonKey("flagged");
    const unknownKey = withheldReasonKey("unknown");
    expect(flaggedKey).not.toBe(unknownKey);
    expect(flaggedKey).toBe("attachment.withheldReason.flagged");
    expect(unknownKey).toBe("attachment.withheldReason.unknown");
  });

  it("falls back to a generic-but-true key for any other state, never nothing", () => {
    expect(withheldReasonKey("clean")).toBe("attachment.withheldReason.generic");
    expect(withheldReasonKey("unscanned")).toBe("attachment.withheldReason.generic");
    expect(withheldReasonKey(undefined)).toBe("attachment.withheldReason.generic");
  });
});
