// SPDX-License-Identifier: AGPL-3.0-only

// The sender card's Learned sorting sentences (#167): one per verdict and
// reason, chosen from fake LearnedSender/get answers.

import { describe, expect, it } from "vitest";

import type { LearnedSender, LearnedSenderEvidence } from "../../../core/jmap/types";
import { forgetScope, learnedSenderSentence, showTrust } from "./learnedSender";

const names: Record<string, string> = { AU: "Updates", AN: "News" };
const name = (id: string): string | null => names[id] ?? null;

function ev(over: Partial<LearnedSenderEvidence>): LearnedSenderEvidence {
  return { areaId: null, filings: 0, verdict: "none", acting: false, reason: "none", ...over };
}
type Over = Partial<Omit<LearnedSender, "address" | "domain">> & {
  address?: Partial<LearnedSenderEvidence>;
  domain?: Partial<LearnedSenderEvidence>;
};
function sender(over: Over): LearnedSender {
  const { address, domain, ...rest } = over;
  return {
    sender: "alice@example.com",
    address: ev(address ?? {}),
    domain: { domain: "example.com", ...ev(domain ?? {}) },
    decidedBy: "none",
    trusted: false,
    forgottenAt: null,
    ...rest,
  };
}

describe("learnedSenderSentence", () => {
  it("says where it files, and how many moves taught it", () => {
    const s = sender({ decidedBy: "address", address: { areaId: "AU", filings: 4, verdict: "confident", acting: true, reason: null } });
    expect(learnedSenderSentence(s, name)).toBe("Files new mail from this sender to Updates (learned from 4 of your moves).");
  });
  it("names the domain when the domain decided", () => {
    const s = sender({ decidedBy: "domain", domain: { areaId: "AN", filings: 6, verdict: "confident", acting: true, reason: null } });
    expect(learnedSenderSentence(s, name)).toBe("Files new mail from this sender's domain to News (learned from 6 of your moves).");
  });
  it("thin: once, and several times", () => {
    expect(learnedSenderSentence(sender({ address: { filings: 1, verdict: "thin", reason: "thin" } }), name)).toBe(
      "Not sure yet: you've filed this sender once.",
    );
    expect(learnedSenderSentence(sender({ address: { filings: 2, verdict: "thin", reason: "thin" } }), name)).toBe(
      "Not sure yet: you've filed this sender 2 times.",
    );
  });
  it.each([
    ["none", "Nothing learned about this sender yet."],
    ["mixed", "You've filed this sender into more than one area, so it waits for you."],
    ["contradicted", "You moved mail from this sender out of where it was placed, so it waits for you."],
    ["unauthenticated", "Mail from this sender can't be verified, so it isn't sorted automatically."],
    ["blocked", "This domain is shared by many people, so its mail isn't sorted automatically."],
    ["off", "Learned sorting is turned off."],
  ])("reason %s", (reason, sentence) => {
    const s = sender({ address: { filings: reason === "none" ? 0 : 3, verdict: reason, reason } });
    expect(learnedSenderSentence(s, name)).toBe(sentence);
  });
  it("gated names the area when it is known", () => {
    const s = sender({ address: { areaId: "AU", filings: 5, verdict: "confident", reason: "gated" } });
    expect(learnedSenderSentence(s, name)).toBe("Learned sorting is paused for Updates after corrections, so this sender waits for you.");
  });
  it("an unknown reason still gets a plain sentence", () => {
    const s = sender({ address: { filings: 2, verdict: "confident", reason: "something-new" } });
    expect(learnedSenderSentence(s, name)).toBe("Not sorted automatically yet.");
  });
});

describe("forgetScope and showTrust", () => {
  it("forgets the domain when the domain decided, the address otherwise, and nothing when empty", () => {
    expect(forgetScope(sender({ decidedBy: "domain", domain: { filings: 3 } }))).toBe("domain");
    expect(forgetScope(sender({ decidedBy: "address", address: { filings: 3 } }))).toBe("address");
    expect(forgetScope(sender({ address: { filings: 0 }, domain: { filings: 0 } }))).toBeNull();
  });
  it("offers trust only for unverified senders or when already on", () => {
    expect(showTrust(sender({ address: { reason: "unauthenticated" } }))).toBe(true);
    expect(showTrust(sender({ trusted: true }))).toBe(true);
    expect(showTrust(sender({ address: { reason: "thin" } }))).toBe(false);
    expect(showTrust(sender({ address: { acting: true, reason: null } }))).toBe(false);
  });
});

describe("server additions (beta 2)", () => {
  it("address_decides on the domain never produces a not-sorted sentence; the address speaks", () => {
    const s = sender({
      decidedBy: "address",
      address: { areaId: "AU", filings: 4, verdict: "confident", acting: true, reason: null },
      domain: { areaId: "AN", filings: 9, verdict: "confident", acting: false, reason: "address_decides" },
    });
    expect(learnedSenderSentence(s, name)).toBe("Files new mail from this sender to Updates (learned from 4 of your moves).");
    // Even if decidedBy were absent, the marker alone picks the address.
    expect(learnedSenderSentence({ ...s, decidedBy: "none" }, name)).toBe(
      "Files new mail from this sender to Updates (learned from 4 of your moves).",
    );
  });
  it("a forgotten sender with no filings since says so instead of 'Nothing learned'", () => {
    const s = sender({ forgottenAt: "2026-11-20T10:00:00Z" });
    expect(learnedSenderSentence(s, name)).toMatch(/^You asked Mailroom to forget this sender on .*2026/);
    expect(learnedSenderSentence(s, name)).not.toContain("Nothing learned");
  });
  it("filings since the forget fall back to the normal sentence", () => {
    const s = sender({ forgottenAt: "2026-11-20T10:00:00Z", address: { filings: 1, verdict: "thin", reason: "thin" } });
    expect(learnedSenderSentence(s, name)).toBe("Not sure yet: you've filed this sender once.");
  });
});
