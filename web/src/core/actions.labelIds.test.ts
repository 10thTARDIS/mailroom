// SPDX-License-Identifier: AGPL-3.0-only

// Local-only labels, applied by id (#704, ADR-031).
//
// The tests that earn their place are the ROLLBACK and RETIRE ones: the label
// menu reads the overlay, so an intent the server refused must vanish (or the
// checkbox lies "on" forever), and one the server confirmed must retire (or the
// message is "pending" forever).

import { describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import type { Call, CallResult, JmapClient } from "./jmap/client";
import type { Email, Label } from "./jmap/types";
import { StorageActionQueue } from "./queue";
import { MemoryStorage } from "./storage";
import { MailStore, effectiveEmail } from "./store";
import type { SyncEngine } from "./sync";

const ACCOUNT = "U1";

function label(id: string, keyword: string | null): Label {
  return { id, name: id, color: null, parentId: null, mirrorToImap: keyword !== null, keyword, tab: null };
}

function email(id: string, over: Partial<Email> = {}): Email {
  return {
    id,
    blobId: "b" + id,
    threadId: "t" + id,
    mailboxIds: {},
    keywords: {},
    labelIds: {},
    size: 1,
    receivedAt: "2026-10-01T00:00:00Z",
    subject: id,
    preview: "",
    hasAttachment: false,
    ...over,
  } as Email;
}

function harness(opts: { refuse?: string[] } = {}) {
  const store = new MailStore(null);
  store.setSession({} as never, ACCOUNT);
  store.setConn("online");
  store.putLabels([label("L-LOCAL", null), label("L-MIRROR", "MailroomLabel-work")]);
  const errors: string[] = [];
  const sets: Call[] = [];
  const client = {
    request: async (calls: Call[]): Promise<CallResult[]> =>
      calls.map((c): CallResult => {
        sets.push(c);
        const update = (c.args["update"] as Record<string, unknown>) ?? {};
        const updated: Record<string, null> = {};
        const notUpdated: Record<string, unknown> = {};
        for (const id of Object.keys(update)) {
          if (opts.refuse?.includes(id)) {
            notUpdated[id] = { type: "invalidPatch", description: "refused for the test" };
          } else {
            updated[id] = null;
          }
        }
        return { id: c.id, name: c.name, value: { updated, notUpdated, newState: "e2" }, error: null };
      }),
  } as unknown as JmapClient;
  const sync = { poll: async () => {}, refreshDesk: async () => {}, refreshView: async () => {} } as unknown as SyncEngine;
  const queue = new StorageActionQueue(new MemoryStorage());
  const actions = new MailActions(client, store, queue, sync, { onError: (m) => errors.push(m) });
  return { store, actions, queue, errors, sets };
}

const on = (h: ReturnType<typeof harness>, id: string, lid: string): boolean =>
  effectiveEmail(h.store.getState(), id)?.labelIds[lid] === true;

describe("MailActions.setLabelById (#704)", () => {
  it("queues labelIds/<id> — never a keyword — and shows the label at once", async () => {
    const h = harness();
    h.store.putEmails([email("m1"), email("m2")]);
    const drain = h.actions.drain.bind(h.actions);
    h.actions.drain = async () => {};
    await h.actions.setLabelById(["m1", "m2"], "L-LOCAL", true);
    h.actions.drain = drain;

    const queued = await h.queue.list();
    expect(queued.map((a) => a.targetId)).toEqual(["m1", "m2"]);
    expect(queued[0]?.payload).toEqual({ "labelIds/L-LOCAL": true });
    expect(on(h, "m1", "L-LOCAL")).toBe(true);
    expect(effectiveEmail(h.store.getState(), "m1")?.pending).toBe(true);
    // A local label is never a keyword, optimistically or otherwise.
    expect(effectiveEmail(h.store.getState(), "m1")?.labelKeywords).toEqual([]);
  });

  it("removal sends null and hides the label at once", async () => {
    const h = harness();
    h.store.putEmails([email("m1", { labelIds: { "L-LOCAL": true } })]);
    h.actions.drain = async () => {};
    await h.actions.setLabelById(["m1"], "L-LOCAL", false);
    expect((await h.queue.list())[0]?.payload).toEqual({ "labelIds/L-LOCAL": null });
    expect(on(h, "m1", "L-LOCAL")).toBe(false);
  });

  it("ROLLS BACK when the server refuses, leaving no pending intent", async () => {
    const h = harness({ refuse: ["m1"] });
    h.store.putEmails([email("m1"), email("m2")]);
    await h.actions.setLabelById(["m1", "m2"], "L-LOCAL", true);
    await h.actions.drain();
    expect(on(h, "m1", "L-LOCAL")).toBe(false);
    expect(h.store.getState().overlay["m1"]?.labels).toBeUndefined();
    // The accepted one stays applied until the server's Email agrees.
    expect(on(h, "m2", "L-LOCAL")).toBe(true);
    expect(h.errors.join(" ")).toContain("Label added");
  });

  it("RETIRES the intent once the server's labelIds agrees", async () => {
    const h = harness();
    h.store.putEmails([email("m1")]);
    h.actions.drain = async () => {};
    await h.actions.setLabelById(["m1"], "L-LOCAL", true);
    // A poll that has not seen the write yet: still pending.
    h.store.putEmails([email("m1")]);
    expect(h.store.getState().overlay["m1"]?.labels).toEqual({ "L-LOCAL": true });
    // The server now reports it.
    h.store.putEmails([email("m1", { labelIds: { "L-LOCAL": true } })]);
    // The label intent is gone. (`answered` is not retired by putEmails at all
    // — store.ts says why — so the entry itself may remain for that.)
    expect(h.store.getState().overlay["m1"]?.labels).toBeUndefined();
    expect(on(h, "m1", "L-LOCAL")).toBe(true);
  });

  it("a refused KEYWORD on the same message does not drop a pending label intent", () => {
    const h = harness();
    h.store.putEmails([email("m1")]);
    h.store.applyLabelId(["m1"], "L-LOCAL", true);
    h.store.applyKeyword(["m1"], "$seen", true);
    h.store.revertKeyword(["m1"], "$seen");
    expect(on(h, "m1", "L-LOCAL")).toBe(true);
  });
});

describe("MailActions.applyLabel (#704)", () => {
  it("routes a MIRRORED label through its keyword atom, unchanged", async () => {
    const h = harness();
    h.store.putEmails([email("m1")]);
    h.actions.drain = async () => {};
    await h.actions.applyLabel(["m1"], label("L-MIRROR", "MailroomLabel-work"), true);
    expect((await h.queue.list())[0]?.payload).toEqual({ "keywords/MailroomLabel-work": true });
    const e = effectiveEmail(h.store.getState(), "m1");
    expect(e?.labelKeywords).toEqual(["MailroomLabel-work"]);
    // …and the id view agrees, through the atom, without a second request.
    expect(e?.labelIds["L-MIRROR"]).toBe(true);
  });

  it("routes a LOCAL label through labelIds/<id>", async () => {
    const h = harness();
    h.store.putEmails([email("m1")]);
    h.actions.drain = async () => {};
    await h.actions.applyLabel(["m1"], label("L-LOCAL", null), true);
    expect((await h.queue.list())[0]?.payload).toEqual({ "labelIds/L-LOCAL": true });
  });
});

describe("MailActions.relabel (#704)", () => {
  it("sends ONE patch per message carrying every add and remove, each on its own path", async () => {
    const h = harness();
    h.store.putEmails([email("m1", { labelIds: { "L-MIRROR": true }, keywords: { "MailroomLabel-work": true } })]);
    h.actions.drain = async () => {};
    await h.actions.relabel(
      [{ id: "m1", add: [label("L-LOCAL", null)], remove: [label("L-MIRROR", "MailroomLabel-work")] }],
      "Filed",
    );
    const queued = await h.queue.list();
    expect(queued).toHaveLength(1);
    expect(queued[0]?.payload).toEqual({ "keywords/MailroomLabel-work": null, "labelIds/L-LOCAL": true });
    expect(on(h, "m1", "L-LOCAL")).toBe(true);
    expect(on(h, "m1", "L-MIRROR")).toBe(false);
  });

  it("a refusal rolls BOTH halves back", async () => {
    const h = harness({ refuse: ["m1"] });
    h.store.putEmails([email("m1", { labelIds: { "L-MIRROR": true }, keywords: { "MailroomLabel-work": true } })]);
    await h.actions.relabel(
      [{ id: "m1", add: [label("L-LOCAL", null)], remove: [label("L-MIRROR", "MailroomLabel-work")] }],
      "Filed",
    );
    await h.actions.drain();
    expect(on(h, "m1", "L-LOCAL")).toBe(false);
    expect(on(h, "m1", "L-MIRROR")).toBe(true);
    const ov = h.store.getState().overlay["m1"];
    expect(ov?.labels).toBeUndefined();
    expect(ov?.keywords ?? {}).toEqual({});
  });

  it("queues nothing for a message with nothing to change", async () => {
    const h = harness();
    h.actions.drain = async () => {};
    await h.actions.relabel([{ id: "m1", add: [], remove: [] }], "Filed");
    expect(await h.queue.list()).toHaveLength(0);
  });
});
