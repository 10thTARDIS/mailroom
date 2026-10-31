// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { pickSignatureFor } from "./signature";

describe("pickSignatureFor", () => {
  it("replaces when the body is untouched — the signature block is exactly what compose put there", () => {
    const current = "Jane Doe\nSales";
    const body = `\n\n${current}\n\n> quoted mail\n> more quoted mail\n`;
    const result = pickSignatureFor(body, current, "New Sig");
    expect(result.kind).toBe("replace");
    expect(result.body).toBe("\n\nNew Sig\n\n> quoted mail\n> more quoted mail\n");
  });

  it("still replaces when text was typed elsewhere and the block itself is untouched", () => {
    const current = "Jane Doe\nSales";
    const body = `Thanks for this!\n\n${current}\n\n> quoted mail\n`;
    const result = pickSignatureFor(body, current, "New Sig");
    expect(result.kind).toBe("replace");
    expect(result.body).toBe("Thanks for this!\n\nNew Sig\n\n> quoted mail\n");
  });

  it("inserts instead when the signature block itself was edited", () => {
    // "Doe" became "D." inside the block: the exact original text is gone, so
    // there is nothing left to find and replace.
    const current = "Jane Doe\nSales";
    const body = `\n\nJane D.\nSales\n\n> quoted mail\n`;
    const result = pickSignatureFor(body, current, "New Sig");
    expect(result.kind).toBe("insert");
    // Nothing already in the body is deleted — the new signature is appended.
    expect(result.body).toBe(`${body}New Sig`);
  });

  it("inserts when there was never a signature block (no default configured)", () => {
    const body = "\n\n> quoted mail\n";
    const result = pickSignatureFor(body, null, "New Sig");
    expect(result.kind).toBe("insert");
    expect(result.body).toBe(`${body}New Sig`);
  });

  // A MANUAL PICK SURVIVES A FROM CHANGE. Compose.tsx's From-reseed effect only
  // rewrites the body when it is still `untouched` (body === the ORIGINAL
  // opening body). A manual pick's result body is a DIFFERENT string from that
  // original — whether it replaced or inserted — so the reseed effect's own
  // equality check already sees a touched body and skips, without this module
  // needing to know anything about identities or effects.
  it("a manual pick's result never equals the original opening body, so it survives a later From change", () => {
    const originalOpening = "\n\nOld Default\n\n> quoted mail\n";
    const picked = pickSignatureFor(originalOpening, "Old Default", "Manually Chosen");
    expect(picked.body).not.toBe(originalOpening);

    // Even the INSERT branch (no default signature to begin with) changes the
    // body, so it survives too.
    const openingWithNoSig = "\n\n> quoted mail\n";
    const insertedPick = pickSignatureFor(openingWithNoSig, null, "Manually Chosen");
    expect(insertedPick.body).not.toBe(openingWithNoSig);
  });

  it("works the same way over HTML as over plain text (rich vs plain case)", () => {
    const current = "<p>Jane Doe</p><p>Sales</p>";
    const body = `<p><br></p>${current}<blockquote>quoted</blockquote>`;
    const replaced = pickSignatureFor(body, current, "<p>New Sig</p>");
    expect(replaced.kind).toBe("replace");
    expect(replaced.body).toBe("<p><br></p><p>New Sig</p><blockquote>quoted</blockquote>");

    const editedHtml = "<p>Jane D.</p><p>Sales</p>";
    const bodyEdited = `<p><br></p>${editedHtml}<blockquote>quoted</blockquote>`;
    const inserted = pickSignatureFor(bodyEdited, current, "<p>New Sig</p>");
    expect(inserted.kind).toBe("insert");
    expect(inserted.body).toBe(`${bodyEdited}<p>New Sig</p>`);
  });

  it("treats an empty current block as none — always inserts", () => {
    const result = pickSignatureFor("hello", "", "sig");
    expect(result.kind).toBe("insert");
    expect(result.body).toBe("hellosig");
  });
});
