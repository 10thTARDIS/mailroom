// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// useFollowUps (#58): asks only when a reminder is switched on; asks again
// after a sync, but not more than once a minute; Done removes a row only once
// the server has recorded it.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { FollowUpItem, FollowUpSettings } from "../../../core/jmap/types";
import { FOLLOW_UP_REFRESH_MS, useFollowUps } from "./useFollowUps";
import type { FollowUpClient, FollowUps } from "./useFollowUps";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const A: FollowUpItem = {
  emailId: "E1",
  threadId: null,
  kind: "outbound",
  since: "2026-10-01T00:00:00Z",
  dueAt: "2026-10-08T00:00:00Z",
  subject: "Plans",
  who: { name: "", email: "ann@example.org" },
};

let clock = 1_000_000;
let latest: FollowUps;
function Probe(props: { client: FollowUpClient; settings: FollowUpSettings | undefined; lastSyncAt: number | null }) {
  latest = useFollowUps(props.client, "U1", props.settings, props.lastSyncAt, () => clock);
  return null;
}

let root: Root;
let host: HTMLElement;
beforeEach(() => {
  clock = 1_000_000;
  host = document.createElement("div");
  root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
});

function client(list: FollowUpItem[] = [A], dismissOK = true) {
  return {
    followUpGet: vi.fn(async () => ({ list })),
    followUpDismiss: vi.fn(async () => dismissOK),
  };
}

const ON: FollowUpSettings = { outbound: true, inbound: false, inboundDays: 3 };

async function render(c: FollowUpClient, settings: FollowUpSettings | undefined, lastSyncAt: number | null) {
  await act(async () => {
    root.render(<Probe client={c} settings={settings} lastSyncAt={lastSyncAt} />);
  });
}

describe("useFollowUps (#58)", () => {
  it("does not ask a server that has no follow-ups, or when both are off", async () => {
    const c = client();
    await render(c, undefined, 1);
    await render(c, { outbound: false, inbound: false, inboundDays: 3 }, 2);
    expect(c.followUpGet).not.toHaveBeenCalled();
    expect(latest.items).toEqual([]);
  });

  it("asks when switched on, and lists what came back", async () => {
    const c = client();
    await render(c, ON, null);
    expect(c.followUpGet).toHaveBeenCalledTimes(1);
    expect(latest.items).toEqual([A]);
  });

  it("asks again after a sync only once a minute has passed", async () => {
    const c = client();
    await render(c, ON, 1);
    const first = c.followUpGet.mock.calls.length;
    clock += FOLLOW_UP_REFRESH_MS - 1;
    await render(c, ON, 2);
    expect(c.followUpGet.mock.calls.length).toBe(first);
    clock += 2;
    await render(c, ON, 3);
    expect(c.followUpGet.mock.calls.length).toBe(first + 1);
  });

  it("removes a row on a recorded Done, and keeps it on a refused one", async () => {
    const ok = client();
    await render(ok, ON, null);
    let res = false;
    await act(async () => {
      res = await latest.dismiss(A);
    });
    expect(res).toBe(true);
    expect(ok.followUpDismiss).toHaveBeenCalledWith("E1", "outbound");
    expect(latest.items).toEqual([]);

    const refused = client([A], false);
    await render(refused, { ...ON, inboundDays: 4 }, null); // settings changed: refetch
    await act(async () => {
      res = await latest.dismiss(A);
    });
    expect(res).toBe(false);
    expect(latest.items).toEqual([A]);
    expect(latest.pending.size).toBe(0);
  });
});
