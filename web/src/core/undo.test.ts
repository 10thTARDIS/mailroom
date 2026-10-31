// SPDX-License-Identifier: AGPL-3.0-only

// The undo log (#375 increment 6).
//
// Every rule here is one a user would feel the moment it broke, and none of
// them is visible from the call site:
//
//   * `take` is what makes an undo ONE-SHOT. If it left the entry behind, the
//     toast button and Ctrl+Z would each run the inverse — two restores, or a
//     re-file undone and then undone again.
//   * the CAP and the TTL are the only things stopping Ctrl+Z from reaching an
//     inverse that is no longer about the mail on screen.
//   * `extend` is the accessibility requirement: a deadline that runs out while
//     the reader is still on the toast is a deadline only fast readers get.
//
// Time is passed in everywhere, so none of this waits on a clock.

import { describe, expect, it } from "vitest";

import { UNDO_KEEP, UNDO_TTL_MS, UndoLog, undoSecondsLeft } from "./undo";

const T0 = 1_700_000_000_000;

function ok(): Promise<boolean> {
  return Promise.resolve(true);
}

describe("UndoLog", () => {
  it("offers the newest entry first", () => {
    const log = new UndoLog();
    log.push("first", ok, T0);
    const second = log.push("second", ok, T0 + 100);
    expect(log.latest(T0 + 200)?.id).toBe(second.id);
    expect(log.latest(T0 + 200)?.label).toBe("second");
  });

  it("keeps at most three, dropping the oldest", () => {
    const log = new UndoLog();
    const first = log.push("1", ok, T0);
    log.push("2", ok, T0);
    log.push("3", ok, T0);
    log.push("4", ok, T0);
    expect(log.size).toBe(UNDO_KEEP);
    // The one that fell off is unreachable by id, not merely un-latest.
    expect(log.take(first.id)).toBeNull();
  });

  it("stops offering an entry once its deadline passes", () => {
    const log = new UndoLog();
    const entry = log.push("archive", ok, T0);
    expect(log.latest(T0 + UNDO_TTL_MS - 1)?.id).toBe(entry.id);
    expect(log.latest(T0 + UNDO_TTL_MS)).toBeNull();
  });

  it("steps over an expired entry to reach a live one behind it", () => {
    const log = new UndoLog();
    // A long-lived entry (a send's hold window), then a short one that has
    // since lapsed. The lapsed one must not shadow the live one.
    const held = log.push("send", ok, T0, 60_000);
    log.push("archive", ok, T0);
    expect(log.latest(T0 + 20_000)?.id).toBe(held.id);
  });

  it("latest() does not consume what it returns", () => {
    const log = new UndoLog();
    const entry = log.push("archive", ok, T0);
    expect(log.latest(T0)?.id).toBe(entry.id);
    expect(log.latest(T0)?.id).toBe(entry.id);
    expect(log.size).toBe(1);
  });

  it("take() removes the entry, so an undo runs exactly once", () => {
    const log = new UndoLog();
    const entry = log.push("archive", ok, T0);
    expect(log.take(entry.id)?.label).toBe("archive");
    expect(log.take(entry.id)).toBeNull();
    expect(log.latest(T0)).toBeNull();
    expect(log.size).toBe(0);
  });

  it("take() of an id that was never pushed is null, not a throw", () => {
    const log = new UndoLog();
    log.push("archive", ok, T0);
    expect(log.take(9999)).toBeNull();
    expect(log.size).toBe(1);
  });

  it("extend() keeps an entry alive past its original deadline", () => {
    const log = new UndoLog();
    const entry = log.push("archive", ok, T0);
    // The pointer is over the toast at 7 s, so the tick pushes the deadline out.
    log.extend(entry.id, T0 + 7_000);
    expect(log.latest(T0 + UNDO_TTL_MS + 1)?.id).toBe(entry.id);
    expect(log.latest(T0 + 7_000 + UNDO_TTL_MS)).toBeNull();
  });

  it("extend() leaves the other entries' deadlines alone", () => {
    const log = new UndoLog();
    const held = log.push("a", ok, T0);
    const other = log.push("b", ok, T0);
    log.extend(other.id, T0 + 7_000);
    // `a` still expires on its own schedule; only `b` was being read.
    expect(log.take(held.id)?.expiresAt).toBe(T0 + UNDO_TTL_MS);
  });

  it("extend() of an entry the log no longer holds does nothing", () => {
    const log = new UndoLog();
    const entry = log.push("archive", ok, T0);
    log.prune(T0 + UNDO_TTL_MS);
    log.extend(entry.id, T0 + UNDO_TTL_MS);
    expect(log.latest(T0 + UNDO_TTL_MS)).toBeNull();
    expect(log.size).toBe(0);
  });

  it("prune() drops the expired and keeps the rest", () => {
    const log = new UndoLog();
    log.push("short", ok, T0);
    const long = log.push("long", ok, T0, 60_000);
    log.prune(T0 + 30_000);
    expect(log.size).toBe(1);
    expect(log.latest(T0 + 30_000)?.id).toBe(long.id);
  });

  it("pushing prunes first, so an expired entry never costs a slot", () => {
    const log = new UndoLog();
    log.push("old", ok, T0);
    const a = log.push("a", ok, T0 + UNDO_TTL_MS);
    const b = log.push("b", ok, T0 + UNDO_TTL_MS);
    const c = log.push("c", ok, T0 + UNDO_TTL_MS);
    expect(log.size).toBe(3);
    // All three of the recent ones survived: the lapsed entry was not competing
    // for the cap.
    for (const entry of [a, b, c]) expect(log.take(entry.id)).not.toBeNull();
  });

  it("hands back the very inverse it was given", async () => {
    const log = new UndoLog();
    let ran = 0;
    const entry = log.push("archive", async () => {
      ran += 1;
      return false;
    }, T0);
    const taken = log.take(entry.id);
    expect(await taken?.inverse()).toBe(false);
    expect(ran).toBe(1);
  });
});

describe("undoSecondsLeft", () => {
  it("shows the full window on the first frame", () => {
    expect(undoSecondsLeft(T0 + UNDO_TTL_MS, T0)).toBe(8);
  });

  it("counts down a second at a time and reaches zero only at the deadline", () => {
    expect(undoSecondsLeft(T0 + 8_000, T0 + 1)).toBe(8);
    expect(undoSecondsLeft(T0 + 8_000, T0 + 1_000)).toBe(7);
    expect(undoSecondsLeft(T0 + 8_000, T0 + 7_999)).toBe(1);
    expect(undoSecondsLeft(T0 + 8_000, T0 + 8_000)).toBe(0);
  });

  it("never counts up from a deadline already passed", () => {
    expect(undoSecondsLeft(T0, T0 + 60_000)).toBe(0);
  });
});
