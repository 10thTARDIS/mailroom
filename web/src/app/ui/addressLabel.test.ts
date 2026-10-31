// SPDX-License-Identifier: AGPL-3.0-only

// #600: a recipient's address must be readable, not merely present in the data.
//
// THE ASSERTION THAT MATTERS IS THAT THE ADDRESS IS ALWAYS THERE. The defect was
// `name || email` — a fallback that silently drops the address for everyone who
// has a display name, which is most people. So every case below checks for the
// address, and the no-name case is what proves the function has not simply
// started printing names in a different arrangement.

import { describe, expect, it } from "vitest";

import { addressLabel, addressLine } from "./addressLabel";

describe("addressLabel", () => {
  it("shows the address alongside the display name — the case that was broken", () => {
    expect(addressLabel({ name: "Dana Vance", email: "dana@example.org" })).toBe(
      "Dana Vance <dana@example.org>",
    );
  });

  it("shows the address alone when there is no name, exactly as before", () => {
    expect(addressLabel({ name: null, email: "parcel@example.net" })).toBe(
      "parcel@example.net",
    );
    expect(addressLabel({ name: "   ", email: "parcel@example.net" })).toBe(
      "parcel@example.net",
    );
  });

  it("does not repeat an address that is also the display name", () => {
    // Providers commonly set the name to the address. "a@b <a@b>" is noise.
    expect(addressLabel({ name: "a@b.example", email: "a@b.example" })).toBe("a@b.example");
    expect(addressLabel({ name: "A@B.example", email: "a@b.example" })).toBe("a@b.example");
  });

  it("never renders a name with no address, for any input with an address", () => {
    // The defect stated as a property rather than as an example.
    for (const name of ["Dana Vance", "", "   ", null, "a@b.example"]) {
      const out = addressLabel({ name, email: "who@example.org" });
      expect(out).toContain("who@example.org");
    }
  });

  it("falls back to the name when there is no address at all", () => {
    // Should not happen, but an empty line is worse than a bare name.
    expect(addressLabel({ name: "Dana Vance", email: "" })).toBe("Dana Vance");
  });
});

describe("addressLine", () => {
  it("names every recipient, each with their own address", () => {
    expect(
      addressLine([
        { name: "Dana Vance", email: "dana@example.org" },
        { name: null, email: "parcel@example.net" },
      ]),
    ).toBe("Dana Vance <dana@example.org>, parcel@example.net");
  });

  it("is empty for no recipients rather than rendering a stray separator", () => {
    expect(addressLine([])).toBe("");
  });
});
