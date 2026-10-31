// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// A BULK ACTION OVER A TRUNCATED "SELECT ALL MATCHING" OFFERS THE REST (#769).
//
// 1,639 messages match, one selection holds 1,000. After a verb has run on them
// the list says "Done 1000 of 1639 — select the next 639", and the next batch is
// found by EXCLUSION (everything already processed is dropped), so it is right
// whether the verb took the mail out of the view (archive) or left it in (pin).
// The route is mounted for real; the network is a stand-in whose "server" really
// forgets archived mail.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Email, Id, Mailbox } from "../../../core/jmap/types";
import { MailStore } from "../../../core/store";
import { viewKey } from "../../../core/views";
import type { View } from "../../../core/views";

const store = new MailStore(null);
const view: View = { kind: "role", role: "inbox" };

const TOTAL = 1639;
const CAP = 1000;
const PAGE = 5;
const eid = (i: number): string => `e${String(i).padStart(4, "0")}`;
const server: Email[] = [];
/** Ids the "server" no longer shows in the view (archived out of it). */
const gone = new Set<Id>();

const inView = (): Email[] => server.filter((e) => !gone.has(e.id));
const allMatchingIds = vi.fn(async (_v: View, cap = CAP, position = 0) => ({
  ids: inView()
    .slice(position, position + cap)
    .map((e) => e.id),
  total: inView().length,
}));
const archive = vi.fn(async (ids: Id[]) => {
  for (const id of ids) gone.add(id);
  return true;
});
const settle = vi.fn(async () => true);
const setPinned = vi.fn(async (_ids: Id[], _on: boolean) => true);
const loadEmails = vi.fn(async () => ({ notFound: [] as Id[], failed: false }));
const actions = new Proxy({ archive, settle, setPinned }, { get: (t, k) => (k in t ? t[k as keyof typeof t] : vi.fn(async () => true)) });
const sync = new Proxy(
  { allMatchingIds, loadEmails },
  { get: (t, k) => (k in t ? t[k as keyof typeof t] : vi.fn(async () => {})) },
);
const toasts: string[] = [];
const offerUndo = (): void => {};
const pushToast = (t: { message: string }): number => (toasts.push(t.message), 1);

vi.mock("../../state", () => ({
  useApp: () => ({ actions, sync, store, view, setView: () => {}, pushToast, offerUndo }),
  useMailState: () => useSyncExternalStore(store.subscribe, store.getState),
}));
vi.mock("../shellContext", () => ({
  useShell: () =>
    new Proxy(
      { paneLayout: "right", paneSplit: 0.5, railKeys: { current: null } },
      { get: (target, key) => (key in target ? target[key as keyof typeof target] : () => {}) },
    ),
}));
vi.mock("../PaneSplitter", () => ({ PaneSplitter: () => null }));
vi.mock("../ThreadPane", () => ({ ThreadPane: () => null }));
vi.mock("../CommandPalette", () => ({ CommandPalette: () => null }));

import { navigate } from "../../routing";
import { ListRoute } from "./ListRoute";

function email(i: number): Email {
  return {
    id: eid(i),
    blobId: "b",
    threadId: `t${i}`,
    mailboxIds: { "mb-in": true },
    keywords: { $seen: true },
    labelIds: {},
    size: 1,
    receivedAt: new Date(Date.UTC(2026, 9, 1) - i * 60_000).toISOString(),
    from: [{ name: `S${i}`, email: `s${i}@example.com` }],
    subject: `Message ${i}`,
    preview: "",
    hasAttachment: false,
  } as unknown as Email;
}

function seed(): void {
  store.setSession({ capabilities: {} } as never, "U1");
  store.putMailboxes(
    [{ id: "mb-in", accountId: "A1", name: "Inbox", parentId: null, role: "inbox", sortOrder: 0 }] as unknown as Mailbox[],
    true,
  );
  server.length = 0;
  gone.clear();
  for (let i = 0; i < TOTAL; i++) server.push(email(i));
  store.putEmails(server.slice(0, PAGE));
  store.setList({
    key: viewKey(view),
    ids: server.slice(0, PAGE).map((e) => e.id),
    total: TOTAL,
    limit: PAGE,
    loading: false,
    error: null,
    searchUnavailable: false,
    epoch: 0,
  });
}

const tick = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));
async function click(el: Element): Promise<void> {
  flushSync(() => {
    el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
  await tick();
}
const byText = (sel: string, text: string): HTMLElement => {
  const found = [...document.querySelectorAll<HTMLElement>(sel)].find((n) => n.textContent?.trim() === text);
  if (!found) throw new Error(`no ${sel} with text ${text}`);
  return found;
};
const banner = (): string | null => document.querySelector(".select-all-banner")?.textContent ?? null;
const count = (): number => Number(/(\d+) selected/.exec(document.body.textContent ?? "")?.[1] ?? 0);

describe("a truncated select-all-matching carries on (#769)", () => {
  let root: Root;
  beforeEach(async () => {
    toasts.length = 0;
    allMatchingIds.mockClear();
    archive.mockClear();
    setPinned.mockClear();
    settle.mockClear();
    settle.mockImplementation(async () => true);
    window.sessionStorage.clear();
    seed();
    navigate({ page: "inbox", tab: null }, { replace: true });
    const app = document.createElement("div");
    document.body.replaceChildren(app);
    root = createRoot(app);
    flushSync(() => root.render(<ListRoute />));
    await tick();
    await click(document.querySelector('input[aria-label="Select all"], .list-toolbar input[type=checkbox]') as Element);
    await click(document.querySelector(".select-all-banner button") as Element);
  });
  afterEach(() => {
    flushSync(() => root.unmount());
  });

  it("archive (removes mail from the view): offers the next 639 and selects exactly the unprocessed ones", async () => {
    expect(count()).toBe(CAP);
    await click(byText(".list-toolbar button", "Archive"));
    expect(archive.mock.calls[0]![0]).toHaveLength(CAP);
    expect(banner()).toBe("Done 1000 of 1639 — select the next 639");
    allMatchingIds.mockClear();
    await click(document.querySelector(".select-all-banner button") as Element);
    expect(settle).toHaveBeenCalledTimes(1);
    // processed + cap from the start: never a position offset.
    expect(allMatchingIds).toHaveBeenCalledWith(view, CAP + CAP, 0);
    expect(count()).toBe(TOTAL - CAP);
    await click(byText(".list-toolbar button", "Archive"));
    expect([...new Set(archive.mock.calls[1]![0] as Id[])].sort()).toEqual(
      server.slice(CAP).map((e) => e.id).sort(),
    );
    // Everything is gone now and nothing is offered.
    expect(banner()).toBeNull();
  });

  it("a verb that leaves the mail in the view (pin): the next batch still skips what was done", async () => {
    await click(byText(".list-toolbar button", "Pin"));
    expect(banner()).toBe("Done 1000 of 1639 — select the next 639");
    await click(document.querySelector(".select-all-banner button") as Element);
    expect(count()).toBe(TOTAL - CAP);
    // The server still holds all 1,639: the first 1,000 were dropped by id.
    expect(allMatchingIds).toHaveBeenLastCalledWith(view, CAP + CAP, 0);
    await click(byText(".list-toolbar button", "Pin"));
    expect(setPinned.mock.calls[1]![0].slice().sort()).toEqual(server.slice(CAP).map((e) => e.id).sort());
    expect(banner()).toBeNull();
  });

  it("an empty next batch says so and clears the offer", async () => {
    await click(byText(".list-toolbar button", "Archive"));
    // The rest vanished meanwhile (archived elsewhere).
    for (const e of server.slice(CAP)) gone.add(e.id);
    await click(document.querySelector(".select-all-banner button") as Element);
    expect(toasts.at(-1)).toBe("That was all of them.");
    expect(banner()).toBeNull();
  });

  it("an unsettled write does not fetch the next batch: an error toast, offer kept", async () => {
    await click(byText(".list-toolbar button", "Archive"));
    allMatchingIds.mockClear();
    settle.mockImplementation(async () => false);
    await click(document.querySelector(".select-all-banner button") as Element);
    expect(allMatchingIds).not.toHaveBeenCalled();
    expect(toasts.at(-1)).toMatch(/has not reached the server/);
    expect(banner()).toBe("Done 1000 of 1639 — select the next 639");
  });

  it("a label menu dismissed without applying anything does not count the batch done", async () => {
    await click(byText(".list-toolbar button", "Label"));
    await click(document.querySelector('[aria-label="Labels"] button[aria-label="Close"], .sheet button[aria-label="Close"]') as Element);
    expect(banner() ?? "").not.toMatch(/^Done/);
  });

  it("a snooze menu cancelled after a real verb keeps the run (and adds nothing)", async () => {
    await click(byText(".list-toolbar button", "Pin"));
    expect(banner()).toBe("Done 1000 of 1639 — select the next 639");
    await click(byText(".list-toolbar button", "Snooze"));
    await click(document.querySelector('.sheet button[aria-label="Close"]') as Element);
    expect(banner()).toBe("Done 1000 of 1639 — select the next 639");
  });

  it("a snooze menu cancelled on an untouched batch does not count it done", async () => {
    await click(byText(".list-toolbar button", "Snooze"));
    await click(document.querySelector('.sheet button[aria-label="Close"]') as Element);
    expect(banner() ?? "").not.toMatch(/^Done/);
  });

  it("a manual selection ends the run", async () => {
    await click(byText(".list-toolbar button", "Archive"));
    expect(banner()).not.toBeNull();
    await click(document.querySelector('.list-toolbar input[type=checkbox]') as Element);
    expect(banner()).not.toBe("Done 1000 of 1639 — select the next 639");
  });
});
