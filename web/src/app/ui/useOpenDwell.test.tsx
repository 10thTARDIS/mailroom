// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The explicit open signal (#842): reported once after the dwell, never on a
// quick flick, never twice for one message, and not while the page is hidden.
// Each "nothing is reported" case is paired with the case that differs from it
// in one respect and DOES report, so a hook that reported nothing at all would
// fail here rather than pass every negative.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { JSX } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { OPEN_DWELL_MS, resetOpenDwellForTests, useOpenDwell } from "./useOpenDwell";

let root: Root;
let host: HTMLElement;
const report = vi.fn();

function Probe(props: { id: string | null }): JSX.Element {
  useOpenDwell(props.id, report);
  return <></>;
}

function show(id: string | null): void {
  flushSync(() => root.render(<Probe id={id} />));
}

function setHidden(hidden: boolean): void {
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => (hidden ? "hidden" : "visible") });
}

beforeEach(() => {
  vi.useFakeTimers();
  resetOpenDwellForTests();
  report.mockReset();
  setHidden(false);
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});

afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
  vi.useRealTimers();
});

describe("useOpenDwell", () => {
  it("is four seconds", () => {
    expect(OPEN_DWELL_MS).toBe(4000);
  });

  it("reports a message once it has been open for the dwell", () => {
    show("m1");
    vi.advanceTimersByTime(OPEN_DWELL_MS - 1);
    expect(report).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(report).toHaveBeenCalledTimes(1);
    expect(report).toHaveBeenCalledWith("m1");
  });

  it("does not report a quick flick past a message, but does report the one the user stops on", () => {
    show("m1");
    vi.advanceTimersByTime(300);
    show("m2");
    vi.advanceTimersByTime(300);
    show("m3");
    vi.advanceTimersByTime(OPEN_DWELL_MS);
    expect(report).toHaveBeenCalledTimes(1);
    expect(report).toHaveBeenCalledWith("m3");
  });

  it("reports a message once per session, however often it is reopened", () => {
    show("m1");
    vi.advanceTimersByTime(OPEN_DWELL_MS);
    show(null);
    show("m1");
    vi.advanceTimersByTime(OPEN_DWELL_MS * 3);
    expect(report).toHaveBeenCalledTimes(1);
  });

  it("closing the pane before the dwell cancels it", () => {
    show("m1");
    vi.advanceTimersByTime(OPEN_DWELL_MS - 500);
    show(null);
    vi.advanceTimersByTime(OPEN_DWELL_MS);
    expect(report).not.toHaveBeenCalled();
  });

  it("does not count a page that was hidden, and counts the message when it is opened again visibly", () => {
    setHidden(true);
    show("m1");
    vi.advanceTimersByTime(OPEN_DWELL_MS);
    expect(report).not.toHaveBeenCalled();
    setHidden(false);
    show(null);
    show("m1");
    vi.advanceTimersByTime(OPEN_DWELL_MS);
    expect(report).toHaveBeenCalledTimes(1);
  });
});
