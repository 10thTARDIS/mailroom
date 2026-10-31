// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { mailroomVerified, senderAuthNotice } from "./senderAuth";
import type { AuthResults } from "./jmap/types";

// #55 increment 1. These cases are the populations increment 0 actually MEASURED
// on a live mailbox, not invented shapes — the counts in the names
// are what the corpus report returned, so a case that stops matching reality is
// visible rather than quietly stale.

const clean: AuthResults = { present: true, spf: ["pass"], dkim: ["pass"], dmarc: ["pass"] };

function results(over: Partial<AuthResults>): AuthResults {
  return { ...clean, ...over };
}

describe("senderAuthNotice", () => {
  it("says nothing about mail that passed — the nearly-all-pass case", () => {
    expect(senderAuthNotice(clean)).toBeNull();
  });

  it("says nothing when the property is null — our own drafts and Sent copies (7)", () => {
    // Silence here is the CORRECT answer, not a missing one. This is the case a
    // badge would get actively wrong by calling the user's own outbox unverified.
    expect(senderAuthNotice(null)).toBeNull();
    expect(senderAuthNotice(undefined)).toBeNull();
  });

  it("distinguishes 'we wrote it' from 'it arrived with no authentication' (5)", () => {
    // Same visible input to a careless implementation — no method results at all —
    // and opposite meanings. The absent-property case is silence; this one is a
    // finding.
    const absent = results({ present: false, spf: null, dkim: null, dmarc: null });
    expect(senderAuthNotice(absent)).toEqual({ severity: "unauthenticated", methods: [] });
  });

  it("reports a method that failed, even when the overall verdict is clean (3)", () => {
    // The three motivating messages: DKIM fails while SPF passes, so the server's
    // stored auth_fail boolean is FALSE and would show the reader nothing at all.
    const notice = senderAuthNotice(results({ dkim: ["fail"] }));
    expect(notice).toEqual({ severity: "failed", methods: ["dkim"] });
  });

  it("does NOT report a method that both failed and passed — multi-signature DKIM", () => {
    // Two DKIM signatures, one valid and one not, is ordinary mail. Accusing it
    // would libel a whole class of legitimate senders. There is no such message in
    // the corpus today, which is exactly why this is pinned rather than assumed.
    expect(senderAuthNotice(results({ dkim: ["fail", "pass"] }))).toBeNull();
  });

  it("reports an evaluation error separately from a failure (6 dmarc permerror)", () => {
    // "Could not be checked" is a weaker claim than "was checked and failed", and
    // collapsing them would overstate what is known about six messages.
    expect(senderAuthNotice(results({ dmarc: ["permerror"] }))).toEqual({
      severity: "unverifiable",
      methods: ["dmarc"],
    });
    expect(senderAuthNotice(results({ spf: ["temperror"] }))).toEqual({
      severity: "unverifiable",
      methods: ["spf"],
    });
  });

  it("stays silent on `none` — 19 messages, and not evidence about the message", () => {
    // `none` says the SENDING DOMAIN publishes no such policy. It is not a signal
    // that this message is forged, the reader cannot act on it, and reporting it
    // would more than double the notice rate for nothing.
    expect(senderAuthNotice(results({ spf: ["none"] }))).toBeNull();
    expect(senderAuthNotice(results({ dkim: ["none"], dmarc: ["none"] }))).toBeNull();
    expect(senderAuthNotice(results({ spf: ["neutral"], dkim: ["softfail"] }))).toBeNull();
  });

  it("prefers a real failure over an error when a message has both", () => {
    const notice = senderAuthNotice(results({ dkim: ["fail"], dmarc: ["permerror"] }));
    expect(notice?.severity).toBe("failed");
  });

  it("names every failing method, in the canonical order", () => {
    const notice = senderAuthNotice(
      results({ spf: ["fail"], dkim: ["fail"], dmarc: ["fail"] }),
    );
    expect(notice).toEqual({ severity: "failed", methods: ["spf", "dkim", "dmarc"] });
  });
});

// #776 / ADR-036: Mailroom's own DKIM verdict.
describe("mailroomVerified", () => {
  const local: AuthResults = {
    present: true,
    spf: null,
    dkim: ["pass"],
    dmarc: null,
    source: "mailroom",
    dkimDomain: "example.com",
  };

  it("names the signing domain for a local aligned pass", () => {
    expect(mailroomVerified(local)).toEqual({ domain: "example.com" });
  });

  it("is not a notice: a local pass raises no caution", () => {
    expect(senderAuthNotice(local)).toBeNull();
  });

  it("is silent for a provider verdict, however clean (no source marker)", () => {
    // CONTROL: same methods, no source. An old server, or a provider verdict, must
    // never be dressed as Mailroom's.
    expect(mailroomVerified({ ...local, source: undefined, dkimDomain: undefined })).toBeNull();
    expect(mailroomVerified({ present: true, spf: ["pass"], dkim: ["pass"], dmarc: ["pass"] })).toBeNull();
  });

  it("requires a passing dkim and a domain", () => {
    expect(mailroomVerified({ ...local, dkim: ["fail"] })).toBeNull();
    expect(mailroomVerified({ ...local, dkim: null })).toBeNull();
    expect(mailroomVerified({ ...local, dkimDomain: "" })).toBeNull();
    expect(mailroomVerified({ ...local, present: false })).toBeNull();
    expect(mailroomVerified(null)).toBeNull();
    expect(mailroomVerified(undefined)).toBeNull();
  });
});
