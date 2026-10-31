// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE DESK'S KEY-EXPIRY BANNER (#795), rendered for real with captured keys:
// it names the key and the date, links to Settings → Keys, dismisses per key
// per state (and the dismissal survives a remount through localStorage), and
// it comes back when the key moves from "expires on" to "expired".

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

const navigate = vi.fn();
vi.mock("../../routing", () => ({ navigate: (...a: unknown[]) => navigate(...a), useRoute: () => ({ page: "inbox" }) }));
vi.mock("../../state", () => ({
  useApp: () => ({ client: { pgpKeyExpiryWarningDays: () => 30 } }),
  useMailState: () => ({ identities: { [IDENTITY_READER.id]: IDENTITY_READER } }),
}));

import { CAPTURED_AT, IDENTITY_READER, USER_KEY_BOUND, USER_KEY_TEN_DAYS } from "../../../core/keyFixtures.testdata";
import { DISMISSED_KEY_WARNINGS } from "../../../core/keyWarnings";
import type { UserKey } from "../../../core/jmap/types";
import { KeyExpiryBanners } from "./KeyExpiryBanner";

let host: HTMLDivElement;
let root: Root;

const BOUND_TEN_DAYS: UserKey = { ...USER_KEY_TEN_DAYS, identityId: IDENTITY_READER.id };

beforeEach(() => {
  localStorage.clear();
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function render(keys: UserKey[] | null): void {
  flushSync(() => root.render(<KeyExpiryBanners keys={keys} now={CAPTURED_AT} />));
}
function remount(keys: UserKey[] | null): void {
  flushSync(() => root.unmount());
  root = createRoot(host);
  render(keys);
}
const banners = () => [...host.querySelectorAll<HTMLElement>(".key-expiry-banner")];
const button = (label: string) =>
  [...host.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent === label);

describe("the banner", () => {
  it("says nothing before the keys are read, or when none is near its date", () => {
    render(null);
    expect(host.childElementCount).toBe(0);
    render([USER_KEY_BOUND]);
    expect(host.childElementCount).toBe(0);
  });

  it("names the address and the date, as a status, and links to Settings → Keys", () => {
    render([USER_KEY_BOUND, BOUND_TEN_DAYS]);
    expect(banners()).toHaveLength(1);
    const status = banners()[0]!.querySelector('[role="status"]')!;
    expect(status.textContent).toBe("Your key for reader@example.com expires on 31 Oct.");
    flushSync(() => button("Extend it")!.click());
    expect(navigate).toHaveBeenCalledWith({ page: "settings", section: "keys" });
  });

  it("an unbound key is named by its fingerprint's tail", () => {
    render([USER_KEY_TEN_DAYS]);
    expect(banners()[0]!.textContent).toContain("Your key 1A90 5815 expires on 31 Oct.");
  });

  it("dismissal hides it, and it stays dismissed across a remount", () => {
    render([BOUND_TEN_DAYS]);
    const dismiss = button("Dismiss")!;
    expect(dismiss.getAttribute("aria-label")).toBe("Dismiss the warning about the key for reader@example.com");
    flushSync(() => dismiss.click());
    expect(banners()).toHaveLength(0);
    expect(localStorage.getItem(DISMISSED_KEY_WARNINGS)).toContain(`${BOUND_TEN_DAYS.id}:expiring`);
    remount([BOUND_TEN_DAYS]);
    expect(banners()).toHaveLength(0);
  });

  it("comes back when the dismissed key expires: a new state is a new warning", () => {
    render([BOUND_TEN_DAYS]);
    flushSync(() => button("Dismiss")!.click());
    remount([{ ...BOUND_TEN_DAYS, expired: true, usable: false, expiringSoon: false }]);
    expect(banners()).toHaveLength(1);
    expect(banners()[0]!.textContent).toContain(
      "Your key for reader@example.com expired on 31 Oct. People can no longer encrypt mail to you with it.",
    );
    expect(button("Extend or replace it")).toBeDefined();
  });
});
