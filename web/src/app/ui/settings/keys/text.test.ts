// SPDX-License-Identifier: AGPL-3.0-only

// No raw code reaches the screen (#796): every KeyActionError the page can be
// handed has its own sentence, and none of those sentences is, or contains, a
// code. The SetError and method-error types are the server's own words, taken
// from responses captured from it (keyFixtures.testdata.ts).

import { describe, expect, it } from "vitest";

import { METHOD_ERRORS, SET_ERRORS } from "../../../../core/keyFixtures.testdata";
import { keyErrorOf, keySetErrorOf } from "../../../../core/userKeys";
import type { KeyActionError } from "../../../../core/userKeys";
import { JmapMethodError } from "../../../../core/jmap/client";
import { KEY_ERROR_TEXT, keyErrorText, keyEventText, refreshText } from "./text";
import { keyHistoryLines, refreshOutcome } from "../../../../core/keyLifecycle";
import {
  C1_HISTORY_BOB,
  C1_METHOD_ERRORS,
  C1_REFRESH_ALICE_DIFFERENT,
  C1_REFRESH_ALICE_NOT_FOUND,
  C1_REFRESH_ALICE_UNCHANGED,
  C1_REFRESH_ALICE_UNREACHABLE,
  C1_REFRESH_CAROL_RENEWED,
} from "../../../../core/keyHistoryFixtures.testdata";

const CODES = Object.keys(KEY_ERROR_TEXT) as KeyActionError[];

describe("keyErrorText", () => {
  it("has a sentence for every error, and no sentence names a code", () => {
    expect(CODES.length).toBeGreaterThan(15);
    for (const code of CODES) {
      const s = keyErrorText(code);
      expect(s, code).not.toBe(KEY_ERROR_TEXT[code]); // the key itself would mean "no catalog entry"
      expect(s.length, code).toBeGreaterThan(15);
      for (const other of CODES) expect(s, `${code} mentions ${other}`).not.toContain(other);
      expect(s).not.toMatch(/[a-z][A-Z]/); // no camelCase identifiers in a sentence
    }
  });

  it("the server's captured refusals land on their own sentences", () => {
    expect(keyErrorText(keySetErrorOf(SET_ERRORS.keyIsPublished))).toBe(
      "This key is published at its address. Withdraw it from the key directory first.",
    );
    expect(keyErrorText(keySetErrorOf(SET_ERRORS.notPublishable))).toContain("can't be published");
    expect(keyErrorText(keyErrorOf(new JmapMethodError("c", METHOD_ERRORS.importPublic)))).toContain(
      "That is a public key",
    );
    // The server's own description is not shown (it is written for developers
    // and may name a parameter); the closed-set sentence is.
    expect(keyErrorText(keyErrorOf(new JmapMethodError("c", METHOD_ERRORS.revokeWrongConfirm)))).not.toContain(
      "01M5EXWWHCGES6KJ87RFYX3ZPX",
    );
  });
});

describe("history and refresh in words (#815, #818)", () => {
  it("a correspondent key's events are short sentences, never a kind or a source code", () => {
    // CAPTURED: imported, refused, removed.
    expect(keyHistoryLines(C1_HISTORY_BOB).map(keyEventText)).toEqual([
      "You imported it",
      "You chose not to use it",
      "You removed it",
    ]);
    // Each kind and source the server documents (the shape CAPTURED; these
    // kinds and sources DERIVED, since the stack produced only three).
    const at = "2026-10-21T18:22:51Z";
    const line = (kind: string, source: string, expiresAt: string | null = null) =>
      keyEventText(keyHistoryLines([{ kind, source, at, expiresAt }])[0]!);
    expect(line("first_seen", "autocrypt")).toBe("First seen via Autocrypt");
    expect(line("first_seen", "wkd")).toBe("First seen via WKD");
    expect(line("renewed", "wkd", "2028-11-03T18:25:11Z")).toBe("Renewed via WKD — now expires 3 Nov 2028");
    expect(line("renewed", "wkd")).toBe("Renewed via WKD");
    expect(line("updated", "keyserver")).toBe("Updated via a key server");
    expect(line("revoked_seen", "wkd")).toBe("Revoked by its owner (seen via WKD)");
    expect(line("revoked_seen", "somewhere")).toBe("Revoked by its owner");
    expect(line("pinned", "user")).toBe("You chose to use it");
    expect(line("teleported", "user")).toBe("Changed");
    for (const s of [line("first_seen", "somewhere"), line("updated", "x")]) {
      expect(s).not.toMatch(/_|[a-z][A-Z]|\s$/);
    }
  });

  it("each refresh answer is its sentence; a too-soon check says when to try again", () => {
    expect(refreshText(refreshOutcome(C1_REFRESH_CAROL_RENEWED))).toBe("Updated — now expires 3 Nov 2028");
    expect(refreshText(refreshOutcome(C1_REFRESH_ALICE_UNCHANGED))).toBe("No newer version published");
    expect(refreshText(refreshOutcome(C1_REFRESH_ALICE_DIFFERENT))).toBe("They've published a different key");
    expect(refreshText(refreshOutcome(C1_REFRESH_ALICE_NOT_FOUND))).toBe(
      "Their key directory doesn't publish a key for them",
    );
    expect(refreshText(refreshOutcome(C1_REFRESH_ALICE_UNREACHABLE))).toBe(
      "Couldn't reach their key directory. Try again later.",
    );
    // CAPTURED: the second check inside 15 minutes.
    const tooSoon = keyErrorOf(new JmapMethodError("c", C1_METHOD_ERRORS.refreshAgain));
    expect(refreshText({ kind: "error", error: tooSoon })).toBe("Checked recently — try again in a few minutes");
    expect(
      refreshText({ kind: "error", error: keyErrorOf(new JmapMethodError("c", { type: "wkdUnavailable" })) }),
    ).toBe("This server can't look keys up in key directories (WKD).");
  });
});
