// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The label menu offers LOCAL-ONLY labels (#704). Until then every such label —
// every area's own label — rendered disabled ("local only"), so mail could not
// be moved between areas from here at all.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { LabelChange } from "../../core/actions";
import type { Area, Email, Label } from "../../core/jmap/types";
import { MailStore, effectiveEmail } from "../../core/store";
import type { MailState } from "../../core/store";

let state: MailState;
let liveStore: MailStore;
const applyLabel = vi.fn(async () => {});
// relabel applies its changes to the store the way MailActions.relabel does, so an
// Undo is checked on the resulting label sets.
const relabel = vi.fn(async (changes: LabelChange[]) => {
  for (const c of changes) {
    for (const l of c.remove) liveStore.applyLabelId([c.id], l.id, false);
    for (const l of c.add) liveStore.applyLabelId([c.id], l.id, true);
  }
});
const undos: Array<{ label: string; inverse: () => Promise<boolean> }> = [];
const ruleSuggestion = vi.fn(async (): Promise<unknown> => null);
const toasts: Array<{ message: string }> = [];
vi.mock("../state", () => ({
  useApp: () => ({
    actions: { applyLabel, relabel, createLabel: async () => null, ruleSuggestion, settle: async () => true },
    sync: { loadEmails: async () => ({ notFound: [], failed: false }) },
    store: liveStore,
    pushToast: (t: { message: string }) => toasts.push(t),
    offerUndo: (label: string, inverse: () => Promise<boolean>) => void undos.push({ label, inverse }),
  }),
  useMailState: () => state,
}));

import { LabelMenu } from "./LabelMenu";

function label(id: string, name: string, keyword: string | null): Label {
  return { id, name, color: null, parentId: null, mirrorToImap: keyword !== null, keyword, tab: null };
}
const LOCAL = label("L-UPD", "Updates", null);
const MIRROR = label("L-WORK", "Work", "MailroomLabel-work");

function email(id: string, over: Partial<Email>): Email {
  return {
    id,
    blobId: "b",
    threadId: "t",
    mailboxIds: {},
    keywords: {},
    size: 1,
    receivedAt: "2026-10-01T00:00:00Z",
    subject: id,
    preview: "",
    hasAttachment: false,
    ...over,
  } as Email;
}

let root: Root;
let host: HTMLElement;
beforeEach(() => {
  applyLabel.mockClear();
  relabel.mockClear();
  ruleSuggestion.mockReset();
  ruleSuggestion.mockImplementation(async () => null);
  undos.length = 0;
  toasts.length = 0;
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function mount(emails: Email[], extra?: (s: MailStore) => void, onSuggestRule?: (id: string) => void): void {
  const store = new MailStore(null);
  store.putLabels([LOCAL, MIRROR]);
  store.putEmails(emails);
  extra?.(store);
  liveStore = store;
  state = store.getState();
  flushSync(() =>
    root.render(<LabelMenu ids={emails.map((e) => e.id)} onClose={() => {}} onSuggestRule={onSuggestRule} />),
  );
}

function box(name: string): HTMLInputElement {
  const choice = [...host.querySelectorAll("label.label-choice")].find((l) => l.textContent?.includes(name));
  const input = choice?.querySelector("input");
  if (!input) throw new Error("no choice for " + name);
  return input;
}

describe("LabelMenu local-only labels (#704)", () => {
  it("offers a local label as selectable, with a non-blocking 'stays in Mailroom' hint", () => {
    mount([email("m1", { labelIds: {} })]);
    expect(box("Updates").disabled).toBe(false);
    const choice = box("Updates").closest("label");
    expect(choice?.textContent).toContain("stays in Mailroom");
    expect(choice?.className).not.toContain("is-disabled");
    // A mirrored label carries no such hint.
    expect(box("Work").closest("label")?.textContent).not.toContain("stays in Mailroom");
  });

  it("reads a local label's state from labelIds", () => {
    mount([email("m1", { labelIds: { "L-UPD": true } })]);
    expect(box("Updates").checked).toBe(true);
    expect(box("Work").checked).toBe(false);
  });

  it("reads a mirrored label's state from its keyword atom, unchanged", () => {
    mount([email("m1", { keywords: { "MailroomLabel-work": true }, labelIds: { "L-WORK": true } })]);
    expect(box("Work").checked).toBe(true);
  });

  it("toggling a local label applies it (and removes it when every selected message has it)", () => {
    mount([email("m1", { labelIds: {} }), email("m2", { labelIds: { "L-UPD": true } })]);
    flushSync(() => box("Updates").click());
    expect(applyLabel).toHaveBeenCalledWith(["m1", "m2"], LOCAL, true);

    applyLabel.mockClear();
    mount([email("m1", { labelIds: { "L-UPD": true } })]);
    flushSync(() => box("Updates").click());
    expect(applyLabel).toHaveBeenCalledWith(["m1"], LOCAL, false);
  });
});

function area(id: string, name: string, position: number, labelId: string): Area {
  return { id, name, position, labelId, tabs: [], labelIds: [], autoArchive: true, autoArchiveDays: null, hiddenFromRail: false } as Area;
}
const PROMO = label("L-PRO", "Promotions", null);
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));
const labelsOf = (id: string): string =>
  Object.keys(effectiveEmail(liveStore.getState(), id)?.labelIds ?? {})
    .sort()
    .join(",");

// An area's own label is a MOVE (#728): the earliest area wins, so a plain add of
// Updates' label to a message in Promotions left it in Promotions.
describe("LabelMenu adding an area's label moves the message (#728)", () => {
  const seedAreas = (s: MailStore): void => {
    s.putLabels([PROMO]);
    s.putAreas([area("A-PRO", "Promotions", 2, "L-PRO"), area("A-UPD", "Updates", 3, "L-UPD")], true);
  };

  it("swaps the other area's label out instead of only adding, and Undo restores exactly", async () => {
    const suggested = vi.fn();
    mount(
      [email("m1", { labelIds: { "L-PRO": true, "L-WORK": true } }), email("m2", { labelIds: {} })],
      seedAreas,
      suggested,
    );
    flushSync(() => box("Updates").click());
    await settle();
    // Not the add-only path.
    expect(applyLabel).not.toHaveBeenCalled();
    expect(relabel).toHaveBeenCalledTimes(1);
    // m1 left Promotions (and kept its non-area label); m2 gained Updates.
    expect(labelsOf("m1")).toBe("L-UPD,L-WORK");
    expect(labelsOf("m2")).toBe("L-UPD");
    expect(undos).toHaveLength(1);
    expect(suggested).toHaveBeenCalledWith("L-UPD");

    await undos[0]!.inverse();
    expect(labelsOf("m1")).toBe("L-PRO,L-WORK");
    expect(labelsOf("m2")).toBe("");
  });

  // One rule prompt at a time: when the targeted offer fires, the older
  // induction prompt stands down; when it stays silent, induction runs as before.
  it("the rule-suggestion offer replaces the induction prompt, and only when it fires", async () => {
    const from = [{ name: "Alerts", email: "noreply@alerts.example" }];
    const offer = { suggest: true, reason: "mixed", address: "noreply@alerts.example", areaId: "A-UPD", filings: 3, earlier: 0 };

    ruleSuggestion.mockImplementation(async () => offer);
    const suggested = vi.fn();
    mount([email("m1", { labelIds: { "L-PRO": true }, from }), email("m2", { labelIds: {}, from })], seedAreas, suggested);
    flushSync(() => box("Updates").click());
    await settle();
    await settle();
    expect(ruleSuggestion).toHaveBeenCalledTimes(1);
    expect(toasts.some((t) => t.message.includes("noreply@alerts.example"))).toBe(true);
    expect(suggested).not.toHaveBeenCalled();

    // Control: the server says no, so the induction prompt runs.
    flushSync(() => root.unmount());
    root = createRoot(host);
    toasts.length = 0;
    ruleSuggestion.mockImplementation(async () => ({ ...offer, suggest: false, reason: "learned" }));
    const suggested2 = vi.fn();
    mount([email("m3", { labelIds: { "L-PRO": true }, from })], seedAreas, suggested2);
    flushSync(() => box("Updates").click());
    await settle();
    await settle();
    expect(suggested2).toHaveBeenCalledWith("L-UPD");
    expect(toasts.some((t) => t.message.includes("noreply@alerts.example"))).toBe(false);
  });

  it("removing an area's label is still a plain removal", () => {
    mount([email("m1", { labelIds: { "L-UPD": true } })], seedAreas);
    flushSync(() => box("Updates").click());
    expect(applyLabel).toHaveBeenCalledWith(["m1"], LOCAL, false);
    expect(relabel).not.toHaveBeenCalled();
  });

  it("a label that is not an area's still adds as before", () => {
    mount([email("m1", { labelIds: {} })], seedAreas);
    flushSync(() => box("Work").click());
    expect(applyLabel).toHaveBeenCalledWith(["m1"], MIRROR, true);
    expect(relabel).not.toHaveBeenCalled();
  });
});
