// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// SETTINGS → KEYS, the v1.93 final scope (#812, #813, #815, #817, #818), rendered
// for real against keys captured from the C1 server and a fake API:
//
//   * each key's title is its primary user ID, the others beneath and revoked
//     ones marked, as TEXT (a user ID holding "<b>" shows the characters);
//     a key whose user IDs do not name its address says so, and so does a bind;
//   * History is collapsed under each list, says it, opens by keyboard, holds
//     the retired, revoked and removed keys with their dates — "date not
//     recorded" when it was not — and "Replaced by" moves to the newer key;
//   * Replace… says what happens, offers generate's expiry choices, and a
//     failure after the new key was made says the new key exists, unused;
//   * "On a date…" sends the date as picked and says the result in words;
//   * Remove says the key is kept and comes back, and points at "Don't use
//     this key"; Show history lists a key's events in words;
//   * Check for an updated key is primary when the key expires soon, says what
//     it found, and "they've published a different key" opens the decision.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Mock } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

vi.mock("../../../state", () => ({
  useApp: () => ({}),
  useMailState: () => ({ accountId: "A" }),
}));

import {
  C1_CAPTURED_AT,
  C1_CONTACTS,
  C1_CONTACT_KEY_ALICE,
  C1_CONTACT_KEY_BOB_REMOVED,
  C1_CONTACT_KEY_CAROL,
  C1_HISTORY_BOB,
  C1_IDENTITY,
  C1_KEYS_AFTER_REPLACE,
  C1_KEY_NOT_LISTED,
  C1_KEY_REPLACED,
  C1_KEY_REPLACEMENT,
  C1_KEY_RETIRED_NOT_RECORDED,
  C1_KEY_TWO_USER_IDS,
  C1_REFRESH_ALICE_DIFFERENT,
  C1_REFRESH_CAROL_RENEWED,
  C1_REPLACE,
} from "../../../../core/keyHistoryFixtures.testdata";
import type { HeldContactKey, Identity, UserKey } from "../../../../core/jmap/types";
import type { KeysApi } from "./api";
import { KeysBody } from "../rows/KeysRow";
import type { KeysReading } from "../rows/KeysRow";

// The identity as the page sees it once the replacement is its signing key.
const IDENTITY: Identity = { ...C1_IDENTITY, signingKeyId: C1_KEY_REPLACEMENT.id };
// DERIVED from bob's captured key: the same key, not removed and not refused
// (a two-year key, so outside the warning window).
const BOB_HELD: HeldContactKey = { ...C1_CONTACT_KEY_BOB_REMOVED, removedAt: null, refused: false, pinned: true, pinnedFor: ["bob@example.org"] };

type FakeApi = { [K in keyof KeysApi]: Mock };

function fakeApi(): FakeApi {
  return {
    generate: vi.fn(async () => ({ ok: true as const, value: { fingerprint: "F" } })),
    importOwn: vi.fn(),
    update: vi.fn(async () => ({ ok: true as const, value: {} })),
    extend: vi.fn(async () => ({ ok: true as const, value: { expiresAt: "2026-11-30T23:59:59Z" } })),
    replace: vi.fn(async () => ({ ok: true as const, value: C1_REPLACE })),
    revoke: vi.fn(),
    exportKey: vi.fn(),
    readCorrespondents: vi.fn(async () => ({
      keys: [C1_CONTACT_KEY_CAROL, C1_CONTACT_KEY_BOB_REMOVED, C1_CONTACT_KEY_ALICE],
      contacts: C1_CONTACTS,
    })),
    removeCorrespondentKey: vi.fn(async () => ({ ok: true as const, value: undefined })),
    correspondentKeyHistory: vi.fn(async () => ({ ok: true as const, value: C1_HISTORY_BOB })),
    refreshCorrespondentKey: vi.fn(async () => ({ ok: true as const, value: C1_REFRESH_CAROL_RENEWED })),
    decideCorrespondentKey: vi.fn(async () => ({ ok: true as const, value: undefined })),
    checkContactKey: vi.fn(),
    importContactKey: vi.fn(),
    afterChange: vi.fn(),
  };
}

let host: HTMLDivElement;
let root: Root;
let api: FakeApi;
const reload = vi.fn();
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

function render(keys: UserKey[] = C1_KEYS_AFTER_REPLACE): void {
  const reading: KeysReading = {
    kind: "loaded",
    keys,
    correspondents: 3,
    wkdConfigured: true,
    wkdDomain: "example.com",
  };
  flushSync(() =>
    root.render(
      <KeysBody
        reading={reading}
        identities={[IDENTITY]}
        reload={reload}
        api={api as unknown as KeysApi}
        keyManagement={true}
        warnDays={30}
        now={C1_CAPTURED_AT}
      />,
    ),
  );
}

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
  api = fakeApi();
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

const ownSection = () => host.querySelector<HTMLElement>(".own-keys")!;
const theirSection = () => host.querySelector<HTMLElement>(".correspondent-keys")!;
const ownCards = () => [...ownSection().querySelectorAll<HTMLElement>(".own-key")];
const cardFor = (fpr: string) =>
  ownCards().find((c) => (c.textContent ?? "").replace(/\s+/g, "").includes(fpr))!;
const buttonIn = (el: ParentNode, label: string) =>
  [...el.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent === label);
const dialog = () => document.querySelector<HTMLElement>('[role="dialog"]');
const done = () => host.querySelector(".setting-done")?.textContent;
const theirCards = () => [...theirSection().querySelectorAll<HTMLElement>(".correspondent-key")];
const theirCard = (address: string) => theirCards().find((c) => (c.textContent ?? "").includes(address))!;

function setValue(el: HTMLInputElement | HTMLSelectElement, value: string): void {
  const proto = el instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
  const setter = Object.getOwnPropertyDescriptor(proto, "value")!.set!;
  flushSync(() => {
    setter.call(el, value);
    el.dispatchEvent(new Event(el instanceof HTMLSelectElement ? "change" : "input", { bubbles: true }));
  });
}
function labelledBy(el: HTMLElement): string {
  return document.querySelector(`label[for="${el.id}"]`)?.textContent ?? "";
}

describe("user IDs (#812)", () => {
  it("each key's title is its primary user ID, whole in the tooltip, the others beneath", () => {
    render();
    const two = cardFor(C1_KEY_TWO_USER_IDS.fingerprint);
    const title = two.querySelector<HTMLElement>("h5 .key-uid")!;
    expect(title.textContent).toBe("Reader at work <reader@work.example>");
    expect(title.getAttribute("title")).toBe("Reader at work <reader@work.example>");
    const others = two.querySelector<HTMLElement>(".key-uids")!;
    expect(others.getAttribute("aria-label")).toBe("Also on this key");
    expect([...others.querySelectorAll("li")].map((l) => l.textContent)).toEqual(["Reader Example <reader@example.com>"]);
    // The address it is used for keeps its own line.
    expect(two.textContent).toContain("Used for reader@example.com");
    expect(cardFor(C1_KEY_REPLACEMENT.fingerprint).querySelector("h5")?.textContent).toBe("reader@example.com");
  });

  it("a stranger's user ID is text: \"<b>\" shows as characters, and a revoked one is marked", async () => {
    render();
    await settle();
    const alice = theirCard("BF68 BDC4");
    expect(alice.querySelector(".correspondent-key-name")?.textContent).toBe("Alice <b>");
    expect(alice.querySelector("b")).toBeNull();
    const uids = [...alice.querySelectorAll(".key-uids li")].map((l) => l.textContent);
    expect(uids).toEqual(["Alice Example <alice@example.org>", "Alice Old <alice.old@example.org> (revoked)"]);
  });

  it("a key whose user IDs don't name its address says so, neutrally; the others don't", () => {
    render();
    expect(cardFor(C1_KEY_NOT_LISTED.fingerprint).querySelector(".key-not-listed")?.textContent).toBe(
      "This key doesn't list reader@example.com; some mail apps may refuse or flag it.",
    );
    expect(cardFor(C1_KEY_TWO_USER_IDS.fingerprint).querySelector(".key-not-listed")).toBeNull();
    expect(cardFor(C1_KEY_REPLACEMENT.fingerprint).querySelector(".key-not-listed")).toBeNull();
  });

  it("a bind the server warns about is made, and says the same", async () => {
    api.update.mockResolvedValueOnce({ ok: true, value: { boundAddressListed: false, warning: "addressNotInKey" } });
    const unbound: UserKey = { ...C1_KEY_NOT_LISTED, identityId: null, boundAddressListed: null };
    render([unbound]);
    const card = ownCards()[0]!;
    setValue(card.querySelector("select")!, IDENTITY.id);
    flushSync(() => buttonIn(card, "Save")!.click());
    await settle();
    expect(api.update).toHaveBeenCalledWith(unbound.id, { identityId: IDENTITY.id });
    expect(done()).toBe(
      "The key is now used for reader@example.com. This key doesn't list reader@example.com; some mail apps may refuse or flag it.",
    );
  });
});

describe("your keys' History (#815)", () => {
  const toggle = () => ownSection().querySelector<HTMLButtonElement>(".key-history-toggle")!;

  it("is collapsed, says so and what it holds, and opens", () => {
    render();
    expect(toggle().getAttribute("aria-expanded")).toBe("false");
    expect(toggle().textContent).toContain("History (2)");
    expect(toggle().getAttribute("aria-label")).toBe("History (2), your keys");
    const region = document.getElementById(toggle().getAttribute("aria-controls")!)!;
    expect(region.hidden).toBe(true);
    // The two past keys are not in the list of keys in use.
    expect(ownCards()).toHaveLength(3);
    flushSync(() => toggle().click());
    expect(toggle().getAttribute("aria-expanded")).toBe("true");
    const open = document.getElementById(toggle().getAttribute("aria-controls")!)!;
    expect(open.hidden).toBe(false);
    expect(open.getAttribute("role")).toBe("region");
    expect(open.querySelectorAll(".own-key")).toHaveLength(2);
    flushSync(() => toggle().click());
    expect(toggle().getAttribute("aria-expanded")).toBe("false");
  });

  it("a replaced key says when it was retired, and links to the key that replaced it", async () => {
    render();
    flushSync(() => toggle().click());
    const old = cardFor(C1_KEY_REPLACED.fingerprint);
    expect(old.textContent).toContain("Retired on 21 Oct 2026");
    expect(old.textContent).not.toContain("Used for");
    const link = buttonIn(old, "reader@example.com, the key ending 6072568B")!;
    expect(old.querySelector(".own-key-replaced")?.textContent).toBe(
      "Replaced by reader@example.com, the key ending 6072568B",
    );
    flushSync(() => link.click());
    await settle();
    expect(document.activeElement?.id).toBe(`key-${C1_KEY_REPLACEMENT.id}`);
  });

  it("a retirement that was not recorded says so, and invents no date", () => {
    render([C1_KEY_REPLACEMENT, C1_KEY_RETIRED_NOT_RECORDED]);
    flushSync(() => toggle().click());
    const old = cardFor(C1_KEY_RETIRED_NOT_RECORDED.fingerprint);
    expect(old.textContent).toContain("Retired (date not recorded)");
    expect(old.textContent).not.toContain("Retired on");
  });
});

describe("Replace (#817)", () => {
  it("is offered on bound keys in use only", () => {
    render();
    flushSync(() => ownSection().querySelector<HTMLButtonElement>(".key-history-toggle")!.click());
    const offered = ownCards().filter((c) => buttonIn(c, "Replace…")).map((c) => c.querySelector("h5")?.id);
    expect(offered).toEqual(
      [C1_KEY_REPLACEMENT, C1_KEY_NOT_LISTED, C1_KEY_TWO_USER_IDS].map((k) => `key-${k.id}`),
    );
  });

  it("says what happens, offers generate's expiry choices, and starts on Cancel", () => {
    render();
    flushSync(() => buttonIn(cardFor(C1_KEY_REPLACEMENT.fingerprint), "Replace…")!.click());
    const d = dialog()!;
    expect(d.getAttribute("aria-modal")).toBe("true");
    expect(document.getElementById(d.getAttribute("aria-labelledby")!)?.textContent).toBe(
      "Replace the key for reader@example.com",
    );
    const text = d.textContent ?? "";
    expect(text).toContain("Mailroom makes a new key for reader@example.com");
    expect(text).toContain("The new key is published in the key directory in place of this one.");
    expect(text).toContain("still opens mail already encrypted to it");
    expect(text).toContain("If you think this key was stolen, revoke it instead.");
    expect(document.activeElement?.textContent).toBe("Cancel");
    const select = d.querySelector("select")!;
    expect(labelledBy(select)).toBe("The new key expires");
    expect([...select.options].map((o) => o.value)).toEqual(["1y", "2y", "5y", "never", "date"]);
  });

  it("an unpublished key's replacement is not published, and the dialog says so", () => {
    render();
    flushSync(() => buttonIn(cardFor(C1_KEY_NOT_LISTED.fingerprint), "Replace…")!.click());
    expect(dialog()!.textContent).toContain("This key isn't published, so the new one won't be either.");
  });

  it("on a date: unavailable until a date is chosen, then sends it and says it was replaced", async () => {
    render();
    flushSync(() => buttonIn(cardFor(C1_KEY_REPLACEMENT.fingerprint), "Replace…")!.click());
    const d = dialog()!;
    setValue(d.querySelector("select")!, "date");
    const input = d.querySelector<HTMLInputElement>('input[type="date"]')!;
    expect(labelledBy(input)).toBe("Expiry date");
    const submit = d.querySelector<HTMLButtonElement>(".key-replace-submit")!;
    expect(submit.getAttribute("aria-disabled")).toBe("true");
    expect(submit.disabled).toBe(false);
    expect(document.getElementById(submit.getAttribute("aria-describedby")!)?.textContent).toBe("Choose a date.");
    flushSync(() => submit.click());
    // Past the browser's own validation too (required): the guard holds.
    flushSync(() => d.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })));
    await settle();
    expect(api.replace).not.toHaveBeenCalled();
    setValue(input, "2026-11-04");
    expect(submit.getAttribute("aria-disabled")).toBe("false");
    flushSync(() => submit.click());
    await settle();
    expect(api.replace).toHaveBeenCalledWith(C1_KEY_REPLACEMENT.id, { expiresAt: "2026-11-04" });
    await settle();
    expect(dialog()).toBeNull();
    expect(done()).toBe(
      "The key for reader@example.com was replaced, and the new one is published. The old one still opens past mail.",
    );
    expect(api.afterChange).toHaveBeenCalled();
  });

  it("a failure after the new key was made says so, lists it, and leaves the dialog open", async () => {
    api.replace.mockResolvedValueOnce({ ok: false, error: "failed", leftNewKey: C1_REPLACE.id });
    render();
    flushSync(() => buttonIn(cardFor(C1_KEY_REPLACEMENT.fingerprint), "Replace…")!.click());
    flushSync(() => dialog()!.querySelector<HTMLButtonElement>(".key-replace-submit")!.click());
    await settle();
    await settle();
    const alert = dialog()!.querySelector('[role="alert"]')!;
    expect(alert.textContent).toContain("That didn't work. Try again.");
    expect(alert.textContent).toContain(
      "A new key was made but not put in place: it's listed below, unused. Your current key is unchanged.",
    );
    expect(reload).toHaveBeenCalled();
    expect(alert.textContent).not.toMatch(/serverFail|[0-9A-HJKMNP-TV-Z]{26}/);
  });

  it("a refusal before anything was made is only its sentence", async () => {
    api.replace.mockResolvedValueOnce({ ok: false, error: "keyRetired", leftNewKey: null });
    render();
    flushSync(() => buttonIn(cardFor(C1_KEY_REPLACEMENT.fingerprint), "Replace…")!.click());
    flushSync(() => dialog()!.querySelector<HTMLButtonElement>(".key-replace-submit")!.click());
    await settle();
    await settle();
    expect(dialog()!.querySelector('[role="alert"]')?.textContent).toBe(
      "A retired or revoked key can't sign, so it can't be used for an address.",
    );
    expect(reload).not.toHaveBeenCalled();
  });
});

describe("an exact expiry date (#813)", () => {
  it("generate: \"On a date…\" says the result in words, warns about the window, and sends the date", async () => {
    render();
    flushSync(() => buttonIn(host, "Make a new key…")!.click());
    const d = dialog()!;
    const expiry = [...d.querySelectorAll("select")][1]!;
    setValue(expiry, "date");
    const input = d.querySelector<HTMLInputElement>('input[type="date"]')!;
    expect(input.min).toBe("2026-10-22");
    expect(input.max > "2036-10-01").toBe(true);
    setValue(input, "2026-11-04");
    const said = (input.getAttribute("aria-describedby") ?? "")
      .split(" ")
      .map((id) => document.getElementById(id)?.textContent)
      .join(" | ");
    expect(said).toBe(
      "Expires on 4 Nov 2026, in 14 days. That is inside the 30-day warning window, so the key will show “Expires soon” straight away. | The key stops working at the end of that day (UTC).",
    );
    flushSync(() => buttonIn(d, "Make key")!.click());
    await settle();
    expect(api.generate).toHaveBeenCalledWith(IDENTITY.id, { expiresAt: "2026-11-04" }, true);
  });

  it("generate: a date the server would refuse is said, and nothing is sent", async () => {
    render();
    flushSync(() => buttonIn(host, "Make a new key…")!.click());
    const d = dialog()!;
    setValue([...d.querySelectorAll("select")][1]!, "date");
    setValue(d.querySelector<HTMLInputElement>('input[type="date"]')!, "2040-01-01");
    expect(d.querySelector(".key-date-result")?.textContent).toContain("a key can last at most 10 years");
    const make = buttonIn(d, "Make key")!;
    expect(make.getAttribute("aria-disabled")).toBe("true");
    flushSync(() => make.click());
    // The browser's own validation (max) stops a click; the dialog's guard must
    // hold even when the form is submitted regardless.
    flushSync(() => d.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })));
    await settle();
    expect(api.generate).not.toHaveBeenCalled();
  });

  it("extend: \"On a date…\" sends expiresAt instead of days", async () => {
    render();
    flushSync(() => buttonIn(cardFor(C1_KEY_TWO_USER_IDS.fingerprint), "Extend…")!.click());
    const d = dialog()!;
    flushSync(() => d.querySelector<HTMLInputElement>('input[type="radio"][value="date"]')!.click());
    setValue(d.querySelector<HTMLInputElement>('input[type="date"]')!, "2026-11-30");
    expect(d.querySelector(".key-date-result")?.textContent).toBe("Expires on 30 Nov 2026, in 40 days.");
    flushSync(() => buttonIn(d, "Extend")!.click());
    await settle();
    expect(api.extend).toHaveBeenCalledWith(C1_KEY_TWO_USER_IDS.id, { expiresAt: "2026-11-30" });
  });
});

describe("correspondents' keys: history and removal (#815)", () => {
  it("Remove says the key is kept and comes back, and points at Don't use this key", async () => {
    render();
    await settle();
    flushSync(() => buttonIn(theirCard("carol@example.org"), "Remove…")!.click());
    const text = dialog()!.textContent ?? "";
    expect(text).toContain("Mailroom stops using this key for Carol Example <carol@example.org> and keeps it in History.");
    expect(text).toContain("It comes back if they send it again, or publish it.");
    expect(text).toContain("To stop using it for good, choose “Don't use this key” instead.");
    expect(text).not.toContain("forgets");
  });

  it("removed keys are under History, with when they were removed and nothing to do but read", async () => {
    render();
    await settle();
    expect(theirCards().some((c) => (c.textContent ?? "").includes("bob@example.org"))).toBe(false);
    const toggle = theirSection().querySelector<HTMLButtonElement>(".key-history-toggle")!;
    expect(toggle.getAttribute("aria-label")).toBe("History (1), correspondents' keys");
    flushSync(() => toggle.click());
    const bob = theirCard("bob@example.org");
    expect(bob.textContent).toContain("Removed on 21 Oct 2026");
    expect(bob.textContent).toContain("Refused by you");
    expect(buttonIn(bob, "Remove…")).toBeUndefined();
    expect(bob.querySelector(".key-refresh")).toBeNull();
    expect(buttonIn(bob, "Don't use this key…")).toBeUndefined();
  });

  it("Show history lists the key's events in words, and hides them again", async () => {
    render();
    await settle();
    flushSync(() => theirSection().querySelector<HTMLButtonElement>(".key-history-toggle")!.click());
    const bob = theirCard("bob@example.org");
    const show = buttonIn(bob, "Show history")!;
    expect(show.getAttribute("aria-expanded")).toBe("false");
    expect(show.getAttribute("aria-label")).toBe("Show history of the key for Bob Example <bob@example.org>");
    flushSync(() => show.click());
    await settle();
    expect(api.correspondentKeyHistory).toHaveBeenCalledWith(C1_CONTACT_KEY_BOB_REMOVED.id);
    const list = bob.querySelector<HTMLElement>(".key-events")!;
    expect(document.getElementById(show.getAttribute("aria-controls")!)?.contains(list)).toBe(true);
    expect([...list.querySelectorAll("li")].map((l) => l.textContent)).toEqual([
      "21 Oct 2026 You imported it",
      "21 Oct 2026 You chose not to use it",
      "21 Oct 2026 You removed it",
    ]);
    expect(list.querySelector("time")?.getAttribute("dateTime")).toBe(C1_HISTORY_BOB[0]!.at);
    const hide = buttonIn(bob, "Hide history")!;
    expect(hide.getAttribute("aria-expanded")).toBe("true");
    flushSync(() => hide.click());
    expect(bob.querySelector(".key-events")).toBeNull();
  });

  it("Don't use this key asks, then refuses it through ContactKey/decide", async () => {
    render();
    await settle();
    flushSync(() => buttonIn(theirCard("carol@example.org"), "Don't use this key…")!.click());
    const d = dialog()!;
    expect(d.textContent).toContain("stays refused if it arrives again");
    expect(api.decideCorrespondentKey).not.toHaveBeenCalled();
    flushSync(() => buttonIn(d, "Don't use this key")!.click());
    await settle();
    expect(api.decideCorrespondentKey).toHaveBeenCalledWith(
      "carol@example.org",
      C1_CONTACT_KEY_CAROL.fingerprint,
      "keep",
    );
    await settle();
    expect(done()).toBe("Mailroom won't use the key for Carol Example <carol@example.org>.");
  });
});

describe("Check for an updated key (#818)", () => {
  const keysWith = (...keys: HeldContactKey[]) => ({ keys, contacts: C1_CONTACTS });

  it("is the primary action on a key that expires soon, and comes last otherwise", async () => {
    api.readCorrespondents.mockResolvedValue(keysWith(C1_CONTACT_KEY_ALICE, BOB_HELD));
    render();
    await settle();
    const actions = (c: HTMLElement) => [...c.querySelectorAll<HTMLButtonElement>(".own-key-actions button")];
    const alice = actions(theirCard("BF68 BDC4"));
    expect(alice[0]!.textContent).toBe("Check for an updated key");
    expect(alice[0]!.className).toContain("button-primary");
    const bob = actions(theirCard("bob@example.org"));
    expect(bob.at(-1)!.textContent).toBe("Check for an updated key");
    expect(bob.at(-1)!.className).not.toContain("button-primary");
  });

  it("is busy while it asks, then says what it found and reloads the list", async () => {
    let answer: (v: unknown) => void = () => {};
    api.refreshCorrespondentKey.mockImplementationOnce(() => new Promise((r) => (answer = r)));
    render();
    await settle();
    const before = api.readCorrespondents.mock.calls.length;
    const card = theirCard("carol@example.org");
    const button = card.querySelector<HTMLButtonElement>(".key-refresh")!;
    expect(button.getAttribute("aria-label")).toBe("Check for an updated key for Carol Example <carol@example.org>");
    flushSync(() => button.click());
    expect(api.refreshCorrespondentKey).toHaveBeenCalledWith(C1_CONTACT_KEY_CAROL.id);
    expect(button.disabled).toBe(true);
    expect(card.querySelector('.key-refresh-result[role="status"]')?.textContent).toBe("Checking for an updated key…");
    answer({ ok: true, value: C1_REFRESH_CAROL_RENEWED });
    await settle();
    await settle();
    expect(theirCard("carol@example.org").querySelector(".key-refresh-result")?.textContent).toBe(
      "Updated — now expires 3 Nov 2028",
    );
    expect(api.readCorrespondents.mock.calls.length).toBeGreaterThan(before);
  });

  it("a check too soon after the last says when to try again", async () => {
    api.refreshCorrespondentKey.mockResolvedValueOnce({ ok: false, error: "rateLimited" });
    render();
    await settle();
    flushSync(() => theirCard("carol@example.org").querySelector<HTMLButtonElement>(".key-refresh")!.click());
    await settle();
    expect(theirCard("carol@example.org").querySelector(".key-refresh-result")?.textContent).toBe(
      "Checked recently — try again in a few minutes",
    );
  });

  it("a different key opens the key-changed decision, and the answer goes to ContactKey/decide", async () => {
    api.refreshCorrespondentKey.mockResolvedValueOnce({ ok: true, value: C1_REFRESH_ALICE_DIFFERENT });
    render();
    await settle();
    const card = theirCard("BF68 BDC4");
    expect(card.querySelector(".key-change")).toBeNull();
    flushSync(() => card.querySelector<HTMLButtonElement>(".key-refresh")!.click());
    await settle();
    expect(card.querySelector(".key-refresh-result")?.textContent).toBe("They've published a different key");
    const decision = card.querySelector<HTMLElement>(".key-change")!;
    expect(decision.textContent).toContain("Alice <b> has published a different key.");
    expect(decision.textContent).toContain("BF68 BDC4 9B10 9EC9 055E B6B0 B1C0 1558 5905 1703");
    expect(decision.textContent).toContain("EFBE 22BB 0259 C046 C679 B00F 4320 3314 5537 55D3");
    flushSync(() => buttonIn(decision, "Trust this key from now on")!.click());
    await settle();
    expect(api.decideCorrespondentKey).toHaveBeenCalledWith(
      "alice@example.org",
      C1_CONTACT_KEY_BOB_REMOVED.fingerprint,
      "trust",
    );
  });
});
