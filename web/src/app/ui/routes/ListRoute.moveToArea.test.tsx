// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// "Move to area" IN THE CLASSIC LIST'S SELECTION TOOLBAR (#708).
//
// The route is mounted for real (real MailStore, real MessageList, real
// moveToArea/refile path); only the network is a stand-in. `relabel` applies
// its changes to the store the way MailActions.relabel does, so Undo is checked
// on the resulting label sets.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { LabelChange } from "../../../core/actions";
import type { Area, Email, Id, Label, Mailbox } from "../../../core/jmap/types";
import { MailStore, effectiveEmail } from "../../../core/store";
import { viewKey } from "../../../core/views";
import type { View } from "../../../core/views";

const store = new MailStore(null);
const view: View = { kind: "role", role: "inbox" };

const TOTAL = 1240;
const CAP = 1000;
const PAGE = 5;
const eid = (i: number): string => `e${String(i).padStart(4, "0")}`;
/** Everything the "server" holds: e0000..e1239. Only the first PAGE are loaded. */
const server: Email[] = [];

const relabel = vi.fn(async (changes: LabelChange[]) => {
  for (const c of changes) {
    for (const l of c.remove) store.applyLabelId([c.id], l.id, false);
    for (const l of c.add) store.applyLabelId([c.id], l.id, true);
  }
});
const loadEmails = vi.fn(async (ids: Id[]) => {
  const have = new Map(server.map((e) => [e.id, e]));
  store.putEmails(ids.flatMap((id) => (have.has(id) ? [have.get(id) as Email] : [])));
  return { notFound: [] as Id[], failed: false };
});
const allMatchingIds = vi.fn(async (_v: View, cap = CAP) => ({
  ids: server.slice(0, cap).map((e) => e.id),
  total: TOTAL,
}));
const actions = new Proxy({ relabel }, { get: (t, k) => (k in t ? t[k as keyof typeof t] : vi.fn(async () => true)) });
const sync = new Proxy(
  { loadEmails, allMatchingIds },
  { get: (t, k) => (k in t ? t[k as keyof typeof t] : vi.fn(async () => {})) },
);
const undos: Array<{ label: string; inverse: () => Promise<boolean> }> = [];
const toasts: string[] = [];
const offerUndo = (label: string, inverse: () => Promise<boolean>): void => void undos.push({ label, inverse });
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

function label(id: string): Label {
  return { id, name: id, color: null, parentId: null, mirrorToImap: false, keyword: null, tab: null };
}
function area(id: string, name: string, position: number, labelId: string): Area {
  return { id, name, position, labelId, tabs: [], labelIds: [], autoArchive: true, autoArchiveDays: null, hiddenFromRail: false } as Area;
}
function email(i: number, labelIds: Id[]): Email {
  return {
    id: eid(i),
    blobId: "b",
    threadId: `t${i}`,
    mailboxIds: { "mb-in": true },
    keywords: { $seen: true },
    labelIds: Object.fromEntries(labelIds.map((l) => [l, true])),
    size: 1,
    receivedAt: new Date(Date.UTC(2026, 9, 1) - i * 60_000).toISOString(),
    from: [{ name: `S${i}`, email: `s${i}@example.com` }],
    subject: `Message ${i}`,
    preview: "",
    hasAttachment: false,
  } as unknown as Email;
}

const labelsOf = (id: Id): string =>
  Object.keys(effectiveEmail(store.getState(), id)?.labelIds ?? {})
    .sort()
    .join(",");

function seed(): void {
  store.setSession({ capabilities: {} } as never, "U1");
  store.putMailboxes(
    [{ id: "mb-in", accountId: "A1", name: "Inbox", parentId: null, role: "inbox", sortOrder: 0 }] as unknown as Mailbox[],
    true,
  );
  store.putLabels([label("L-WORK"), label("L-NEWS")]);
  store.putAreas([area("A-WORK", "Work", 1, "L-WORK"), area("A-NEWS", "News", 2, "L-NEWS")], true);
  server.length = 0;
  for (let i = 0; i < TOTAL; i++) {
    // e0000 is already in Work (a no-op); every third is in News; the rest nowhere.
    server.push(email(i, i === 0 ? ["L-WORK"] : i % 3 === 0 ? ["L-NEWS"] : []));
  }
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

const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));
async function click(el: Element): Promise<void> {
  flushSync(() => {
    el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
  await settle();
}
const byText = (sel: string, text: string): HTMLElement => {
  const found = [...document.querySelectorAll<HTMLElement>(sel)].find((n) => n.textContent?.trim() === text);
  if (!found) throw new Error(`no ${sel} with text ${text}`);
  return found;
};

describe("Move to area in the classic list's selection toolbar (#708)", () => {
  let root: Root;
  beforeEach(async () => {
    undos.length = 0;
    toasts.length = 0;
    relabel.mockClear();
    loadEmails.mockClear();
    allMatchingIds.mockClear();
    window.sessionStorage.clear();
    seed();
    navigate({ page: "inbox", tab: null }, { replace: true });
    const app = document.createElement("div");
    document.body.replaceChildren(app);
    root = createRoot(app);
    flushSync(() => root.render(<ListRoute />));
    await settle();
  });
  afterEach(() => {
    flushSync(() => root.unmount());
  });

  const selectPage = async (): Promise<void> => {
    await click(document.querySelector('input[aria-label="Select all"], .list-toolbar input[type=checkbox]') as Element);
  };

  it("offers the areas in the desk's order, under a button whose name contains its visible text", async () => {
    expect(document.querySelector('[role="menu"]')).toBeNull();
    await selectPage();
    const btn = byText(".list-toolbar button", "Move to area ▾");
    // WCAG 2.5.3: no aria-label to drop the visible words; the name IS the text.
    expect(btn.getAttribute("aria-label")).toBeNull();
    expect(btn.getAttribute("aria-haspopup")).toBe("menu");
    await click(btn);
    expect(btn.getAttribute("aria-expanded")).toBe("true");
    const items = [...document.querySelectorAll('[role="menuitem"]')].map((n) => n.textContent);
    expect(items).toEqual(["Work", "News"]);
  });

  it("re-files the selected page: swaps labels, counts the already-there one as a no-op, Undo restores exactly", async () => {
    await selectPage();
    await click(byText(".list-toolbar button", "Move to area ▾"));
    await click(byText('[role="menuitem"]', "Work"));
    // e0000 already in Work; e0003 News->Work; e0001, e0002, e0004 none->Work.
    expect(labelsOf(eid(0))).toBe("L-WORK");
    expect(labelsOf(eid(3))).toBe("L-WORK");
    expect(labelsOf(eid(1))).toBe("L-WORK");
    expect(undos).toHaveLength(1);
    expect(undos[0]!.label).toBe("Moved 4 messages to Work. 1 already in Work.");
    await undos[0]!.inverse();
    expect(labelsOf(eid(0))).toBe("L-WORK");
    expect(labelsOf(eid(3))).toBe("L-NEWS");
    expect(labelsOf(eid(1))).toBe("");
  });

  it("an all-matching selection moves the capped set and SAYS it was capped: Moved 1,000 of 1,240", async () => {
    await selectPage();
    await click(document.querySelector(".select-all-banner button") as Element);
    expect(allMatchingIds).toHaveBeenCalledTimes(1);
    await click(byText(".list-toolbar button", "Move to area ▾"));
    await click(byText('[role="menuitem"]', "News"));
    // The 995 unloaded messages were READ before the swap, not silently skipped.
    expect(loadEmails.mock.calls.flatMap((c) => c[0])).toHaveLength(CAP - PAGE);
    const sent = relabel.mock.calls[0]![0];
    // The messages already in News are no-ops, so they are not in the changes.
    const inNews = server.slice(0, CAP).filter((e) => e.labelIds && e.labelIds["L-NEWS"]).length;
    expect(sent).toHaveLength(CAP - inNews);
    expect(undos[0]!.label).toBe(
      `Moved ${(CAP - inNews).toLocaleString("en-US")} of 1,240 messages to News. ${inNews} already in News. 240 more were not touched.`,
    );
    // Exact Undo over the 1,000.
    await undos[0]!.inverse();
    expect(labelsOf(eid(0))).toBe("L-WORK");
    expect(labelsOf(eid(3))).toBe("L-NEWS");
    expect(labelsOf(eid(1))).toBe("");
    expect(labelsOf(eid(999))).toBe(server[999]!.labelIds?.["L-NEWS"] ? "L-NEWS" : "");
  });
});
