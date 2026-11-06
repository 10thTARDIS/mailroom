// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import fixture from "./flowedWire.fixture.json";
import { unflow } from "./flowed";

// Twin of internal/compose/flowed_test.go (TestFlowedWireFixtureMatchesEncoder,
// #93). The fixture pairs hold text a user typed and the exact bytes the Go
// composer sends for it; the Go test fails if the encoder drifts from the file.
// Here the PRODUCTION reader must turn each wire form back into the original,
// so the sender and the reader are checked against one another and not each
// against its own idea of RFC 3676.

describe("composer output read back by unflow (#93)", () => {
  it("has fixtures that really are soft-wrapped", () => {
    // Control: a fixture set with no soft break would pass against any reader.
    expect(fixture.some((p) => p.wire.includes(" \r\n"))).toBe(true);
  });

  fixture.forEach((p, i) => {
    it(`round trips fixture ${i}`, () => {
      expect(unflow(p.wire, false)).toBe(p.original.replace(/\r\n/g, "\n"));
    });
  });
});
