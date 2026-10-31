// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { censusSentence, countText, desklineSentence, shapeSentence, wouldSentence } from "./sentences";
import type { FolderCensus, SetupCounts } from "../../../core/jmap/types";

// Every sentence day-one says is a claim about thousands of somebody's messages,
// made immediately before a bulk write. Two properties are asserted for each: a
// clause whose number is UNKNOWN is absent, and the one zero that is KEPT is
// `moves` — because "move 0 messages" is the reassurance the step exists to give.

function folder(over: Partial<FolderCensus> = {}): FolderCensus {
  return {
    mailboxId: "01JBQZ3K7WX2N4Y6P8R0S1T2F1",
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

function counts(over: Partial<SetupCounts> = {}): SetupCounts {
  return {
    labels: 4404,
    archived: 4276,
    moves: 0,
    leave: 55,
    unclassified: 37,
    perFolder: [],
    estimatedSeconds: 240,
    ...over,
  };
}

describe("countText", () => {
  it("groups thousands the way the catalog's sentences are written", () => {
    expect(countText(0)).toBe("0");
    expect(countText(55)).toBe("55");
    expect(countText(999)).toBe("999");
    expect(countText(1000)).toBe("1,000");
    expect(countText(4612)).toBe("4,612");
    expect(countText(1234567)).toBe("1,234,567");
  });
});

describe("shapeSentence — the shape word, never alone", () => {
  it("carries the numbers the word was derived from", () => {
    expect(shapeSentence(folder({ shape: "oneDomain" }))).toBe("one sender domain");
    // The concentration figure WITH its denominator, so a reader who disagrees
    // with "a handful" can see what was measured.
    expect(shapeSentence(folder({ shape: "fewSenders", sendersFor90: 5, live: 190 }))).toBe(
      "5 senders cover 190",
    );
    expect(shapeSentence(folder({ shape: "bulk", distinctSenders: 38 }))).toBe(
      "bulk mail · 38 senders",
    );
    expect(shapeSentence(folder({ shape: "mixed" }))).toBe("mixed senders");
    expect(shapeSentence(folder({ shape: "empty", live: 0 }))).toBe("empty");
  });
});

describe("censusSentence", () => {
  it("counts the messages and the folders", () => {
    const sentence = censusSentence([
      folder({ role: "inbox", name: "INBOX", live: 4292 }),
      folder({ live: 320 }),
    ]);
    expect(sentence).toBe(
      "Found 4,612 messages in 2 folders. Special folders (Sent, Drafts, Archive, Junk, Trash) " +
        "are handled for you. The rest look like this:",
    );
  });

  it("says so outright when there are no plain folders at all", () => {
    // A small test instance. The generic sentence would announce "1
    // folder" and then show a table with one row and a decision about feeding
    // folders that do not exist.
    expect(censusSentence([folder({ role: "inbox", name: "INBOX", live: 128 })])).toBe(
      "Found 128 messages in INBOX and no other folders. Special folders " +
        "(Sent, Drafts, Archive, Junk, Trash) are handled for you.",
    );
  });

  it("uses the singular for one message", () => {
    expect(censusSentence([folder({ role: "inbox", live: 1 })])).toContain("Found 1 message in");
  });
});

describe("wouldSentence — the last thing read before the only button that writes", () => {
  it("states all five clauses", () => {
    expect(wouldSentence(counts())).toBe(
      "This would write 4,404 labels · archive 4,276 in Mailroom (nothing moves on the server) · " +
        "move 0 messages · leave 55 for later · about 4 minutes.",
    );
  });

  it("KEEPS `move 0 messages`", () => {
    // The clause the whole step exists for: zero moves is the reassurance, and a
    // sentence silent about the only provider-visible number is the wrong one.
    expect(wouldSentence(counts({ moves: 0 }))).toContain("move 0 messages");
  });

  it("drops the clauses whose numbers are not there to state", () => {
    // Nothing left behind: there is no "later" to talk about, and "leave 0 for
    // later" invites the reader to look for the folders it is about.
    const nothingLeft = wouldSentence(counts({ leave: 0 })) as string;
    expect(nothingLeft).not.toContain("for later");
    // Nothing to write: "about 0 minutes" is a duration for work that is not
    // going to happen.
    const nothingToDo = wouldSentence(
      counts({ labels: 0, archived: 0, moves: 0, leave: 0, estimatedSeconds: 0 }),
    ) as string;
    expect(nothingToDo).toBe(
      "This would write 0 labels · archive 0 in Mailroom (nothing moves on the server) · " +
        "move 0 messages.",
    );
  });

  it("says `under a minute` rather than rounding a short run to zero", () => {
    expect(wouldSentence(counts({ estimatedSeconds: 11 }))).toContain("under a minute");
    expect(wouldSentence(counts({ estimatedSeconds: 60 }))).toContain("about 1 minute");
    expect(wouldSentence(counts({ estimatedSeconds: 150 }))).toContain("about 3 minutes");
  });

  it("is null with no preview, which is not the same as a plan that does nothing", () => {
    expect(wouldSentence(null)).toBeNull();
  });
});

describe("desklineSentence — the one sentence about a screen not yet seen", () => {
  const full = {
    tray: 0,
    review: 21,
    drawers: 107,
    allMail: 4276,
    leave: 55,
    leaveFolders: ["Tech"],
  };

  it("states every measured clause", () => {
    expect(desklineSentence(full)).toBe(
      "Your desk will open with: 0 in the tray · 21 to review · 107 in drawers · " +
        "4,276 in All mail by area and label · 55 in Needs sorting when you import Tech.",
    );
  });

  it("keeps a measured zero — `0 in the tray` is a count somebody took", () => {
    expect(desklineSentence(full)).toContain("0 in the tray");
  });

  it("omits a clause whose number is unknown rather than printing zero", () => {
    // A wizard opened from a pasted URL has never loaded the desk, so there is no
    // tray count at all — and "0 in the tray" would read exactly like a tray that
    // was counted and found empty.
    const noTray = desklineSentence({ ...full, tray: null }) as string;
    expect(noTray).not.toContain("in the tray");
    expect(noTray).toContain("21 to review");
    const noPreview = desklineSentence({ ...full, review: null, drawers: null }) as string;
    expect(noPreview).not.toContain("to review");
    expect(noPreview).not.toContain("in drawers");
    expect(noPreview).toContain("4,276 in All mail");
  });

  it("drops Needs sorting when nothing was left, and names the folders when it was", () => {
    const nothingLeft = desklineSentence({ ...full, leave: 0, leaveFolders: [] }) as string;
    expect(nothingLeft).not.toContain("Needs sorting");
    expect(
      desklineSentence({ ...full, leave: 55, leaveFolders: ["Tech", "Old"] }),
    ).toContain("55 in Needs sorting when you import Tech, Old");
    // A count with no names is still worth saying — the names are optional inside
    // the clause rather than a condition on it.
    expect(desklineSentence({ ...full, leaveFolders: [] })).toContain(
      "55 in Needs sorting when you import them",
    );
  });

  it("is null when nothing at all is known", () => {
    // A sentence that is only its own lead-in is worse than no sentence.
    expect(
      desklineSentence({
        tray: null,
        review: null,
        drawers: null,
        allMail: null,
        leave: null,
        leaveFolders: [],
      }),
    ).toBeNull();
  });
});
