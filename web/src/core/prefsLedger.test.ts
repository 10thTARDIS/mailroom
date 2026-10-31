// SPDX-License-Identifier: AGPL-3.0-only

// The ordering rule behind #562, on its own: which server read may speak for a
// field this device just wrote. The end-to-end sequences (a poll answered
// before the second click, two clicks answered out of order) are in
// clientPreferences.race.test.ts; these pin the rule's edges.

import { describe, expect, it } from "vitest";

import type { ClientPreferences } from "./jmap/types";
import { PrefsLedger } from "./prefsLedger";

function rec(over: Partial<ClientPreferences> = {}): ClientPreferences {
  return {
    id: "CP-WEB",
    family: "web",
    theme: "system",
    density: "normal",
    messageOpens: "beside",
    paneSplitPct: 40,
    textSize: "m",
    rowLayout: "oneLine",
    ...over,
  };
}

describe("PrefsLedger.reconcile", () => {
  it("passes a record through untouched when nothing of this device's is outstanding", () => {
    const l = new PrefsLedger();
    const r = rec({ theme: "dark" });
    // Identity, not just equality: SERVER WINS AT LOAD, byte for byte.
    expect(l.reconcile(r, l.beginRead())).toBe(r);
  });

  it("holds a read sent before a write was answered to the value the user chose", () => {
    const l = new PrefsLedger();
    const w = l.beginWrite("web", { messageOpens: "beside" });
    const readAt = l.beginRead();
    l.settle("web", [w]);
    expect(l.reconcile(rec({ messageOpens: "focused" }), readAt).messageOpens).toBe("beside");
  });

  it("holds a read sent while the write is still unanswered", () => {
    const l = new PrefsLedger();
    l.beginWrite("web", { theme: "light" });
    expect(l.reconcile(rec({ theme: "dark" }), l.beginRead()).theme).toBe("light");
  });

  it("lets a read sent AFTER the write was answered speak for the field", () => {
    const l = new PrefsLedger();
    const w = l.beginWrite("web", { messageOpens: "beside" });
    l.settle("web", [w]);
    // Another device has since chosen focused; this read was sent after our
    // answer, so it saw our write and whatever came after it.
    expect(l.reconcile(rec({ messageOpens: "focused" }), l.beginRead()).messageOpens).toBe(
      "focused",
    );
  });

  it("holds back ONLY the fields being written — the rest of a stale read is still delivered", () => {
    const l = new PrefsLedger();
    l.beginWrite("web", { messageOpens: "beside" });
    const out = l.reconcile(rec({ messageOpens: "focused", theme: "dark" }), l.beginRead());
    expect(out.messageOpens).toBe("beside");
    expect(out.theme).toBe("dark");
  });

  it("keeps holding an OLDER read after a fresher one has spoken for the field", () => {
    // The accessibility lane's order: the poll's read was sent before the
    // second write, the write's own /get lands first, the poll's lands last.
    const l = new PrefsLedger();
    const stale = l.beginRead();
    const w = l.beginWrite("web", { messageOpens: "beside" });
    const own = l.beginRead();
    expect(l.reconcile(rec({ messageOpens: "beside" }), own, [w]).messageOpens).toBe("beside");
    l.settle("web", [w]);
    expect(l.reconcile(rec({ messageOpens: "focused" }), stale).messageOpens).toBe("beside");
  });

  it("an older write's answer does not settle a newer write of the same field", () => {
    const l = new PrefsLedger();
    const a = l.beginWrite("web", { messageOpens: "focused" });
    l.beginWrite("web", { messageOpens: "beside" });
    l.settle("web", [a]);
    // Sent after A's answer but while B is out: B still holds.
    expect(l.reconcile(rec({ messageOpens: "focused" }), l.beginRead()).messageOpens).toBe(
      "beside",
    );
  });

  it("the write's own /get is the server's word, normalisation included, for later-arriving older reads too", () => {
    const l = new PrefsLedger();
    const stale = l.beginRead();
    const w = l.beginWrite("web", { paneSplitPct: 37.4 });
    const own = l.beginRead();
    // The server stored 37, not 37.4: the confirming read says so...
    expect(l.reconcile(rec({ paneSplitPct: 37 }), own, [w]).paneSplitPct).toBe(37);
    l.settle("web", [w]);
    // ...and an older read held back afterwards is held to what it STORED.
    expect(l.reconcile(rec({ paneSplitPct: 40 }), stale).paneSplitPct).toBe(37);
  });

  it("a REFUSED write keeps the device's value against a read that raced it, and yields to the next", () => {
    // A refusal is an answer: it settles the write, and the record the server
    // sends afterwards is its word again — the same "until the next record" a
    // refusal always meant.
    const l = new PrefsLedger();
    const w = l.beginWrite("web", { density: "relaxed" });
    const raced = l.beginRead();
    l.settle("web", [w]); // refused: nothing is put for it
    expect(l.reconcile(rec({ density: "normal" }), raced).density).toBe("relaxed");
    expect(l.reconcile(rec({ density: "normal" }), l.beginRead()).density).toBe("normal");
  });

  it("does not touch another client family's row", () => {
    const l = new PrefsLedger();
    l.beginWrite("web", { theme: "light" });
    const phone = rec({ id: "CP-PHONE", family: "phone", theme: "dark" });
    expect(l.reconcile(phone, l.beginRead())).toBe(phone);
  });
});
