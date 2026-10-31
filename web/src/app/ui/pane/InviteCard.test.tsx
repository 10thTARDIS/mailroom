// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE INVITATION CARD (#725), rendered for real: what it shows, what its
// buttons are called and say about the recorded answer, what a click sends,
// and that a cancelled card, a reply card and an unanswerable card offer no
// buttons at all.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { CalendarInvite, Email } from "../../../core/jmap/types";

const replyToInvite = vi.fn(async () => ({ submissionId: "S1", sendAt: new Date(Date.now() + 10_000).toISOString() }));
const withdrawInviteReply = vi.fn(async () => true);
const offerUndo = vi.fn();
const pushToast = vi.fn();
let submissions: Record<string, { mailroomState?: string }> = {};

vi.mock("../../state", () => ({
  useApp: () => ({ actions: { replyToInvite, withdrawInviteReply }, offerUndo, pushToast }),
  useMailState: () => ({ submissions }),
}));

import { InviteCard } from "./InviteCard";

const t = (s: string) => ({ kind: "utc" as const, utc: s, date: null, local: null, timeZone: null });

function invite(over: Partial<CalendarInvite> = {}): CalendarInvite {
  return {
    problem: null,
    method: "REQUEST",
    uid: "u1@example.com",
    sequence: 0,
    status: "CONFIRMED",
    summary: "Weekly sync",
    location: "Room 2, Building B",
    start: t("2026-10-20T15:00:00Z"),
    end: t("2026-10-20T15:30:00Z"),
    organizer: { name: "Alice Example", email: "alice@example.com" },
    attendees: [
      { name: "Alice Example", email: "alice@example.com", partstat: "ACCEPTED", role: "CHAIR" },
      { name: null, email: "bob@example.net", partstat: "NEEDS-ACTION", role: "REQ-PARTICIPANT" },
    ],
    attendeesOmitted: 0,
    recurrence: null,
    recurrenceId: null,
    cancelled: false,
    cancelledByEmailId: null,
    newerEmailId: null,
    updatesEmailId: null,
    response: null,
    canReply: true,
    cannotReplyReason: null,
    ...over,
  };
}

const email = (inv: CalendarInvite | null): Email =>
  ({ id: "M1", threadId: "T1", deliveredTo: "bob@example.net", calendarInvite: inv }) as unknown as Email;

let host: HTMLDivElement;
let root: Root;
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  submissions = {};
  vi.clearAllMocks();
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function render(inv: CalendarInvite | null): void {
  flushSync(() => root.render(<InviteCard email={email(inv)} />));
}

const region = () => host.querySelector("section.invite-card");
const group = () => host.querySelector('[role="group"]');
const buttons = () => [...host.querySelectorAll<HTMLButtonElement>('[role="group"] button')];
const pressed = () => buttons().map((b) => `${b.textContent}:${b.getAttribute("aria-pressed")}`);

describe("an invitation", () => {
  it("is a region named for what it is, with the facts and the three answers", () => {
    render(invite());
    expect(region()?.getAttribute("aria-label")).toBe("Calendar invitation: Weekly sync");
    expect(host.querySelector("h2")?.textContent).toBe("Weekly sync");
    const facts = host.querySelector("dl")?.textContent ?? "";
    expect(facts).toContain("Room 2, Building B");
    expect(facts).toContain("Alice Example <alice@example.com>");
    expect(facts).toContain("bob@example.net — no answer yet");
    expect(group()?.getAttribute("aria-label")).toBe("Respond to this invitation");
    expect(pressed()).toEqual(["Accept:false", "Maybe:false", "Decline:false"]);
    expect(host.textContent).toContain("Your answer is sent to Alice Example <alice@example.com> from bob@example.net.");
  });

  it("Accept sends 'accepted' for THIS message and offers the Undo Send undo", async () => {
    render(invite());
    flushSync(() => buttons()[0]!.click());
    await settle();
    expect(replyToInvite).toHaveBeenCalledWith("T1", "M1", "accepted");
    expect(offerUndo).toHaveBeenCalledTimes(1);
    const [label, inverse, ttl] = offerUndo.mock.calls[0] as [string, () => Promise<boolean>, number];
    expect(label).toBe("Accepted — your answer is on its way to Alice Example <alice@example.com>.");
    expect(ttl).toBeGreaterThan(0);
    await inverse();
    expect(withdrawInviteReply).toHaveBeenCalledWith("T1", "M1", "S1");
  });

  it("Maybe and Decline send their own words", async () => {
    render(invite());
    flushSync(() => buttons()[1]!.click());
    await settle();
    flushSync(() => buttons()[2]!.click());
    await settle();
    expect(replyToInvite.mock.calls.map((c) => (c as unknown[])[2])).toEqual(["tentative", "declined"]);
  });

  it("a failed answer offers no undo and leaves the buttons usable", async () => {
    replyToInvite.mockResolvedValueOnce(null as never);
    render(invite());
    flushSync(() => buttons()[0]!.click());
    await settle();
    expect(offerUndo).not.toHaveBeenCalled();
    expect(buttons().every((b) => !b.disabled)).toBe(true);
  });

  it("the recorded answer presses its button and is said in words", () => {
    render(invite({ response: { partstat: "ACCEPTED", sequence: 0, respondedAt: "x", submissionId: "S1", emailId: "M1" } }));
    expect(pressed()).toEqual(["Accept:true", "Maybe:false", "Decline:false"]);
    expect(host.textContent).toContain("You accepted.");
  });

  it("an answer to an EARLIER version presses nothing, and says so", () => {
    render(
      invite({
        sequence: 2,
        updatesEmailId: "M0",
        response: { partstat: "DECLINED", sequence: 1, respondedAt: "x", submissionId: "S1", emailId: "M0" },
      }),
    );
    expect(pressed()).toEqual(["Accept:false", "Maybe:false", "Decline:false"]);
    expect(host.textContent).toContain("You declined. That was for an earlier version.");
    expect(host.textContent).toContain("This updates an earlier invitation.");
  });

  it("a reply whose send failed says it was not sent", () => {
    submissions = { S1: { mailroomState: "failed" } };
    render(invite({ response: { partstat: "ACCEPTED", sequence: 0, respondedAt: "x", submissionId: "S1", emailId: "M1" } }));
    expect(host.textContent).toContain("Your answer has not been sent");
  });

  it("an older version with a newer one says the details may be out of date", () => {
    render(invite({ newerEmailId: "M2" }));
    expect(host.textContent).toContain("There is a newer version of this invitation");
  });

  it("an invitation that cannot be answered says why, and has no buttons", () => {
    render(invite({ canReply: false, cannotReplyReason: "notAddressedToYou" }));
    expect(group()).toBeNull();
    expect(host.textContent).toContain("not sent to an address you can send from");
  });

  it("strips a direction override from the title, the region's name and the people", () => {
    render(invite({ summary: "Pay‮roll", organizer: { name: "Al‮ice", email: "alice@example.com" } }));
    expect(region()?.getAttribute("aria-label")).toBe("Calendar invitation: Payroll");
    expect(host.textContent).not.toContain("‮");
  });

  it("lists a bounded guest list as bounded", () => {
    render(invite({ attendeesOmitted: 7 }));
    expect(host.textContent).toContain("and 7 more");
  });
});

describe("a cancellation", () => {
  it("is a cancelled card with no buttons", () => {
    render(invite({ method: "CANCEL", cancelled: true, canReply: false, cannotReplyReason: "notAnInvitation" }));
    expect(region()?.getAttribute("aria-label")).toBe("Cancelled event: Weekly sync");
    expect(host.textContent).toContain("This event has been cancelled.");
    expect(group()).toBeNull();
  });

  it("never offers buttons, even if a server claimed it could be answered", () => {
    // The server already says canReply:false for a cancelled event; this is
    // the client's own rule, so it is tested on its own rather than through it.
    render(invite({ cancelled: true, canReply: true }));
    expect(group()).toBeNull();
  });

  it("an invitation cancelled by a later message reads the same", () => {
    render(invite({ cancelled: true, cancelledByEmailId: "M9", canReply: false, cannotReplyReason: "cancelled" }));
    expect(host.querySelector("section")?.className).toContain("invite-cancelled");
    expect(group()).toBeNull();
    // No "cannot answer" sentence: the state line already said why.
    expect(host.querySelectorAll("p.field-hint").length).toBe(0);
  });
});

describe("verification (#725 review)", () => {
  const note = "could not be verified as coming from the organiser";
  it("an unverified cancellation says it could not be verified", () => {
    render(invite({ method: "CANCEL", cancelled: true, verified: false, canReply: false, cannotReplyReason: "notAnInvitation" }));
    expect(host.textContent).toContain(note);
  });
  it("an unverified update says so too", () => {
    render(invite({ sequence: 2, verified: false }));
    expect(host.textContent).toContain(note);
  });
  it("a verified cancellation, and a first invitation, say nothing of the kind", () => {
    render(invite({ method: "CANCEL", cancelled: true, verified: true, canReply: false }));
    expect(host.textContent).not.toContain(note);
    render(invite({ sequence: 0, verified: false }));
    expect(host.textContent).not.toContain(note);
  });
  it("an unverified reply names the guest, not the organiser", () => {
    render(invite({ method: "REPLY", verified: false, canReply: false }));
    expect(host.textContent).toContain("could not be verified as coming from the guest");
  });
});

describe("a reply", () => {
  it("says who answered and how, and offers nothing to click", () => {
    render(
      invite({
        method: "REPLY",
        summary: "Design review",
        attendees: [{ name: "Chen Wei", email: "chen@example.org", partstat: "DECLINED", role: "REQ-PARTICIPANT" }],
        canReply: false,
        cannotReplyReason: "notAnInvitation",
      }),
    );
    expect(region()?.getAttribute("aria-label")).toBe("Reply to an invitation: Design review");
    expect(host.textContent).toContain("Chen Wei <chen@example.org> declined.");
    expect(group()).toBeNull();
  });
});

describe("no card", () => {
  it("renders nothing for a message with no calendar", () => {
    render(null);
    expect(host.childNodes.length).toBe(0);
  });
  it("says a calendar was there but too large, rather than nothing", () => {
    render({ problem: "tooLarge" });
    expect(host.textContent).toContain("too large to read safely");
  });
});
