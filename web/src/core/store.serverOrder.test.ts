// SPDX-License-Identifier: AGPL-3.0-only

// Best match (#724) is rendered in the server's order: the list must not
// re-sort it by date (or by pin) on the way to the screen.

import { describe, expect, it } from "vitest";

import type { Email } from "./jmap/types";
import { MailStore, visibleIds } from "./store";

function email(id: string, receivedAt: string, pinned = false): Email {
  return {
    id,
    blobId: "b",
    threadId: "t" + id,
    mailboxIds: {},
    keywords: {},
    size: 1,
    receivedAt,
    subject: id,
    preview: "",
    hasAttachment: false,
    ...(pinned ? { isPinned: true, pinnedAt: "2026-10-03T00:00:00Z" } : {}),
  } as unknown as Email;
}

describe("visibleIds order", () => {
  const store = new MailStore(null);
  store.putEmails([
    email("BEST", "2020-01-01T00:00:00Z"),
    email("NEWEST", "2026-10-02T00:00:00Z"),
    email("PINNED", "2025-01-01T00:00:00Z", true),
  ]);
  store.setList({ key: "search:relevance:x", ids: ["BEST", "NEWEST", "PINNED"], total: 3 });

  it("keeps the server's order for best match", () => {
    expect(visibleIds(store.getState(), null, null, "server")).toEqual(["BEST", "NEWEST", "PINNED"]);
  });

  it("control: the default order re-sorts the same ids (pinned first, then newest)", () => {
    expect(visibleIds(store.getState(), null, null, "arrival")).toEqual(["PINNED", "NEWEST", "BEST"]);
  });
});
