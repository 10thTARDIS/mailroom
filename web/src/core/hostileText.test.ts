// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import {
  hasBidiControls,
  senderIdentity,
  senderText,
  stripBidiControls,
} from "./hostileText";

// The #307 payloads, built with String.fromCodePoint rather than pasted, for the
// same reason BIDI_RANGES is written as hex: a test file full of invisible
// characters is a test file nobody can review. Each constant is named for what it
// does so a failure message reads as a sentence.
const LRE = String.fromCodePoint(0x202a); // LEFT-TO-RIGHT EMBEDDING
const RLE = String.fromCodePoint(0x202b); // RIGHT-TO-LEFT EMBEDDING
const PDF = String.fromCodePoint(0x202c); // POP DIRECTIONAL FORMATTING
const LRO = String.fromCodePoint(0x202d); // LEFT-TO-RIGHT OVERRIDE
const RLO = String.fromCodePoint(0x202e); // RIGHT-TO-LEFT OVERRIDE
const LRI = String.fromCodePoint(0x2066); // LEFT-TO-RIGHT ISOLATE
const RLI = String.fromCodePoint(0x2067); // RIGHT-TO-LEFT ISOLATE
const FSI = String.fromCodePoint(0x2068); // FIRST STRONG ISOLATE
const PDI = String.fromCodePoint(0x2069); // POP DIRECTIONAL ISOLATE

const LRM = String.fromCodePoint(0x200e); // LEFT-TO-RIGHT MARK — deliberately kept
const RLM = String.fromCodePoint(0x200f); // RIGHT-TO-LEFT MARK — deliberately kept

describe("stripBidiControls", () => {
  it("removes every scoping control, and only those", () => {
    // Named individually rather than looped over a range, so a failure says
    // WHICH character survived instead of "one of nine".
    for (const [name, ch] of Object.entries({ LRE, RLE, PDF, LRO, RLO, LRI, RLI, FSI, PDI })) {
      expect(stripBidiControls(`a${ch}b`), `${name} survived`).toBe("ab");
      expect(hasBidiControls(`a${ch}b`), `${name} not detected`).toBe(true);
    }
  });

  it("leaves LRM and RLM alone — they cannot reorder a following run", () => {
    // The negative control for the range: a rule that strips these would corrupt
    // legitimate Arabic/Hebrew display names, and would still be "passing".
    expect(stripBidiControls(`a${LRM}b`)).toBe(`a${LRM}b`);
    expect(stripBidiControls(`a${RLM}b`)).toBe(`a${RLM}b`);
    expect(hasBidiControls(`a${LRM}${RLM}b`)).toBe(false);
  });

  it("removes several controls in one string", () => {
    expect(stripBidiControls(`${RLO}gpj.exe${PDF}`)).toBe("gpj.exe");
  });

  it("leaves ordinary text byte-identical, including non-Latin scripts and emoji", () => {
    for (const s of [
      "",
      "Ada Lovelace",
      "receipts@example.com",
      "Re: your order — shipped 📦",
      "אדה לאבלייס", // Hebrew: right-to-left WITHOUT any control character
      "アダ・ラブレス",
    ]) {
      expect(stripBidiControls(s)).toBe(s);
      expect(hasBidiControls(s)).toBe(false);
    }
  });

  it("is total on null and undefined", () => {
    expect(stripBidiControls(null)).toBe("");
    expect(stripBidiControls(undefined)).toBe("");
    expect(hasBidiControls(null)).toBe(false);
    expect(hasBidiControls(undefined)).toBe(false);
  });

  it("is a fixed point: stripping twice changes nothing", () => {
    const once = stripBidiControls(`${RLO}a${LRI}b${PDI}`);
    expect(stripBidiControls(once)).toBe(once);
  });

  it("does not mis-step over a surrogate pair", () => {
    // U+1F4E6 is two UTF-16 units. A naive index-by-unit scan can split it; the
    // low surrogate (0xDCE6) is nowhere near the bidi ranges, but a scan that
    // treated units as code points would still be wrong on some input, and this
    // is the cheap place to notice.
    const pkg = String.fromCodePoint(0x1f4e6);
    expect(stripBidiControls(`${pkg}${RLO}x`)).toBe(`${pkg}x`);
    expect([...stripBidiControls(`${pkg}x`)].length).toBe(2);
  });
});

describe("senderIdentity", () => {
  it("shows the name alone for an ordinary From", () => {
    const id = senderIdentity({ name: "Ada Lovelace", email: "ada@example.com" });
    expect(id).toEqual({ name: "Ada Lovelace", address: "ada@example.com", showAddress: false });
    expect(senderText(id, "Unknown")).toBe("Ada Lovelace");
  });

  it("shows the ADDRESS TOO when the display name is itself an address (#307)", () => {
    // The attack in the issue: the row would otherwise read `security@bank.example`
    // for a message from evil.example.
    const id = senderIdentity({ name: "security@bank.example", email: "attacker@evil.example" });
    expect(id.showAddress).toBe(true);
    expect(senderText(id, "Unknown")).toBe("security@bank.example <attacker@evil.example>");
  });

  it("shows the address too when the display name was crafted with a bidi control", () => {
    const id = senderIdentity({ name: `${RLO}Bank Security`, email: "attacker@evil.example" });
    expect(id.name).toBe("Bank Security"); // stripped
    expect(id.showAddress).toBe(true);
    expect(senderText(id, "Unknown")).toBe("Bank Security <attacker@evil.example>");
  });

  it("strips bidi controls from the address as well as the name", () => {
    const id = senderIdentity({ name: "Ada", email: `ada${LRO}@example.com` });
    expect(id.address).toBe("ada@example.com");
  });

  it("falls back to the address when there is no display name, and does not repeat it", () => {
    const id = senderIdentity({ name: "", email: "noreply@example.com" });
    expect(id.showAddress).toBe(false); // `x@y <x@y>` would be noise, not safety
    expect(senderText(id, "Unknown")).toBe("noreply@example.com");
  });

  it("falls back to the caller's string when the From carries nothing", () => {
    for (const from of [null, undefined, {}, { name: "", email: "" }, { name: null, email: null }]) {
      const id = senderIdentity(from);
      expect(senderText(id, "No sender")).toBe("No sender");
      expect(id.showAddress).toBe(false);
    }
  });

  it("does not claim an address it does not have", () => {
    // A name that looks like an address with NO address behind it must not
    // produce `security@bank.example <>`.
    const id = senderIdentity({ name: "security@bank.example", email: "" });
    expect(id.showAddress).toBe(false);
    expect(senderText(id, "Unknown")).toBe("security@bank.example");
  });

  it("trims, so surrounding whitespace cannot hide the address off the end", () => {
    const id = senderIdentity({ name: "  Ada  ", email: "  ada@example.com  " });
    expect(id.name).toBe("Ada");
    expect(id.address).toBe("ada@example.com");
  });

  it("STATES THE GAP: a lookalike with no @ is not caught here", () => {
    // Not an aspiration — an assertion, so that if somebody later widens the rule
    // this test goes red and the widening is a decision rather than a drift.
    // Catching this case needs an AUTHENTICATED From (DMARC/ARC, #55), not a
    // heuristic over a string the sender chose.
    const id = senderIdentity({ name: "Bank Security", email: "attacker@evil.example" });
    expect(id.showAddress).toBe(false);
  });
});
