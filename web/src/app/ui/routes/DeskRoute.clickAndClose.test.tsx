// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// A PLAIN CLICK, AND CLOSING THE READING PANE — two operator reports on 1.86.0.
//
// 1. "The ctrl/shift+click-selected emails aren't clearing if I click another
//    message without holding ctrl or shift." A selection built with modified
//    clicks (#708) survived a plain click on another row, and `targets` prefers
//    the selection over the open message, so the next verb acted on rows the
//    user thought they had left.
//
// 2. "Pressing Esc doesn't clear it. It does jump me around between messages I've
//    opened." Every opening PUSHED a history entry, and closing is one step BACK
//    (routing.ts leaveTo) — so closing C showed B, then A.
//
// Both are facts about the ROUTE (its selection, its navigation), so the route is
// mounted here for real with a real MailStore, the same harness as
// ./DeskRoute.archiveScroll.test.tsx. The history half is also checked in a real
// browser by scripts/a11y-check.cjs ("the reading pane closes in one step"):
// jsdom's session history is an implementation of the spec, not Chromium's.
// ⚠ jsdom has no layout: the geometry is the same stub VirtualList.test.tsx uses.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { MailStore } from "../../../core/store";
import { sectionKey } from "../../../core/desk";
import type { Area, Email, Mailbox } from "../../../core/jmap/types";

const ROW_H = 68;
const CHROME_H = 160;
const VIEWPORT = 640;
const N = 12;

const store = new MailStore(null);

const actions = {
  archive: vi.fn(async () => true),
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
  // The pane's own ← is `onClose` (ui/pane/PaneToolbar.tsx `.pane-back`).
  ThreadPane: ({ email, onClose }: { email: { id: string }; onClose: () => void }) => (
    <div className="thread-pane" data-open={email.id}>
      <button type="button" className="pane-back" onClick={onClose}>
        Back
      </button>
    </div>
  ),
}));

import { currentRoute, escapeNow, navigate } from "../../routing";
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
      receivedAt: new Date(Date.UTC(2026, 9, 1) - i * 60_000).toISOString(),
      keywords: { $seen: true },
      size: 1,
      from: [{ name: `Sender ${i}`, email: `s${i}@example.com` }],
      subject: `Message ${i}`,
      preview: "",
    } as unknown as Email);
  }
  store.putEmails(emails);
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

let restore: (() => void)[] = [];

function stubGeometry(): void {
  const proto = HTMLElement.prototype;
  const saved = {
    clientHeight: Object.getOwnPropertyDescriptor(proto, "clientHeight"),
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
  proto.getBoundingClientRect = function (this: HTMLElement): DOMRect {
    const rect = (top: number, height: number): DOMRect =>
      ({ top, height, bottom: top + height, left: 0, right: 0, width: 0, x: 0, y: top }) as DOMRect;
    if (isPane(this)) return rect(0, VIEWPORT);
    if (this.classList.contains("desk-probe")) return rect(0, ROW_H);
    if (this.classList.contains("desk-list")) return rect(CHROME_H, 0);
    return rect(0, 0);
  };
  globalThis.ResizeObserver = class {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  };
  restore.push(() => {
    if (saved.clientHeight) Object.defineProperty(proto, "clientHeight", saved.clientHeight);
    proto.getBoundingClientRect = saved.rect;
    globalThis.ResizeObserver = saved.observer;
  });
}

/** A frame, then a task — long enough for a measurement, its render, and a
 * hashchange the navigation queued. */
const settle = (): Promise<void> =>
  new Promise((resolve) => requestAnimationFrame(() => setTimeout(resolve, 0)));

function cell(i: number, which: "main" | "sender"): HTMLElement {
  const found = document.querySelector<HTMLElement>(`li[id$="-item-r-${id(i)}"] .desk-${which}`);
  if (!found) throw new Error(`no ${which} cell for ${id(i)}`);
  return found;
}

async function click(target: HTMLElement, mods: MouseEventInit = {}): Promise<void> {
  flushSync(() => {
    target.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, ...mods }));
  });
  await settle();
}

/** The ids the grid says are selected — `aria-selected` is the selection, read
 * the way assistive technology reads it. */
function selectedIds(): string[] {
  return [...document.querySelectorAll('li[role=row][aria-selected="true"]')]
    .map((li) => li.id.replace(/^.*-item-r-/, ""))
    .sort();
}

function openId(): string | null {
  const r = currentRoute() as { m?: string };
  return r.m ?? null;
}

describe("the desk's row clicks and its reading pane", () => {
  let root: Root;

  beforeEach(async () => {
    restore = [];
    // The pushed-entry count lives in sessionStorage (routing.ts ownDepth);
    // each test starts on a list with nothing of ours beneath it.
    window.sessionStorage.clear();
    seed();
    stubGeometry();
    navigate({ page: "inbox", tab: "review" }, { replace: true });
    const app = document.createElement("div");
    document.body.replaceChildren(app);
    root = createRoot(app);
    flushSync(() => root.render(<DeskRoute />));
    await settle();
    await settle();
  });

  afterEach(() => {
    flushSync(() => root.unmount());
    for (const undo of restore) undo();
    navigate({ page: "inbox", tab: null }, { replace: true });
  });

  describe("a plain click on a message clears the ticked selection", () => {
    it("a plain click on another row's message opens it and clears a Ctrl-click selection", async () => {
      await click(cell(1, "main"), { ctrlKey: true });
      await click(cell(2, "main"), { ctrlKey: true });
      expect(selectedIds()).toEqual([id(1), id(2)]);

      await click(cell(5, "main"));

      expect(openId()).toBe(id(5));
      expect(selectedIds()).toEqual([]);
    });

    it("clears a Shift-click range too", async () => {
      await click(cell(1, "sender"));
      await click(cell(4, "main"), { shiftKey: true });
      expect(selectedIds()).toEqual([id(1), id(2), id(3), id(4)]);

      await click(cell(7, "main"));

      expect(selectedIds()).toEqual([]);
    });

    it("a Ctrl-click and a Shift-click add to the selection and open nothing", async () => {
      await click(cell(1, "main"), { ctrlKey: true });
      await click(cell(3, "main"), { shiftKey: true });
      await click(cell(6, "main"), { ctrlKey: true });

      expect(selectedIds()).toEqual([id(1), id(2), id(3), id(6)]);
      expect(openId()).toBeNull();
    });

    it("a plain click on the sender still toggles that row and keeps the rest (#375)", async () => {
      await click(cell(1, "main"), { ctrlKey: true });
      await click(cell(3, "sender"));
      expect(selectedIds()).toEqual([id(1), id(3)]);

      await click(cell(3, "sender"));
      expect(selectedIds()).toEqual([id(1)]);
      expect(openId()).toBeNull();
    });

    it("the keyboard is unaffected: Space selects and Enter opens without clearing", async () => {
      const grid = document.querySelector<HTMLElement>("[role=grid]");
      if (!grid) throw new Error("no grid");
      const key = async (k: string): Promise<void> => {
        flushSync(() => {
          grid.dispatchEvent(new KeyboardEvent("keydown", { key: k, bubbles: true, cancelable: true }));
        });
        await settle();
      };
      grid.focus();
      // The cursor starts on the first row; select it and the next one.
      await key(" ");
      await key("ArrowDown");
      await key(" ");
      expect(selectedIds()).toEqual([id(0), id(1)]);

      await key("Enter");

      expect(openId()).toBe(id(1));
      expect(selectedIds()).toEqual([id(0), id(1)]);
    });
  });

  describe("closing the reading pane closes it, however many messages it showed", () => {
    const openThree = async (): Promise<number> => {
      const before = window.history.length;
      await click(cell(1, "main"));
      await click(cell(2, "main"));
      await click(cell(3, "main"));
      expect(openId()).toBe(id(3));
      expect(document.querySelector(".thread-pane")?.getAttribute("data-open")).toBe(id(3));
      return before;
    };

    it("opening one message after another adds ONE history entry, not one each", async () => {
      const before = await openThree();
      expect(window.history.length).toBe(before + 1);
    });

    it("the pane's close button returns to the list, not to the previous message", async () => {
      await openThree();

      await click(document.querySelector<HTMLElement>(".thread-pane .pane-back") as HTMLElement);
      await settle();

      expect(openId()).toBeNull();
      expect(document.querySelector(".thread-pane")).toBeNull();
    });

    it("Escape returns to the list, not to the previous message", async () => {
      await openThree();

      // What the shell's Escape does once nothing smaller is open (ui/Shell.tsx).
      expect(escapeNow(currentRoute())).toBe(true);
      await settle();
      await settle();

      expect(openId()).toBeNull();
      expect(document.querySelector(".thread-pane")).toBeNull();
    });
  });
});
