// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// SETTINGS → SIGNED-IN DEVICES (#937), rendered for real against a fake API:
// the capability states, this device first with its tag, the per-device and
// all-others sign-out (with the destroy shape), this device's button going to
// the logout route and making no destroy call, the honest sentence, axe.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { SignedInDevice } from "../../../../core/jmap/types";
import type { DevicesApi } from "../devices/api";
import { axeViolations } from "../apikeys/axe.testdata";
import { DevicesBody, devicesFacts } from "./DevicesRow";
import type { DevicesReading } from "./DevicesRow";

const NOW = Date.parse("2026-10-30T12:00:00Z");
const THIS: SignedInDevice = {
  id: "S-THIS",
  description: "Firefox on Linux",
  createdAt: "2026-10-01T00:00:00Z",
  lastSeenAt: "2026-10-30T11:59:30Z",
  lastAddr: "203.0.113.7",
  isCurrent: true,
};
const PHONE: SignedInDevice = {
  id: "S-PHONE",
  description: "Safari on iPhone",
  createdAt: "2026-10-20T00:00:00Z",
  lastSeenAt: "2026-10-30T09:00:00Z",
  lastAddr: "",
  isCurrent: false,
};
const OLD: SignedInDevice = {
  id: "S-OLD",
  description: "Chrome on Windows",
  createdAt: "2026-09-01T00:00:00Z",
  lastSeenAt: "2026-10-01T00:00:00Z",
  lastAddr: "198.51.100.2",
  isCurrent: false,
};

let host: HTMLDivElement;
let root: Root;
const signOut = vi.fn(async (ids: string[] | "others") => ({
  destroyed: ids === "others" ? ["S-PHONE", "S-OLD"] : ids,
  notDestroyed: [] as string[],
}));
const signOutHere = vi.fn();
const reload = vi.fn();
const notify = vi.fn();
const api: DevicesApi = {
  capability: () => ({}),
  list: async () => [],
  signOut: (ids) => signOut(ids),
  signOutHere: () => signOutHere(),
};
const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
  vi.restoreAllMocks();
});

function render(reading: DevicesReading): void {
  flushSync(() =>
    root.render(
      <section aria-label="Settings">
        <h3>Signed-in devices</h3>
        <DevicesBody reading={reading} reload={reload} api={api} notify={notify} now={() => NOW} />
      </section>,
    ),
  );
}
const loaded = (devices: SignedInDevice[]): DevicesReading => ({ kind: "loaded", devices });
const text = () => document.body.textContent ?? "";
const button = (label: string) => [...document.body.querySelectorAll("button")].find((b) => b.textContent === label);

describe("the state line", () => {
  it("counts devices, and says so for dev-auth and an unavailable server", () => {
    expect(devicesFacts(loaded([THIS, PHONE]))).toEqual([{ text: "2 signed-in devices" }]);
    expect(devicesFacts(loaded([THIS]))).toEqual([{ text: "1 signed-in device" }]);
    expect(devicesFacts(loaded([]))).toEqual([{ text: "no sessions (development mode)" }]);
    expect(devicesFacts({ kind: "asking" })).toEqual([]);
    expect(devicesFacts({ kind: "unavailable" })).toEqual([{ text: "not available on this server" }]);
  });
});

describe("the states", () => {
  it("says the server does not offer it", () => {
    render({ kind: "unavailable" });
    expect(text()).toContain("Signed-in devices are not available on this server.");
  });

  it("states no facts while asking", () => {
    render({ kind: "asking" });
    expect(text()).toContain("Reading your signed-in devices");
    expect(document.querySelector(".device-list")).toBeNull();
  });

  it("explains an empty list as development mode, not as 0 devices", () => {
    render(loaded([]));
    expect(text()).toContain("This server signs in without sessions (development mode).");
    expect(text()).not.toContain("0 ");
  });
});

describe("the list", () => {
  it("puts this device first even when it is not the most recent, then the others by last activity", () => {
    // This device was last active a day ago: it leads on being THIS device, not on recency.
    const staleThis = { ...THIS, lastSeenAt: "2026-10-29T12:00:00Z" };
    render(loaded([OLD, staleThis, PHONE]));
    const items = [...host.querySelectorAll(".device-list > li")].map((li) => li.textContent ?? "");
    expect(items.map((i) => i.split("Last active")[0])).toEqual([
      expect.stringContaining("Firefox on Linux"),
      expect.stringContaining("Safari on iPhone"),
      expect.stringContaining("Chrome on Windows"),
    ]);
    expect(items[0]).toContain("This device");
    expect(items[1]).not.toContain("This device");
  });

  it("states each device's activity, sign-in date and address", () => {
    render(loaded([THIS, PHONE, OLD]));
    const items = [...host.querySelectorAll(".device-list > li")].map((li) => li.textContent ?? "");
    expect(items[0]).toContain("from 203.0.113.7");
    expect(items[0]).toContain("Last active just now");
    expect(items[0]).toContain("Signed in 1 Oct 2026");
    expect(items[1]).toContain("Last active 3 h ago");
    expect(items[1]).not.toContain("from ");
    expect(items[2]).toContain("Last active 1 Oct 2026");
  });

  it("carries the honest sentence verbatim", () => {
    render(loaded([THIS, PHONE]));
    expect(text()).toContain(
      "Signing out a device you don't recognise ends that sign-in, but whoever used it may still know your password, so change your password in your sign-in provider too. Ending your sessions there doesn't sign Mailroom devices out; signing them out here does.",
    );
  });

  it("passes axe", async () => {
    render(loaded([THIS, PHONE, OLD]));
    expect(await axeViolations()).toEqual([]);
  });
});

describe("signing out", () => {
  it("signs out one device by id, says so, and re-reads the list", async () => {
    render(loaded([THIS, PHONE]));
    flushSync(() => button("Sign out")!.click());
    await settle();
    expect(signOut).toHaveBeenCalledWith(["S-PHONE"]);
    expect(notify).toHaveBeenCalledWith({ kind: "info", message: "Signed out of Safari on iPhone" });
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("on failure shows an error toast and does not re-read", async () => {
    signOut.mockResolvedValueOnce({ destroyed: [], notDestroyed: ["S-PHONE"] });
    render(loaded([THIS, PHONE]));
    flushSync(() => button("Sign out")!.click());
    await settle();
    expect(notify).toHaveBeenCalledWith({ kind: "error", message: "Could not sign out. Nothing was changed." });
    expect(reload).not.toHaveBeenCalled();
  });

  it("treats a call that could not be made as a failure", async () => {
    signOut.mockResolvedValueOnce(null as never);
    render(loaded([THIS, PHONE]));
    flushSync(() => button("Sign out")!.click());
    await settle();
    expect(notify.mock.calls[0]![0]).toMatchObject({ kind: "error" });
    expect(reload).not.toHaveBeenCalled();
  });

  it("signs out all others only after confirming, with the wildcard form", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    render(loaded([THIS, PHONE, OLD]));
    flushSync(() => button("Sign out all other devices")!.click());
    await settle();
    expect(confirm).toHaveBeenCalledWith("Sign out of 2 other devices? They will need to sign in again.");
    expect(signOut).toHaveBeenCalledWith("others");
    expect(notify).toHaveBeenCalledWith({ kind: "info", message: "Signed out of 2 devices" });
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("does nothing when the confirmation is declined", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(false);
    render(loaded([THIS, PHONE]));
    flushSync(() => button("Sign out all other devices")!.click());
    await settle();
    expect(signOut).not.toHaveBeenCalled();
  });

  it("hides sign out all others when this is the only device", () => {
    render(loaded([THIS]));
    expect(button("Sign out all other devices")).toBeUndefined();
  });

  it("this device's button goes to the logout route and makes no destroy call", async () => {
    render(loaded([THIS, PHONE]));
    flushSync(() => button("Sign out of this device")!.click());
    await settle();
    expect(signOutHere).toHaveBeenCalledTimes(1);
    expect(signOut).not.toHaveBeenCalled();
  });
});
