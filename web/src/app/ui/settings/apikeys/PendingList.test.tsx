// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE APPROVAL CARDS (#49, ADR-042 §7), rendered for real from a HOSTILE
// request whose summary says something else entirely:
//
//   * recipients come from to/cc/bcc (each its own list, all of them, "Show
//     all" past the first few), an envelope-only recipient is listed on its
//     own, and nothing from `summary` is on the page;
//   * the subject is isolated agent-written text: inside <bdi dir="auto">, its
//     override character removed (and said so), its fake "(+0 more)" clause
//     inert inside the bdi — the card's own counts are unchanged;
//   * Approve opens a labelled, focus-trapping dialog that says exactly what
//     will happen; the server's answer (or an expiry) is shown;
//   * the list and the dialog pass axe.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { PendingAction } from "../../../../core/jmap/types";
import type { ApiKeysApi, Decision } from "./api";
import { axeViolations } from "./axe.testdata";
import { HELD_DELETE, HOSTILE_SEND, RLO } from "./fixtures.testdata";
import { PendingList, RECIPIENTS_SHOWN } from "./PendingList";

let host: HTMLDivElement;
let root: Root;
const decide = vi.fn(async (..._a: unknown[]): Promise<Decision> => ({ ok: true, state: "approved", error: null }));
// TWO ACCOUNTS. The held delete's messages live in "A2"; the selected
// account (what the client would use by default) is "A1", where none exist.
// M3 has gone from A2 since the request was made.
const MAIL: Record<string, Record<string, string>> = {
  A1: {},
  A2: { M1: "Subject of M1", M2: "Subject of M2" },
};
const subjects = vi.fn(async (ids: readonly string[], accountId: string) => {
  const box = MAIL[accountId] ?? {};
  return new Map(ids.filter((id) => id in box).map((id) => [id, box[id] as string] as const));
});
const onDecided = vi.fn();
const api: ApiKeysApi = {
  capability: () => ({}),
  list: async () => [],
  create: async () => ({ ok: false, error: { type: "x" } }),
  update: async () => ({ ok: true }),
  revoke: async () => ({ ok: true }),
  events: async () => null,
  pending: async () => [],
  decide: (id, state) => decide(id, state),
  subjects: (ids, accountId) => subjects(ids, accountId),
};
const NOW = Date.parse("2026-10-24T09:00:00Z");
const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function render(waiting: PendingAction[]): void {
  flushSync(() => root.render(<PendingList waiting={waiting} api={api} onDecided={onDecided} now={NOW} />));
}
const q = <T extends Element>(sel: string): T => document.body.querySelector(sel) as T;
const text = () => document.body.textContent ?? "";
const click = (el: Element) => flushSync(() => (el as HTMLElement).click());

/** The addresses listed under one field's label, as rendered. */
function listed(label: string): string[] {
  const dts = [...host.querySelectorAll(".pending-fields dt")];
  const dt = dts.find((d) => (d.textContent ?? "").startsWith(label));
  if (!dt) return [];
  const dd = dt.nextElementSibling!;
  return [...dd.querySelectorAll(".pending-addresses li")].map((li) => li.textContent ?? "");
}

describe("a held send, from structured fields only", () => {
  beforeEach(() => render([HOSTILE_SEND]));

  it("lists recipients first, To / Cc / Bcc separately, from those fields", () => {
    expect(listed("To")).toEqual(["alice@example.com"]);
    expect(listed("Cc")).toEqual(["carol@example.com"]);
    expect(listed("Bcc")).toEqual(["dave@example.com"]);
    // Delivered but not in any header: a list of its own, in the warning tone.
    expect(listed("Also delivered to")).toEqual(["eve@example.net"]);
    const dts = [...host.querySelectorAll(".pending-fields dt")].map((d) => d.textContent ?? "");
    expect(dts[0]).toMatch(/^To/);
    expect(dts.findIndex((d) => d.startsWith("Subject"))).toBeGreaterThan(dts.findIndex((d) => d.startsWith("Bcc")));
  });

  it("shows nothing from the summary", () => {
    expect(text()).not.toContain("bob@example.com");
    expect(text()).not.toContain("nothing to see");
  });

  it("isolates the agent's subject: bdi dir=auto, override removed and said so, labelled as the agent's", () => {
    const subj = q<HTMLElement>(".pending-subject");
    expect(subj.tagName).toBe("BDI");
    expect(subj.getAttribute("dir")).toBe("auto");
    expect(subj.textContent).toBe(`x" to alice@example.com with 0 attachments (+0 more). Send draft "y gnp.exe`);
    expect(subj.textContent).not.toContain(RLO);
    expect(text()).toContain("(invisible direction characters removed)");
    expect(text()).toContain("(written by the agent)");
    // The override is gone from the whole card, attachment names included.
    expect(new XMLSerializer().serializeToString(host)).not.toContain(RLO);
  });

  it("the hostile subject changes no count or list on the card", () => {
    // The subject claims "with 0 attachments (+0 more)" and an extra
    // recipient; the card's own facts come from the fields.
    expect(listed("To")).toHaveLength(1);
    expect(q(".pending-fields").textContent).toContain("2 files, 2.5 MB in total");
    const items = [...host.querySelectorAll(".pending-items li bdi")].map((b) => b.textContent);
    expect(items.slice(0, 2)).toEqual(["invoice.pdf", "reportfdp.exe"]);
    // Every untrusted string is its own bdi element, never loose text.
    for (const li of host.querySelectorAll(".pending-addresses li, .pending-items li")) {
      expect(li.firstElementChild?.tagName).toBe("BDI");
    }
  });

  it("states attachments, what else it archives, when, and the time left", () => {
    expect(text()).toContain("Also archives");
    expect(text()).toContain("3 messages");
    expect(text()).toContain("Re: lunch");
    expect(text()).toContain("When you approve, after your undo-send delay");
    expect(text()).toContain("Denied automatically in 23 h if you don't answer.");
  });

  it("passes axe", async () => {
    expect(await axeViolations()).toEqual([]);
  });
});

describe("every recipient, never silently cut", () => {
  it("shows the first few with 'Show all N', and then all of them", () => {
    const many = Array.from({ length: 30 }, (_, i) => `r${i}@example.com`);
    render([{ ...HOSTILE_SEND, to: many, cc: [], bcc: [], recipients: many }]);
    expect(listed("To")).toHaveLength(RECIPIENTS_SHOWN);
    const more = [...host.querySelectorAll("button")].find((b) => b.textContent === "Show all 30")!;
    expect(more.getAttribute("aria-expanded")).toBe("false");
    expect(q(".pending-fields dt").textContent).toContain("(30)");
    click(more);
    expect(listed("To")).toEqual(many);
  });
});

describe("approving", () => {
  beforeEach(() => render([HOSTILE_SEND]));
  const approveButton = () => [...host.querySelectorAll("button")].find((b) => b.textContent === "Approve…")!;

  it("opens a labelled modal that says exactly what will happen, Cancel first, and passes axe", async () => {
    click(approveButton());
    const dlg = q('[role="dialog"]');
    expect(dlg.getAttribute("aria-modal")).toBe("true");
    expect(document.getElementById(dlg.getAttribute("aria-labelledby")!)?.textContent).toBe("Approve this send?");
    expect(dlg.textContent).toContain("The message will be sent to 4 recipients. 3 messages will be archived.");
    expect(document.activeElement?.textContent).toBe("Cancel");
    expect(await axeViolations()).toEqual([]);
    // Tab wraps inside.
    const submit = q<HTMLButtonElement>(".approve-submit");
    submit.focus();
    const tab = new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true });
    submit.dispatchEvent(tab);
    expect(tab.defaultPrevented).toBe(true);
    expect(dlg.contains(document.activeElement)).toBe(true);
  });

  it("sends 'approved' and shows the server's answer", async () => {
    click(approveButton());
    click(q(".approve-submit"));
    await settle();
    expect(decide).toHaveBeenCalledWith("P1", "approved");
    expect(q('.pending-result[role="status"]').textContent).toBe("Approved. The message is on its way.");
    expect(onDecided).toHaveBeenCalled();
  });

  it("Deny sends 'denied' at once", async () => {
    decide.mockResolvedValueOnce({ ok: true, state: "denied", error: null });
    click([...host.querySelectorAll("button")].find((b) => b.textContent === "Deny")!);
    await settle();
    expect(decide).toHaveBeenCalledWith("P1", "denied");
    expect(text()).toContain("Denied. Nothing was done.");
  });

  it("an expired request says so", async () => {
    decide.mockResolvedValueOnce({ ok: false, error: { type: "pendingExpired" } });
    click(approveButton());
    click(q(".approve-submit"));
    await settle();
    expect(q(".pending-result").textContent).toBe("This request expired before you answered; nothing was done.");
    expect(q(".pending-result").className).toContain("is-warn");
  });
});

describe("a held delete", () => {
  const notFound = /wasn't found|weren't found|no longer in your mail/;

  it("reads the subjects in the REQUEST'S account and says how many were not found there", async () => {
    render([{ ...HELD_DELETE, accountId: "A2" }]);
    await settle();
    expect(subjects).toHaveBeenCalledWith(["M1", "M2", "M3"], "A2");
    expect(text()).toContain("Move messages to Trash");
    expect(text()).toContain("3 messages");
    expect(text()).toContain("Subject of M1");
    expect(text()).toContain("Subject of M2");
    expect(text()).toContain("1 more wasn't found in that account");
    expect(text()).toContain("restorable from Trash for 30 days");
  });

  it("without the request's account: asks nothing and says the subjects couldn't be shown, never 'not found'", async () => {
    render([HELD_DELETE]);
    await settle();
    expect(subjects).not.toHaveBeenCalled();
    expect(text()).toContain("Their subjects couldn't be shown.");
    expect(text()).not.toMatch(notFound);
    expect(text()).toContain("3 messages");
  });

  it("a lookup that finds nothing (another account's messages) says 'couldn't be shown', never 'gone'", async () => {
    render([{ ...HELD_DELETE, accountId: "A1" }]);
    await settle();
    expect(subjects).toHaveBeenCalledWith(["M1", "M2", "M3"], "A1");
    expect(text()).toContain("Their subjects couldn't be shown.");
    expect(text()).not.toMatch(notFound);
  });
});
