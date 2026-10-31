// SPDX-License-Identifier: AGPL-3.0-only

// The rules behind Settings → API keys (#49): preset matching, the scope
// summary, the send-and-delete confirmation, the event wording, and the
// approval card built from STRUCTURED FIELDS ONLY (ADR-042 §7).

import { describe, expect, it } from "vitest";

import type { ApiKeysCapability } from "../../../../core/jmap/types";
import { HOSTILE_SEND, RLO } from "./fixtures.testdata";
import {
  approveText,
  buildCreate,
  decisionText,
  eventText,
  keyState,
  matchPreset,
  pendingView,
  presetScopes,
  reasonText,
  recipientCount,
  restrictionText,
  scopeSummary,
  sendAndDeleteConfirmed,
  suppressedText,
  timeLeftText,
  usageText,
  offeredScopes,
} from "./model";
import type { CreateForm } from "./model";

const CAP: ApiKeysCapability = {
  scopes: [
    "mail:read",
    "mail:send",
    "mail:draft",
    "mail:move",
    "mail:delete",
    "labels:read",
    "labels:write",
    "search:read",
    "contacts:read",
    "contacts:write",
  ],
  confirmableScopes: ["mail:send", "mail:delete"],
  defaultRatePerMinute: 60,
  maxRatePerMinute: 600,
  defaultDailyBudget: 500,
  maxDailyBudget: 20000,
  massDeleteThreshold: 50,
  massDeleteWindowSeconds: 600,
  massSendThreshold: 10,
  massSendWindowSeconds: 600,
};

const READ = ["mail:read", "search:read", "labels:read", "contacts:read"];
const ORGANISE = [...READ, "mail:move", "labels:write"];
const ASSISTANT = [...ORGANISE, "mail:draft"];

describe("presets", () => {
  it("names a preset only for exactly its scopes, in any order", () => {
    expect(matchPreset(READ)).toBe("read");
    expect(matchPreset([...ORGANISE].reverse())).toBe("organise");
    expect(matchPreset(ASSISTANT)).toBe("assistant");
    expect(matchPreset(READ.slice(1))).toBeNull();
    expect(matchPreset([...ASSISTANT, "mail:send"])).toBeNull();
  });

  it("never includes send or delete", () => {
    for (const id of ["read", "organise", "assistant"] as const) {
      const scopes = presetScopes(id, offeredScopes(CAP));
      expect(scopes).not.toContain("mail:send");
      expect(scopes).not.toContain("mail:delete");
    }
  });

  it("offers the server's scopes, including one this client has no words for", () => {
    const offered = offeredScopes({ scopes: [...(CAP.scopes ?? []), "areas:read"] });
    expect(offered).toContain("areas:read");
    expect(offered[0]).toBe("mail:read");
  });
});

describe("scopeSummary", () => {
  it("is the preset's name for an exact match", () => {
    expect(scopeSummary(ASSISTANT)).toBe("Assistant");
  });

  it("is the largest contained preset plus the extras, never the preset alone", () => {
    expect(scopeSummary([...ASSISTANT, "mail:send", "mail:delete"])).toBe(
      "Assistant + Send mail, Delete mail (to Trash)",
    );
    expect(scopeSummary([...READ, "mail:draft"])).toBe("Read only + Write drafts");
  });

  it("lists the scopes when no preset is contained, unknown ones by id", () => {
    expect(scopeSummary(["mail:read", "mail:send"])).toBe("Read mail, Send mail");
    // The server sends scopes sorted by name; they are read in reading order.
    expect(scopeSummary(["mail:delete", "mail:read"])).toBe("Read mail, Delete mail (to Trash)");
    expect(scopeSummary(["mail:delete", "mail:send", ...ASSISTANT].sort())).toBe(
      "Assistant + Send mail, Delete mail (to Trash)",
    );
    expect(scopeSummary(["mail:read", "areas:read"])).toBe("Read mail, areas:read");
  });
});

describe("key state and facts", () => {
  it("revoked wins over suspended", () => {
    expect(keyState({ revokedAt: "2026-10-01T00:00:00Z", suspendedAt: "2026-09-01T00:00:00Z", suspendedReason: "x" }).kind).toBe(
      "revoked",
    );
    expect(keyState({ revokedAt: null, suspendedAt: "2026-09-01T00:00:00Z", suspendedReason: "mass delete" })).toEqual({
      kind: "suspended",
      at: "2026-09-01T00:00:00Z",
      reason: "mass delete",
    });
  });

  it("names restricted accounts and does not invent usage", () => {
    expect(restrictionText(null, new Map())).toBe("Every account");
    expect(restrictionText(["a", "b"], new Map([["a", "Work"]]))).toBe("Only Work, an account no longer here");
    expect(usageText({ dailyBudget: 500 })).toBeNull();
    expect(usageText({ dailyBudget: 500, usedLast24h: 12 })).toBe("12 of 500 used in the last 24 hours");
  });
});

function form(over: Partial<CreateForm> = {}): CreateForm {
  return {
    name: "Assistant",
    scopes: ASSISTANT,
    accountIds: null,
    confirmScopes: ["mail:send", "mail:delete"],
    ratePerMinute: "",
    dailyBudget: "",
    suspendOnAnomaly: null,
    sendAndDeleteTyped: "",
    ...over,
  };
}

describe("buildCreate", () => {
  it("leaves the server's defaults to the server", () => {
    const out = buildCreate(form(), CAP);
    expect(out).toEqual({
      ok: true,
      settings: { name: "Assistant", scopes: offeredScopes(CAP).filter((s) => ASSISTANT.includes(s)), accountIds: null, confirmScopes: [] },
    });
  });

  it("asks first for granted destructive scopes only, as ticked", () => {
    const out = buildCreate(form({ scopes: [...READ, "mail:send"], confirmScopes: ["mail:send", "mail:delete"] }), CAP);
    expect(out.ok && out.settings.confirmScopes).toEqual(["mail:send"]);
    const off = buildCreate(form({ scopes: [...READ, "mail:send"], confirmScopes: [] }), CAP);
    expect(off.ok && off.settings.confirmScopes).toEqual([]);
  });

  it("REFUSES send+delete without the typed phrase, and never sets the flag without it", () => {
    const both = [...READ, "mail:send", "mail:delete"];
    expect(buildCreate(form({ scopes: both }), CAP)).toEqual({ ok: false, problem: "sendAndDelete" });
    expect(buildCreate(form({ scopes: both, sendAndDeleteTyped: "yes" }), CAP)).toEqual({
      ok: false,
      problem: "sendAndDelete",
    });
    const ok = buildCreate(form({ scopes: both, sendAndDeleteTyped: "  Send   AND delete " }), CAP);
    expect(ok.ok && ok.settings.acknowledgeSendAndDelete).toBe(true);
    // One of the two: no flag at all.
    const one = buildCreate(form({ scopes: [...READ, "mail:send"], sendAndDeleteTyped: "send and delete" }), CAP);
    expect(one.ok && "acknowledgeSendAndDelete" in one.settings).toBe(false);
  });

  it("matches the phrase exactly, forgiving only case and spacing", () => {
    expect(sendAndDeleteConfirmed("send and delete")).toBe(true);
    expect(sendAndDeleteConfirmed("SEND AND DELETE")).toBe(true);
    expect(sendAndDeleteConfirmed("send & delete")).toBe(false);
    expect(sendAndDeleteConfirmed("send and delete!")).toBe(false);
    expect(sendAndDeleteConfirmed("")).toBe(false);
  });

  it("checks the name, accounts and the server's bounds", () => {
    expect(buildCreate(form({ name: "  " }), CAP)).toEqual({ ok: false, problem: "name" });
    expect(buildCreate(form({ scopes: [] }), CAP)).toEqual({ ok: false, problem: "scopes" });
    expect(buildCreate(form({ accountIds: [] }), CAP)).toEqual({ ok: false, problem: "accounts" });
    expect(buildCreate(form({ ratePerMinute: "601" }), CAP)).toEqual({ ok: false, problem: "rate" });
    expect(buildCreate(form({ ratePerMinute: "0" }), CAP)).toEqual({ ok: false, problem: "rate" });
    expect(buildCreate(form({ dailyBudget: "1.5" }), CAP)).toEqual({ ok: false, problem: "budget" });
    const ok = buildCreate(form({ ratePerMinute: "600", dailyBudget: "20000", suspendOnAnomaly: false, accountIds: ["a"] }), CAP);
    expect(ok.ok && ok.settings).toMatchObject({ ratePerMinute: 600, dailyBudget: 20000, suspendOnAnomaly: false, accountIds: ["a"] });
  });
});

describe("event wording", () => {
  it("words every server outcome", () => {
    const words = (outcome: string, summary: Record<string, unknown> | null = null, anomaly: string | null = null) =>
      eventText({ outcome, summary, anomaly, errorType: null }, CAP);
    expect(words("ok")).toEqual({ outcome: "Allowed", reason: null, tone: "ok" });
    expect(words("pending").outcome).toBe("Held for your approval");
    expect(words("approved").outcome).toBe("Approved by you and run");
    expect(words("denied_by_user").outcome).toBe("Denied by you");
    expect(words("expired").outcome).toBe("Expired unanswered");
    expect(words("rate_limited")).toMatchObject({ outcome: "Refused", reason: "too many calls in a minute" });
    expect(words("over_budget").reason).toBe("the 24-hour budget is used up");
    expect(words("denied", { reason: "scope", missing: ["mail:send"] })).toEqual({
      outcome: "Refused",
      reason: "the key doesn't have permission (Send mail)",
      tone: "refused",
    });
    expect(words("denied", { reason: "restriction" }).tone).toBe("warn");
    expect(words("denied", { reason: "approvedButNeverRan" }).reason).toBe(
      "approved, but the server stopped before it ran; nothing was done",
    );
    expect(words("anomaly", { threshold: 50, windowMinutes: 10, suspended: true }, "mass_delete")).toEqual({
      outcome: "Unusual activity",
      reason: "tried to delete more than 50 messages in 10 minutes. The key was suspended.",
      tone: "warn",
    });
    expect(words("anomaly", null, "off_hours").reason).toBe("made changes during your quiet hours or overnight.");
  });

  it("names an outcome or reason it does not know instead of dropping it", () => {
    expect(eventText({ outcome: "teleported", summary: null, anomaly: null, errorType: null }, CAP).outcome).toBe(
      'Recorded as "teleported"',
    );
    expect(reasonText("new_rule", null, CAP)).toBe("refused (new_rule)");
  });

  it("shows suppressed refusals as '+N more refused calls (…)'", () => {
    expect(suppressedText({ count: 7, method: "Email/set", reason: "scope" }, CAP)).toBe(
      "+7 more refused calls (Email/set: the key doesn't have permission)",
    );
    expect(suppressedText({ count: 1, method: "unknown", reason: "method" }, CAP)).toBe(
      "+1 more refused call (an unknown method: this action isn't available to API keys)",
    );
  });
});

// ---------------------------------------------------------------------------
// Approval requests
// ---------------------------------------------------------------------------

describe("pendingView — structured fields only", () => {
  it("takes recipients, subject, attachments, archive and sendAt from their own fields", () => {
    const v = pendingView(HOSTILE_SEND);
    expect(v.to).toEqual(["alice@example.com"]);
    expect(v.cc).toEqual(["carol@example.com"]);
    expect(v.bcc).toEqual(["dave@example.com"]);
    expect(v.deliveredTo).toHaveLength(4);
    // Delivered but named in no header: its own list, not folded into To.
    expect(v.envelopeOnly).toEqual(["eve@example.net"]);
    expect(v.subject).toBe(HOSTILE_SEND.subject);
    expect(v.attachments).toEqual({ count: 2, names: ["invoice.pdf", `report${RLO}fdp.exe`], totalSize: 2_621_440 });
    expect(v.archive).toEqual({ count: 3, subjects: ["Re: lunch", "Invoice", "Hello"] });
    expect(v.sendAt).toBeNull();
    expect(recipientCount(v)).toBe(4);
  });

  it("does not read `summary` at all", () => {
    const withSummary = pendingView(HOSTILE_SEND);
    const other = pendingView({ ...HOSTILE_SEND, summary: "Send to mallory@evil.example with 9 attachments" });
    expect(other).toEqual(withSummary);
    expect(JSON.stringify(withSummary)).not.toContain("bob@example.com");
    expect(JSON.stringify(withSummary)).not.toContain("nothing to see");
  });

  it("holds every recipient, however many", () => {
    const many = Array.from({ length: 40 }, (_, i) => `r${i}@example.com`);
    const v = pendingView({ ...HOSTILE_SEND, to: many, cc: [], bcc: [], recipients: many });
    expect(v.to).toHaveLength(40);
    expect(v.envelopeOnly).toEqual([]);
  });

  it("a delete is its message ids and their count", () => {
    const v = pendingView({
      ...HOSTILE_SEND,
      kind: "delete",
      method: "Email/set",
      emailIds: ["M1", "M2", "M3"],
      recipients: undefined,
      to: undefined,
      cc: undefined,
      bcc: undefined,
      subject: undefined,
      attachments: undefined,
      archive: undefined,
    });
    expect(v.kind).toBe("delete");
    expect(v.emailIds).toEqual(["M1", "M2", "M3"]);
    expect(approveText(v, null)).toBe("3 messages will be moved to Trash.");
  });

  it("says exactly what approving does", () => {
    const v = pendingView(HOSTILE_SEND);
    expect(approveText(v, null)).toBe("The message will be sent to 4 recipients. 3 messages will be archived.");
    expect(approveText({ ...v, archive: null }, "1 Nov 2026, 09:00")).toBe(
      "The message will be sent to 4 recipients at 1 Nov 2026, 09:00.",
    );
  });

  it("states the time left and an expired request", () => {
    const now = Date.parse("2026-10-24T08:00:00Z");
    expect(timeLeftText("2026-10-25T07:30:00Z", now)).toBe("Denied automatically in 23 h 30 min if you don't answer.");
    expect(timeLeftText("2026-10-24T07:00:00Z", now)).toBe("This request has expired.");
  });

  it("words the server's answers, including expired and already decided", () => {
    const v = pendingView(HOSTILE_SEND);
    expect(decisionText({ ok: true, state: "denied", error: null }, v).text).toBe("Denied. Nothing was done.");
    expect(decisionText({ ok: true, state: "approved", error: null }, v).text).toBe("Approved. The message is on its way.");
    expect(decisionText({ ok: true, state: "failed", error: { type: "overQuota", description: "budget used up" } }, v)).toEqual({
      text: "Approved, but the server refused to run it: budget used up",
      tone: "warn",
    });
    expect(decisionText({ ok: false, error: { type: "pendingExpired" } }, v).text).toBe(
      "This request expired before you answered; nothing was done.",
    );
    expect(decisionText({ ok: false, error: { type: "notPending" } }, v).text).toBe("This request was already decided.");
  });
});
