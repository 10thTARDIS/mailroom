// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// SETTINGS → KEYS (#796), rendered for real against captured keys and a fake
// API: each key states its fingerprint in fours, its address, its dates and
// chips; the verbs follow the key's state and the server's keyManagement; a
// publish that cannot happen says why; retiring a bound key unbinds it in the
// same step; a backup sends the passphrase once and keeps nothing; refusals
// are sentences; the correspondents' list removes with a confirm.

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
  CAPTURED_AT,
  CONTACT_KEY_ALICE,
  CONTACT_KEY_CAROL_EXPIRED,
  CONTACTS_FOR_KEYS,
  EXPORT_RESPONSE,
  IDENTITY_READER,
  USER_KEYS_WKD,
  USER_KEY_BOUND,
  USER_KEY_REVOKED,
  USER_KEY_TEN_DAYS,
} from "../../../../core/keyFixtures.testdata";
import type { Identity, UserKey } from "../../../../core/jmap/types";
import type { KeysApi } from "../keys/api";
import { KeysBody, keysStateText } from "./KeysRow";
import type { KeysReading } from "./KeysRow";

const BOUND_IDENTITY: Identity = { ...IDENTITY_READER, signingKeyId: USER_KEY_BOUND.id };

type FakeApi = { [K in keyof KeysApi]: Mock };

function fakeApi(): FakeApi {
  return {
    generate: vi.fn(async () => ({ ok: true as const, value: { fingerprint: "F" } })),
    importOwn: vi.fn(async () => ({ ok: true as const, value: { alreadyStored: false } })),
    update: vi.fn(async () => ({ ok: true as const, value: {} })),
    extend: vi.fn(async () => ({ ok: true as const, value: { expiresAt: "2027-10-21T09:50:22Z" } })),
    replace: vi.fn(),
    revoke: vi.fn(async () => ({ ok: true as const, value: undefined })),
    exportKey: vi.fn(async () => ({ ok: true as const, value: { armoredKey: EXPORT_RESPONSE.armoredKey, fileName: EXPORT_RESPONSE.fileName } })),
    readCorrespondents: vi.fn(async () => ({ keys: [CONTACT_KEY_ALICE, CONTACT_KEY_CAROL_EXPIRED], contacts: CONTACTS_FOR_KEYS })),
    removeCorrespondentKey: vi.fn(async () => ({ ok: true as const, value: undefined })),
    correspondentKeyHistory: vi.fn(async () => ({ ok: true as const, value: [] })),
    refreshCorrespondentKey: vi.fn(),
    decideCorrespondentKey: vi.fn(async () => ({ ok: true as const, value: undefined })),
    checkContactKey: vi.fn(),
    importContactKey: vi.fn(),
    afterChange: vi.fn(),
  };
}

let host: HTMLDivElement;
let root: Root;
let api: ReturnType<typeof fakeApi>;
const reload = vi.fn();
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

function loaded(keys: UserKey[], over: Partial<Extract<KeysReading, { kind: "loaded" }>> = {}): KeysReading {
  return { kind: "loaded", keys, correspondents: 2, ...USER_KEYS_WKD, ...over };
}

function render(reading: KeysReading, opts: { keyManagement?: boolean | null; identities?: Identity[] } = {}): void {
  flushSync(() =>
    root.render(
      <KeysBody
        reading={reading}
        identities={opts.identities ?? [BOUND_IDENTITY]}
        reload={reload}
        api={api as unknown as KeysApi}
        keyManagement={opts.keyManagement === undefined ? true : opts.keyManagement}
        warnDays={30}
        now={CAPTURED_AT}
      />,
    ),
  );
}

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  api = fakeApi();
  vi.clearAllMocks();
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

const cards = () => [...host.querySelectorAll<HTMLElement>(".own-key")];
const buttonIn = (el: ParentNode, label: string) =>
  [...el.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent === label);
const dialog = () => document.querySelector<HTMLElement>('[role="dialog"]');

function setValue(el: HTMLInputElement | HTMLSelectElement, value: string): void {
  const proto = el instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
  const setter = Object.getOwnPropertyDescriptor(proto, "value")!.set!;
  flushSync(() => {
    setter.call(el, value);
    el.dispatchEvent(new Event(el instanceof HTMLSelectElement ? "change" : "input", { bubbles: true }));
  });
}

describe("the state line", () => {
  it("says a key expires, warn-toned, from the server's flag (not from a client threshold)", () => {
    const facts = keysStateText(loaded([USER_KEY_BOUND, USER_KEY_TEN_DAYS]), 30, CAPTURED_AT);
    expect(facts).toContainEqual({ text: "a key expires 31 Oct", tone: "warn" });
    expect(keysStateText(loaded([USER_KEY_BOUND]), 30, CAPTURED_AT).some((f) => f.tone === "warn")).toBe(false);
    expect(keysStateText({ kind: "asking" })).toEqual([]);
  });
});

describe("your keys", () => {
  it("each key states its fingerprint in fours, address, dates and chips", () => {
    render(loaded([USER_KEY_TEN_DAYS, USER_KEY_BOUND]));
    const [bound, ten] = cards();
    expect(bound!.querySelector("h5")?.textContent).toBe("Key for reader@example.com");
    expect(bound!.textContent).toContain("5DFC 3469 CCE5 4FD9 59E8 B15A C64E 290E D3B2 1461");
    expect(bound!.textContent).toContain("Created 21 Oct 2026");
    expect(bound!.textContent).toContain("Expires 20 Oct 2028");
    expect(bound!.textContent).toContain("Signs as reader@example.com");
    expect(ten!.querySelector("h5")?.textContent).toBe("Key not used for any address");
    expect(ten!.textContent).toContain("Expires soon");
    expect(host.textContent).toContain("You're warned 30 days before a key expires.");
  });

  it("without key management, the crypto verbs are not drawn and the page says why", () => {
    render(loaded([USER_KEY_BOUND]), { keyManagement: false });
    const card = cards()[0]!;
    for (const verb of ["Extend…", "Back up…", "Revoke…"]) expect(buttonIn(card, verb), verb).toBeUndefined();
    expect(buttonIn(card, "Retire…")).toBeDefined();
    expect(buttonIn(host, "Make a new key…")).toBeUndefined();
    expect(host.textContent).toContain("can't make, import, extend, back up or revoke keys");
  });

  it("a revoked key is under History, and offers only a backup", () => {
    render(loaded([USER_KEY_REVOKED]));
    expect(cards()).toHaveLength(0);
    flushSync(() => host.querySelector<HTMLButtonElement>(".key-history-toggle")!.click());
    const card = cards()[0]!;
    expect([...card.querySelectorAll(".own-key-actions button")].map((b) => b.textContent)).toEqual(["Back up…"]);
    expect(card.textContent).toContain("Revoked on 21 Oct 2026");
    expect(card.querySelector("select")).toBeNull();
  });

  it("Publish is offered for the bound key and shows where it will be; for the unbound one it says why not", () => {
    render(loaded([USER_KEY_TEN_DAYS, USER_KEY_BOUND]));
    const [bound, ten] = cards();
    const pub = buttonIn(bound!, "Publish in the key directory")!;
    expect(pub.disabled).toBe(false);
    expect(bound!.textContent).toContain(USER_KEY_BOUND.wkdUrl!);
    const blocked = buttonIn(ten!, "Publish in the key directory")!;
    expect(blocked.disabled).toBe(true);
    expect(document.getElementById(blocked.getAttribute("aria-describedby")!)?.textContent).toBe(
      "Choose an address for this key before publishing it.",
    );
  });

  it("publishing sends wkdPublished:true, then reloads and says so", async () => {
    render(loaded([USER_KEY_BOUND]));
    flushSync(() => buttonIn(cards()[0]!, "Publish in the key directory")!.click());
    await settle();
    expect(api.update).toHaveBeenCalledWith(USER_KEY_BOUND.id, { wkdPublished: true });
    expect(reload).toHaveBeenCalled();
    expect(api.afterChange).toHaveBeenCalled();
    expect(host.querySelector(".setting-done")?.textContent).toBe("The key is published.");
  });

  it("a refused change is a sentence, not a code", async () => {
    api.update.mockResolvedValueOnce({ ok: false, error: "keyIsPublished" });
    render(loaded([USER_KEY_BOUND]));
    setValue(cards()[0]!.querySelector("select")!, "");
    flushSync(() => buttonIn(cards()[0]!, "Save")!.click());
    await settle();
    expect(api.update).toHaveBeenCalledWith(USER_KEY_BOUND.id, { identityId: null });
    expect(host.querySelector('[role="alert"]')?.textContent).toBe(
      "This key is published at its address. Withdraw it from the key directory first.",
    );
    expect(host.textContent).not.toContain("keyIsPublished");
  });

  it("retiring a bound key explains itself and unbinds in the same step", async () => {
    render(loaded([USER_KEY_BOUND]));
    flushSync(() => buttonIn(cards()[0]!, "Retire…")!.click());
    const d = dialog()!;
    expect(d.textContent).toContain("still be opened");
    expect(d.textContent).toContain("It stops being the key for reader@example.com.");
    expect(d.textContent).toContain("can't be undone");
    flushSync(() => buttonIn(d, "Retire key")!.click());
    await settle();
    expect(api.update).toHaveBeenCalledWith(USER_KEY_BOUND.id, { decryptOnly: true, identityId: null });
    await settle();
    expect(dialog()).toBeNull();
  });

  it("extend offers one or two years or custom days, and sends the days", async () => {
    render(loaded([USER_KEY_TEN_DAYS]));
    flushSync(() => buttonIn(cards()[0]!, "Extend…")!.click());
    const d = dialog()!;
    const custom = d.querySelector<HTMLInputElement>('input[type="radio"][value="custom"]')!;
    flushSync(() => custom.click());
    const days = d.querySelector<HTMLInputElement>(".key-days")!;
    setValue(days, "99999");
    expect(buttonIn(d, "Extend")!.disabled).toBe(true);
    expect(d.textContent).toContain("from 1 to 3650");
    setValue(days, "90");
    flushSync(() => buttonIn(d, "Extend")!.click());
    await settle();
    expect(api.extend).toHaveBeenCalledWith(USER_KEY_TEN_DAYS.id, 90);
    await settle();
    expect(host.querySelector(".setting-done")?.textContent).toBe("The key now expires on 21 Oct 2027.");
  });

  it("a backup needs 12 characters twice, sends the passphrase once, and keeps nothing", async () => {
    const saved: string[] = [];
    render(loaded([USER_KEY_BOUND]));
    flushSync(() => buttonIn(cards()[0]!, "Back up…")!.click());
    let d = dialog()!;
    const [p1, p2] = [...d.querySelectorAll<HTMLInputElement>('input[type="password"]')];
    expect(p1!.autocomplete).toBe("new-password");
    setValue(p1!, "short");
    setValue(p2!, "short");
    flushSync(() => buttonIn(d, "Download backup")!.click());
    await settle();
    expect(api.exportKey).not.toHaveBeenCalled();
    expect(d.querySelector('[role="alert"]')?.textContent).toBe("Use at least 12 characters.");
    setValue(p1!, "correct horse battery");
    setValue(p2!, "correct horse battery");
    // The save is the real one (a revoked Blob URL), observed through URL.
    const create = vi.spyOn(URL, "createObjectURL").mockImplementation(() => {
      saved.push("created");
      return "blob:x";
    });
    const revoke = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => saved.push("revoked"));
    flushSync(() => buttonIn(d, "Download backup")!.click());
    await settle();
    await settle();
    expect(api.exportKey).toHaveBeenCalledWith(USER_KEY_BOUND.id, "correct horse battery");
    expect(saved).toEqual(["created", "revoked"]);
    expect(dialog()).toBeNull();
    expect(host.querySelector(".setting-done")?.textContent).toContain("Downloaded mailroom-key-1a905815.asc");
    // Nothing of the passphrase or the key is left on the page: not in its
    // text, not in any field's value.
    const left = [
      document.body.textContent ?? "",
      ...[...document.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>("input, textarea")].map((i) => i.value),
    ].join("\n");
    expect(left).not.toContain("correct horse battery");
    expect(left).not.toContain("PRIVATE KEY");
    // Opened again, the dialog starts empty.
    flushSync(() => buttonIn(cards()[0]!, "Back up…")!.click());
    d = dialog()!;
    expect([...d.querySelectorAll<HTMLInputElement>('input[type="password"]')].map((i) => i.value)).toEqual(["", ""]);
    create.mockRestore();
    revoke.mockRestore();
  });

  it("the passphrase leaves the form the moment it is sent, before the server answers", async () => {
    let answer: (v: unknown) => void = () => {};
    api.exportKey.mockImplementationOnce(() => new Promise((r) => (answer = r)));
    render(loaded([USER_KEY_BOUND]));
    flushSync(() => buttonIn(cards()[0]!, "Back up…")!.click());
    const d = dialog()!;
    const [p1, p2] = [...d.querySelectorAll<HTMLInputElement>('input[type="password"]')];
    setValue(p1!, "correct horse battery");
    setValue(p2!, "correct horse battery");
    flushSync(() => buttonIn(d, "Download backup")!.click());
    await settle();
    expect(api.exportKey).toHaveBeenCalledTimes(1);
    expect([p1!.value, p2!.value]).toEqual(["", ""]);
    answer({ ok: false, error: "rateLimited" });
    await settle();
    expect(d.querySelector('[role="alert"]')?.textContent).toBe(
      "Too many key operations in a row. Wait a minute and try again.",
    );
  });

  it("generate lists fixed identities only and defaults to two years, bound", async () => {
    const wild = { ...IDENTITY_READER, id: "W", email: "*@example.com", isWildcard: true } as Identity;
    render(loaded([]), { identities: [wild, BOUND_IDENTITY] });
    flushSync(() => buttonIn(host, "Make a new key…")!.click());
    const d = dialog()!;
    const [identity, expiry] = [...d.querySelectorAll("select")];
    expect([...identity!.options].map((o) => o.textContent)).toEqual(["reader@example.com"]);
    expect(expiry!.value).toBe("2y");
    expect(d.querySelector<HTMLInputElement>('input[type="checkbox"]')!.checked).toBe(true);
    flushSync(() => buttonIn(d, "Make key")!.click());
    await settle();
    // Two years is the server's default: no expiry argument at all.
    expect(api.generate).toHaveBeenCalledWith(IDENTITY_READER.id, {}, true);
  });

  it("import clears the key and passphrase as soon as they are sent", async () => {
    api.importOwn.mockResolvedValueOnce({ ok: false, error: "invalidKey" });
    render(loaded([]));
    flushSync(() => buttonIn(host, "Import your own key…")!.click());
    const d = dialog()!;
    const area = d.querySelector("textarea")!;
    const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
    flushSync(() => {
      setter.call(area, "-----BEGIN PGP PRIVATE KEY BLOCK-----\nx\n-----END PGP PRIVATE KEY BLOCK-----");
      area.dispatchEvent(new Event("input", { bubbles: true }));
    });
    setValue(d.querySelector<HTMLInputElement>('input[type="password"]')!, "hunter2hunter2");
    flushSync(() => buttonIn(d, "Import")!.click());
    await settle();
    expect(api.importOwn).toHaveBeenCalledWith(
      "-----BEGIN PGP PRIVATE KEY BLOCK-----\nx\n-----END PGP PRIVATE KEY BLOCK-----",
      "hunter2hunter2",
    );
    await settle();
    expect(area.value).toBe("");
    expect(d.querySelector<HTMLInputElement>('input[type="password"]')!.value).toBe("");
    expect(d.querySelector('[role="alert"]')?.textContent).toBe(
      "That isn't a private key Mailroom can read, or the passphrase is wrong.",
    );
  });
});

describe("correspondents' keys", () => {
  it("lists each key with its source, expiry, pin, and removes one after a confirm", async () => {
    render(loaded([]));
    await settle();
    await settle();
    const rows = [...host.querySelectorAll<HTMLElement>(".correspondent-key")];
    expect(rows.map((r) => r.querySelector("strong")?.textContent)).toEqual(["alice@example.org", "carol@example.org"]);
    expect(rows[0]!.textContent).toContain("Imported by you");
    expect(rows[0]!.textContent).toContain("Expires 31 Oct 2026");
    expect(rows[0]!.textContent).toContain("Used for alice@example.org");
    expect(rows[1]!.textContent).toContain("Expired on 21 Oct 2026");
    const remove = buttonIn(rows[1]!, "Remove…")!;
    expect(remove.getAttribute("aria-label")).toBe("Remove the key for carol@example.org");
    flushSync(() => remove.click());
    const d = dialog()!;
    expect(d.textContent).toContain("can't encrypt to them until there is another");
    expect(api.removeCorrespondentKey).not.toHaveBeenCalled();
    flushSync(() => buttonIn(d, "Remove key")!.click());
    await settle();
    expect(api.removeCorrespondentKey).toHaveBeenCalledWith(CONTACT_KEY_CAROL_EXPIRED.id);
    await settle();
    expect(host.querySelector(".setting-done")?.textContent).toBe("The key for carol@example.org was removed.");
  });

  it("a server that cannot list them says so, never none", async () => {
    api.readCorrespondents.mockResolvedValue(null);
    render(loaded([]));
    await settle();
    expect(host.textContent).toContain("This server didn't list the keys it holds.");
    expect(host.textContent).not.toContain("You don't hold a key for anyone yet.");
  });
});
