// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import {
  SETUP_WINDOW_DEFAULT,
  choiceFor,
  clearSetupState,
  defaultFolderChoice,
  foldersToFeed,
  initialSetupState,
  loadSetupState,
  parseSetupState,
  planFor,
  saveSetupState,
  setupReducer,
} from "./setupState";
import type { SetupState } from "./setupState";
import { MemoryStorage } from "../../../core/storage";
import type { FolderCensus } from "../../../core/jmap/types";

// THE DEFAULT PLAN IS THE THING UNDER TEST HERE. Everything else in day-one shows
// a report before it writes; this module decides what that report is a report OF,
// so a wrong default is a wrong 4,000-message write that the preview then
// faithfully confirms.

const INBOX = "01JBQZ3K7WX2N4Y6P8R0S1T2I0";
const CREATORS = "01JBQZ3K7WX2N4Y6P8R0S1T2F1";
const SHOPPING = "01JBQZ3K7WX2N4Y6P8R0S1T2F2";
const AREA = "01JBQZ3K7WX2N4Y6P8R0S1T2A1";

function folder(over: Partial<FolderCensus> = {}): FolderCensus {
  return {
    mailboxId: CREATORS,
    accountId: "01JBQZ3K7WX2N4Y6P8R0S1T2AC",
    name: "Creators",
    role: "",
    feedsDesk: false,
    live: 320,
    inWindow: 20,
    older: 300,
    unclassified: 0,
    distinctSenders: 1,
    distinctDomains: 1,
    sendersFor90: 1,
    bulkShare: 0,
    shape: "oneDomain",
    existingLabelId: null,
    existingAreaId: null,
    ...over,
  };
}

const inboxRow = folder({ mailboxId: INBOX, name: "INBOX", role: "inbox", shape: "mixed" });

describe("defaultFolderChoice — what the wizard proposes before anybody touches it", () => {
  it("labels and archives a plain folder into a new area named after it", () => {
    expect(defaultFolderChoice(folder())).toEqual({
      action: "labelArchive",
      labelName: "Creators",
      area: { kind: "new", name: "Creators" },
    });
  });

  it("feeds an EXISTING area of the same name rather than minting a second one", () => {
    expect(defaultFolderChoice(folder({ existingAreaId: AREA })).area).toEqual({
      kind: "existing",
      areaId: AREA,
    });
  });

  it("never proposes bringToDesk", () => {
    // The one action with a provider-visible consequence. A default that moved
    // mail is the accident the whole wizard exists to prevent.
    for (const f of [folder(), folder({ shape: "bulk" }), inboxRow]) {
      expect(defaultFolderChoice(f).action).toBe("labelArchive");
    }
  });

  it("gives INBOX neither a label nor an area", () => {
    // Its mail is labelled with each message's own resolved area label, and the
    // server mints nothing for it. An area called "INBOX" would be this client
    // inventing a decision the server does not offer.
    expect(defaultFolderChoice(inboxRow)).toEqual({
      action: "labelArchive",
      labelName: "",
      area: { kind: "none" },
    });
  });
});

describe("planFor — the wire plan, derived against the census", () => {
  const census = [inboxRow, folder(), folder({ mailboxId: SHOPPING, name: "Shopping" })];

  it("defaults the window to the server's own, and lists every census folder", () => {
    const plan = planFor(initialSetupState(), census);
    expect(plan.windowDays).toBe(SETUP_WINDOW_DEFAULT);
    expect(plan.folders.map((f) => f.mailboxId)).toEqual([INBOX, CREATORS, SHOPPING]);
  });

  it("sends no label and no area for INBOX", () => {
    const line = planFor(initialSetupState(), census).folders[0];
    expect(line).toEqual({ mailboxId: INBOX, action: "labelArchive" });
  });

  it("sends areaName for a new area and areaId for an existing one", () => {
    const plan = planFor(initialSetupState(), [
      folder(),
      folder({ mailboxId: SHOPPING, name: "Shopping", existingAreaId: AREA }),
    ]);
    expect(plan.folders[0]).toEqual({
      mailboxId: CREATORS,
      action: "labelArchive",
      labelName: "Creators",
      areaName: "Creators",
    });
    expect(plan.folders[1]).toEqual({
      mailboxId: SHOPPING,
      action: "labelArchive",
      labelName: "Shopping",
      areaId: AREA,
    });
  });

  it("says `leave` out loud rather than omitting the folder", () => {
    // Omission means the same thing to the server, but a folder missing from the
    // preview's perFolder reads exactly like a folder the server declined to
    // count. A `leave` line carries no label and joins no area.
    const state = setupReducer(initialSetupState(), {
      type: "folderAction",
      mailboxId: CREATORS,
      action: "leave",
      folder: folder(),
    });
    const plan = planFor(state, census);
    expect(plan.folders[1]).toEqual({ mailboxId: CREATORS, action: "leave" });
  });

  it("falls back to the folder name when the label has been emptied", () => {
    const state = setupReducer(initialSetupState(), {
      type: "folderLabel",
      mailboxId: CREATORS,
      labelName: "   ",
      folder: folder(),
    });
    expect(planFor(state, census).folders[1]?.labelName).toBe("Creators");
  });

  it("drops the area entirely for `no area`", () => {
    const state = setupReducer(initialSetupState(), {
      type: "folderArea",
      mailboxId: CREATORS,
      area: { kind: "none" },
      folder: folder(),
    });
    const line = planFor(state, census).folders[1];
    expect(line).toEqual({ mailboxId: CREATORS, action: "labelArchive", labelName: "Creators" });
  });

  it("carries ONE folder in import mode", () => {
    const plan = planFor(initialSetupState(), census, CREATORS);
    expect(plan.folders).toHaveLength(1);
    expect(plan.folders[0]?.mailboxId).toBe(CREATORS);
  });
});

describe("the reducer", () => {
  const f = folder();

  it("keeps the other two fields when one is edited", () => {
    // The override map starts empty, so the FIRST edit to a folder has to write
    // the other two fields — and the only correct values for them are the
    // proposal. Without that, changing the action would blank the label.
    const edited = setupReducer(initialSetupState(), {
      type: "folderAction",
      mailboxId: CREATORS,
      action: "bringToDesk",
      folder: f,
    });
    expect(choiceFor(edited, f)).toEqual({
      action: "bringToDesk",
      labelName: "Creators",
      area: { kind: "new", name: "Creators" },
    });
  });

  it("accepts a rule once, however many times it is pressed", () => {
    const once = setupReducer(initialSetupState(), { type: "acceptRule", key: "k" });
    const twice = setupReducer(once, { type: "acceptRule", key: "k" });
    expect(twice.acceptedRules).toEqual(["k"]);
    // Unchanged reference: nothing re-renders and nothing is re-created.
    expect(twice).toBe(once);
  });

  it("carries the window, the feed decision, the dials and the run", () => {
    let s = initialSetupState();
    s = setupReducer(s, { type: "window", days: 90 });
    s = setupReducer(s, { type: "feed", feed: "mailroom" });
    s = setupReducer(s, { type: "line", line: 0.7 });
    s = setupReducer(s, { type: "drawerDays", days: null });
    s = setupReducer(s, { type: "nagDays", days: 3 });
    s = setupReducer(s, { type: "run", runId: "01JBQZ3K7WX2N4Y6P8R0S1T2R1" });
    expect(s).toMatchObject({
      windowDays: 90,
      feed: "mailroom",
      line: 0.7,
      drawerDays: null,
      nagDays: 3,
      runId: "01JBQZ3K7WX2N4Y6P8R0S1T2R1",
    });
  });
});

describe("foldersToFeed — what `Folders feed the desk` points at the desk", () => {
  it("is the plain folders the plan does not leave alone", () => {
    const census = [inboxRow, folder(), folder({ mailboxId: SHOPPING, name: "Shopping" })];
    const state = setupReducer(initialSetupState(), {
      type: "folderAction",
      mailboxId: SHOPPING,
      action: "leave",
      folder: folder({ mailboxId: SHOPPING, name: "Shopping" }),
    });
    // INBOX is excluded because it already feeds the desk by definition; the
    // `leave` folder because pointing it at the desk would put the whole backlog
    // the user just deferred onto the surface.
    expect(foldersToFeed(state, census)).toEqual([CREATORS]);
  });
});

describe("persistence — the wizard is resumable, and the store is not trusted", () => {
  const state: SetupState = { ...initialSetupState(), windowDays: 7, line: 0.6 };

  it("round-trips through a KeyValueStorage, per login", () => {
    const storage = new MemoryStorage();
    saveSetupState(storage, "login-a", state);
    expect(loadSetupState(storage, "login-a")).toEqual(state);
    // Another login resumes nothing: the state describes THAT login's folders.
    expect(loadSetupState(storage, "login-b")).toEqual(initialSetupState());
    clearSetupState(storage, "login-a");
    expect(loadSetupState(storage, "login-a")).toEqual(initialSetupState());
  });

  it("falls back per FIELD, not per blob, on anything unrecognised", () => {
    // A hand-mangled `line` must not cost the folder plan somebody spent two
    // minutes building.
    const parsed = parseSetupState(
      JSON.stringify({
        windowDays: "soon",
        feed: "sideways",
        line: 44,
        nagDays: -3,
        folders: {
          [CREATORS]: { action: "labelArchive", labelName: "Creators", area: { kind: "new", name: "Creators" } },
          [SHOPPING]: { action: "teleport" },
          nonsense: 7,
        },
        acceptedRules: ["k", 3],
      }),
    );
    expect(parsed.windowDays).toBe(SETUP_WINDOW_DEFAULT);
    expect(parsed.feed).toBe("folders");
    expect(parsed.line).toBe(initialSetupState().line);
    expect(parsed.nagDays).toBe(initialSetupState().nagDays);
    expect(parsed.acceptedRules).toEqual(["k"]);
    // The good row survives; the two bad ones are dropped rather than carried as
    // a choice nothing can act on.
    expect(Object.keys(parsed.folders)).toEqual([CREATORS]);
  });

  it("keeps `never` for the drawers, which is a value and not a missing field", () => {
    expect(parseSetupState(JSON.stringify({ drawerDays: null })).drawerDays).toBeNull();
    expect(parseSetupState(JSON.stringify({})).drawerDays).toBe(initialSetupState().drawerDays);
  });

  it("never throws on garbage", () => {
    for (const raw of [null, "", "{", "[]", "7", '"x"', "null"]) {
      expect(() => parseSetupState(raw)).not.toThrow();
    }
    expect(parseSetupState("{")).toEqual(initialSetupState());
  });
});
