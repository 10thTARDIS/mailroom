// SPDX-License-Identifier: AGPL-3.0-only

// Moving a selection (or an all-matching / whole-sender set) into an area (#708).
//
// Real MailStore, real refileChanges; only the network (loadEmails) and the
// write (relabel) are stand-ins. `relabel` APPLIES its changes to the store the
// way MailActions.relabel does (local-only labels ride labelIds/<id>), so "Undo
// restores exactly" is asserted on the resulting label sets, not on a mock call.

import { describe, expect, it, vi } from "vitest";

import type { LabelChange } from "../../core/actions";
import type { Area, Email, Id, Label } from "../../core/jmap/types";
import { MailStore, effectiveEmail } from "../../core/store";
import { moveToArea } from "./moveToArea";

function label(id: string): Label {
  return { id, name: id, color: null, parentId: null, mirrorToImap: false, keyword: null, tab: null };
}
function area(id: string, name: string, position: number, labelId: string): Area {
  return { id, name, position, labelId, tabs: [], labelIds: [], autoArchive: true, autoArchiveDays: null, hiddenFromRail: false };
}
function email(id: string, labelIds: Id[]): Email {
  return {
    id,
    blobId: "b" + id,
    threadId: "t" + id,
    mailboxIds: {},
    keywords: {},
    labelIds: Object.fromEntries(labelIds.map((l) => [l, true])),
    size: 1,
    receivedAt: "2026-10-01T00:00:00Z",
    subject: id,
    preview: "",
    hasAttachment: false,
  } as Email;
}

const labelsOf = (store: MailStore, id: Id): string =>
  Object.keys(effectiveEmail(store.getState(), id)?.labelIds ?? {})
    .sort()
    .join(",");

function harness(loaded: Email[], onServer: Email[] = []) {
  const store = new MailStore(null);
  store.putLabels([label("L-WORK"), label("L-NEWS")]);
  store.putAreas([area("A-WORK", "Work", 1, "L-WORK"), area("A-NEWS", "News", 2, "L-NEWS")]);
  store.putEmails(loaded);
  const relabel = vi.fn(async (changes: LabelChange[]) => {
    for (const c of changes) {
      for (const l of c.remove) store.applyLabelId([c.id], l.id, false);
      for (const l of c.add) store.applyLabelId([c.id], l.id, true);
    }
  });
  const loadEmails = vi.fn(async (ids: Id[]) => {
    const have = new Map(onServer.map((e) => [e.id, e]));
    store.putEmails(ids.flatMap((id) => (have.has(id) ? [have.get(id) as Email] : [])));
    return { notFound: ids.filter((id) => !have.has(id)), failed: false };
  });
  const undos: Array<{ label: string; inverse: () => Promise<boolean> }> = [];
  const toasts: string[] = [];
  const deps = {
    store,
    sync: { loadEmails },
    actions: { relabel },
    offerUndo: (label: string, inverse: () => Promise<boolean>) => void undos.push({ label, inverse }),
    pushToast: (t: { message: string }) => (toasts.push(t.message), 1),
  };
  return { store, relabel, loadEmails, undos, toasts, deps };
}

describe("moveToArea (#708)", () => {
  it("swaps labels for every selected message, counts already-there as no-ops, one exact Undo", async () => {
    const h = harness([
      email("m1", ["L-NEWS"]), // in News -> moves to Work
      email("m2", []), // nowhere -> Work
      email("m3", ["L-WORK"]), // already exactly in Work -> no-op
    ]);
    const out = await moveToArea(h.deps, ["m1", "m2", "m3"], "A-WORK");
    expect(out).toMatchObject({ moved: 2, alreadyThere: 1, unreadable: 0, truncated: false });
    // The Email/set changes are the desk's swap: News off, Work on.
    const sent = h.relabel.mock.calls[0]![0];
    expect(sent.map((c) => [c.id, c.add.map((l) => l.id), c.remove.map((l) => l.id)])).toEqual([
      ["m1", ["L-WORK"], ["L-NEWS"]],
      ["m2", ["L-WORK"], []],
    ]);
    expect(labelsOf(h.store, "m1")).toBe("L-WORK");
    expect(labelsOf(h.store, "m3")).toBe("L-WORK");
    // ONE toast with an Undo; it says how many moved and that one was already there.
    expect(h.undos).toHaveLength(1);
    expect(h.undos[0]!.label).toBe("Moved 2 messages to Work. 1 already in Work.");
    // Undo restores each message's prior label set exactly (m3 untouched).
    await h.undos[0]!.inverse();
    expect(labelsOf(h.store, "m1")).toBe("L-NEWS");
    expect(labelsOf(h.store, "m2")).toBe("");
    expect(labelsOf(h.store, "m3")).toBe("L-WORK");
  });

  it("reads messages that are not loaded before swapping (all-matching ids are mostly unloaded)", async () => {
    const server = [email("a1", ["L-NEWS"]), email("a2", [])];
    const h = harness([], server);
    const out = await moveToArea(h.deps, ["a1", "a2"], "A-WORK");
    expect(h.loadEmails).toHaveBeenCalledWith(["a1", "a2"]);
    expect(out?.moved).toBe(2);
    expect(labelsOf(h.store, "a1")).toBe("L-WORK");
  });

  it("fetches in bounded chunks for 1,000 ids", async () => {
    const server = Array.from({ length: 1000 }, (_, i) => email(`b${i}`, []));
    const h = harness([], server);
    const out = await moveToArea(
      h.deps,
      server.map((e) => e.id),
      "A-WORK",
    );
    expect(out?.moved).toBe(1000);
    expect(h.loadEmails.mock.calls.map((c) => c[0].length)).toEqual([200, 200, 200, 200, 200]);
    expect(h.relabel.mock.calls[0]![0]).toHaveLength(1000);
  });

  it("says plainly when the all-matching cap truncated: 'Moved 1,000 of 1,240'", async () => {
    const server = Array.from({ length: 1000 }, (_, i) => email(`c${i}`, []));
    const h = harness([], server);
    const out = await moveToArea(
      h.deps,
      server.map((e) => e.id),
      "A-WORK",
      1240,
    );
    expect(out).toMatchObject({ moved: 1000, total: 1240, truncated: true });
    expect(h.undos[0]!.label).toBe("Moved 1,000 of 1,240 messages to Work. 240 more were not touched.");
  });

  it("counts messages it could not read instead of dropping them silently", async () => {
    const h = harness([], [email("d1", [])]);
    const out = await moveToArea(h.deps, ["d1", "gone"], "A-WORK");
    expect(out).toMatchObject({ moved: 1, unreadable: 1 });
    expect(h.undos[0]!.label).toContain("1 could not be read and were skipped.");
  });

  it("an all-already-there selection is a notice, not an error, and offers no Undo", async () => {
    const h = harness([email("e1", ["L-WORK"]), email("e2", ["L-WORK"])]);
    const out = await moveToArea(h.deps, ["e1", "e2"], "A-WORK");
    expect(out).toMatchObject({ moved: 0, alreadyThere: 2 });
    expect(h.relabel).not.toHaveBeenCalled();
    expect(h.undos).toHaveLength(0);
    expect(h.toasts).toEqual(["Nothing to move to Work. 2 already in Work."]);
  });

  it("a failed read moves nothing and says so", async () => {
    const h = harness([]);
    h.loadEmails.mockResolvedValueOnce({ notFound: [], failed: true });
    expect(await moveToArea(h.deps, ["x"], "A-WORK")).toBeNull();
    expect(h.relabel).not.toHaveBeenCalled();
    expect(h.toasts).toEqual(["Could not read those messages, so nothing was moved."]);
  });
});
