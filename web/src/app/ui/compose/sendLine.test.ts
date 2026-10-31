// SPDX-License-Identifier: AGPL-3.0-only

// The send line's four branches (#448), and the two server facts that decide
// `ownKey` (#492).
//
// THE ASSERTIONS THAT MATTER ARE THE NEGATIVE ONES. Two claims must be
// impossible for this function to make:
//
//   * the SEAL tone while any recipient has no key (the seal is the only cue a
//     user would act on, and acting on a false one means sending something in
//     the clear believing it sealed);
//   * the word "encrypt" anywhere, in any branch — encryption is v1.69.0, and
//     this line exists to describe SIGNING plus who a future encryption COULD
//     reach.
//
// Both are checked directly rather than implied by the positive cases.

import { describe, expect, it } from "vitest";

import { joinNames, sendLine, willNotSignReason, willSign } from "./sendLine";
import type { SendLineChip } from "./sendLine";

const held = (name: string): SendLineChip => ({ name, fact: "held" });
const none = (name: string): SendLineChip => ({ name, fact: "none" });
const unknown = (name: string): SendLineChip => ({ name, fact: "unknown" });

const OWN = "house@example.com";

// Identity fixtures for willSign/willNotSignReason — only the one property
// either function reads.
const BOUND = { signingKeyId: "01KEY" };
const UNBOUND_NULL = { signingKeyId: null };
const UNBOUND_UNDEFINED = {}; // signingKeyId absent entirely

describe("joinNames", () => {
  it("joins nothing, one, two and three", () => {
    expect(joinNames([])).toBe("");
    expect(joinNames(["A friend"])).toBe("A friend");
    expect(joinNames(["A friend", "Partner"])).toBe("A friend and Partner");
    expect(joinNames(["A", "B", "C"])).toBe("A, B and C");
  });
});

describe("willSign", () => {
  // Every combination of the two facts (#492): a table, not a handful of
  // representative cases, because this function is the ENTIRE rule and a gap
  // in the table is a gap in the rule.
  const cases: Array<{
    identity: { signingKeyId?: string | null } | undefined;
    signing: boolean | null;
    want: boolean | null;
    label: string;
  }> = [
    { identity: BOUND, signing: true, want: true, label: "bound key + server can sign" },
    { identity: BOUND, signing: false, want: false, label: "bound key + server cannot sign" },
    { identity: BOUND, signing: null, want: null, label: "bound key + signing unaskable" },
    { identity: UNBOUND_NULL, signing: true, want: false, label: "null key + server can sign" },
    { identity: UNBOUND_NULL, signing: false, want: false, label: "null key + server cannot sign" },
    { identity: UNBOUND_NULL, signing: null, want: null, label: "null key + signing unaskable" },
    {
      identity: UNBOUND_UNDEFINED,
      signing: true,
      want: false,
      label: "undefined key + server can sign",
    },
    {
      identity: UNBOUND_UNDEFINED,
      signing: false,
      want: false,
      label: "undefined key + server cannot sign",
    },
    {
      identity: UNBOUND_UNDEFINED,
      signing: null,
      want: null,
      label: "undefined key + signing unaskable",
    },
    { identity: undefined, signing: true, want: false, label: "no identity + server can sign" },
    { identity: undefined, signing: false, want: false, label: "no identity + server cannot sign" },
    { identity: undefined, signing: null, want: null, label: "no identity + signing unaskable" },
  ];

  for (const { identity, signing, want, label } of cases) {
    it(`${label} → ${String(want)}`, () => {
      expect(willSign(identity, signing)).toBe(want);
    });
  }

  it("is true ONLY when both a bound key and server signing hold", () => {
    // The control for the table above: if any single case regressed to
    // "true whenever either fact holds", this is the one that would catch it.
    expect(cases.filter((c) => c.want === true)).toHaveLength(1);
  });
});

describe("willNotSignReason", () => {
  it("says 'cannotSign' when the server cannot sign at all, regardless of the key", () => {
    expect(willNotSignReason(BOUND, false)).toBe("cannotSign");
    expect(willNotSignReason(UNBOUND_NULL, false)).toBe("cannotSign");
    expect(willNotSignReason(undefined, false)).toBe("cannotSign");
  });

  it("says 'noKeyBound' when the server could sign but no key is bound here", () => {
    expect(willNotSignReason(UNBOUND_NULL, true)).toBe("noKeyBound");
    expect(willNotSignReason(UNBOUND_UNDEFINED, true)).toBe("noKeyBound");
    expect(willNotSignReason(undefined, true)).toBe("noKeyBound");
  });
});

describe("sendLine", () => {
  it("seals when we have a key and every recipient's key is held", () => {
    const ownKey = willSign(BOUND, true);
    const line = sendLine(ownKey, [held("A friend"), held("Partner")], OWN);
    expect(line.tone).toBe("seal");
    expect(line.parts).toEqual([
      { key: "compose.send.willSign" },
      { key: "compose.send.keysHeldFor", params: { names: "A friend and Partner" } },
    ]);
  });

  // ⚠ THE CONTROL FOR THE SEAL. Without this, "seal" could be what every branch
  // returns and the assertion above would still pass.
  it("warns — never seals — when one recipient has no key, and names them", () => {
    const ownKey = willSign(BOUND, true);
    const line = sendLine(ownKey, [held("A friend"), none("Partner")], OWN);
    expect(line.tone).toBe("warn");
    expect(line.parts).toEqual([
      { key: "compose.send.willSign" },
      { key: "compose.send.noKeyFor", params: { names: "Partner" } },
      { key: "compose.send.plainToAll.other", params: { count: 2 } },
    ]);
  });

  it("treats a recipient we could not resolve as a recipient with no key in hand", () => {
    // "unknown" is a fact about our knowledge; "could this be encrypted" only
    // asks whether we HOLD a key, and we do not.
    const ownKey = willSign(BOUND, true);
    expect(sendLine(ownKey, [held("A friend"), unknown("stranger@example.org")], OWN).tone).toBe(
      "warn",
    );
  });

  it("names the sending address and says no key is bound, when a key is bound to no identity here", () => {
    const ownKey = willSign(UNBOUND_NULL, true);
    const reason = willNotSignReason(UNBOUND_NULL, true);
    const line = sendLine(ownKey, [held("A friend")], OWN, reason);
    expect(line.tone).toBe("plain");
    expect(line.parts).toEqual([
      { key: "compose.send.willNotSign.noKey", params: { address: OWN } },
    ]);
  });

  it("says the server itself cannot sign, and names no address, when signing is false", () => {
    const ownKey = willSign(BOUND, false);
    const reason = willNotSignReason(BOUND, false);
    const line = sendLine(ownKey, [held("A friend")], OWN, reason);
    expect(line.tone).toBe("plain");
    expect(line.parts).toEqual([{ key: "compose.send.willNotSign.cannotSign" }]);
  });

  it("does not let held recipient keys turn 'no own key' into a signature", () => {
    // The direction that matters: without a key of our own there is no signature
    // however many recipient keys are held.
    const ownKey = willSign(UNBOUND_NULL, true);
    expect(sendLine(ownKey, [held("A"), held("B")], OWN).tone).toBe("plain");
  });

  it("says the status is unknown when the server cannot be asked", () => {
    const ownKey = willSign(BOUND, null);
    const line = sendLine(ownKey, [held("A friend")], OWN);
    expect(line.tone).toBe("plain");
    expect(line.parts).toEqual([{ key: "compose.send.unknownServer" }]);
  });

  it("distinguishes 'could not ask' from 'you have no key'", () => {
    // #176: a null and a false are different facts, and the two sentences differ.
    const unaskable = willSign(BOUND, null);
    const noKey = willSign(UNBOUND_NULL, true);
    expect(sendLine(unaskable, [], OWN).parts).not.toEqual(sendLine(noKey, [], OWN).parts);
  });

  it("does not claim keys are held for nobody when there are no recipients yet", () => {
    const ownKey = willSign(BOUND, true);
    const line = sendLine(ownKey, [], OWN);
    expect(line.tone).toBe("plain");
    expect(line.parts).toEqual([{ key: "compose.send.willSign" }]);
  });

  it("uses the singular plain-text clause for a lone recipient", () => {
    const ownKey = willSign(BOUND, true);
    const line = sendLine(ownKey, [none("Partner")], OWN);
    expect(line.parts[2]).toEqual({ key: "compose.send.plainToAll.one", params: { count: 1 } });
  });

  // ⚠ NO BRANCH MAY PROMISE ENCRYPTION (v1.69.0). The keys are the whole
  // vocabulary this line can print, so checking them checks the sentences.
  it("names no key that could render an encryption promise", () => {
    const cases = [
      sendLine(willSign(BOUND, true), [held("A")], OWN),
      sendLine(willSign(BOUND, true), [none("A")], OWN),
      sendLine(willSign(BOUND, true), [], OWN),
      sendLine(willSign(UNBOUND_NULL, true), [], OWN, willNotSignReason(UNBOUND_NULL, true)),
      sendLine(willSign(BOUND, false), [], OWN, willNotSignReason(BOUND, false)),
      sendLine(willSign(BOUND, null), [], OWN),
    ];
    for (const c of cases) {
      for (const p of c.parts) {
        expect(p.key.toLowerCase()).not.toContain("encrypt");
      }
    }
  });
});
