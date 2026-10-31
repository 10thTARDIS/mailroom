// SPDX-License-Identifier: AGPL-3.0-only

// The failure worth guarding is not "the wrong blue". It is a PAUSED account that
// goes on looking live because the user tinted it once — the dot is the rail's
// only per-row statement about whether mail is arriving, and a colour that
// outranked it would quietly withdraw that statement.

import { describe, expect, it } from "vitest";

import { ACCOUNT_COLOR_PRESETS, accountColorOrToken, isHexColor } from "./accountColor";

describe("isHexColor", () => {
  it("accepts exactly what the server accepts", () => {
    expect(isHexColor("#3B6FB6")).toBe(true);
    expect(isHexColor("#3b6fb6")).toBe(true);
  });

  it("refuses the shorthand, because the server would", () => {
    // internal/api/account.go validHexColor is #RRGGBB only. A picker that let
    // #abc through would produce a write refused per object, with the dot already
    // repainted.
    expect(isHexColor("#abc")).toBe(false);
  });

  it("refuses anything that is not a hex colour at all", () => {
    expect(isHexColor("red")).toBe(false);
    expect(isHexColor("")).toBe(false);
    expect(isHexColor("#3B6FB6;background:url(x)")).toBe(false);
    expect(isHexColor("rgb(1,2,3)")).toBe(false);
  });
});

describe("accountColorOrToken", () => {
  it("uses the chosen colour and still reports a token", () => {
    // Both, always: the class is the fallback if the inline style is ever dropped,
    // so there is no state in which the dot has no colour at all.
    expect(accountColorOrToken({ color: "#3B6FB6", paused: false }, 0)).toEqual({
      tone: "1",
      color: "#3B6FB6",
    });
  });

  it("falls back to the ordinal tone when no colour is set", () => {
    expect(accountColorOrToken({ color: null, paused: false }, 0)).toEqual({
      tone: "1",
      color: null,
    });
    expect(accountColorOrToken({ color: null, paused: false }, 1)).toEqual({
      tone: "2",
      color: null,
    });
  });

  it("cycles the two declared tones rather than naming one that does not exist", () => {
    // tokens.css declares --acct-1 and --acct-2 and the build refuses an
    // undeclared token, so a third account must reuse the first tone.
    expect(accountColorOrToken({ color: null, paused: false }, 2).tone).toBe("1");
    expect(accountColorOrToken({ color: null, paused: false }, 3).tone).toBe("2");
  });

  it("PAUSED BEATS THE CHOSEN COLOUR", () => {
    // The one that matters. --acct-off is how the rail says "not syncing"; a tint
    // that survived a pause would leave the dot claiming the account is live.
    expect(accountColorOrToken({ color: "#3B6FB6", paused: true }, 0)).toEqual({
      tone: "off",
      color: null,
    });
  });

  it("refuses a malformed colour instead of putting it in a style attribute", () => {
    // A value read back from a server is input. Falling back to the ordinal is
    // what the dot looked like before colours existed, which is the safe answer.
    expect(accountColorOrToken({ color: "not a colour", paused: false }, 0)).toEqual({
      tone: "1",
      color: null,
    });
  });

  it("cannot index off the end of the tone list", () => {
    expect(accountColorOrToken({ color: null, paused: false }, -1).tone).toBe("2");
    expect(accountColorOrToken({ color: null, paused: false }, 1.7).tone).toBe("2");
  });
});

describe("ACCOUNT_COLOR_PRESETS", () => {
  it("offers only colours the server will accept", () => {
    for (const preset of ACCOUNT_COLOR_PRESETS) expect(isHexColor(preset.hex)).toBe(true);
  });

  it("offers no colour twice", () => {
    // The reason there are four rather than the six tokens the design named:
    // tokens.css sets --label-3 to --acct-2's exact hex and --label-4 to
    // --acct-1's. Two pairs of identical swatches read as a broken picker, so
    // this asserts the de-duplication rather than trusting the list.
    const hexes = ACCOUNT_COLOR_PRESETS.map((p) => p.hex.toUpperCase());
    expect(new Set(hexes).size).toBe(hexes.length);
  });

  it("names every swatch, so choosing one is not a colour-only decision", () => {
    for (const preset of ACCOUNT_COLOR_PRESETS) expect(preset.name).not.toBe("");
  });
});
