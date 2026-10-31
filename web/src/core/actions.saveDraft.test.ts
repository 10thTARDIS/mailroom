// SPDX-License-Identifier: AGPL-3.0-only

// MailActions.saveDraft recording which draft became which (#610 M2, #612).
//
// The succession is what lets the list's cursor and the composer's URL follow a
// draft across an autosave, so recording it WRONGLY is the harm to test: a
// succession written when the server refused the destroy would move the cursor
// off a draft that still exists, onto a second copy.

import { describe, expect, it } from "vitest";

import { MailActions } from "./actions";
import type { JmapClient } from "./jmap/client";
import { StorageActionQueue } from "./queue";
import { MemoryStorage } from "./storage";
import { MailStore } from "./store";
import type { SyncEngine } from "./sync";

const ACCOUNT = "U1";

function harness(answer: (args: Record<string, unknown>) => Record<string, unknown>) {
  const store = new MailStore(null);
  store.setSession({} as never, ACCOUNT);
  store.setConn("online");
  const client = {
    emailSet: async (args: Record<string, unknown>) => answer(args),
  } as unknown as JmapClient;
  const sync = { poll: async () => {} } as unknown as SyncEngine;
  const actions = new MailActions(client, store, new StorageActionQueue(new MemoryStorage()), sync, {});
  return { store, actions };
}

const created = { d: { id: "draft-2", blobId: "b2", threadId: "t", size: 1 } };

const input = {
  from: { name: null, email: "you@example.com" },
  to: [],
  subject: "s",
  text: "t",
};

describe("MailActions.saveDraft — the succession (#610, #612)", () => {
  it("records old → new when the server created the new revision and retired the old", async () => {
    const h = harness(() => ({ created, destroyed: ["draft-1"] }));
    const res = await h.actions.saveDraft({ ...input, previousDraftId: "draft-1" });
    expect(res).toMatchObject({ ok: true, draftId: "draft-2", retiredPrevious: true });
    expect(h.store.getState().draftSuccessors).toEqual({ "draft-1": "draft-2" });
  });

  it("NEGATIVE CONTROL: records nothing when the destroy was REFUSED", async () => {
    // The old revision still exists, so it is a second draft, not a replaced one,
    // and nothing holding its id should leave it.
    const h = harness(() => ({
      created,
      destroyed: [],
      notDestroyed: { "draft-1": { type: "forbidden" } },
    }));
    const res = await h.actions.saveDraft({ ...input, previousDraftId: "draft-1" });
    expect(res).toMatchObject({ ok: true, retiredPrevious: false });
    expect(h.store.getState().draftSuccessors).toEqual({});
  });

  it("records nothing when the CREATE was refused, even though the old one was retired", async () => {
    // There is no successor to follow — the composer still holds the text and
    // its next save writes a fresh draft.
    const h = harness(() => ({
      notCreated: { d: { type: "overQuota", description: "full" } },
      destroyed: ["draft-1"],
    }));
    const res = await h.actions.saveDraft({ ...input, previousDraftId: "draft-1" });
    expect(res).toMatchObject({ ok: false, retiredPrevious: true });
    expect(h.store.getState().draftSuccessors).toEqual({});
  });

  it("records nothing on a composer's FIRST save, which replaces nothing", async () => {
    const h = harness(() => ({ created }));
    await h.actions.saveDraft({ ...input, previousDraftId: null });
    expect(h.store.getState().draftSuccessors).toEqual({});
  });
});
