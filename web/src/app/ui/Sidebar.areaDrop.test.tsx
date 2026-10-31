// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// DROPPING MAIL ON AN AREA IN THE RAIL MOVES IT (#728).
//
// The rail's area entries were not drop targets, and an area's own label is
// hidden from the Labels list, so the only "drop on an area" was an add of the
// label -- which leaves a message in its earlier area (the earliest area wins).
// A drop on an area now swaps the other areas' labels out through the one path
// Re-file and Move to area use (core/refile.ts via ui/moveToArea.ts), with one
// Undo. The real Sidebar is mounted over a real MailStore; only the network is
// a stand-in.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { LabelChange } from "../../core/actions";
import { DRAG_MIME, canDrop, encodeDrag } from "../../core/dragDrop";
import type { Area, Email, Id, Label } from "../../core/jmap/types";
import { MailStore, effectiveEmail } from "../../core/store";

const store = new MailStore(null);
const relabel = vi.fn(async (changes: LabelChange[]) => {
  for (const c of changes) {
    for (const l of c.remove) store.applyLabelId([c.id], l.id, false);
    for (const l of c.add) store.applyLabelId([c.id], l.id, true);
  }
});
const applyLabel = vi.fn(async () => {});
const undos: Array<{ label: string; inverse: () => Promise<boolean> }> = [];

vi.mock("../state", () => ({
  useApp: () => ({
    actions: new Proxy({ relabel, applyLabel }, { get: (t, k) => (k in t ? t[k as keyof typeof t] : vi.fn(async () => {})) }),
    sync: { loadEmails: async () => ({ notFound: [], failed: false }) },
    store,
    pushToast: () => 1,
    offerUndo: (label: string, inverse: () => Promise<boolean>) => void undos.push({ label, inverse }),
  }),
  useMailState: () => useSyncExternalStore(store.subscribe, store.getState),
}));
vi.mock("../routing", () => ({
  useRoute: () => ({ page: "all" }),
  useListRoute: () => ({ page: "all" }),
}));
vi.mock("./useAccounts", () => ({ useAccounts: () => ({ accounts: [] }) }));
vi.mock("./AccountSwitcher", () => ({ AccountSwitcher: () => null }));

import { Sidebar } from "./Sidebar";

function label(id: string, name: string): Label {
  return { id, name, color: null, parentId: null, mirrorToImap: false, keyword: null, tab: null };
}
function area(id: string, name: string, position: number, labelId: string): Area {
  return { id, name, position, labelId, tabs: [], labelIds: [], autoArchive: true, autoArchiveDays: null, hiddenFromRail: false } as Area;
}
function email(id: string, labelIds: Id[]): Email {
  return {
    id,
    blobId: "b",
    threadId: id,
    mailboxIds: { "mb-in": true },
    keywords: {},
    labelIds: Object.fromEntries(labelIds.map((l) => [l, true])),
    size: 1,
    receivedAt: "2026-10-01T00:00:00Z",
    subject: id,
    preview: "",
    hasAttachment: false,
  } as unknown as Email;
}
const labelsOf = (id: Id): string =>
  Object.keys(effectiveEmail(store.getState(), id)?.labelIds ?? {})
    .sort()
    .join(",");
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

/** A drop event carrying `ids`, the way MessageList's dragstart packs them. */
function dropEvent(type: string, ids: Id[]): Event {
  const ev = new Event(type, { bubbles: true, cancelable: true });
  const data = new Map([[DRAG_MIME, encodeDrag(ids)]]);
  Object.defineProperty(ev, "dataTransfer", {
    value: { getData: (k: string) => data.get(k) ?? "", dropEffect: "none" },
  });
  return ev;
}
const entry = (name: string): HTMLElement => {
  const found = [...document.querySelectorAll<HTMLElement>("button.nav-item")].find((n) =>
    n.querySelector(".nav-item-label")?.textContent === name,
  );
  if (!found) throw new Error("no rail entry " + name);
  return found;
};

describe("Dropping on an area in the rail moves the mail (#728)", () => {
  let root: Root;
  let dragIds: Id[];
  beforeEach(() => {
    undos.length = 0;
    relabel.mockClear();
    applyLabel.mockClear();
    store.setSession({ capabilities: {} } as never, "U1");
    store.putLabels([label("L-PRO", "Promotions"), label("L-UPD", "Updates"), label("L-WORK", "Work")]);
    store.putAreas([area("A-PRO", "Promotions", 2, "L-PRO"), area("A-UPD", "Updates", 3, "L-UPD")], true);
    store.putEmails([email("m1", ["L-PRO", "L-WORK"]), email("m2", [])]);
    dragIds = ["m1", "m2"];
    const host = document.createElement("div");
    document.body.replaceChildren(host);
    root = createRoot(host);
    flushSync(() =>
      root.render(
        <Sidebar
          view={{ kind: "role", role: "inbox" }}
          onNavigate={() => {}}
          onCompose={() => {}}
          dragIds={dragIds}
          showSwitcher={false}
        />,
      ),
    );
  });
  afterEach(() => {
    flushSync(() => root.unmount());
  });

  it("an area entry accepts the drag, and the drop SWAPS labels; Undo restores exactly", async () => {
    const target = entry("Updates");
    const over = dropEvent("dragover", dragIds);
    flushSync(() => void target.dispatchEvent(over));
    expect(over.defaultPrevented).toBe(true); // preventDefault is what makes it a drop target

    flushSync(() => void target.dispatchEvent(dropEvent("drop", dragIds)));
    await settle();
    // Not the add-only path: m1 left Promotions, kept its non-area label, gained Updates.
    expect(applyLabel).not.toHaveBeenCalled();
    expect(labelsOf("m1")).toBe("L-UPD,L-WORK");
    expect(labelsOf("m2")).toBe("L-UPD");
    expect(undos).toHaveLength(1);

    await undos[0]!.inverse();
    expect(labelsOf("m1")).toBe("L-PRO,L-WORK");
    expect(labelsOf("m2")).toBe("");
  });

  it("refuses a drag that would change nothing (already exactly there)", () => {
    // m1 and m2 both exactly in Updates.
    flushSync(() => {
      store.applyLabelId(["m1"], "L-PRO", false);
      store.applyLabelId(["m1", "m2"], "L-UPD", true);
    });
    expect(labelsOf("m1")).toBe("L-UPD,L-WORK");
    const over = dropEvent("dragover", dragIds);
    flushSync(() => void entry("Updates").dispatchEvent(over));
    expect(over.defaultPrevented).toBe(false);
  });
});

describe("canDrop for an area target (#728)", () => {
  const target = { kind: "area", areaId: "A-UPD", labelId: "L-UPD" } as const;
  const ctx = (on: string[]) => ({
    ids: ["m"],
    currentRole: null,
    labelsOn: { m: on },
    areaLabelIds: ["L-PRO", "L-UPD"],
  });
  it("allows a message with the label but also an earlier area's (it would move)", () => {
    expect(canDrop(target, ctx(["L-UPD", "L-PRO"]))).toBe(true);
  });
  it("refuses a message with the target's label alone", () => {
    expect(canDrop(target, ctx(["L-UPD", "L-WORK"]))).toBe(false);
  });
  it("allows a message with no area label", () => {
    expect(canDrop(target, ctx([]))).toBe(true);
  });
});
