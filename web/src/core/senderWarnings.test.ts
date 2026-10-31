// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { senderWarningsList } from "./senderWarnings";
import type { SenderWarningRaw } from "./jmap/types";

describe("senderWarningsList", () => {
  it("says nothing when the property is null or undefined — the overwhelming majority of mail", () => {
    // Excludes a defect where the absence of a finding (`null`, per #397's
    // server contract) is confused with an empty array being something to
    // render — either way this must produce an empty list, never throw.
    expect(senderWarningsList(null)).toEqual([]);
    expect(senderWarningsList(undefined)).toEqual([]);
  });

  it("says nothing for an already-empty array", () => {
    // Excludes a defect where `[]` is treated as "unknown" and rejected, or
    // where the function assumes a non-empty input and indexes into nothing.
    expect(senderWarningsList([])).toEqual([]);
  });

  it("re-orders selfSpoof before lookalikeDomain before punycodeDomain, regardless of input order", () => {
    // Excludes a defect where the client TRUSTS the server's order instead of
    // asserting it. The server (internal/senderwarn.Check) already returns
    // worst-first, so a test that only fed sorted input would pass even if the
    // client-side sort were missing entirely — this feeds the reverse order to
    // make the sort itself the thing under test.
    const raw: SenderWarningRaw[] = [
      { kind: "punycodeDomain", domain: "xn--80ak6aa92e.com" },
      { kind: "lookalikeDomain", domain: "paypa1.com", resembles: "paypal.com" },
      { kind: "selfSpoof", domain: "example.com" },
    ];
    expect(senderWarningsList(raw).map((w) => w.kind)).toEqual([
      "selfSpoof",
      "lookalikeDomain",
      "punycodeDomain",
    ]);
  });

  it("keeps multipleFrom (#754) and orders it right after selfSpoof", () => {
    const raw: SenderWarningRaw[] = [
      { kind: "punycodeDomain", domain: "xn--80ak6aa92e.com" },
      { kind: "multipleFrom", domain: "" },
      { kind: "selfSpoof", domain: "example.com" },
    ];
    expect(senderWarningsList(raw).map((w) => w.kind)).toEqual(["selfSpoof", "multipleFrom", "punycodeDomain"]);
  });

  it("drops a kind it does not recognise instead of passing it through", () => {
    // Excludes the exact defect the module exists to prevent: a future server
    // kind reaching the reader as an untranslated raw string inside a security
    // notice. The known warning either side of it must survive, so this also
    // excludes a defect where one bad element discards the whole list.
    const raw = [
      { kind: "selfSpoof", domain: "example.com" },
      { kind: "newFutureKind", domain: "unknown.example" },
      { kind: "punycodeDomain", domain: "xn--80ak6aa92e.com" },
    ] as unknown as SenderWarningRaw[];
    expect(senderWarningsList(raw).map((w) => w.kind)).toEqual(["selfSpoof", "punycodeDomain"]);
  });

  it("carries domain, resembles and unicode through unchanged for the kinds that have them", () => {
    // Excludes a defect where normalising drops or renames a field the render
    // layer needs verbatim — `resembles` is what makes a lookalike warning
    // actionable rather than merely ominous, and `unicode` is the whole point
    // of the punycode notice.
    const raw: SenderWarningRaw[] = [
      { kind: "lookalikeDomain", domain: "paypa1.com", resembles: "paypal.com" },
      { kind: "punycodeDomain", domain: "xn--e1aybc.com", unicode: "тест.com" },
    ];
    expect(senderWarningsList(raw)).toEqual([
      { kind: "lookalikeDomain", domain: "paypa1.com", resembles: "paypal.com" },
      { kind: "punycodeDomain", domain: "xn--e1aybc.com", unicode: "тест.com" },
    ]);
  });

  it("does not invent resembles or unicode when the wire omitted them", () => {
    // Excludes a defect where normalising writes `resembles: undefined` onto
    // every entry — which would pass a shallow truthy check but fail a
    // toEqual comparison against a plain object that never had the key, and
    // could confuse a render layer that checks `"resembles" in warning`.
    const raw: SenderWarningRaw[] = [{ kind: "selfSpoof", domain: "example.com" }];
    const list = senderWarningsList(raw);
    expect(list).toHaveLength(1);
    const warning = list[0]!;
    expect(warning).toEqual({ kind: "selfSpoof", domain: "example.com" });
    expect("resembles" in warning).toBe(false);
    expect("unicode" in warning).toBe(false);
  });
});
