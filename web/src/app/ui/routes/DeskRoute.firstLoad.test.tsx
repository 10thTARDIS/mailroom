// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE FIRST DESK LOAD GOES OUT BEFORE THE AREAS ARE KNOWN (#673, item 2).
//
// On boot the session lands (and `accountId` with it) BEFORE the reference
// data, which is where the areas come from. The desk route mounts in between,
// finds no areas, and asks for the tray plus Needs sorting alone. Nothing about
// that is wrong by itself; the question is where the CURSOR ends up. The route
// seeds the cursor from the first row it has, and the first row it has is a row
// of Needs sorting, so once the areas arrive the cursor sits at the BOTTOM of
// the desk while the top of it is the first area's surface.
//
// Mounted for real over the REAL SyncEngine and a client that answers with
// realistic delays (reference data slow, the desk fast), not an instant fake:
// the order of the two answers is the whole bug. ⚠ jsdom has no layout; the
// geometry is the same stub DeskRoute.archiveScroll.test.tsx uses.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { MailStore } from "../../../core/store";
import { SyncEngine } from "../../../core/sync";
import type { Call, CallResult, JmapClient } from "../../../core/jmap/client";

const ROW_H = 68;
const CHROME_H = 160;
const VIEWPORT = 640;

const store = new MailStore(null);
let engine: SyncEngine;

const actions = {
  archive: vi.fn(async () => true),
  restoreToInbox: vi.fn(async () => true),
  setSeen: vi.fn(async () => {}),
};

vi.mock("../../state", () => ({
  useApp: () => ({ actions, sync: engine, pushToast: () => {}, offerUndo: () => {} }),
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
vi.mock("../ThreadPane", () => ({ ThreadPane: () => null }));

import { navigate } from "../../routing";
import { DeskRoute } from "./DeskRoute";

const ACCOUNT = "U1";
const AREA_IDS = ["a00", "a01", "a02"];
const UNSORTED_IDS = ["u00", "u01", "u02"];

const msg = (id: string) => ({
  id,
  blobId: "b",
  threadId: `t-${id}`,
  mailboxIds: { "mb-in": true },
  receivedAt: "2026-10-01T00:00:00Z",
  keywords: { $seen: true },
  size: 1,
  from: [{ name: "Someone", email: "someone@example.com" }],
  subject: id,
  preview: "",
});

/** A server whose reference data is SLOW and whose desk is FAST — what a real
 * mailbox looks like on a cold boot: the areas are one of a dozen reads, the
 * desk is one request. */
class SlowReferenceClient {
  readonly calls: Call[][] = [];
  get accountId(): string {
    return ACCOUNT;
  }
  pushUrl(): string {
    return "/jmap/eventsource/";
  }
  vapidPublicKey(): null {
    return null;
  }
  async connect() {
    return { capabilities: { "urn:ietf:params:jmap:core": { maxCallsInRequest: 64 } } } as never;
  }
  request(calls: Call[]): Promise<CallResult[]> {
    this.calls.push(calls);
    const isReference = calls.some((c) => c.name === "Mailbox/get");
    const delay = isReference ? 150 : 25;
    return new Promise((resolve) => {
      setTimeout(() => resolve(calls.map((c) => this.answer(c))), delay);
    });
  }
  private answer(c: Call): CallResult {
    const ok = (value: unknown): CallResult => ({ id: c.id, name: c.name, error: null, value }) as never;
    switch (c.name) {
      case "Mailbox/get":
        return ok({
          accountId: ACCOUNT,
          state: "m1",
          list: [{ id: "mb-in", accountId: "A1", name: "Inbox", parentId: null, role: "inbox", sortOrder: 0 }],
        });
      case "Area/get":
        return ok({
          accountId: ACCOUNT,
          state: "ar1",
          list: [{ id: "AR1", name: "Work", position: 0, labelId: "L1", tabs: [], labelIds: [] }],
        });
      case "Area/getSummary":
        return ok({ accountId: ACCOUNT, state: "s", threshold: 0.4, list: [] });
      case "Email/query": {
        const f = (c.args["filter"] ?? {}) as Record<string, unknown>;
        const ids = c.id === "all" ? [] : f["unsorted"] ? UNSORTED_IDS : f["inArea"] ? AREA_IDS : [];
        return ok({ ids, total: ids.length, queryState: "q1", position: 0 });
      }
      case "Email/get": {
        const ids = [...AREA_IDS, ...UNSORTED_IDS];
        return ok({ accountId: ACCOUNT, state: "e1", list: ids.map(msg), notFound: [] });
      }
      default:
        return ok({ accountId: ACCOUNT, state: "x", list: [], notFound: [] });
    }
  }
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

const wait = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

describe("the first desk load before the areas are known (#673)", () => {
  let root: Root;
  let client: SlowReferenceClient;

  beforeEach(() => {
    scrollTop = 0;
    restore = [];
    stubGeometry();
    client = new SlowReferenceClient();
    engine = new SyncEngine(client as unknown as JmapClient, store, {
      push: null,
      setTimer: () => 0,
      clearTimer: () => {},
    });
    const app = document.createElement("div");
    document.body.replaceChildren(app);
    root = createRoot(app);
  });

  afterEach(() => {
    flushSync(() => root.unmount());
    engine.stop();
    for (const undo of restore) undo();
    navigate({ page: "inbox", tab: null }, { replace: true });
  });

  it("puts the cursor on the top of the desk, not on Needs sorting", async () => {
    navigate({ page: "inbox", tab: "review" }, { replace: true });
    flushSync(() => root.render(<DeskRoute />));
    // The boot sequence, as App does it: connect, reference data, then a view.
    const started = engine.start({ kind: "role", role: "inbox" });
    // Long enough for the desk to answer BEFORE the reference data does.
    await wait(60);
    const early = document.querySelector("[role=grid]")?.getAttribute("aria-activedescendant") ?? "";
    await started;
    await wait(200);

    // Every desk ask names the area (tray + area + Needs sorting). One with only
    // the tray and Needs sorting is the load that went out before the areas were
    // known; it is what seeds the cursor on the wrong section.
    const sizes = client.calls
      .map((cs) => cs.filter((c) => c.name === "Email/query" && c.id !== "all").length)
      .filter((n) => n > 0);
    expect(sizes.length).toBeGreaterThan(0);
    expect(sizes).not.toContain(2);
    expect(sizes).toContain(3);

    const grid = document.querySelector("[role=grid]");
    expect(document.querySelector(`li[id$="-item-r-a00"]`)).not.toBeNull();
    expect(document.querySelector(`li[id$="-item-r-u00"]`)).not.toBeNull();
    const active = grid?.getAttribute("aria-activedescendant") ?? "";
    expect(early).toBe("");
    expect(active).toMatch(/-item-r-a00$/);
  });
});
