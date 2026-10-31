// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// "Move all from <address> to…" on the sender card (#708).
//
// Real MailStore and the real move path; the network is a stand-in. What is
// pinned: the query is the EXACT-address `from:` search, the count is shown and
// confirmed in the popover before anything moves (no window.confirm), the move
// is the desk's label swap, and one Undo restores every message exactly.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { LabelChange } from "../../../core/actions";
import type { Area, Email, Id, Label } from "../../../core/jmap/types";
import { MailStore, effectiveEmail } from "../../../core/store";
import type { EffectiveEmail } from "../../../core/store";
import type { View } from "../../../core/views";
import { JmapMethodError } from "../../../core/jmap/client";

const store = new MailStore(null);
const ADDRESS = "news@example.com";
const eid = (i: number): string => `n${String(i).padStart(4, "0")}`;
const server: Email[] = [];
let matching: { ids: Id[]; total: number } = { ids: [], total: 0 };

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
const allMatchingIds = vi.fn(async (_v: View, _cap?: number, position = 0) => ({
  ids: matching.ids.slice(position, position + 1000),
  total: matching.total,
}));
const undos: Array<{ label: string; inverse: () => Promise<boolean> }> = [];
const toasts: string[] = [];
const confirmSpy = vi.fn(() => true);

vi.mock("../../state", () => ({
  useApp: () => ({
    actions: { relabel },
    sync: { loadEmails, allMatchingIds },
    store,
    offerUndo: (label: string, inverse: () => Promise<boolean>) => void undos.push({ label, inverse }),
    pushToast: (t: { message: string }) => (toasts.push(t.message), 1),
  }),
  useMailState: () => useSyncExternalStore(store.subscribe, store.getState),
}));
vi.mock("../shellContext", () => ({ useShell: () => ({ openRuleDraft: () => {} }) }));
vi.mock("../useMediaQuery", async (orig) => ({ ...(await orig<object>()), useMediaQuery: () => false }));

import { SenderPopover } from "./SenderPopover";

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
    mailboxIds: {},
    keywords: {},
    labelIds: Object.fromEntries(labelIds.map((l) => [l, true])),
    size: 1,
    receivedAt: "2026-10-01T00:00:00Z",
    from: [{ name: "News Desk", email: ADDRESS }],
    subject: `News ${i}`,
    preview: "",
    hasAttachment: false,
  } as unknown as Email;
}
const labelsOf = (id: Id): string =>
  Object.keys(effectiveEmail(store.getState(), id)?.labelIds ?? {})
    .sort()
    .join(",");

const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));
async function click(el: Element): Promise<void> {
  flushSync(() => {
    el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
  await settle();
  await settle();
}
const byText = (sel: string, text: string): HTMLElement => {
  const found = [...document.querySelectorAll<HTMLElement>(sel)].find((n) => n.textContent?.trim() === text);
  if (!found) throw new Error(`no ${sel} with text ${text}; have ${document.body.textContent}`);
  return found;
};

describe("Move all from <address> to… (#708)", () => {
  let root: Root;
  let shown: EffectiveEmail;

  async function open(): Promise<void> {
    flushSync(() => root.render(<SenderPopover email={shown} />));
    await click(document.querySelector("button.message-from-button") as Element);
    await click(byText("button", `Move all from ${ADDRESS} to… ▾`));
  }

  beforeEach(() => {
    undos.length = 0;
    toasts.length = 0;
    relabel.mockClear();
    loadEmails.mockClear();
    allMatchingIds.mockClear();
    confirmSpy.mockClear();
    vi.stubGlobal("confirm", confirmSpy);
    store.setSession({ capabilities: {} } as never, "U1");
    store.putLabels([label("L-WORK"), label("L-NEWS")]);
    store.putAreas([area("A-WORK", "Work", 1, "L-WORK"), area("A-NEWS", "News", 2, "L-NEWS")], true);
    server.length = 0;
    // 7 messages from the address: one already in Work, two in News, four nowhere.
    for (let i = 0; i < 7; i++) server.push(email(i, i === 0 ? ["L-WORK"] : i < 3 ? ["L-NEWS"] : []));
    matching = { ids: server.map((e) => e.id), total: 7 };
    store.putEmails(server.slice(0, 1));
    shown = effectiveEmail(store.getState(), eid(0)) as EffectiveEmail;
    const host = document.createElement("div");
    document.body.replaceChildren(host);
    root = createRoot(host);
  });
  afterEach(() => {
    flushSync(() => root.unmount());
    vi.unstubAllGlobals();
  });

  it("queries the exact address, confirms with the count in the popover, moves, and Undo restores exactly", async () => {
    await open();
    expect(allMatchingIds).not.toHaveBeenCalled();
    await click(byText('[role="menuitem"]', "Work"));

    // The exact-address `from:` search, the list views' own definition of the set.
    expect(allMatchingIds).toHaveBeenCalledTimes(1);
    expect(allMatchingIds.mock.calls[0]![0]).toEqual({ kind: "search", query: `from:${ADDRESS}` });

    // NOTHING has moved yet: the count is asked first, in the popover.
    expect(relabel).not.toHaveBeenCalled();
    expect(document.querySelector('[role="dialog"]')?.textContent).toContain(
      `Move all 7 messages from ${ADDRESS} to Work?`,
    );
    expect(document.querySelector('[role="dialog"]')?.textContent).toContain(
      "Future mail from this sender follows once Mailroom has learned it, except mail that cannot be authenticated, which needs a rule.",
    );

    await click(byText("button", "Move to Work"));
    expect(confirmSpy).not.toHaveBeenCalled();
    expect(relabel).toHaveBeenCalledTimes(1);
    expect(relabel.mock.calls[0]![0]).toHaveLength(6); // 7 minus the one already in Work
    for (let i = 0; i < 7; i++) expect(labelsOf(eid(i))).toBe("L-WORK");
    expect(undos).toHaveLength(1);
    expect(undos[0]!.label).toBe("Moved 6 messages to Work. 1 already in Work.");

    await undos[0]!.inverse();
    expect(labelsOf(eid(0))).toBe("L-WORK");
    expect(labelsOf(eid(1))).toBe("L-NEWS");
    expect(labelsOf(eid(2))).toBe("L-NEWS");
    expect(labelsOf(eid(5))).toBe("");
  });

  it("search turned off on the server says so, never \"no messages\", and moves nothing", async () => {
    allMatchingIds.mockRejectedValueOnce(
      new JmapMethodError("q0", { type: "unsupportedFilter", description: "search is disabled" }),
    );
    await open();
    await click(byText('[role="menuitem"]', "Work"));
    expect(toasts).toEqual([
      `Search is turned off on this server, so Mailroom cannot find every message from ${ADDRESS}.`,
    ]);
    expect(document.querySelector('[role="dialog"]')?.textContent).not.toContain("Cancel");
    expect(relabel).not.toHaveBeenCalled();
  });

  it("Cancel in the confirm step moves nothing", async () => {
    await open();
    await click(byText('[role="menuitem"]', "Work"));
    await click(byText("button", "Cancel"));
    expect(relabel).not.toHaveBeenCalled();
    expect(undos).toHaveLength(0);
  });

  it("a sender with more than the cap says so up front, then offers the rest from where it stopped", async () => {
    server.length = 0;
    for (let i = 0; i < 1240; i++) server.push(email(i, []));
    matching = { ids: server.map((e) => e.id), total: 1240 };
    store.putEmails(server.slice(0, 1));
    shown = effectiveEmail(store.getState(), eid(0)) as EffectiveEmail;
    await open();
    await click(byText('[role="menuitem"]', "Work"));
    expect(document.querySelector('[role="dialog"]')?.textContent).toContain(
      `${ADDRESS} has 1,240 messages. Move the first 1,000 to Work?`,
    );
    await click(byText("button", "Move to Work"));
    expect(undos[0]!.label).toBe("Moved 1,000 of 1,240 messages to Work. 240 more were not touched.");
    // Not left looking finished: the popover offers the remainder.
    await click(byText("button", "Move the rest (240)"));
    expect(allMatchingIds.mock.calls[1]![2]).toBe(1000);
    for (const i of [0, 999, 1000, 1239]) expect(labelsOf(eid(i))).toBe("L-WORK");
    expect(undos[1]!.label).toBe("Moved 240 messages to Work.");
  });
});
