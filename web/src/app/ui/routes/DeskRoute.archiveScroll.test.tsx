// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// ARCHIVE-AND-ADVANCE DOES NOT MOVE THE LIST (#685).
//
// Production 1.81.6: archive the open message on the desk and the list scrolls
// so the newly opened row sits at the very bottom of the pane. The scroll is
// VirtualList's cursor-into-view effect doing exactly what it is for — the
// question is why the cursor went anywhere that needed scrolling. It is this
// route's own cursor repair: the archive's optimistic overlay drops the row in
// one render, and the advance only arrives after `actions.archive` has awaited
// its queue. In between, "keep the cursor on a row that still exists" put it on
// the FIRST row of the desk — the pane scrolled to the top — and then the
// advance put it on the row after the archived one, now far below the viewport:
// scrolled again, bottom-aligned.
//
// That is a fact about the order of two renders inside one route, so the route
// is mounted here for real, with a real MailStore. Everything around it that is
// not about the list (the header, the strip, the reading pane's body) is
// stubbed. ⚠ jsdom has no layout: the geometry is the same stub
// ui/desk/VirtualList.test.tsx uses.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSyncExternalStore } from "react";
import type { JSX } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { MailStore } from "../../../core/store";
import { sectionKey } from "../../../core/desk";
import type { Area, Email, Mailbox } from "../../../core/jmap/types";

const ROW_H = 68;
const CHROME_H = 160;
const VIEWPORT = 640;
const N = 40;

const store = new MailStore(null);
/** What the archive awaits between its overlay and its return — the real one
 * awaits the durable queue (core/actions.ts archive). Resolved by the test. */
let archiveGate: Promise<void> = Promise.resolve();

const actions = {
  archive: vi.fn(async (ids: string[]) => {
    store.applyMove(ids, "archive");
    await archiveGate;
    return true;
  }),
  restoreToInbox: vi.fn(async () => true),
  setSeen: vi.fn(async () => {}),
};
const sync = {
  currentDesk: { tab: "review", scope: null },
  loadDesk: vi.fn(async () => {}),
  refreshDesk: vi.fn(async () => {}),
  loadThread: vi.fn(async () => {}),
  loadDrawer: vi.fn(async () => {}),
  poll: vi.fn(async () => {}),
};

vi.mock("../../state", () => ({
  useApp: () => ({ actions, sync, pushToast: () => {}, offerUndo: () => {} }),
  useMailState: () => useSyncExternalStore(store.subscribe, store.getState),
}));
vi.mock("../shellContext", () => ({
  useShell: () =>
    new Proxy(
      { paneLayout: "right", paneSplit: 0.5 },
      { get: (target, key) => (key in target ? target[key as keyof typeof target] : () => {}) },
    ),
}));
vi.mock("../useAccounts", () => ({ useAccounts: () => ({ byId: new Map(), accounts: [] }) }));
vi.mock("../useHeaderState", () => ({ useHeaderState: () => ({ kind: "fine" }) }));
vi.mock("../useShadowNotices", () => ({ useShadowNotices: () => [] }));
vi.mock("../useMediaQuery", async (orig) => ({
  ...(await orig<object>()),
  useMediaQuery: () => false,
}));
vi.mock("../desk/DeskHeader", () => ({ DeskHeader: () => null }));
vi.mock("../desk/ShadowBanner", () => ({ ShadowBanners: () => null }));
vi.mock("../desk/TrayStrip", async (orig) => ({ ...(await orig<object>()), TrayStrip: () => null }));
vi.mock("../PaneSplitter", () => ({ PaneSplitter: () => null }));
vi.mock("../ThreadPane", () => ({
  ThreadPane: ({ email, onArchive }: { email: { id: string }; onArchive: () => void }) => (
    <div className="thread-pane" data-open={email.id}>
      <button type="button" className="archive" onClick={onArchive}>
        Archive
      </button>
    </div>
  ),
}));

import { navigate } from "../../routing";
import { DeskRoute } from "./DeskRoute";

const id = (i: number): string => `e${String(i).padStart(2, "0")}`;

function seed(): void {
  store.setSession({ capabilities: {} } as never, "U1");
  store.putMailboxes(
    [
      { id: "mb-in", accountId: "A1", name: "Inbox", parentId: null, role: "inbox", sortOrder: 0 },
      { id: "mb-ar", accountId: "A1", name: "Archive", parentId: null, role: "archive", sortOrder: 1 },
    ] as unknown as Mailbox[],
    true,
  );
  store.putAreas(
    [{ id: "AR1", name: "Work", position: 0, labelId: "L1", tabs: [], labelIds: [] } as unknown as Area],
    true,
  );
  const emails: Email[] = [];
  for (let i = 0; i < N; i++) {
    emails.push({
      id: id(i),
      blobId: "b",
      threadId: `t${i}`,
      mailboxIds: { "mb-in": true },
      // Newest first, so the section's own sort keeps e00..e39 in order.
      receivedAt: new Date(Date.UTC(2026, 9, 1) - i * 60_000).toISOString(),
      keywords: { $seen: true },
      size: 1,
      from: [{ name: "Someone", email: "someone@example.com" }],
      subject: `Message ${i}`,
      preview: "",
    } as unknown as Email);
  }
  store.putEmails(emails);
}

/** The desk's load landing — AFTER the route mounted, as it does for real: the
 * route's first render has no rows at all. */
function loadSection(): void {
  store.setSection({
    key: sectionKey("surface", "AR1", null),
    ids: Array.from({ length: N }, (_, i) => id(i)),
    total: N,
    limit: N,
    loading: false,
    error: null,
    loadedAt: 1,
    epoch: 0,
  });
}

let scrollTop = 0;
let restore: (() => void)[] = [];

function stubGeometry(): void {
  const proto = HTMLElement.prototype;
  const saved = {
    clientHeight: Object.getOwnPropertyDescriptor(proto, "clientHeight"),
    scrollTop: Object.getOwnPropertyDescriptor(proto, "scrollTop"),
    rect: proto.getBoundingClientRect,
    observer: globalThis.ResizeObserver,
  };
  const isPane = (el: HTMLElement): boolean => el.classList.contains("list-pane");
  Object.defineProperty(proto, "clientHeight", {
    configurable: true,
    get(this: HTMLElement) {
      return isPane(this) ? VIEWPORT : 0;
    },
  });
  Object.defineProperty(proto, "scrollTop", {
    configurable: true,
    get(this: HTMLElement) {
      return isPane(this) ? scrollTop : 0;
    },
    set(this: HTMLElement, value: number) {
      if (!isPane(this)) return;
      scrollTop = Math.max(0, value);
      this.dispatchEvent(new Event("scroll"));
    },
  });
  proto.getBoundingClientRect = function (this: HTMLElement): DOMRect {
    const rect = (top: number, height: number): DOMRect =>
      ({ top, height, bottom: top + height, left: 0, right: 0, width: 0, x: 0, y: top }) as DOMRect;
    if (isPane(this)) return rect(0, VIEWPORT);
    if (this.classList.contains("desk-probe")) return rect(0, ROW_H);
    if (this.classList.contains("desk-list")) return rect(CHROME_H - scrollTop, 0);
    return rect(0, 0);
  };
  globalThis.ResizeObserver = class {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  };
  restore.push(() => {
    if (saved.clientHeight) Object.defineProperty(proto, "clientHeight", saved.clientHeight);
    if (saved.scrollTop) Object.defineProperty(proto, "scrollTop", saved.scrollTop);
    proto.getBoundingClientRect = saved.rect;
    globalThis.ResizeObserver = saved.observer;
  });
}

const settle = (): Promise<void> =>
  new Promise((resolve) => requestAnimationFrame(() => setTimeout(resolve, 0)));

/** The item index of a message's row, read off the grid. */
function rowIndexOfId(emailId: string): number {
  const li = document.querySelector(`li[id$="-item-r-${emailId}"]`);
  return li ? Number(li.getAttribute("aria-rowindex")) - 1 : -1;
}

describe("archive-and-advance on the desk (#685)", () => {
  let root: Root;

  beforeEach(() => {
    scrollTop = 0;
    restore = [];
    seed();
    stubGeometry();
    const app = document.createElement("div");
    document.body.replaceChildren(app);
    root = createRoot(app);
  });

  afterEach(() => {
    flushSync(() => root.unmount());
    for (const undo of restore) undo();
    navigate({ page: "inbox", tab: null }, { replace: true });
  });

  it("does not scroll the list when the next row is already on screen", async () => {
    navigate({ page: "inbox", tab: "review" }, { replace: true });
    flushSync(() => root.render(<DeskRoute />));
    await settle();
    loadSection();
    await settle();
    // The reader opens a message from the loaded list.
    navigate({ page: "inbox", tab: "review", m: id(20) });
    await settle();
    const pane = document.querySelector(".list-pane") as HTMLElement;

    // Scrolled so rows 17..23 or so are on screen, the open row and the one
    // after it comfortably inside.
    const at = rowIndexOfId(id(20));
    expect(at).toBeGreaterThan(0);
    pane.scrollTop = (at - 3) * ROW_H;
    await settle();
    const before = scrollTop;
    const nextAt = rowIndexOfId(id(21));
    expect(CHROME_H + nextAt * ROW_H).toBeGreaterThanOrEqual(before + CHROME_H);
    expect(CHROME_H + (nextAt + 1) * ROW_H).toBeLessThanOrEqual(before + VIEWPORT);

    // Every value the pane is scrolled to between the click and the end.
    const visited: number[] = [];
    pane.addEventListener("scroll", () => visited.push(scrollTop));

    let open!: () => void;
    archiveGate = new Promise<void>((r) => (open = r));
    (document.querySelector(".thread-pane .archive") as HTMLButtonElement).click();
    await settle();
    // The overlay has dropped the row and the archive is still awaiting.
    open();
    await settle();
    await settle();

    // The pane advanced to the next message...
    expect(document.querySelector(".thread-pane")?.getAttribute("data-open")).toBe(id(21));
    expect(document.querySelector("[role=grid]")?.getAttribute("aria-activedescendant")).toMatch(
      new RegExp(`-item-r-${id(21)}$`),
    );
    // ...and the list did not move — not even and back.
    expect(visited).toEqual([]);
    expect(scrollTop).toBe(before);
  });
});
