// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The clean-up view (#53). Real MailStore; the server and the writes are
// stand-ins that record what they were asked. Pinned: the ranking request
// follows the controls; the bound is stated ("Showing N of M") and "Show more"
// asks for more; each grouping's actions search for exactly that row's mail
// (`from:`, `from:@`, `list:`) and move nothing before the confirm; block on a
// list row blocks every listed address in ONE Contact/set and moves nothing
// unless asked; unsubscribe reads the message with its body (where the
// unsubscribe routes are); a key the search syntax cannot carry gets no
// actions at all.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Email, Id, Mailbox, SenderStat, SenderStatsResult } from "../../../../core/jmap/types";
import { MailStore } from "../../../../core/store";
import type { View } from "../../../../core/views";

const store = new MailStore(null);
let ranking: SenderStatsResult;
const senderStatsGet = vi.fn(async (args: { groupBy: string; sort: string; sinceDays: number | null; limit?: number }) => ({
  ...ranking,
  groupBy: args.groupBy,
  sort: args.sort,
  sinceDays: args.sinceDays,
}));
const allMatchingIds = vi.fn(async (_v: View, _cap?: number, _pos?: number) => ({ ids: ["e1", "e2"], total: 2 }));
const senderStatsEmailIds = vi.fn(async (_a: { groupBy: string; key: string; since: string | null; position: number; limit: number }) => ({
  ids: ["e1", "e2"],
  total: 2,
}));
const loadEmails = vi.fn(async (ids: Id[], _opts?: { body?: boolean }) => {
  store.putEmails(ids.map((id) => email(id)));
  return { notFound: [] as Id[], failed: false };
});
const archive = vi.fn(async (_ids: Id[]) => true);
const setContactsMarkers = vi.fn(async (ids: Id[]) => ids);
const undos: string[] = [];

vi.mock("../../../state", () => ({
  useApp: () => ({
    client: { senderStatsGet, senderStatsEmailIds },
    actions: { archive, setContactsMarkers, relabel: vi.fn(), moveToJunk: vi.fn(async () => true), trash: vi.fn(), restoreToInbox: vi.fn(), untrash: vi.fn(), settle: vi.fn(async () => true) },
    sync: { allMatchingIds, loadEmails },
    store,
    offerUndo: (label: string) => void undos.push(label),
    pushToast: () => 1,
  }),
  useMailState: () => useSyncExternalStore(store.subscribe, store.getState),
}));
vi.mock("../../UnsubscribePanel", () => ({ UnsubscribePanel: () => <p className="unsub-panel">unsubscribe panel</p> }));

import { CleanupBody, actionable } from "./CleanupRow";

function email(id: string): Email {
  return {
    id, blobId: "b", threadId: "t" + id, mailboxIds: { INBOX: true }, keywords: {}, labelIds: {}, size: 1,
    receivedAt: "2026-10-01T00:00:00Z", subject: id, preview: "", hasAttachment: false,
  } as unknown as Email;
}
function stat(key: string, over: Partial<SenderStat> = {}): SenderStat {
  return {
    key, displayName: "", total: 12, unread: 9, neglected: 10, neglectShare: 0.833, lastReceivedAt: "2026-10-15T08:00:00Z",
    addressCount: 1, addresses: [{ address: key, contactId: "C-" + key, isVip: false, isBlocked: false }],
    unsubscribeEmailId: null, ...over,
  };
}

const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));
async function click(el: Element): Promise<void> {
  flushSync(() => {
    el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
  await settle();
  await settle();
}
function within(li: Element, text: string): HTMLElement {
  const b = [...li.querySelectorAll<HTMLElement>("button")].find((n) => n.textContent?.trim() === text);
  if (!b) throw new Error(`no button ${text} in ${li.textContent}`);
  return b;
}
const rows = (): HTMLElement[] => [...document.querySelectorAll<HTMLElement>("li.cleanup-item")];

describe("Clean up (#53)", () => {
  let root: Root;
  async function render(): Promise<void> {
    flushSync(() => root.render(<CleanupBody />));
    await settle();
    await settle();
  }
  beforeEach(() => {
    for (const f of [senderStatsGet, allMatchingIds, senderStatsEmailIds, loadEmails, archive, setContactsMarkers]) f.mockClear();
    undos.length = 0;
    store.setSession({ capabilities: {} } as never, "U1");
    store.putMailboxes([{ id: "INBOX", accountId: "A", name: "INBOX", parentId: null, role: "inbox", sortOrder: 0, totalEmails: 0, unreadEmails: 0 } as Mailbox,
      { id: "ARCH", accountId: "A", name: "Archive", parentId: null, role: "archive", sortOrder: 1, totalEmails: 0, unreadEmails: 0 } as Mailbox]);
    ranking = {
      accountId: "U1", groupBy: "address", sort: "volume", sinceDays: 90, position: 0, total: 344,
      list: [stat("news@list.example", { displayName: "News", unsubscribeEmailId: "e9" }), stat("odd key@example.com")],
    };
    const host = document.createElement("div");
    document.body.replaceChildren(host);
    root = createRoot(host);
  });
  afterEach(() => flushSync(() => root.unmount()));

  it("asks for the last 90 days by sender, states the bound, and Show more asks for more", async () => {
    await render();
    expect(senderStatsGet).toHaveBeenLastCalledWith({ groupBy: "address", sort: "volume", sinceDays: 90, position: 0, limit: 50 });
    expect(document.body.textContent).toContain("Showing 2 of 344 senders.");
    expect(rows()[0]!.textContent).toContain("12 messages · 9 unread · 83% ignored");
    await click([...document.querySelectorAll("button")].find((b) => b.textContent === "Show more")!);
    expect(senderStatsGet).toHaveBeenLastCalledWith(expect.objectContaining({ limit: 100 }));
  });

  it("Archive all reads the row's OWN messages, in the ranking's window, and moves nothing before the confirm", async () => {
    ranking.since = "2026-07-19T00:00:00Z";
    await render();
    await click(within(rows()[0]!, "Archive all"));
    // The row's enumeration, never a search rebuilt from its key.
    expect(senderStatsEmailIds).toHaveBeenCalledWith({
      groupBy: "address", key: "news@list.example", since: "2026-07-19T00:00:00Z", position: 0, limit: 1000,
    });
    expect(allMatchingIds).not.toHaveBeenCalled();
    expect(archive).not.toHaveBeenCalled();
    expect(rows()[0]!.textContent).toContain("Move all 2 messages from news@list.example to Archive?");
    await click(within(rows()[0]!, "Move to Archive"));
    expect(archive).toHaveBeenCalledWith(["e1", "e2"]);
    expect(undos).toHaveLength(1);
  });

  it("a key the search syntax cannot carry gets no actions", async () => {
    await render();
    expect(rows()[1]!.querySelector("button")).toBeNull();
    expect(actionable("address", { key: "odd key@example.com" })).toBe(false);
  });

  // A From with no local part would be read by from: as a DOMAIN (#712), so
  // "archive all" on one "address" would sweep a whole domain.
  it.each(["example.com", "@example.com", "*.example.com", ".example.com", "alice@", "a@b@example.com"])(
    "an address row keyed %s is never actionable",
    async (key) => {
      expect(actionable("address", { key })).toBe(false);
      ranking.list = [stat(key)];
      await render();
      expect(rows()[0]!.querySelector("button")).toBeNull();
      expect(rows()[0]!.textContent).toContain("nothing to act on");
    },
  );

  it("the server's own refusal wins, and a well-formed address or domain is actionable", () => {
    expect(actionable("address", { key: "alice@example.com" })).toBe(true);
    expect(actionable("address", { key: "alice@example.com", actionable: false })).toBe(false);
    expect(actionable("domain", { key: "example.com" })).toBe(true);
    expect(actionable("domain", { key: "*.example.com" })).toBe(false);
    expect(actionable("domain", { key: "a@example.com" })).toBe(false);
  });

  it("a domain row says it is everyone at the domain", async () => {
    ranking.list = [stat("shop.example", { addressCount: 2 })];
    await render();
    await click([...document.querySelectorAll("label")].find((l) => l.textContent === "Domain")!.querySelector("input")!);
    expect(rows()[0]!.textContent).toContain("everyone at shop.example");
  });

  it("a list row's move enumerates that list exactly — no list: search that would take its sub-lists", async () => {
    ranking.list = [stat("news.example.org"), stat("x.news.example.org")];
    await render();
    await click([...document.querySelectorAll("label")].find((l) => l.textContent === "Mailing list")!.querySelector("input")!);
    await click(within(rows()[0]!, "Archive all"));
    expect(senderStatsEmailIds).toHaveBeenCalledWith(expect.objectContaining({ groupBy: "list", key: "news.example.org" }));
    expect(allMatchingIds).not.toHaveBeenCalled();
  });

  const groupRow = (): SenderStat =>
    stat("news.list.example", {
      addressCount: 5,
      addresses: [
        { address: "a@list.example", contactId: "C1", isVip: false, isBlocked: false, correspondent: false },
        { address: "b@list.example", contactId: "C2", isVip: false, isBlocked: true, correspondent: false },
        { address: "boss@list.example", contactId: "C3", name: "Boss", isVip: true, isBlocked: false, correspondent: false },
        { address: "friend@list.example", contactId: "C4", isVip: false, isBlocked: false, correspondent: true },
      ],
    });
  async function openGroupBlock(): Promise<HTMLFormElement> {
    ranking.list = [groupRow()];
    await render();
    await click([...document.querySelectorAll("label")].find((l) => l.textContent === "Mailing list")!.querySelector("input")!);
    expect(senderStatsGet).toHaveBeenLastCalledWith(expect.objectContaining({ groupBy: "list" }));
    await click(within(rows()[0]!, "Block…"));
    return rows()[0]!.querySelector("form") as HTMLFormElement;
  }
  const pick = (form: HTMLFormElement, address: string): HTMLInputElement =>
    [...form.querySelectorAll("label")].find((l) => l.textContent?.includes(address))!.querySelector("input")!;

  it("a group block lists the exact addresses and leaves VIPs and correspondents unticked", async () => {
    const form = await openGroupBlock();
    expect(pick(form, "a@list.example").checked).toBe(true);
    expect(pick(form, "boss@list.example").checked).toBe(false);
    expect(form.textContent).toContain("Boss <boss@list.example> (VIP)");
    expect(pick(form, "friend@list.example").checked).toBe(false);
    expect(form.textContent).toContain("(you have written to them)");
    expect(pick(form, "b@list.example").disabled).toBe(true);
    expect(form.textContent).toContain("Older addresses not blocked: 1.");
    // No "move existing mail" on a group: its search would take everyone.
    expect(form.textContent).not.toContain("Also move the mail already here");
    // The button states the count that will actually be blocked.
    await click(within(rows()[0]!, "Block 1 address"));
    expect(setContactsMarkers).toHaveBeenCalledTimes(1);
    expect(setContactsMarkers).toHaveBeenCalledWith(["C1"], { isBlocked: true, blockDestination: "junk" }, expect.any(String));
    expect(allMatchingIds).not.toHaveBeenCalled();
  });

  it("a VIP or a correspondent is blocked only when ticked by hand", async () => {
    const form = await openGroupBlock();
    await click(pick(form, "boss@list.example"));
    await click(within(rows()[0]!, "Block 2 addresses"));
    expect(setContactsMarkers).toHaveBeenCalledWith(["C1", "C3"], { isBlocked: true, blockDestination: "junk" }, expect.any(String));
  });

  it("an address row blocks that address and moves nothing unless asked", async () => {
    await render();
    await click(within(rows()[0]!, "Block…"));
    const form = rows()[0]!.querySelector("form") as HTMLFormElement;
    expect((form.querySelector('input[type="checkbox"]') as HTMLInputElement).checked).toBe(false);
    await click(within(rows()[0]!, "Block 1 address"));
    expect(setContactsMarkers).toHaveBeenCalledWith(["C-news@list.example"], { isBlocked: true, blockDestination: "junk" }, expect.any(String));
    expect(allMatchingIds).not.toHaveBeenCalled();
  });

  it("Unsubscribe reads the message WITH its body and shows the existing affordance", async () => {
    await render();
    await click(within(rows()[0]!, "Unsubscribe…"));
    expect(loadEmails).toHaveBeenCalledWith(["e9"], { body: true });
    expect(rows()[0]!.querySelector(".unsub-panel")).not.toBeNull();
  });

  it("switching to Domain asks the server for domains", async () => {
    await render();
    const domain = [...document.querySelectorAll("label")].find((l) => l.textContent === "Domain")!.querySelector("input")!;
    await click(domain);
    expect(senderStatsGet).toHaveBeenLastCalledWith(expect.objectContaining({ groupBy: "domain" }));
  });
});
